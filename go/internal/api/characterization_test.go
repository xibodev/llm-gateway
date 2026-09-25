package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
	"llmgw/internal/router"

	codexauth "github.com/xibodev/llm-provider-auth/codex"
)

// Characterization tests pin the gateway's observable HTTP behaviour before
// the modularization programme moves code between modules (llm-gateway#68).
// Each golden file records what the client received and every request the
// gateway sent upstream. Regenerate with LLMGW_UPDATE_GOLDEN=1 only for an
// intended behaviour change, and review the diff.

const characterizationToken = "fixture-gateway-token"

type recordedCall struct {
	method string
	path   string
	body   []byte
}

type characterizationUpstream struct {
	mu                sync.Mutex
	calls             []recordedCall
	failover          string
	failNextCodexCall bool
}

func (u *characterizationUpstream) reset() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.calls = nil
}

func (u *characterizationUpstream) snapshot() []recordedCall {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]recordedCall(nil), u.calls...)
}

func (u *characterizationUpstream) countPath(path string) int {
	count := 0
	for _, call := range u.snapshot() {
		if call.path == path {
			count++
		}
	}
	return count
}

func (u *characterizationUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	u.mu.Lock()
	u.calls = append(u.calls, recordedCall{method: r.Method, path: r.URL.Path, body: body})
	failover := u.failover
	failCodex := u.failNextCodexCall && r.URL.Path == "/backend-api/codex/responses"
	if failCodex {
		u.failNextCodexCall = false
	}
	u.mu.Unlock()
	var payload map[string]any
	_ = json.Unmarshal(body, &payload)
	stream := payload["stream"] == true
	model, _ := payload["model"].(string)
	switch r.URL.Path {
	case "/f/models":
		writeJSON(w, http.StatusOK, map[string]any{"data": []any{
			map[string]any{"id": "chat-model", "supported_endpoints": []string{"/chat/completions"}},
			map[string]any{"id": "responses-model", "supported_endpoints": []string{"/chat/completions", "/responses"}},
		}})
	case "/a/models":
		writeJSON(w, http.StatusOK, map[string]any{"data": []any{map[string]any{"id": "model-a", "supported_endpoints": []string{"/chat/completions"}}}})
	case "/b/models":
		writeJSON(w, http.StatusOK, map[string]any{"data": []any{map[string]any{"id": "model-b", "supported_endpoints": []string{"/chat/completions"}}}})
	case "/f/chat/completions", "/b/chat/completions":
		writeCharacterizationChat(w, model, stream)
	case "/a/chat/completions":
		if failover == "before-output" {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]any{"message": "fixture unavailable", "type": "server_error"}})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"id\":\"chatcmpl_fixture\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Partial\"},\"finish_reason\":null}]}\n\n", model)
		w.(http.Flusher).Flush()
		panic(http.ErrAbortHandler)
	case "/f/responses":
		writeCharacterizationResponses(w, model, stream)
	case "/n/v1/models":
		writeJSON(w, http.StatusOK, map[string]any{"data": []any{map[string]any{
			"id": "claude-fixture", "type": "model", "display_name": "Claude Fixture", "created_at": "2025-01-01T00:00:00Z",
		}}, "has_more": false})
	case "/n/v1/messages":
		writeCharacterizationAnthropic(w, model, stream)
	case "/backend-api/codex/models":
		// Current upstream rows can omit the legacy supported_endpoints field.
		writeJSON(w, http.StatusOK, map[string]any{"data": []any{map[string]any{
			"id": "gpt-fixture", "owned_by": "openai", "description": "GPT Fixture",
			"supported_in_api": true, "visibility": "list",
		}}})
	case "/backend-api/codex/responses":
		if failCodex {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_fixture\",\"status\":\"in_progress\",\"output\":[]}}\n\n"+
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"Hello from codex\"}\n\n"+
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_fixture\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"gpt-fixture\",\"output\":[{\"type\":\"message\",\"id\":\"msg_fixture\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"Hello from codex\"}]}],\"usage\":{\"input_tokens\":3,\"output_tokens\":4,\"total_tokens\":7}}}\n\n")
	case "/oauth/token":
		writeJSON(w, http.StatusOK, map[string]any{"access_token": "fixture-access-rotated", "refresh_token": "fixture-refresh-rotated"})
	default:
		http.NotFound(w, r)
	}
}

func writeCharacterizationChat(w http.ResponseWriter, model string, stream bool) {
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := func(delta, finish, usage string) string {
			return fmt.Sprintf("data: {\"id\":\"chatcmpl_fixture\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":%s,\"finish_reason\":%s}]%s}\n\n", model, delta, finish, usage)
		}
		_, _ = fmt.Fprint(w, chunk(`{"role":"assistant","content":"Hello"}`, "null", "")+
			chunk(`{"content":" from chat"}`, "null", "")+
			chunk(`{}`, `"stop"`, `,"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}`)+
			"data: [DONE]\n\n")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": "chatcmpl_fixture", "object": "chat.completion", "created": 1700000000, "model": model,
		"choices": []any{map[string]any{"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": "Hello from chat"}}},
		"usage":   map[string]any{"prompt_tokens": 3, "completion_tokens": 4, "total_tokens": 7},
	})
}

func writeCharacterizationResponses(w http.ResponseWriter, model string, stream bool) {
	message := map[string]any{
		"id": "msg_fixture", "type": "message", "status": "completed", "role": "assistant",
		"content": []any{map[string]any{"type": "output_text", "text": "Hello from responses", "annotations": []any{}}},
	}
	completed := map[string]any{
		"id": "resp_fixture", "object": "response", "created_at": 1700000000, "status": "completed", "model": model,
		"output": []any{message}, "usage": map[string]any{"input_tokens": 3, "output_tokens": 4, "total_tokens": 7},
	}
	if !stream {
		writeJSON(w, http.StatusOK, completed)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	events := []map[string]any{
		{"type": "response.created", "response": map[string]any{"id": "resp_fixture", "object": "response", "created_at": 1700000000, "status": "in_progress", "model": model, "output": []any{}}},
		{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"id": "msg_fixture", "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}}},
		{"type": "response.output_text.delta", "item_id": "msg_fixture", "output_index": 0, "content_index": 0, "delta": "Hello"},
		{"type": "response.output_text.delta", "item_id": "msg_fixture", "output_index": 0, "content_index": 0, "delta": " from responses"},
		{"type": "response.output_text.done", "item_id": "msg_fixture", "output_index": 0, "content_index": 0, "text": "Hello from responses"},
		{"type": "response.output_item.done", "output_index": 0, "item": message},
		{"type": "response.completed", "response": completed},
	}
	for index, event := range events {
		event["sequence_number"] = index
		data, _ := json.Marshal(event)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], data)
	}
}

func writeCharacterizationAnthropic(w http.ResponseWriter, model string, stream bool) {
	if !stream {
		writeJSON(w, http.StatusOK, map[string]any{
			"id": "msg_fixture", "type": "message", "role": "assistant", "model": model,
			"content":     []any{map[string]any{"type": "text", "text": "Hello from messages"}},
			"stop_reason": "end_turn", "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": 3, "output_tokens": 4},
		})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	events := []string{
		fmt.Sprintf(`{"type":"message_start","message":{"id":"msg_fixture","type":"message","role":"assistant","model":%q,"content":[],"stop_reason":null,"usage":{"input_tokens":3,"output_tokens":0}}}`, model),
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" from messages"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":4}}`,
		`{"type":"message_stop"}`,
	}
	for _, event := range events {
		var decoded map[string]any
		_ = json.Unmarshal([]byte(event), &decoded)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", decoded["type"], event)
	}
}

type characterizationFixture struct {
	handler  http.Handler
	upstream *characterizationUpstream
	humanKey string
}

func setupCharacterization(t *testing.T) *characterizationFixture {
	t.Helper()
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	old := *config.Get()
	iam.ResetForTests()
	router.ResetSavingsState()
	router.ResetTelemetryState()
	upstream := &characterizationUpstream{}
	server := httptest.NewServer(upstream)
	// Registered before the cleanup below, so it restores after the server
	// has closed.
	providers.SetCodexEndpointsForTests(t, providers.CodexEndpoints{
		OAuth:            codexauth.Endpoints{OAuthTokenURL: server.URL + "/oauth/token"},
		ResponsesBaseURL: server.URL + "/backend-api/codex",
		ModelsURL:        server.URL + "/backend-api/codex/models",
	})
	t.Cleanup(func() {
		server.Close()
		providers.ResetProviders()
		iam.ResetForTests()
		router.ResetSavingsState()
		router.ResetTelemetryState()
		config.Update(func(s *config.Settings) { *s = old })
	})
	encryptionKey := make([]byte, 32)
	config.Update(func(s *config.Settings) {
		*s = *config.Defaults()
		s.APIKey = characterizationToken
		s.AllowUnauthenticatedAPI = false
		s.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(encryptionKey)
		s.OpenAICodexClientID = "fixture-client"
		s.Providers = map[string]*config.ProviderConfig{
			"fixture":   {Type: "openai_compatible", BaseURL: server.URL + "/f", APIKey: "fixture-upstream-token"},
			"fixture-a": {Type: "openai_compatible", BaseURL: server.URL + "/a", APIKey: "fixture-upstream-token"},
			"fixture-b": {Type: "openai_compatible", BaseURL: server.URL + "/b", APIKey: "fixture-upstream-token"},
			"native":    {Type: "anthropic", BaseURL: server.URL + "/n", APIKey: "fixture-upstream-token"},
			"codex":     {Type: "openai_compatible", RegistryID: "openai_codex"},
		}
		s.Endpoints = map[string]*config.EndpointConfig{
			"fixture-failover": {Failover: []config.EndpointMember{
				{Provider: "fixture-a", Model: "model-a"}, {Provider: "fixture-b", Model: "model-b"},
			}},
		}
		s.Policies.Defaults.RetryMaxAttempts = 1
		s.Policies.Defaults.CircuitFailureThreshold = 0
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	providers.ResetProviders()
	for _, provider := range []string{"fixture", "fixture-a", "fixture-b", "native"} {
		if rows := providers.RefreshCatalog(provider); len(rows) == 0 {
			t.Fatalf("%s catalog is empty", provider)
		}
	}
	human, err := iam.CreatePrincipal("human", "fixture:characterization", "", "Characterization owner")
	if err != nil {
		t.Fatal(err)
	}
	project, err := iam.CreateProject("fixture-characterization", "Characterization")
	if err != nil {
		t.Fatal(err)
	}
	if err := iam.SetMembership(project.ID, human.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
		PrincipalID: human.ID, ProviderID: "codex", Kind: "openai_codex_oauth", Source: iam.ConnectionSourceUser,
		AccessToken: "fixture-access", RefreshToken: "fixture-refresh", TokenType: "Bearer",
		ExpiresAt: time.Now().Add(time.Hour).Unix(), OAuthProfile: "device_client_id", OAuthClientID: "fixture-client",
	}); err != nil {
		t.Fatal(err)
	}
	issued, err := iam.IssueKey(iam.KeyCreate{ProjectID: project.ID, PrincipalID: human.ID, Name: "characterization"})
	if err != nil {
		t.Fatal(err)
	}
	// Model listing reads principal-scoped catalog caches without refreshing,
	// so warm the owner's view the way a console catalog sync would.
	owner := &config.Principal{PrincipalID: human.ID, PrincipalKind: "human", ProjectID: project.ID, Project: project.Slug}
	for _, provider := range []string{"fixture", "fixture-a", "fixture-b", "native", "codex"} {
		if rows := providers.RefreshCatalogForPrincipal(provider, callerOf(owner)); len(rows) == 0 {
			t.Fatalf("%s catalog for the human owner is empty", provider)
		}
	}
	upstream.reset()
	return &characterizationFixture{handler: NewServer(), upstream: upstream, humanKey: issued.Token}
}

func (f *characterizationFixture) do(method, path, token string, payload any) *httptest.ResponseRecorder {
	var body io.Reader
	if payload != nil {
		encoded, _ := json.Marshal(payload)
		body = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, path, body)
	request.Header.Set("Authorization", "Bearer "+token)
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, request)
	return recorder
}

func TestHTTPCharacterization(t *testing.T) {
	fixture := setupCharacterization(t)
	chat := func(model string, stream bool) map[string]any {
		return map[string]any{"model": model, "stream": stream, "messages": []any{map[string]any{"role": "user", "content": "Say hello"}}}
	}
	responses := func(model string, stream bool) map[string]any {
		return map[string]any{"model": model, "stream": stream, "input": "Say hello"}
	}
	messages := func(model string, stream bool) map[string]any {
		return map[string]any{"model": model, "stream": stream, "max_tokens": 64, "messages": []any{map[string]any{"role": "user", "content": "Say hello"}}}
	}
	cases := []struct {
		name, title, path string
		payload           map[string]any
	}{
		{"openai-chat-native", "Chat Completions, native OpenAI-compatible", "/v1/chat/completions", chat("fixture/chat-model", false)},
		{"openai-chat-native-stream", "Chat Completions stream, native OpenAI-compatible", "/v1/chat/completions", chat("fixture/chat-model", true)},
		{"openai-responses-native", "Responses, native OpenAI-compatible", "/v1/responses", responses("fixture/responses-model", false)},
		{"openai-responses-native-stream", "Responses stream, native OpenAI-compatible", "/v1/responses", responses("fixture/responses-model", true)},
		{"openai-responses-via-chat", "Responses translated to a Chat-only model", "/v1/responses", responses("fixture/chat-model", false)},
		{"openai-responses-via-chat-stream", "Responses stream translated to a Chat-only model", "/v1/responses", responses("fixture/chat-model", true)},
		{"openai-messages-via-chat", "Messages translated to a Chat-only model", "/v1/messages", messages("fixture/chat-model", false)},
		{"openai-messages-via-chat-stream", "Messages stream translated to a Chat-only model", "/v1/messages", messages("fixture/chat-model", true)},
		{"anthropic-messages-native", "Messages, native Anthropic", "/v1/messages", messages("native/claude-fixture", false)},
		{"anthropic-messages-native-stream", "Messages stream, native Anthropic", "/v1/messages", messages("native/claude-fixture", true)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture.upstream.reset()
			recorder := fixture.do(http.MethodPost, tc.path, characterizationToken, tc.payload)
			assertCharacterization(t, tc.name, renderExchange(tc.title, http.MethodPost+" "+tc.path, recorder, fixture.upstream.snapshot()))
		})
	}

	t.Run("failover-before-output-stream", func(t *testing.T) {
		fixture.upstream.reset()
		fixture.upstream.failover = "before-output"
		providers.ResetProviders()
		recorder := fixture.do(http.MethodPost, "/v1/chat/completions", characterizationToken, chat("fixture-failover", true))
		if fixture.upstream.countPath("/a/chat/completions") != 1 || fixture.upstream.countPath("/b/chat/completions") != 1 {
			t.Fatalf("failover before output must try both members once: %+v", fixture.upstream.snapshot())
		}
		assertCharacterization(t, "failover-before-output-stream", renderExchange(
			"First member fails before output; the second member serves", "POST /v1/chat/completions", recorder, fixture.upstream.snapshot()))
	})

	t.Run("failover-after-output-stream", func(t *testing.T) {
		fixture.upstream.reset()
		fixture.upstream.failover = "after-output"
		providers.ResetProviders()
		recorder := fixture.do(http.MethodPost, "/v1/chat/completions", characterizationToken, chat("fixture-failover", true))
		if fixture.upstream.countPath("/b/chat/completions") != 0 {
			t.Fatalf("failover after output reached the caller must not try the next member: %+v", fixture.upstream.snapshot())
		}
		assertCharacterization(t, "failover-after-output-stream", renderExchange(
			"First member fails after output; no failover", "POST /v1/chat/completions", recorder, fixture.upstream.snapshot()))
	})

	t.Run("codex-models", func(t *testing.T) {
		fixture.upstream.reset()
		recorder := fixture.do(http.MethodGet, "/v1/models", fixture.humanKey, nil)
		var listed struct {
			Data []map[string]any `json:"data"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &listed); err != nil {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		found := false
		for _, row := range listed.Data {
			if row["id"] != "codex/gpt-fixture" {
				continue
			}
			found = true
			supported, _ := row["supported_surfaces"].([]any)
			native, _ := row["native_surfaces"].([]any)
			if !slices.Contains(supported, any("/responses")) || !slices.Contains(native, any("/v1/responses")) {
				t.Fatalf("Codex row lost its Responses surface: %+v", row)
			}
		}
		if !found {
			t.Fatalf("Codex row missing from /v1/models: %s", recorder.Body.String())
		}
		assertCharacterization(t, "codex-models", renderExchange(
			"Model list for a human owner, including Codex rows without supported_endpoints", "GET /v1/models", recorder, fixture.upstream.snapshot()))
	})

	t.Run("codex-responses-native", func(t *testing.T) {
		fixture.upstream.reset()
		recorder := fixture.do(http.MethodPost, "/v1/responses", fixture.humanKey, responses("codex/gpt-fixture", false))
		assertCharacterization(t, "codex-responses-native", renderExchange(
			"Responses, native Codex", "POST /v1/responses", recorder, fixture.upstream.snapshot()))
	})

	t.Run("codex-chat-completions", func(t *testing.T) {
		fixture.upstream.reset()
		recorder := fixture.do(http.MethodPost, "/v1/chat/completions", fixture.humanKey, chat("codex/gpt-fixture", false))
		assertCharacterization(t, "codex-chat-completions", renderExchange(
			"Chat Completions to a Codex model", "POST /v1/chat/completions", recorder, fixture.upstream.snapshot()))
	})

	t.Run("codex-messages", func(t *testing.T) {
		fixture.upstream.reset()
		recorder := fixture.do(http.MethodPost, "/v1/messages", fixture.humanKey, messages("codex/gpt-fixture", false))
		assertCharacterization(t, "codex-messages", renderExchange(
			"Messages to a Codex model", "POST /v1/messages", recorder, fixture.upstream.snapshot()))
	})

	t.Run("codex-responses-refresh-replay", func(t *testing.T) {
		fixture.upstream.reset()
		fixture.upstream.mu.Lock()
		fixture.upstream.failNextCodexCall = true
		fixture.upstream.mu.Unlock()
		recorder := fixture.do(http.MethodPost, "/v1/responses", fixture.humanKey, responses("codex/gpt-fixture", false))
		if refreshes, attempts := fixture.upstream.countPath("/oauth/token"), fixture.upstream.countPath("/backend-api/codex/responses"); refreshes != 1 || attempts != 2 {
			t.Fatalf("401 must cause exactly one refresh and one replay: refreshes=%d attempts=%d status=%d body=%s", refreshes, attempts, recorder.Code, recorder.Body.String())
		}
		assertCharacterization(t, "codex-responses-refresh-replay", renderExchange(
			"Responses to Codex; upstream 401, one refresh, one replay", "POST /v1/responses", recorder, fixture.upstream.snapshot()))
	})
}

func assertCharacterization(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "characterization", name+".golden")
	if os.Getenv("LLMGW_UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("missing golden %s; generate it with LLMGW_UPDATE_GOLDEN=1: %v", path, err)
	}
	if strings.ReplaceAll(string(want), "\r\n", "\n") != got {
		t.Errorf("%s changed. If the change is intended, regenerate with LLMGW_UPDATE_GOLDEN=1 and review the diff.\n--- want\n%s\n--- got\n%s", path, want, got)
	}
}

var timingHeaders = map[string]bool{"X-Llmgw-Duration-Ms": true, "X-Llmgw-Fallback-Ms": true, "X-Llmgw-Ttfb-Ms": true}

func renderExchange(title, request string, recorder *httptest.ResponseRecorder, calls []recordedCall) string {
	var out strings.Builder
	fmt.Fprintf(&out, "# %s\nrequest: %s\nstatus: %d\n", title, request, recorder.Code)
	headers := recorder.Result().Header
	names := make([]string, 0, len(headers))
	for name := range headers {
		if name != "Date" && name != "Content-Length" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		value := strings.Join(headers.Values(name), ", ")
		if timingHeaders[name] {
			value = "<ms>"
		}
		fmt.Fprintf(&out, "header %s: %s\n", name, value)
	}
	for _, call := range calls {
		fmt.Fprintf(&out, "upstream: %s %s\n", call.method, call.path)
		if len(bytes.TrimSpace(call.body)) > 0 {
			fmt.Fprintf(&out, "upstream body: %s\n", normalizedJSON(call.body, false))
		}
	}
	out.WriteString("body:\n")
	body := strings.ReplaceAll(recorder.Body.String(), "\r\n", "\n")
	if strings.HasPrefix(headers.Get("Content-Type"), "text/event-stream") {
		var frames []string
		for _, frame := range strings.Split(body, "\n\n") {
			if strings.TrimSpace(frame) == "" {
				continue
			}
			lines := strings.Split(frame, "\n")
			for index, line := range lines {
				if data, ok := strings.CutPrefix(line, "data: "); ok && strings.HasPrefix(data, "{") {
					lines[index] = "data: " + normalizedJSON([]byte(data), false)
				}
			}
			frames = append(frames, strings.Join(lines, "\n"))
		}
		out.WriteString(strings.Join(frames, "\n\n") + "\n")
		return out.String()
	}
	out.WriteString(normalizedJSON([]byte(body), true) + "\n")
	return out.String()
}

var volatileTimeKeys = map[string]bool{
	"created": true, "created_at": true, "discovered_at": true, "verified_at": true,
	"expires_at": true, "observed_at": true, "refreshed_at": true,
}

// normalizedJSON renders JSON with sorted keys, replacing only values that
// legitimately differ between runs: timestamps and identifiers the gateway
// generates. Fixture identifiers contain "fixture" and are kept verbatim.
func normalizedJSON(raw []byte, indent bool) string {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return strings.TrimSpace(string(raw))
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if indent {
		encoder.SetIndent("", "  ")
	}
	_ = encoder.Encode(normalizeCharacterizationValue("", value))
	return strings.TrimRight(out.String(), "\n")
}

func normalizeCharacterizationValue(key string, value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for name, item := range typed {
			out[name] = normalizeCharacterizationValue(name, item)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for index, item := range typed {
			out[index] = normalizeCharacterizationValue(key, item)
		}
		return out
	case json.Number:
		if volatileTimeKeys[key] {
			return "<time>"
		}
		return typed
	case string:
		if volatileTimeKeys[key] && typed != "" {
			return "<time>"
		}
		if (key == "id" || strings.HasSuffix(key, "_id")) && typed != "" && !strings.Contains(typed, "fixture") {
			if separator := strings.IndexAny(typed, "-_"); separator > 0 {
				return "<generated " + typed[:separator+1] + ">"
			}
			return "<generated>"
		}
		return typed
	default:
		return typed
	}
}
