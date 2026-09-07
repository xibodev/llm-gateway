package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
	"llmgw/internal/router"
)

func setupAPISmokeFixture(t *testing.T, upstream http.Handler) http.Handler {
	t.Helper()
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	old := *config.Get()
	iam.ResetForTests()
	router.ResetSavingsState()
	router.ResetTelemetryState()
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
			"fixture": {Type: "openai_compatible", BaseURL: server.URL, APIKey: "fixture-upstream-token"},
		}
		s.Endpoints = map[string]*config.EndpointConfig{}
		s.Policies.Defaults.RetryMaxAttempts = 1
		s.Policies.Defaults.CircuitFailureThreshold = 0
	})
	providers.ResetProviders()
	return NewServer()
}

func apiSmokeRequest(handler http.Handler, path string, payload any) *httptest.ResponseRecorder {
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer fixture-gateway-token")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

func fixtureTool(id, arguments string) map[string]any {
	return map[string]any{"id": id, "type": "function", "function": map[string]any{
		"name": "add_fixture", "arguments": arguments,
	}}
}

func TestChatToolFinishNonstreamContract(t *testing.T) {
	valid := fixtureTool("call_fixture_a", `{"a":2,"b":3}`)
	missingID := fixtureTool("", `{}`)
	missingName := fixtureTool("call_fixture_b", `{}`)
	missingName["function"].(map[string]any)["name"] = ""
	missingArgs := fixtureTool("call_fixture_b", `{}`)
	delete(missingArgs["function"].(map[string]any), "arguments")
	wrongType := fixtureTool("call_fixture_b", `{}`)
	wrongType["type"] = "custom"
	tests := []struct {
		name   string
		tools  any
		reason any
		want   any
	}{
		{"complete function", []any{valid}, "stop", "tool_calls"},
		{"two complete functions", []any{valid, fixtureTool("call_fixture_b", `{}`)}, "stop", "tool_calls"},
		{"text only", nil, "stop", "stop"},
		{"empty tools", []any{}, "stop", "stop"},
		{"partial JSON", []any{fixtureTool("call_fixture_a", `{"a":`)}, "stop", "stop"},
		{"one partial call", []any{valid, fixtureTool("call_fixture_b", `{"a":`)}, "stop", "stop"},
		{"missing id", []any{missingID}, "stop", "stop"},
		{"missing name", []any{missingName}, "stop", "stop"},
		{"missing arguments", []any{missingArgs}, "stop", "stop"},
		{"empty arguments", []any{fixtureTool("call_fixture_a", "")}, "stop", "stop"},
		{"array arguments", []any{fixtureTool("call_fixture_a", `[]`)}, "stop", "stop"},
		{"null arguments", []any{fixtureTool("call_fixture_a", `null`)}, "stop", "stop"},
		{"wrong tool type", []any{wrongType}, "stop", "stop"},
		{"duplicate ids", []any{valid, valid}, "stop", "stop"},
		{"malformed entry", []any{valid, "bad"}, "stop", "stop"},
		{"malformed tools", map[string]any{"id": "call_fixture_a"}, "stop", "stop"},
		{"length", []any{valid}, "length", "length"},
		{"content filter", []any{valid}, "content_filter", "content_filter"},
		{"provider reason", []any{valid}, "provider_reason", "provider_reason"},
		{"already correct", []any{valid}, "tool_calls", "tool_calls"},
		{"missing reason", []any{valid}, nil, nil},
		{"null reason", []any{valid}, json.RawMessage(`null`), nil},
	}
	var response map[string]any
	handler := setupAPISmokeFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected upstream path %s", r.URL.Path)
		}
		writeJSON(w, 200, response)
	}))
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			message := map[string]any{"role": "assistant", "content": nil, "refusal": nil, "annotations": []any{}}
			if tc.tools != nil {
				message["tool_calls"] = tc.tools
			}
			choice := map[string]any{"index": 3, "message": message, "logprobs": nil, "provider_choice": "keep"}
			if tc.reason != nil {
				choice["finish_reason"] = tc.reason
			}
			response = map[string]any{
				"id": "chatcmpl_fixture", "object": "chat.completion", "created": 123, "model": "model",
				"choices":        []any{choice, map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "text"}, "finish_reason": "stop"}},
				"usage":          map[string]any{"prompt_tokens": 9, "completion_tokens": 4, "total_tokens": 13},
				"provider_extra": map[string]any{"trace": "fixture", "quota": 7}, "system_fingerprint": "fp_fixture",
			}
			w := apiSmokeRequest(handler, "/v1/chat/completions", map[string]any{"model": "fixture/model", "messages": []any{}})
			if w.Code != 200 {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if tc.reason != nil {
				choice["finish_reason"] = tc.want
			}
			want, _ := json.Marshal(response)
			var gotJSON, wantJSON any
			if json.Unmarshal(w.Body.Bytes(), &gotJSON) != nil || json.Unmarshal(want, &wantJSON) != nil || !reflect.DeepEqual(gotJSON, wantJSON) {
				t.Fatalf("fields changed: got=%s want=%s", w.Body.String(), want)
			}
		})
	}
}

func TestChatToolFinishStreamContract(t *testing.T) {
	chunk := func(choices ...any) string {
		b, _ := json.Marshal(map[string]any{"id": "chatcmpl_fixture", "object": "chat.completion.chunk", "choices": choices, "provider_extra": json.RawMessage(`{"large":9007199254740993}`)})
		return string(b)
	}
	delta := func(choice, index int, id, args string) map[string]any {
		tool := fixtureTool(id, args)
		tool["index"] = index
		return map[string]any{"index": choice, "delta": map[string]any{"tool_calls": []any{tool}}, "finish_reason": nil}
	}
	terminal := func(index int, reason any) map[string]any {
		return map[string]any{"index": index, "delta": map[string]any{}, "finish_reason": reason, "logprobs": nil}
	}
	chunks := []string{
		chunk(delta(3, 1, "call_fixture_b", `{"a":`), delta(0, 0, "call_fixture_a", `{}`)),
		chunk(map[string]any{"index": 3, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": 1, "function": map[string]any{"arguments": `2}`}}}}}),
		chunk(delta(3, 0, "call_fixture_c", `{}`), delta(2, 0, "call_fixture_partial", `{"a":`)),
		chunk(delta(4, 0, "call_fixture_length", `{}`), delta(5, 0, "call_fixture_filter", `{}`), delta(6, 0, "call_fixture_reason", `{}`)),
		chunk(terminal(1, "stop"), terminal(0, "stop"), terminal(3, "stop"), terminal(2, "stop")),
		chunk(terminal(4, "length"), terminal(5, "content_filter"), terminal(6, "provider_reason")),
		chunk(delta(7, 0, "call_fixture_missing_reason", `{}`)),
		chunk(terminal(7, nil)),
		chunk(terminal(0, "stop")),
		`{"object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":9,"completion_tokens":4,"total_tokens":13}}`,
	}
	handler := setupAPISmokeFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, data := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", data)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	// Repeat to ensure tool state cannot leak between requests.
	for attempt := 0; attempt < 2; attempt++ {
		w := apiSmokeRequest(handler, "/v1/chat/completions", map[string]any{"model": "fixture/model", "messages": []any{}, "stream": true})
		records := strings.Split(strings.TrimSuffix(w.Body.String(), "\n\n"), "\n\n")
		if w.Code != 200 || len(records) != len(chunks)+1 || records[len(chunks)] != "data: [DONE]" {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		for i, original := range chunks {
			var want, got map[string]json.RawMessage
			_ = json.Unmarshal([]byte(original), &want)
			if err := json.Unmarshal([]byte(strings.TrimPrefix(records[i], "data: ")), &got); err != nil {
				t.Fatal(err)
			}
			if i == 4 {
				var choices []map[string]json.RawMessage
				_ = json.Unmarshal(want["choices"], &choices)
				choices[1]["finish_reason"] = json.RawMessage(`"tool_calls"`)
				choices[2]["finish_reason"] = json.RawMessage(`"tool_calls"`)
				want["choices"], _ = json.Marshal(choices)
			}
			wantBytes, _ := json.Marshal(want)
			gotBytes, _ := json.Marshal(got)
			// RawMessage retains large numbers and all provider fields exactly.
			if string(wantBytes) != string(gotBytes) {
				t.Fatalf("chunk %d: got=%s want=%s", i, gotBytes, wantBytes)
			}
		}
	}
}

func TestChatToolFinishStreamRejectsAmbiguousOrMalformedDeltas(t *testing.T) {
	start := `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_fixture","type":"function","function":{"name":"add_fixture","arguments":"{}"}}]}}]}`
	stop := `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`
	for _, invalid := range []string{
		`{"choices":[{"delta":{"tool_calls":[]}}]}`,
		`{"choices":[{"index":null,"delta":{}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"function":{"arguments":"{}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":null}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":null}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":{}}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[null]}}]}`,
		`{"choices":[{"index":0,"delta":"bad"}]}`,
		`{"choices":["bad"]}`,
	} {
		t.Run(invalid, func(t *testing.T) {
			s := chatToolStream{choices: map[int]*chatToolChoice{}}
			if s.normalize(start) != start || s.normalize(invalid) != invalid || s.normalize(stop) != stop {
				t.Fatal("malformed/ambiguous tool stream was promoted or altered")
			}
		})
	}
	t.Run("bounded arguments", func(t *testing.T) {
		s := chatToolStream{choices: map[int]*chatToolChoice{}}
		large := strings.Replace(start, `"arguments":"{}"`, `"arguments":"{\"a\":\"`+strings.Repeat("a", 1<<20)+`\"}"`, 1)
		if s.normalize(large) != large || s.normalize(stop) != stop || !s.disabled || s.choices != nil {
			t.Fatal("oversized tracking state was retained or promoted")
		}
	})
}
