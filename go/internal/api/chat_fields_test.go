package api

import (
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
	"llmgw/internal/router"

	core "github.com/xibodev/llmgw-core"
)

// chatFieldUpstream serves an OpenAI-compatible provider under /o and a
// native Anthropic provider under /n, and keeps the last body each path
// received.
type chatFieldUpstream struct {
	mu     sync.Mutex
	bodies map[string]map[string]any
}

// take returns the last body received at path and forgets it, so a case
// never passes on a request an earlier one sent.
func (u *chatFieldUpstream) take(path string) map[string]any {
	u.mu.Lock()
	defer u.mu.Unlock()
	body := u.bodies[path]
	delete(u.bodies, path)
	return body
}

func (u *chatFieldUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	if r.Method == http.MethodPost {
		_ = json.Unmarshal(raw, &body)
		u.mu.Lock()
		u.bodies[r.URL.Path] = body
		u.mu.Unlock()
	}
	switch r.URL.Path {
	case "/o/models":
		writeJSON(w, http.StatusOK, map[string]any{"data": []any{
			map[string]any{"id": "chat-model", "supported_endpoints": []string{"/chat/completions"}},
		}})
	case "/o/chat/completions":
		if body["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, `data: {"id":"chatcmpl_fixture","object":"chat.completion.chunk","model":"chat-model","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}`+"\n\n"+
				`data: {"id":"chatcmpl_fixture","object":"chat.completion.chunk","model":"chat-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n"+
				"data: [DONE]\n\n")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id": "chatcmpl_fixture", "object": "chat.completion", "model": body["model"],
			"choices": []any{map[string]any{"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": "ok"}}},
			"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		})
	case "/n/v1/messages":
		writeJSON(w, http.StatusOK, map[string]any{
			"id": "msg_fixture", "type": "message", "role": "assistant", "model": body["model"],
			"content":     []any{map[string]any{"type": "text", "text": "ok"}},
			"stop_reason": "end_turn", "usage": map[string]any{"input_tokens": 1, "output_tokens": 1},
		})
	default:
		http.NotFound(w, r)
	}
}

func setupChatFieldFixture(t *testing.T) (http.Handler, *chatFieldUpstream) {
	t.Helper()
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	old := *config.Get()
	iam.ResetForTests()
	router.ResetSavingsState()
	router.ResetTelemetryState()
	upstream := &chatFieldUpstream{bodies: map[string]map[string]any{}}
	server := httptest.NewServer(upstream)
	t.Cleanup(func() {
		server.Close()
		providers.ResetProviders()
		iam.ResetForTests()
		router.ResetSavingsState()
		router.ResetTelemetryState()
		config.Update(func(s *config.Settings) { *s = old })
	})
	config.Update(func(s *config.Settings) {
		*s = *config.Defaults()
		s.APIKey = "fixture-gateway-token"
		s.AllowUnauthenticatedAPI = false
		s.Providers = map[string]*config.ProviderConfig{
			"openai": {Type: "openai_compatible", BaseURL: server.URL + "/o", APIKey: "fixture-upstream-token"},
			"native": {Type: "anthropic", BaseURL: server.URL + "/n", APIKey: "fixture-upstream-token"},
		}
		s.Endpoints = map[string]*config.EndpointConfig{"mixed": {Failover: []config.EndpointMember{
			{Provider: "native", Model: "claude-fixture"}, {Provider: "openai", Model: "chat-model"},
		}}}
		s.Policies.Defaults.RetryMaxAttempts = 1
		s.Policies.Defaults.CircuitFailureThreshold = 0
	})
	providers.InstallForTests(t)
	return NewServer(Runtime{}), upstream
}

// chatFields are fields a Chat client sets that the gateway does not read
// itself, a vendor's own top_k among them.
func chatFields() map[string]any {
	return map[string]any{
		"response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{
			"name": "answer", "schema": map[string]any{"type": "object"},
		}},
		"seed": 7.0, "n": 2.0, "parallel_tool_calls": false, "logprobs": true, "top_logprobs": 2.0,
		"user": "fixture-user", "top_k": 40.0,
	}
}

func chatFieldRequest(model string, stream bool, fields map[string]any) map[string]any {
	payload := map[string]any{
		"model": model, "stream": stream, "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	maps.Copy(payload, fields)
	return payload
}

// assertChatFieldsSent checks that body carries every field as the client
// set it and none of the gateway's own.
func assertChatFieldsSent(t *testing.T, body, fields map[string]any) {
	t.Helper()
	if body == nil {
		t.Fatal("the request did not reach the upstream")
	}
	for name, value := range fields {
		if !reflect.DeepEqual(body[name], value) {
			t.Errorf("%s=%#v, want %#v", name, body[name], value)
		}
	}
	for name := range body {
		switch {
		case strings.HasPrefix(name, "_"), name == "fallback_timeout_ms", name == "affinity_key", name == "force_api_support":
			t.Errorf("the gateway's own %q reached the upstream", name)
		}
	}
}

// errorMessage is the message of an OpenAI-shaped error body.
func errorMessage(body string) string {
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal([]byte(body), &envelope)
	return envelope.Error.Message
}

func TestChatFieldsReachANativeOpenAICompatibleUpstream(t *testing.T) {
	handler, upstream := setupChatFieldFixture(t)
	for _, stream := range []bool{false, true} {
		payload := chatFieldRequest("openai/chat-model", stream, chatFields())
		payload["fallback_timeout_ms"], payload["affinity_key"], payload["force_api_support"] = 30000, "fixture", false
		w := apiSmokeRequest(handler, "/v1/chat/completions", payload)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "ok") {
			t.Fatalf("stream=%v: status=%d body=%s", stream, w.Code, w.Body.String())
		}
		assertChatFieldsSent(t, upstream.take("/o/chat/completions"), chatFields())
	}
}

// Transparent mode serves an exact target whose catalog row confirms the
// surface with trusted evidence, and forwards the fields as the client set
// them as well.
func TestChatFieldsReachTheUpstreamInTransparentMode(t *testing.T) {
	handler, upstream := setupChatFieldFixture(t)
	trustCatalog(t, "openai")
	body, _ := json.Marshal(chatFieldRequest("openai/chat-model", false, chatFields()))
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	request.Header.Set("Authorization", "Bearer fixture-gateway-token")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(transportModeHeader, "transparent")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, request)
	if w.Code != http.StatusOK || w.Header().Get(transportModeHeader) != "transparent" {
		t.Fatalf("status=%d mode=%q body=%s", w.Code, w.Header().Get(transportModeHeader), w.Body.String())
	}
	assertChatFieldsSent(t, upstream.take("/o/chat/completions"), chatFields())
}

// trustCatalog discovers provider's catalog and stores its rows as reported
// by the upstream with high confidence, the evidence transparent mode needs,
// then installs a Runtime that reads them.
func trustCatalog(t *testing.T, provider string) {
	t.Helper()
	if rows := providers.RefreshCatalog(provider); len(rows) == 0 {
		t.Fatalf("%s catalog is empty", provider)
	}
	path := filepath.Join(config.StateDir(), "catalog.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var entries map[string]map[string]json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil || entries[provider] == nil {
		t.Fatalf("catalog entries=%s err=%v", raw, err)
	}
	var rows []providers.ModelInfo
	if err := json.Unmarshal(entries[provider]["models"], &rows); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.TypedCapabilities == nil {
			t.Fatalf("row %s has no typed capabilities", row.ID)
		}
		row.TypedCapabilities.Provenance = core.ModelCapabilityProvenance{
			Source: core.ModelCapabilitySourceUpstreamReported, Confidence: core.ModelCapabilityConfidenceHigh,
		}
	}
	entries[provider]["models"], _ = json.Marshal(rows)
	raw, _ = json.Marshal(entries)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	providers.InstallForTests(t)
}

// A field the Anthropic facade does not carry into Messages is refused
// before anything is sent, with a 400 naming it, while a value that asks
// for nothing is served.
func TestChatFieldsAnthropicCannotSendAreRefused(t *testing.T) {
	handler, upstream := setupChatFieldFixture(t)
	for _, tc := range []struct {
		field string
		value any
	}{
		{"response_format", map[string]any{"type": "json_object"}},
		{"logprobs", true},
		{"n", 2},
	} {
		for _, stream := range []bool{false, true} {
			w := apiSmokeRequest(handler, "/v1/chat/completions", chatFieldRequest("native/claude-fixture", stream, map[string]any{tc.field: tc.value}))
			if w.Code != http.StatusBadRequest || !strings.Contains(errorMessage(w.Body.String()), `"`+tc.field+`"`) {
				t.Fatalf("%s stream=%v: status=%d body=%s", tc.field, stream, w.Code, w.Body.String())
			}
			if sent := upstream.take("/n/v1/messages"); sent != nil {
				t.Fatalf("%s stream=%v: a refused request reached the upstream: %v", tc.field, stream, sent)
			}
		}
	}

	nothing := map[string]any{"n": 1, "logprobs": false, "response_format": map[string]any{"type": "text"}, "parallel_tool_calls": true}
	w := apiSmokeRequest(handler, "/v1/chat/completions", chatFieldRequest("native/claude-fixture", false, nothing))
	sent := upstream.take("/n/v1/messages")
	if w.Code != http.StatusOK || sent == nil {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	for name := range nothing {
		if _, found := sent[name]; found {
			t.Fatalf("%s reached the Messages body: %v", name, sent)
		}
	}
}

// An endpoint member that cannot send a field the request sets is skipped,
// as one capability filtering excludes is, and the next member serves.
func TestChatFieldsSkipAnEndpointMemberThatCannotSendThem(t *testing.T) {
	handler, upstream := setupChatFieldFixture(t)
	fields := map[string]any{"response_format": map[string]any{"type": "json_object"}}
	for _, stream := range []bool{false, true} {
		w := apiSmokeRequest(handler, "/v1/chat/completions", chatFieldRequest("mixed", stream, fields))
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"model":"chat-model"`) {
			t.Fatalf("stream=%v: status=%d body=%s", stream, w.Code, w.Body.String())
		}
		if sent := upstream.take("/n/v1/messages"); sent != nil {
			t.Fatalf("stream=%v: the skipped member was called: %v", stream, sent)
		}
		assertChatFieldsSent(t, upstream.take("/o/chat/completions"), fields)
	}

	w := apiSmokeRequest(handler, "/v1/chat/completions", chatFieldRequest("mixed", false, nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"model":"claude-fixture"`) || upstream.take("/n/v1/messages") == nil {
		t.Fatalf("without the field the first member serves: status=%d body=%s", w.Code, w.Body.String())
	}
}

// A name that begins with an underscore is the gateway's own kwarg, so a
// client's field with one is refused rather than taken as the gateway's.
func TestChatFieldsWithReservedNamesAreRefused(t *testing.T) {
	handler, upstream := setupChatFieldFixture(t)
	w := apiSmokeRequest(handler, "/v1/chat/completions", chatFieldRequest("openai/chat-model", false, map[string]any{"_affinity_key": "fixture"}))
	if w.Code != http.StatusBadRequest || !strings.Contains(errorMessage(w.Body.String()), `"_affinity_key"`) {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if sent := upstream.take("/o/chat/completions"); sent != nil {
		t.Fatalf("a refused request reached the upstream: %v", sent)
	}
}
