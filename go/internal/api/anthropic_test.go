package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
	"llmgw/internal/router"
)

func TestAnthropicKwargsMapsAdaptiveEffort(t *testing.T) {
	req := &anthropicRequest{
		Thinking:     map[string]any{"type": "adaptive"},
		OutputConfig: map[string]any{"effort": "xhigh"},
	}
	kw := anthropicKwargs(req)

	if got := kw["reasoning_effort"]; got != "xhigh" {
		t.Fatalf("reasoning_effort = %v, want xhigh", got)
	}
	if got, ok := kw["thinking"].(map[string]any); !ok || got["type"] != "adaptive" {
		t.Fatalf("thinking = %#v, want adaptive map", kw["thinking"])
	}
	if got, ok := kw["output_config"].(map[string]any); !ok || got["effort"] != "xhigh" {
		t.Fatalf("output_config = %#v, want xhigh map", kw["output_config"])
	}
}

func anthropicFixtureRequest(t *testing.T, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer gateway-token")
	recorder := httptest.NewRecorder()
	NewServer().ServeHTTP(recorder, req)
	return recorder
}

func TestAnthropicMessagesRejectsLossBeforeDispatch(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer upstream.Close()
	config.Update(func(s *config.Settings) {
		s.APIKey = "gateway-token"
		s.Providers = map[string]*config.ProviderConfig{"adapted": {Type: "openai_compatible", BaseURL: upstream.URL}}
	})
	providers.ResetProviders()
	t.Cleanup(func() {
		providers.ResetProviders()
		iam.ResetForTests()
		router.ResetTelemetryState()
		router.ResetSavingsState()
	})
	for _, stream := range []bool{false, true} {
		response := anthropicFixtureRequest(t, map[string]any{
			"model": "adapted/model", "max_tokens": 8, "stream": stream,
			"messages": []any{map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "document", "source": map[string]any{"type": "base64"}},
				map[string]any{"type": "thinking", "thinking": "secret"},
			}}}, "output_config": map[string]any{"effort": "high"},
		})
		if response.Code != http.StatusBadRequest {
			t.Fatalf("stream=%v status=%d body=%s", stream, response.Code, response.Body.String())
		}
		body := response.Body.String()
		if !strings.Contains(body, "messages.0.content.0, messages.0.content.1, output_config") {
			t.Fatalf("non-deterministic paths: %s", body)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("upstream calls=%d", calls.Load())
	}
}

func TestAnthropicMessagesRejectsMalformedNestedFields(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer upstream.Close()
	config.Update(func(s *config.Settings) {
		s.APIKey = "gateway-token"
		s.Providers = map[string]*config.ProviderConfig{"adapted": {Type: "openai_compatible", BaseURL: upstream.URL}}
	})
	providers.ResetProviders()
	t.Cleanup(func() {
		providers.ResetProviders()
		iam.ResetForTests()
		router.ResetTelemetryState()
		router.ResetSavingsState()
	})
	tests := []struct {
		name   string
		mutate func(map[string]any)
		path   string
	}{
		{"message unknown", func(p map[string]any) { p["messages"].([]any)[0].(map[string]any)["extra"] = true }, "messages.0.extra"},
		{"text cache", func(p map[string]any) {
			p["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["cache_control"] = map[string]any{}
		}, "messages.0.content.0.cache_control"},
		{"top cache", func(p map[string]any) { p["cache_control"] = true }, "cache_control"},
		{"system cache", func(p map[string]any) {
			p["system"] = []any{map[string]any{"type": "text", "text": "s", "cache_control": map[string]any{}}}
		}, "system.0.cache_control"},
		{"image cache", func(p map[string]any) {
			p["messages"].([]any)[0].(map[string]any)["content"] = []any{map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "AA=="}, "cache_control": true}}
		}, "messages.0.content.0.cache_control"},
		{"tool use cache", func(p map[string]any) {
			message := p["messages"].([]any)[0].(map[string]any)
			message["role"] = "assistant"
			message["content"] = []any{map[string]any{"type": "tool_use", "id": "x", "name": "x", "input": map[string]any{}, "cache_control": true}}
		}, "messages.0.content.0.cache_control"},
		{"tool result cache", func(p map[string]any) {
			p["messages"].([]any)[0].(map[string]any)["content"] = []any{map[string]any{"type": "tool_result", "tool_use_id": "x", "content": "x", "cache_control": true}}
		}, "messages.0.content.0.cache_control"},
		{"thinking", func(p map[string]any) {
			p["messages"].([]any)[0].(map[string]any)["content"] = []any{map[string]any{"type": "thinking"}}
		}, "messages.0.content.0"},
		{"redacted thinking", func(p map[string]any) {
			p["messages"].([]any)[0].(map[string]any)["content"] = []any{map[string]any{"type": "redacted_thinking"}}
		}, "messages.0.content.0"},
		{"document", func(p map[string]any) {
			p["messages"].([]any)[0].(map[string]any)["content"] = []any{map[string]any{"type": "document"}}
		}, "messages.0.content.0"},
		{"error result", func(p map[string]any) {
			p["messages"].([]any)[0].(map[string]any)["content"] = []any{map[string]any{"type": "tool_result", "tool_use_id": "x", "content": "bad", "is_error": true}}
		}, "messages.0.content.0"},
		{"bad source", func(p map[string]any) {
			p["messages"].([]any)[0].(map[string]any)["content"] = []any{map[string]any{"type": "image", "source": map[string]any{"type": "url", "media_type": "image/png", "data": "x", "extra": true}}}
		}, "messages.0.content.0.source.extra"},
		{"bad mime", func(p map[string]any) {
			p["messages"].([]any)[0].(map[string]any)["content"] = []any{map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/svg+xml", "data": "x"}}}
		}, "messages.0.content.0"},
		{"bad tool", func(p map[string]any) {
			p["tools"] = []any{map[string]any{"name": "x", "description": 1, "input_schema": "bad", "cache_control": true}}
		}, "tools.0.cache_control"},
		{"bad choice", func(p map[string]any) { p["tool_choice"] = map[string]any{"type": "auto", "name": "x", "extra": true} }, "tool_choice.extra"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			payload := map[string]any{"model": "adapted/model", "max_tokens": 8, "messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "hi"}}}}}
			tc.mutate(payload)
			response := anthropicFixtureRequest(t, payload)
			if response.Code != 400 || !strings.Contains(response.Body.String(), tc.path) {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("upstream calls=%d", calls.Load())
	}
}

func TestAnthropicMessagesNativeFailureFallsBackToCoreAdapter(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	var nativeCalls atomic.Int32
	native := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/messages" {
			nativeCalls.Add(1)
			http.Error(w, "temporary", http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, 200, map[string]any{"data": []any{map[string]any{"id": "native"}}})
	}))
	defer native.Close()
	var adapted map[string]any
	chat := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			writeJSON(w, 200, map[string]any{"data": []any{map[string]any{"id": "vision", "capabilities": map[string]any{"supports": map[string]any{"vision": true}}}}})
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&adapted); err != nil {
			t.Fatal(err)
		}
		writeJSON(w, 200, map[string]any{"id": "chat-id", "choices": []any{map[string]any{"message": map[string]any{"content": "ok"}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 4, "completion_tokens": 2}})
	}))
	defer chat.Close()
	config.Update(func(s *config.Settings) {
		s.APIKey = "gateway-token"
		s.Providers = map[string]*config.ProviderConfig{"native": {Type: "anthropic", BaseURL: native.URL}, "adapted": {Type: "openai_compatible", BaseURL: chat.URL}}
		s.Endpoints = map[string]*config.EndpointConfig{"claude": {Failover: []config.EndpointMember{{Provider: "native", Model: "native"}, {Provider: "adapted", Model: "vision"}}}}
		s.Policies.Defaults.RetryMaxAttempts = 1
	})
	providers.ResetProviders()
	if rows := providers.RefreshCatalog("adapted"); len(rows) != 1 {
		t.Fatalf("catalog=%#v", rows)
	}
	t.Cleanup(func() {
		providers.ResetProviders()
		iam.ResetForTests()
		router.ResetTelemetryState()
		router.ResetSavingsState()
	})
	response := anthropicFixtureRequest(t, map[string]any{
		"model": "claude", "max_tokens": 16, "stop_sequences": []any{"END"},
		"tools":       []any{map[string]any{"name": "lookup", "description": "find", "input_schema": map[string]any{"type": "object"}}},
		"tool_choice": map[string]any{"type": "tool", "name": "lookup"},
		"messages": []any{
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "call-1", "name": "lookup", "input": map[string]any{"q": "x"}}}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "call-1", "content": []any{map[string]any{"type": "text", "text": "found"}}},
				map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "AA=="}},
			}},
		},
	})
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if nativeCalls.Load() != 1 || adapted["model"] != "vision" || adapted["stop"] == nil || adapted["tool_choice"] == nil {
		t.Fatalf("native=%d adapted=%#v", nativeCalls.Load(), adapted)
	}
	messages := adapted["messages"].([]any)
	if len(messages) != 3 || !strings.Contains(response.Body.String(), `"input_tokens":4`) || !strings.Contains(response.Body.String(), `"stop_reason":"end_turn"`) {
		t.Fatalf("messages=%#v response=%s", messages, response.Body.String())
	}
}

func TestAnthropicNativeUsageNumbersPersistExactly(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"native","type":"message","content":[],"usage":{"input_tokens":9007199254740993,"output_tokens":17}}`))
	}))
	defer upstream.Close()
	config.Update(func(s *config.Settings) {
		s.APIKey = "gateway-token"
		s.Providers = map[string]*config.ProviderConfig{"native": {Type: "anthropic", BaseURL: upstream.URL}}
		s.Policies.Defaults.RetryMaxAttempts = 1
	})
	providers.ResetProviders()
	t.Cleanup(func() {
		providers.ResetProviders()
		iam.ResetForTests()
		router.ResetTelemetryState()
		router.ResetSavingsState()
	})
	response := anthropicFixtureRequest(t, map[string]any{"model": "native/model", "max_tokens": 1, "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	if response.Code != 200 {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	db, err := iam.DB()
	if err != nil {
		t.Fatal(err)
	}
	var input, output int64
	if err := db.QueryRow(`SELECT input_tokens, output_tokens FROM usage_events WHERE endpoint='anthropic.messages'`).Scan(&input, &output); err != nil {
		t.Fatal(err)
	}
	if input != 9007199254740993 || output != 17 {
		t.Fatalf("usage=%d/%d", input, output)
	}
}

func TestFirstIntSafeNumericTypes(t *testing.T) {
	for _, tc := range []struct {
		value any
		want  int
	}{{json.Number("42"), 42}, {json.Number("42.0"), 42}, {int64(43), 43}, {1.5, 0}, {json.Number("bad"), 0}, {json.Number("1e100"), 0}} {
		if got := firstInt(map[string]any{"n": tc.value}, "n"); got != tc.want {
			t.Errorf("firstInt(%v)=%d want=%d", tc.value, got, tc.want)
		}
	}
}

func TestAnthropicMessagesFailoverEligibility(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 503} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			t.Setenv("LLMGW_STATE_DIR", t.TempDir())
			var first, second atomic.Int32
			bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/messages" {
					first.Add(1)
					http.Error(w, "failure", status)
					return
				}
				writeJSON(w, 200, map[string]any{"data": []any{}})
			}))
			defer bad.Close()
			good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/messages" {
					second.Add(1)
					writeJSON(w, 200, map[string]any{"id": "ok", "type": "message", "content": []any{}, "usage": map[string]any{}})
					return
				}
				writeJSON(w, 200, map[string]any{"data": []any{}})
			}))
			defer good.Close()
			config.Update(func(s *config.Settings) {
				s.APIKey = "gateway-token"
				s.Providers = map[string]*config.ProviderConfig{"bad": {Type: "anthropic", BaseURL: bad.URL}, "good": {Type: "anthropic", BaseURL: good.URL}}
				s.Endpoints = map[string]*config.EndpointConfig{"route": {Failover: []config.EndpointMember{{Provider: "bad", Model: "a"}, {Provider: "good", Model: "b"}}}}
				s.Policies.Defaults.RetryMaxAttempts = 1
			})
			providers.ResetProviders()
			response := anthropicFixtureRequest(t, map[string]any{"model": "route", "max_tokens": 1, "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
			wantSecond, wantStatus := int32(0), status
			if status == 503 {
				wantSecond, wantStatus = 1, 200
			}
			if response.Code != wantStatus || first.Load() != 1 || second.Load() != wantSecond {
				t.Fatalf("status=%d calls=%d/%d body=%s", response.Code, first.Load(), second.Load(), response.Body.String())
			}
			providers.ResetProviders()
			iam.ResetForTests()
			router.ResetTelemetryState()
			router.ResetSavingsState()
		})
	}
}

func TestAnthropicMessagesImageRequiresVerifiedVision(t *testing.T) {
	for _, vision := range []any{nil, false} {
		t.Run(fmt.Sprint(vision), func(t *testing.T) {
			t.Setenv("LLMGW_STATE_DIR", t.TempDir())
			var completions atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/models" {
					model := map[string]any{"id": "model"}
					if vision != nil {
						model["capabilities"] = map[string]any{"supports": map[string]any{"vision": vision}}
					}
					writeJSON(w, 200, map[string]any{"data": []any{model}})
					return
				}
				completions.Add(1)
			}))
			defer upstream.Close()
			config.Update(func(s *config.Settings) {
				s.APIKey = "gateway-token"
				s.Providers = map[string]*config.ProviderConfig{"adapted": {Type: "openai_compatible", BaseURL: upstream.URL}}
			})
			providers.ResetProviders()
			providers.RefreshCatalog("adapted")
			response := anthropicFixtureRequest(t, map[string]any{"model": "adapted/model", "max_tokens": 1, "messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "AA=="}}}}}})
			if response.Code != 400 || completions.Load() != 0 {
				t.Fatalf("status=%d calls=%d body=%s", response.Code, completions.Load(), response.Body.String())
			}
			providers.ResetProviders()
			iam.ResetForTests()
			router.ResetTelemetryState()
			router.ResetSavingsState()
		})
	}
}
