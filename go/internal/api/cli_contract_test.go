package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
	"llmgw/internal/router"
)

type cliUpstreamCall struct {
	provider string
	path     string
	model    string
	marker   string
}

func setupCLIContractTest(t *testing.T) (*httptest.Server, func() []cliUpstreamCall) {
	t.Helper()
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	router.ResetSavingsState()
	router.ResetTelemetryState()
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var calls []cliUpstreamCall
	newUpstream := func(provider, model string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/models" {
				writeJSON(w, http.StatusOK, map[string]any{"data": []any{map[string]any{
					"id": model, "supported_endpoints": []string{"/chat/completions", "/responses"},
				}}})
				return
			}
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				http.Error(w, "invalid fixture JSON", http.StatusBadRequest)
				return
			}
			marker, shapeError := validateCLIUpstreamShape(r.URL.Path, payload)
			wireModel, _ := payload["model"].(string)
			mu.Lock()
			calls = append(calls, cliUpstreamCall{provider, r.URL.Path, wireModel, marker})
			mu.Unlock()
			if wireModel != model || shapeError != "" {
				http.Error(w, fmt.Sprintf("fixture contract: model=%q shape=%s", wireModel, shapeError), http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/chat/completions":
				writeFixtureChat(w, model, payload["stream"] == true)
			case "/responses":
				writeFixtureResponse(w, model, payload["stream"] == true)
			default:
				http.NotFound(w, r)
			}
		}))
	}
	upstreamA := newUpstream("fixture-a", "model-a")
	upstreamB := newUpstream("fixture-b", "model-b")
	config.Update(func(s *config.Settings) {
		s.APIKey = "gateway-token"
		s.APIKeys = nil
		s.AllowUnauthenticatedAPI = false
		s.Providers = map[string]*config.ProviderConfig{
			"fixture-a": {Type: "openai_compatible", BaseURL: upstreamA.URL, APIKey: "upstream-secret-do-not-leak"},
			"fixture-b": {Type: "openai_compatible", BaseURL: upstreamB.URL, APIKey: "upstream-secret-do-not-leak"},
		}
		s.Endpoints = map[string]*config.EndpointConfig{
			"coding": {Failover: []config.EndpointMember{{Provider: "fixture-b", Model: "model-b"}}},
		}
	})
	providers.ResetProviders()
	for _, provider := range []string{"fixture-a", "fixture-b"} {
		if rows := providers.RefreshCatalog(provider); len(rows) != 1 {
			t.Fatalf("%s catalog=%+v", provider, rows)
		}
	}
	gateway := httptest.NewServer(NewServer())
	t.Cleanup(func() {
		gateway.Close()
		upstreamA.Close()
		upstreamB.Close()
		iam.ResetForTests()
		router.ResetSavingsState()
		router.ResetTelemetryState()
		providers.ResetProviders()
	})
	return gateway, func() []cliUpstreamCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]cliUpstreamCall(nil), calls...)
	}
}

func validateCLIUpstreamShape(path string, payload map[string]any) (string, string) {
	tools, _ := payload["tools"].([]any)
	if len(tools) != 1 {
		return "", "one tool required"
	}
	tool, _ := tools[0].(map[string]any)
	if path == "/responses" {
		marker, _ := payload["input"].(string)
		parameters, _ := tool["parameters"].(map[string]any)
		if marker == "" || tool["type"] != "function" || tool["name"] != "lookup" || parameters["type"] != "object" {
			return marker, "Responses function tool changed"
		}
		return marker, ""
	}
	if path != "/chat/completions" {
		return "", "unexpected upstream path"
	}
	messages, _ := payload["messages"].([]any)
	if len(messages) != 1 {
		return "", "one Chat message required"
	}
	message, _ := messages[0].(map[string]any)
	marker, _ := message["content"].(string)
	function, _ := tool["function"].(map[string]any)
	parameters, _ := function["parameters"].(map[string]any)
	if marker == "" || message["role"] != "user" || tool["type"] != "function" || function["name"] != "lookup" || parameters["type"] != "object" {
		return marker, "Chat message or function tool changed"
	}
	return marker, ""
}

func writeFixtureChat(w http.ResponseWriter, model string, stream bool) {
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"id\":\"chatcmpl_fixture\",\"object\":\"chat.completion.chunk\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_fixture\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":\\\"fixture\\\"}\"}}]},\"finish_reason\":null}]}\n\ndata: {\"id\":\"chatcmpl_fixture\",\"object\":\"chat.completion.chunk\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n", model, model)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": "chatcmpl_fixture", "object": "chat.completion", "model": model,
		"choices": []any{map[string]any{"index": 0, "finish_reason": "tool_calls", "message": map[string]any{
			"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{
				"id": "call_fixture", "type": "function", "function": map[string]any{"name": "lookup", "arguments": `{"q":"fixture"}`},
			}},
		}}},
		"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
	})
}

func writeFixtureResponse(w http.ResponseWriter, model string, stream bool) {
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.output_item.added\",\"sequence_number\":0,\"output_index\":0,\"item\":{\"id\":\"fc_fixture\",\"type\":\"function_call\",\"status\":\"in_progress\",\"call_id\":\"call_fixture\",\"name\":\"lookup\",\"arguments\":\"\"}}\n\ndata: {\"type\":\"response.function_call_arguments.done\",\"sequence_number\":1,\"item_id\":\"fc_fixture\",\"output_index\":0,\"arguments\":\"{\\\"q\\\":\\\"fixture\\\"}\"}\n\ndata: {\"type\":\"response.completed\",\"sequence_number\":2,\"response\":{\"id\":\"resp_fixture\",\"object\":\"response\",\"status\":\"completed\",\"model\":%q,\"output\":[]}}\n\n", model)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": "resp_fixture", "object": "response", "status": "completed", "model": model,
		"output": []any{map[string]any{"id": "fc_fixture", "type": "function_call", "status": "completed", "call_id": "call_fixture", "name": "lookup", "arguments": `{"q":"fixture"}`}},
		"usage":  map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2},
	})
}

func cliTool(openAI bool) []any {
	tool := map[string]any{"name": "lookup", "parameters": map[string]any{"type": "object"}}
	if openAI {
		return []any{map[string]any{"type": "function", "function": tool}}
	}
	return []any{map[string]any{"name": "lookup", "input_schema": map[string]any{"type": "object"}}}
}

func TestCLIModelDiscoveryAuthenticationContract(t *testing.T) {
	server, _ := setupCLIContractTest(t)
	for _, tc := range []struct {
		name, apiKey, bearer string
		wantStatus           int
	}{
		{"claude x-api-key", "gateway-token", "", http.StatusOK},
		{"claude bearer", "", "gateway-token", http.StatusOK},
		{"copilot bearer", "", "gateway-token", http.StatusOK},
		{"x-api-key precedes bearer", "wrong", "gateway-token", http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/models", nil)
			req.Header.Set("x-api-key", tc.apiKey)
			if tc.bearer != "" {
				req.Header.Set("Authorization", "Bearer "+tc.bearer)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status=%d body=%s", resp.StatusCode, body)
			}
			if tc.wantStatus == http.StatusOK && (!bytes.Contains(body, []byte(`"id":"fixture-a/model-a"`)) || !bytes.Contains(body, []byte(`"id":"coding"`))) {
				t.Fatalf("models=%s", body)
			}
		})
	}
}

func TestCLIRequestRoutingAndShapeContract(t *testing.T) {
	server, readCalls := setupCLIContractTest(t)
	for _, tc := range []struct {
		name, path, model, marker, provider, wirePath, wireModel string
		body                                                     map[string]any
		want                                                     []string
	}{
		{"claude exact", "/v1/messages", "fixture-a/model-a", "claude-exact", "fixture-a", "/chat/completions", "model-a", map[string]any{"max_tokens": 32, "messages": []any{map[string]any{"role": "user", "content": "claude-exact"}}, "tools": cliTool(false)}, []string{`"type":"message"`, `"model":"model-a"`, `"type":"tool_use"`}},
		{"claude endpoint alias", "/messages", "coding", "claude-endpoint", "fixture-b", "/chat/completions", "model-b", map[string]any{"max_tokens": 32, "messages": []any{map[string]any{"role": "user", "content": "claude-endpoint"}}, "tools": cliTool(false)}, []string{`"type":"message"`, `"model":"model-b"`, `"type":"tool_use"`}},
		{"codex exact", "/v1/responses", "fixture-a/model-a", "codex-exact", "fixture-a", "/responses", "model-a", map[string]any{"input": "codex-exact", "tools": []any{map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}}}}, []string{`"object":"response"`, `"model":"model-a"`, `"type":"function_call"`}},
		{"codex endpoint alias", "/responses", "coding", "codex-endpoint", "fixture-b", "/responses", "model-b", map[string]any{"input": "codex-endpoint", "tools": []any{map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}}}}, []string{`"object":"response"`, `"model":"model-b"`, `"type":"function_call"`}},
		{"copilot exact", "/v1/chat/completions", "fixture-a/model-a", "copilot-exact", "fixture-a", "/chat/completions", "model-a", map[string]any{"messages": []any{map[string]any{"role": "user", "content": "copilot-exact"}}, "tools": cliTool(true)}, []string{`"object":"chat.completion"`, `"model":"model-a"`, `"tool_calls"`}},
		{"copilot endpoint alias", "/chat/completions", "coding", "copilot-endpoint", "fixture-b", "/chat/completions", "model-b", map[string]any{"messages": []any{map[string]any{"role": "user", "content": "copilot-endpoint"}}, "tools": cliTool(true)}, []string{`"object":"chat.completion"`, `"model":"model-b"`, `"tool_calls"`}},
		{"copilot responses", "/v1/responses", "coding", "copilot-responses", "fixture-b", "/responses", "model-b", map[string]any{"input": "copilot-responses", "tools": []any{map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}}}}, []string{`"object":"response"`, `"model":"model-b"`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.body["model"] = tc.model
			status, responseBody := cliRequest(t, server.URL+tc.path, tc.body)
			if status != http.StatusOK {
				t.Fatalf("status=%d body=%s", status, responseBody)
			}
			for _, want := range tc.want {
				if !bytes.Contains(responseBody, []byte(want)) {
					t.Fatalf("body=%s missing %s", responseBody, want)
				}
			}
			calls := readCalls()
			got := calls[len(calls)-1]
			if got != (cliUpstreamCall{tc.provider, tc.wirePath, tc.wireModel, tc.marker}) {
				t.Fatalf("upstream call=%+v", got)
			}
		})
	}

	status, body := cliRequest(t, server.URL+"/v1/messages/count_tokens", map[string]any{"model": "fixture-a/model-a", "messages": []any{map[string]any{"role": "user", "content": "count this"}}})
	if status != http.StatusOK || !bytes.Contains(body, []byte(`"input_tokens":`)) {
		t.Fatalf("count_tokens status=%d body=%s", status, body)
	}
}

func TestCLIStreamContract(t *testing.T) {
	server, _ := setupCLIContractTest(t)
	for _, tc := range []struct {
		name, path string
		body       map[string]any
		want       []string
	}{
		{"claude", "/v1/messages", map[string]any{"model": "fixture-a/model-a", "messages": []any{map[string]any{"role": "user", "content": "claude-stream"}}, "tools": cliTool(false), "max_tokens": 32, "stream": true}, []string{"event: message_start", `"type":"tool_use"`, "event: message_stop"}},
		{"codex", "/v1/responses", map[string]any{"model": "fixture-a/model-a", "input": "codex-stream", "tools": []any{map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}}}, "stream": true}, []string{"event: response.output_item.added", `"type":"function_call"`, "event: response.completed"}},
		{"copilot", "/v1/chat/completions", map[string]any{"model": "fixture-a/model-a", "messages": []any{map[string]any{"role": "user", "content": "copilot-stream"}}, "tools": cliTool(true), "stream": true}, []string{`"model":"model-a"`, `"tool_calls"`, "data: [DONE]"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := cliRequest(t, server.URL+tc.path, tc.body)
			if status != http.StatusOK {
				t.Fatalf("status=%d body=%s", status, body)
			}
			for _, want := range tc.want {
				if !bytes.Contains(body, []byte(want)) {
					t.Fatalf("body=%s missing %s", body, want)
				}
			}
		})
	}
}

func TestCLIErrorEnvelopeContract(t *testing.T) {
	server, _ := setupCLIContractTest(t)
	for _, tc := range []struct {
		name, path string
		body       map[string]any
	}{
		{"claude", "/v1/messages", map[string]any{"model": "missing/model", "messages": []any{}, "max_tokens": 1}},
		{"copilot", "/v1/chat/completions", map[string]any{"model": "missing/model", "messages": []any{map[string]any{"role": "user", "content": "error"}}}},
		{"codex", "/v1/responses", map[string]any{"model": "missing/model", "input": "error"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, raw := cliRequest(t, server.URL+tc.path, tc.body)
			var envelope map[string]any
			if err := json.Unmarshal(raw, &envelope); err != nil {
				t.Fatal(err)
			}
			errorBody, _ := envelope["error"].(map[string]any)
			message, _ := errorBody["message"].(string)
			if status != http.StatusNotFound || errorBody["type"] != "invalid_request_error" || errorBody["code"] != "404" ||
				!strings.Contains(message, `Model "missing/model"`) || !strings.Contains(message, "GET /v1/models") {
				t.Fatalf("status=%d envelope=%+v", status, envelope)
			}
			for _, unsafe := range []string{"upstream-secret-do-not-leak", "fixture-a", "fixture-b", "contract:"} {
				if strings.Contains(string(raw), unsafe) {
					t.Fatalf("unsafe error detail %q in %s", unsafe, raw)
				}
			}
		})
	}
}

func cliRequest(t *testing.T, url string, body map[string]any) (int, []byte) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer gateway-token")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	responseBody, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, responseBody
}
