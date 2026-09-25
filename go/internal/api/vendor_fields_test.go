package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
	"llmgw/internal/router"
)

// Gemini attaches a thought signature to each tool call and needs it back on
// the next turn. The gateway is stateless: the client keeps the history and
// replays it to whichever target serves the next turn, and no other surface
// has a place for a signature. These tests pin that a history or a response
// carrying one is still served across surfaces instead of being rejected.

// signatureToolCall carries a signature in each place a Gemini-backed
// provider puts one.
func signatureToolCall() map[string]any {
	return map[string]any{
		"id": "call_fixture", "type": "function",
		"function": map[string]any{
			"name": "lookup", "arguments": `{"q":"fixture"}`, "thought_signature": "fixture-signature",
		},
		"thought_signature": "fixture-signature",
		"extra_content": map[string]any{"google": map[string]any{
			"thought_signature": "fixture-signature",
		}},
	}
}

// vendorFieldUpstream serves a native Anthropic provider under /n, a
// Responses-only model under /r and a Chat model that answers with a
// signed tool call under /c, and keeps the last request body per path.
type vendorFieldUpstream struct {
	mu     sync.Mutex
	bodies map[string]map[string]any
}

// take returns the last body received at path and forgets it, so a subtest
// never passes on a request an earlier one sent.
func (u *vendorFieldUpstream) take(path string) map[string]any {
	u.mu.Lock()
	defer u.mu.Unlock()
	body := u.bodies[path]
	delete(u.bodies, path)
	return body
}

func (u *vendorFieldUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	u.mu.Lock()
	u.bodies[r.URL.Path] = body
	u.mu.Unlock()
	switch r.URL.Path {
	case "/n/v1/messages":
		writeJSON(w, http.StatusOK, map[string]any{
			"id": "msg_fixture", "type": "message", "role": "assistant", "model": body["model"],
			"content":     []any{map[string]any{"type": "text", "text": "ok"}},
			"stop_reason": "end_turn", "usage": map[string]any{"input_tokens": 1, "output_tokens": 1},
		})
	case "/r/models":
		writeJSON(w, http.StatusOK, map[string]any{"data": []any{
			map[string]any{"id": "responses-only", "supported_endpoints": []string{"/responses"}},
		}})
	case "/r/responses":
		writeJSON(w, http.StatusOK, map[string]any{
			"id": "resp_fixture", "object": "response", "status": "completed", "model": body["model"],
			"output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{
				map[string]any{"type": "output_text", "text": "ok"},
			}}},
		})
	case "/c/chat/completions":
		writeJSON(w, http.StatusOK, map[string]any{
			"id": "chatcmpl_fixture", "object": "chat.completion", "model": body["model"],
			"choices": []any{map[string]any{"index": 0, "finish_reason": "tool_calls", "message": map[string]any{
				"role": "assistant", "content": nil, "tool_calls": []any{signatureToolCall()},
			}}},
			"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
		})
	default:
		http.NotFound(w, r)
	}
}

func setupVendorFieldFixture(t *testing.T) (http.Handler, *vendorFieldUpstream) {
	t.Helper()
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	old := *config.Get()
	iam.ResetForTests()
	router.ResetSavingsState()
	router.ResetTelemetryState()
	upstream := &vendorFieldUpstream{bodies: map[string]map[string]any{}}
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
			"native": {Type: "anthropic", BaseURL: server.URL + "/n", APIKey: "fixture-upstream-token"},
			"adapt":  {Type: "openai_compatible", BaseURL: server.URL + "/r", APIKey: "fixture-upstream-token", ForceApiSupport: true},
			"chat":   {Type: "openai_compatible", BaseURL: server.URL + "/c", APIKey: "fixture-upstream-token"},
		}
		s.Endpoints = map[string]*config.EndpointConfig{}
		s.Policies.Defaults.RetryMaxAttempts = 1
		s.Policies.Defaults.CircuitFailureThreshold = 0
	})
	providers.ResetProviders()
	if rows := providers.RefreshCatalog("adapt"); len(rows) != 1 {
		t.Fatalf("adapt catalog=%+v", rows)
	}
	return NewServer(), upstream
}

func TestChatHistoryWithThoughtSignaturesIsServedByTranslatedTargets(t *testing.T) {
	handler, upstream := setupVendorFieldFixture(t)
	tools := []any{map[string]any{"type": "function", "function": map[string]any{
		"name": "lookup", "parameters": map[string]any{"type": "object"},
	}}}
	history := []any{
		map[string]any{"role": "user", "content": "Look it up"},
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{signatureToolCall()}},
		map[string]any{"role": "tool", "tool_call_id": "call_fixture", "content": "found"},
		map[string]any{"role": "user", "content": "Summarize"},
	}
	for _, tc := range []struct {
		name, model, path string
		stream            bool
	}{
		{"anthropic native", "native/claude-fixture", "/n/v1/messages", false},
		{"chat to responses", "adapt/responses-only", "/r/responses", false},
		{"chat to responses stream", "adapt/responses-only", "/r/responses", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := apiSmokeRequest(handler, "/v1/chat/completions", map[string]any{
				"model": tc.model, "stream": tc.stream, "messages": history, "tools": tools,
			})
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "ok") {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			sent, _ := json.Marshal(upstream.take(tc.path))
			if !strings.Contains(string(sent), "call_fixture") {
				t.Fatalf("the tool call did not reach the upstream: %s", sent)
			}
		})
	}
}

func TestChatResponseWithThoughtSignaturesIsServedToTranslatedClients(t *testing.T) {
	handler, _ := setupVendorFieldFixture(t)
	for _, tc := range []struct {
		name, path, want string
		payload          map[string]any
	}{
		{"anthropic messages", "/v1/messages", `"type":"tool_use"`, map[string]any{
			"model": "chat/gemini-fixture", "max_tokens": 64,
			"messages": []any{map[string]any{"role": "user", "content": "Look it up"}},
			"tools": []any{map[string]any{
				"name": "lookup", "description": "find", "input_schema": map[string]any{"type": "object"},
			}},
		}},
		{"responses", "/v1/responses", `"type":"function_call"`, map[string]any{
			"model": "chat/gemini-fixture", "input": "Look it up",
			"tools": []any{map[string]any{
				"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"},
			}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := apiSmokeRequest(handler, tc.path, tc.payload)
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), tc.want) ||
				!strings.Contains(w.Body.String(), "call_fixture") {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestChatSystemCacheControlReachesAnthropicAsSystemBlocks(t *testing.T) {
	handler, upstream := setupVendorFieldFixture(t)
	breakpoint := map[string]any{"type": "ephemeral"}
	stable := map[string]any{"type": "text", "text": "Stable instructions"}
	volatile := map[string]any{"type": "text", "text": "Volatile note"}
	cached := map[string]any{"type": "text", "text": "Stable instructions", "cache_control": breakpoint}
	for _, tc := range []struct {
		name   string
		system []any
		want   any
	}{
		{"breakpoint", []any{cached, volatile}, []any{cached, volatile}},
		// Without a breakpoint the system stays the string it always was.
		{"no breakpoint", []any{stable, volatile}, "Stable instructions\nVolatile note"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := apiSmokeRequest(handler, "/v1/chat/completions", map[string]any{
				"model": "native/claude-fixture",
				"messages": []any{
					map[string]any{"role": "system", "content": tc.system},
					map[string]any{"role": "user", "content": "hi"},
				},
			})
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if got := upstream.take("/n/v1/messages")["system"]; !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("system=%#v, want %#v", got, tc.want)
			}
		})
	}
}
