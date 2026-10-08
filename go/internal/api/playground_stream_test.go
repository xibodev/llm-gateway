package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
	"llmgw/internal/router"

	core "github.com/xibodev/llmgw-core"
)

type playgroundEvent struct {
	name string
	data map[string]any
}

// playgroundStreamFixture serves the playground of a project owner, signed in
// as "stream-owner", with the providers given and a route "fast" whose first
// member cannot be reached and whose second is echo.
func playgroundStreamFixture(t *testing.T, extra map[string]*config.ProviderConfig) (*httptest.Server, iam.Project, iam.Principal) {
	t.Helper()
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	providers.ResetProviders()
	router.ResetSavingsState()
	router.ResetTelemetryState()
	old := *config.Get()
	t.Cleanup(func() {
		iam.ResetForTests()
		providers.ResetProviders()
		router.ResetSavingsState()
		router.ResetTelemetryState()
		config.Update(func(s *config.Settings) { *s = old })
	})
	config.Update(func(s *config.Settings) {
		s.SSOEnabled, s.SSOSharedSecret, s.SSOAutoProvision = true, "proxy-secret", true
		s.Providers = map[string]*config.ProviderConfig{
			"echo":        {Type: "echo"},
			"unreachable": {Type: "openai_compatible", BaseURL: "http://127.0.0.1:1/v1"},
		}
		for id, provider := range extra {
			s.Providers[id] = provider
		}
		s.Endpoints = map[string]*config.EndpointConfig{"fast": {Failover: []config.EndpointMember{
			{Provider: "unreachable", Model: "model"}, {Provider: "echo", Model: "echo-default"},
		}}}
		s.Policies.Defaults = config.ProviderPolicy{RetryMaxAttempts: 1}
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	owner, err := iam.EnsurePrincipalBySubject("human", "authentik:stream-owner", "", "Stream Owner")
	if err != nil {
		t.Fatal(err)
	}
	project, err := iam.CreateProject("stream-project", "Stream Project")
	if err != nil {
		t.Fatal(err)
	}
	if err := iam.SetMembership(project.ID, owner.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(NewServer(Runtime{}))
	t.Cleanup(server.Close)
	return server, project, owner
}

// playgroundStreamRequest posts body to the signed-in owner's playground
// surface path, under ctx.
func playgroundStreamRequest(t *testing.T, ctx context.Context, server *httptest.Server, path string, body map[string]any) *http.Response {
	t.Helper()
	payload, _ := json.Marshal(body)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/user/api/playground"+path, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(ssoSecretHeader, "proxy-secret")
	request.Header.Set(ssoSubjectHeader, "stream-owner")
	request.Header.Set(ssoUsernameHeader, "stream-owner")
	request.Header.Set("Origin", server.URL)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

// nextPlaygroundEvent reads one server-sent event, or reports false at the
// end of the stream.
func nextPlaygroundEvent(t *testing.T, reader *bufio.Reader) (playgroundEvent, bool) {
	t.Helper()
	var event playgroundEvent
	for {
		line, err := reader.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(line, "event: "):
			event.name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event.data) != nil {
				t.Fatalf("event %q carries data that is not JSON: %s", event.name, line)
			}
		case line == "" && event.name != "":
			return event, true
		}
		if err != nil {
			return event, false
		}
	}
}

func readPlaygroundEvents(t *testing.T, response *http.Response) []playgroundEvent {
	t.Helper()
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	events := []playgroundEvent{}
	for {
		event, ok := nextPlaygroundEvent(t, reader)
		if !ok {
			return events
		}
		events = append(events, event)
	}
}

// A playground request that streams shows, on every text surface, the route
// members it tried as its stream opens, its text as it arrives, and then the
// answer as a request that did not stream returns it. It is metered and
// audited as the playground's other requests are.
func TestPlaygroundStreamsEveryTextSurface(t *testing.T) {
	server, project, owner := playgroundStreamFixture(t, nil)
	wantTrace := []any{
		map[string]any{"provider": "unreachable", "model": "model", "status": "failed"},
		map[string]any{"provider": "echo", "model": "echo-default", "status": "served"},
	}
	for _, test := range []struct {
		surface, path string
		body          map[string]any
		answer        func(map[string]any) string
	}{
		{"chat", "/v1/chat/completions", map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi chat"}}},
			func(raw map[string]any) string {
				return raw["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"].(string)
			}},
		{"responses", "/v1/responses", map[string]any{"input": "hi responses"},
			func(raw map[string]any) string {
				text := ""
				for _, item := range raw["output"].([]any) {
					for _, part := range item.(map[string]any)["content"].([]any) {
						text += part.(map[string]any)["text"].(string)
					}
				}
				return text
			}},
		{"messages", "/v1/messages", map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi messages"}}, "max_tokens": 64},
			func(raw map[string]any) string {
				return raw["content"].([]any)[0].(map[string]any)["text"].(string)
			}},
	} {
		t.Run(test.surface, func(t *testing.T) {
			test.body["project_id"], test.body["model"], test.body["stream"] = project.ID, "fast", true
			response := playgroundStreamRequest(t, context.Background(), server, test.path, test.body)
			if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
				body, _ := io.ReadAll(response.Body)
				t.Fatalf("status=%d type=%q body=%s", response.StatusCode, response.Header.Get("Content-Type"), body)
			}
			events := readPlaygroundEvents(t, response)
			if len(events) < 3 || events[0].name != "route" || events[len(events)-1].name != "done" {
				t.Fatalf("events %+v, want route, deltas and done", events)
			}
			route, done := events[0].data, events[len(events)-1].data
			served := map[string]any{"provider": "echo", "model": "echo-default"}
			if !reflect.DeepEqual(route["served"], served) || !reflect.DeepEqual(route["fallback_trace"], wantTrace) || route["surface"] != test.path {
				t.Fatalf("route %+v", route)
			}
			text := ""
			for _, event := range events[1 : len(events)-1] {
				if event.name != "delta" {
					t.Fatalf("event %+v between route and done", event)
				}
				text += event.data["text"].(string)
			}
			want := "echo:hi " + test.surface
			if text != want {
				t.Fatalf("streamed text %q, want %q", text, want)
			}
			if !reflect.DeepEqual(done["served"], served) || !reflect.DeepEqual(done["fallback_trace"], wantTrace) || done["project_id"] != project.ID {
				t.Fatalf("done %+v", done)
			}
			if answer := test.answer(done["raw_response"].(map[string]any)); answer != want {
				t.Fatalf("the assembled answer says %q, want %q", answer, want)
			}
		})
	}
	db, err := iam.DB()
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query("SELECT endpoint,status_code,COALESCE(project_id,''),COALESCE(principal_id,''),COALESCE(key_id,'') FROM usage_events ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	recorded := []string{}
	for rows.Next() {
		var endpoint, projectID, principalID, keyID string
		var status int
		if err := rows.Scan(&endpoint, &status, &projectID, &principalID, &keyID); err != nil {
			t.Fatal(err)
		}
		if status != http.StatusOK || projectID != project.ID || principalID != owner.ID || keyID != "" {
			t.Fatalf("%s recorded status=%d project=%q principal=%q key=%q", endpoint, status, projectID, principalID, keyID)
		}
		recorded = append(recorded, endpoint)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(recorded, []string{"playground.chat", "playground.responses", "playground.messages"}) {
		t.Fatalf("recorded usage %v", recorded)
	}
	var audited int
	if err := db.QueryRow("SELECT COUNT(*) FROM audit_events WHERE action='playground.execute' AND result='success' AND detail_json LIKE '%\"stream\":true%'").Scan(&audited); err != nil || audited != 3 {
		t.Fatalf("audited streams=%d err=%v", audited, err)
	}
}

// A stream the console stops ends there and is recorded as one its client
// left; a request no route member serves is refused before any stream opens.
func TestPlaygroundStreamStopsWithTheConsoleAndRefusesBeforeOpening(t *testing.T) {
	left := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"content":"first"},"finish_reason":null}]}`+"\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		left <- struct{}{}
	}))
	defer upstream.Close()
	server, project, _ := playgroundStreamFixture(t, map[string]*config.ProviderConfig{
		"slow": {Type: "openai_compatible", BaseURL: upstream.URL},
	})
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	response := playgroundStreamRequest(t, ctx, server, "/v1/chat/completions", map[string]any{
		"project_id": project.ID, "model": "slow/model", "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	reader := bufio.NewReader(response.Body)
	for _, want := range []string{"route", "delta"} {
		if event, ok := nextPlaygroundEvent(t, reader); !ok || event.name != want {
			t.Fatalf("event %+v, want %s", event, want)
		}
	}
	stop()
	_ = response.Body.Close()
	select {
	case <-left:
	case <-time.After(5 * time.Second):
		t.Fatal("the upstream stream went on after the console stopped it")
	}
	db, err := iam.DB()
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		var code string
		err := db.QueryRow("SELECT COALESCE(error_code,'') FROM usage_events WHERE endpoint='playground.chat' AND status_code=499").Scan(&code)
		if err == nil && code == "client_cancelled" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the stopped stream was not recorded: code=%q err=%v", code, err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	refused := playgroundStreamRequest(t, context.Background(), server, "/v1/chat/completions", map[string]any{
		"project_id": project.ID, "model": "unreachable/model", "stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	defer refused.Body.Close()
	var envelope struct {
		Error struct{ Message string } `json:"error"`
	}
	if refused.StatusCode < 500 || !strings.HasPrefix(refused.Header.Get("Content-Type"), "application/json") ||
		json.NewDecoder(refused.Body).Decode(&envelope) != nil || envelope.Error.Message == "" {
		t.Fatalf("a stream no member serves: status=%d type=%q", refused.StatusCode, refused.Header.Get("Content-Type"))
	}
}

// A request that does not stream shows the route members it tried on every
// text surface, as a Chat request always did.
func TestPlaygroundShowsTheRouteMembersTriedOnEverySurface(t *testing.T) {
	server, project, _ := playgroundStreamFixture(t, nil)
	for path, body := range map[string]map[string]any{
		"/v1/responses": {"input": "hi"},
		"/v1/messages":  {"messages": []any{map[string]any{"role": "user", "content": "hi"}}, "max_tokens": 64},
	} {
		body["project_id"], body["model"] = project.ID, "fast"
		status, response := ssoConnectionRequest(t, server.URL, "stream-owner", http.MethodPost, "/user/api/playground"+path, body)
		trace, _ := response["fallback_trace"].([]any)
		if status != http.StatusOK || len(trace) != 2 || trace[0].(map[string]any)["status"] != "failed" ||
			trace[1].(map[string]any)["provider"] != "echo" || trace[1].(map[string]any)["status"] != "served" {
			t.Fatalf("%s: status=%d trace=%+v", path, status, response["fallback_trace"])
		}
	}
}

// Each surface's stream is assembled into the answer the surface returns when
// it does not stream: tool calls and reasoning included, and a failure is
// named.
func TestPlaygroundAssemblersBuildTheSurfaceAnswer(t *testing.T) {
	chat := newPlaygroundAssembler(core.ModelSurfaceChatCompletions)
	for _, chunk := range []string{
		`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"think"}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"x\"}"}}]},"finish_reason":"tool_calls"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":4}}`,
	} {
		chat.consume("", chunk)
	}
	if piece := chat.consume("", "[DONE]"); !piece.completed {
		t.Fatal("[DONE] did not complete the chat stream")
	}
	message := chat.answer()["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	call := message["tool_calls"].([]any)[0].(map[string]any)
	if message["reasoning_content"] != "think" || call["id"] != "call_1" ||
		call["function"].(map[string]any)["arguments"] != `{"q":"x"}` || chat.answer()["usage"] == nil {
		t.Fatalf("chat answer %+v", chat.answer())
	}
	if piece := chat.consume("", `{"error":{"message":"Upstream provider stream failed."}}`); piece.failure != "Upstream provider stream failed." {
		t.Fatalf("chat failure %+v", piece)
	}

	messages := newPlaygroundAssembler(core.ModelSurfaceMessages)
	for _, event := range []string{
		`{"type":"message_start","message":{"id":"msg_1","model":"m","content":[],"usage":{"input_tokens":5}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"lookup","input":{}}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"q\":\"x\"}"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":7}}`,
	} {
		messages.consume("", event)
	}
	if piece := messages.consume("message_stop", `{"type":"message_stop"}`); !piece.completed {
		t.Fatal("message_stop did not complete the Messages stream")
	}
	answer := messages.answer()
	content := answer["content"].([]any)
	usage := answer["usage"].(map[string]any)
	if content[0].(map[string]any)["thinking"] != "hmm" || !reflect.DeepEqual(content[1].(map[string]any)["input"], map[string]any{"q": "x"}) ||
		answer["stop_reason"] != "tool_use" || usage["input_tokens"] != float64(5) || usage["output_tokens"] != float64(7) {
		t.Fatalf("messages answer %+v", answer)
	}

	responses := newPlaygroundAssembler(core.ModelSurfaceResponses)
	if piece := responses.consume("response.failed", `{"type":"response.failed","response":{"error":{"message":"overloaded"}}}`); piece.failure != "overloaded" {
		t.Fatalf("responses failure %+v", piece)
	}
}
