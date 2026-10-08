package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
)

// googleChatUpstream is a synthetic Gemini API: a stream answers with one
// function call event, as Gemini streams one, and any other call with the
// answer it holds. It records each body it receives.
func googleChatUpstream(t *testing.T) (func() []string, string) {
	t.Helper()
	var mu sync.Mutex
	var bodies []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, r.URL.Path+" "+string(body))
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, ":streamGenerateContent") {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `data: {"candidates": [{"content": {"parts": [{"functionCall": {"name": "lookup","args": {"q": "crane"}},`+
				`"thoughtSignature": "c2lnbmF0dXJl"}],"role": "model"},"finishReason": "STOP","index": 0}],`+
				`"usageMetadata": {"promptTokenCount": 11,"candidatesTokenCount": 5,"totalTokenCount": 16},"modelVersion": "gemini-fixture","responseId": "resp-fixture"}`+"\r\n\r\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"role":"model","parts":[{"text":"A crane is a bird."}]},"finishReason":"STOP"}],`+
			`"usageMetadata":{"promptTokenCount":11,"candidatesTokenCount":6,"totalTokenCount":17}}`)
	}))
	t.Cleanup(upstream.Close)
	old := config.Get().Providers
	t.Cleanup(func() { config.Update(func(s *config.Settings) { s.Providers = old }) })
	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{"studio": {Type: "ai_studio", BaseURL: upstream.URL, APIKey: "key"}}
		s.AllowUnauthenticatedAPI = true
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		taken := bodies
		bodies = nil
		return taken
	}, upstream.URL
}

const googleLookupTool = `"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}}]`

// A Chat stream to a Google model streams, its tools declared to Gemini,
// and Gemini's function call reaches the client as a tool call delta with
// its thought signature, the finish reason tool_calls, the usage the client
// asked for, which the gateway records, and [DONE].
func TestGoogleChatCompletionStreamsToolCalls(t *testing.T) {
	resetState(t)
	take, _ := googleChatUpstream(t)
	rec := httptest.NewRecorder()
	NewServer(Runtime{}).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(
		`{"model":"studio/gemini-fixture","stream":true,"stream_options":{"include_usage":true},`+
			`"messages":[{"role":"user","content":"What is a crane?"}],`+googleLookupTool+`,"tool_choice":"auto"}`))))
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status=%d type=%q body=%s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`"tool_calls":[{"extra_content":{"google":{"thought_signature":"c2lnbmF0dXJl"}},"function":{"arguments":"{\"q\":\"crane\"}","name":"lookup"},"id":"call_resp-fixture_0","index":0,"type":"function"}]`,
		`"finish_reason":"tool_calls"`, `"usage":{"completion_tokens":5,"prompt_tokens":11,"total_tokens":16}`, "data: [DONE]",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream lacks %s:\n%s", want, body)
		}
	}
	seen := take()
	if len(seen) != 1 || !strings.HasPrefix(seen[0], "/models/gemini-fixture:streamGenerateContent ") ||
		!strings.Contains(seen[0], `"tools":[{"functionDeclarations":[{"name":"lookup","parametersJsonSchema":`) {
		t.Fatalf("upstream = %q", seen)
	}
	db, err := iam.DB()
	if err != nil {
		t.Fatal(err)
	}
	var count, input, output int
	if err := db.QueryRow(`SELECT COUNT(*), input_tokens, output_tokens FROM usage_events`).Scan(&count, &input, &output); err != nil {
		t.Fatal(err)
	}
	if count != 1 || input != 11 || output != 5 {
		t.Fatalf("usage=(count=%d input=%d output=%d)", count, input, output)
	}
}

// A streamed Messages or Responses request to a Google model streams too,
// translated from the Chat stream: Gemini's function call reaches a Messages
// client as a tool_use block and a Responses client as a function call, and
// each stream ends as its protocol ends one.
func TestGoogleStreamsServeMessagesAndResponses(t *testing.T) {
	resetState(t)
	take, _ := googleChatUpstream(t)
	for _, call := range []struct {
		path, body string
		want       []string
	}{
		{"/v1/messages", `{"model":"studio/gemini-fixture","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"What is a crane?"}]}`, []string{
			`"content_block":{"id":"call_resp-fixture_0","input":{},"name":"lookup","type":"tool_use"}`, `"stop_reason":"tool_use"`, "event: message_stop",
		}},
		{"/v1/responses", `{"model":"studio/gemini-fixture","stream":true,"input":"What is a crane?"}`, []string{
			`"name":"lookup","status":"completed","type":"function_call"`, "event: response.completed",
		}},
	} {
		rec := httptest.NewRecorder()
		NewServer(Runtime{}).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, call.path, strings.NewReader(call.body)))
		if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/event-stream") {
			t.Fatalf("%s: status=%d body=%s", call.path, rec.Code, rec.Body.String())
		}
		for _, want := range call.want {
			if !strings.Contains(rec.Body.String(), want) {
				t.Fatalf("%s stream lacks %s:\n%s", call.path, want, rec.Body.String())
			}
		}
		if seen := take(); len(seen) != 1 || !strings.HasPrefix(seen[0], "/models/gemini-fixture:streamGenerateContent ") {
			t.Fatalf("%s upstream = %q", call.path, seen)
		}
	}
}

// A Chat request that calls tools is served by a Google model, and a tool
// choice Gemini cannot be sent is refused before anything reaches it.
func TestGoogleChatCompletionTakesTools(t *testing.T) {
	resetState(t)
	take, _ := googleChatUpstream(t)
	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		NewServer(Runtime{}).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
		return rec
	}
	rec := post(`{"model":"studio/gemini-fixture","messages":[{"role":"user","content":"What is a crane?"}],` + googleLookupTool + `,"tool_choice":"none"}`)
	var completion struct {
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &completion) != nil || len(completion.Choices) != 1 ||
		completion.Choices[0].Message.Content != "A crane is a bird." {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if seen := take(); len(seen) != 1 || !strings.Contains(seen[0], `"toolConfig":{"functionCallingConfig":{"mode":"NONE"}}`) {
		t.Fatalf("upstream = %q", seen)
	}
	rec = post(`{"model":"studio/gemini-fixture","messages":[{"role":"user","content":"hi"}],"tool_choice":"required"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "tool_choice") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if seen := take(); len(seen) != 0 {
		t.Fatalf("a refused request reached Google: %q", seen)
	}
}
