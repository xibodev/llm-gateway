package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
	"llmgw/internal/router"
)

func TestCanceledNonStreamingRequestsCancelUpstreamWithoutRetry(t *testing.T) {
	tests := []struct {
		name, path, providerType, registryID, body string
	}{
		{"OpenAI Chat", "/v1/chat/completions", "openai_compatible", "", `{"model":"cancel-route","messages":[{"role":"user","content":"hi"}]}`},
		{"OpenAI Responses", "/v1/responses", "openai_compatible", "openai", `{"model":"cancel-route","input":"hi"}`},
		{"Google Chat", "/v1/chat/completions", "ai_studio", "", `{"model":"cancel-route","messages":[{"role":"user","content":"hi"}]}`},
		{"adapted Claude", "/v1/messages", "openai_compatible", "", `{"model":"cancel-route","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			var calls atomic.Int32
			started := make(chan struct{}, 1)
			canceled := make(chan struct{}, 1)
			release := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				started <- struct{}{}
				select {
				case <-r.Context().Done():
					canceled <- struct{}{}
				case <-release:
				}
			}))
			t.Cleanup(func() { close(release) })
			defer upstream.Close()

			t.Setenv("LLMGW_STATE_DIR", t.TempDir())
			iam.ResetForTests()
			router.ResetSavingsState()
			router.ResetTelemetryState()
			t.Cleanup(func() {
				iam.ResetForTests()
				router.ResetSavingsState()
				router.ResetTelemetryState()
			})
			config.Update(func(settings *config.Settings) {
				settings.AllowUnauthenticatedAPI = true
				settings.APIKey = ""
				settings.Providers = map[string]*config.ProviderConfig{
					"upstream": {
						Type: testCase.providerType, RegistryID: testCase.registryID,
						BaseURL: upstream.URL, APIKey: "fixture",
					},
					"second": {
						Type: testCase.providerType, RegistryID: testCase.registryID,
						BaseURL: upstream.URL, APIKey: "fixture",
					},
				}
				settings.Endpoints = map[string]*config.EndpointConfig{
					"cancel-route": {Failover: []config.EndpointMember{
						{Provider: "upstream", Model: "model"},
						{Provider: "second", Model: "model"},
					}},
				}
				settings.Policies.Defaults.RetryMaxAttempts = 3
				settings.Policies.Defaults.RetryInitialBackoffSeconds = 0.01
			})
			providers.ResetProviders()
			t.Cleanup(providers.ResetProviders)

			ctx, cancel := context.WithCancel(context.Background())
			request := httptest.NewRequest(http.MethodPost, testCase.path, strings.NewReader(testCase.body)).WithContext(ctx)
			request.Header.Set("Content-Type", "application/json")
			done := make(chan struct{})
			go func() {
				NewServer().ServeHTTP(httptest.NewRecorder(), request)
				close(done)
			}()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("upstream request did not start")
			}
			cancel()
			select {
			case <-canceled:
			case <-time.After(time.Second):
				t.Fatal("upstream request context was not canceled")
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("gateway handler did not return promptly")
			}
			time.Sleep(25 * time.Millisecond)
			if got := calls.Load(); got != 1 {
				t.Fatalf("upstream calls = %d, want 1", got)
			}
		})
	}
}

type fixtureStreamIter struct {
	chunks []string
	err    error
	index  int
}

func (f *fixtureStreamIter) Next() (string, bool) {
	if f.index >= len(f.chunks) {
		return "", false
	}
	chunk := f.chunks[f.index]
	f.index++
	return chunk, true
}
func (f *fixtureStreamIter) Err() error   { return f.err }
func (f *fixtureStreamIter) Close() error { return nil }

func TestResponsesToChatRequestPreservesReasoningControls(t *testing.T) {
	rr := &responsesRequest{
		Model:           "gpt-5.6-sol",
		Input:           "hello",
		Instructions:    "be terse",
		MaxOutputTokens: 128,
		Reasoning:       map[string]any{"effort": "xhigh"},
		Temperature:     0.2,
		TopP:            0.9,
		Tools:           []any{map[string]any{"type": "function", "name": "lookup"}},
		ToolChoice:      "auto",
		Metadata:        map[string]any{"trace": "t1"},
	}
	req := responsesToChatRequest(rr)
	if req.ReasoningEffort != "xhigh" {
		t.Fatalf("reasoning_effort = %v, want xhigh", req.ReasoningEffort)
	}
	if req.MaxCompletionTokens != 128 || req.Temperature != 0.2 || req.TopP != 0.9 {
		t.Fatalf("request controls were not preserved: %+v", req)
	}
	if len(req.Messages) != 2 || req.Messages[0]["role"] != "system" ||
		req.Messages[1]["content"] != "hello" {
		t.Fatalf("messages = %+v", req.Messages)
	}
}

func TestChatResponseEnvelopeNormalization(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	router.ResetSavingsState()
	router.ResetTelemetryState()
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	streamChunks := []string{
		`{"id":"chatcmpl_real","created":1777777777,"model":"provider-model","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
		`{"id":"chatcmpl_real","created":1777777777,"model":"provider-model","choices":[{"index":3,"delta":{"content":"hello"}}],"provider_extra":{"trace":"keep"}}`,
		`{"id":"chatcmpl_real","object":null,"created":1777777777,"model":"provider-model","choices":[{"index":0,"delta":{"content":" null"}}]}`,
		`{"id":"chatcmpl_real","object":"","created":1777777777,"model":"provider-model","choices":[{"index":0,"delta":{"content":" empty"}}]}`,
		`{"id":"chatcmpl_real","object":"provider.chunk","created":1777777777,"model":"provider-model","choices":[{"index":4,"delta":{"content":" preserved"},"finish_reason":null}]}`,
		`{"id":"chatcmpl_real","created":1777777777,"model":"provider-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":2,"total_tokens":11},"copilot_usage":{"quota_snapshots":{"chat":7}}}`,
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			writeJSON(w, http.StatusOK, map[string]any{"data": []any{
				map[string]any{"id": "missing", "supported_endpoints": []string{"/chat/completions"}},
				map[string]any{"id": "empty", "supported_endpoints": []string{"/chat/completions"}},
				map[string]any{"id": "preserved", "supported_endpoints": []string{"/chat/completions"}},
				map[string]any{"id": "stream", "supported_endpoints": []string{"/chat/completions"}},
				map[string]any{"id": "adapted-exact", "supported_endpoints": []string{"/responses"}},
				map[string]any{"id": "adapted-route-model", "supported_endpoints": []string{"/responses"}},
			}})
			return
		}
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "invalid fixture request", http.StatusBadRequest)
			return
		}
		model, _ := request["model"].(string)
		switch r.URL.Path {
		case "/chat/completions":
			switch model {
			case "stream":
				w.Header().Set("Content-Type", "text/event-stream")
				for _, chunk := range streamChunks {
					_, _ = io.WriteString(w, "data: "+chunk+"\n\n")
				}
				_, _ = io.WriteString(w, "data: provider-heartbeat\n\ndata: [DONE]\n\n")
			case "missing":
				writeJSON(w, http.StatusOK, map[string]any{"model": "upstream", "choices": []any{
					map[string]any{"message": map[string]any{}}, map[string]any{"index": 8}, "provider-entry",
				}, "copilot_usage": map[string]any{"quota": "preserved"}})
			case "empty":
				writeJSON(w, http.StatusOK, map[string]any{"object": "", "choices": []any{map[string]any{}}})
			case "preserved":
				writeJSON(w, http.StatusOK, map[string]any{"object": "provider.chat", "choices": []any{map[string]any{"index": 11}}})
			default:
				http.Error(w, "adapted model reached chat", http.StatusBadRequest)
			}
		case "/responses":
			writeJSON(w, http.StatusOK, map[string]any{
				"id": "resp_fixture", "object": "response", "status": "completed", "model": model,
				"output": []any{map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": "ok"}}}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	config.Update(func(s *config.Settings) {
		s.APIKey = "gateway-token"
		s.AllowUnauthenticatedAPI = false
		s.Providers = map[string]*config.ProviderConfig{"fixture": {Type: "openai_compatible", BaseURL: upstream.URL, APIKey: "fixture"}}
		s.Endpoints = map[string]*config.EndpointConfig{
			"empty-route":   {Failover: []config.EndpointMember{{Provider: "fixture", Model: "empty"}}},
			"adapted-route": {Failover: []config.EndpointMember{{Provider: "fixture", Model: "adapted-route-model"}}},
		}
	})
	providers.ResetProviders()
	if models := providers.RefreshCatalog("fixture"); len(models) != 6 {
		t.Fatalf("catalog=%+v", models)
	}
	server := httptest.NewServer(NewServer())
	t.Cleanup(func() {
		server.Close()
		providers.ResetProviders()
		iam.ResetForTests()
		router.ResetSavingsState()
		router.ResetTelemetryState()
	})
	for _, tc := range []struct {
		name, requested, wantModel, wantObject string
		force, defect                          bool
		wantIndex                              float64
	}{
		{"exact native defaults", "fixture/missing", "missing", "chat.completion", false, true, 0},
		{"endpoint native empty object", "empty-route", "empty", "chat.completion", false, false, 0},
		{"exact native preserves", "fixture/preserved", "preserved", "provider.chat", false, false, 11},
		{"exact adapted", "fixture/adapted-exact", "adapted-exact", "chat.completion", true, false, 0},
		{"endpoint adapted", "adapted-route", "adapted-route-model", "chat.completion", true, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := map[string]any{"model": tc.requested, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
			if tc.force {
				payload["force_api_support"] = true
			}
			status, response := jsonRequest(t, server.URL+"/v1/chat/completions", http.MethodPost, "gateway-token", payload)
			choices, _ := response["choices"].([]any)
			if status != http.StatusOK || response["model"] != tc.wantModel || response["object"] != tc.wantObject || len(choices) == 0 || choices[0].(map[string]any)["index"] != tc.wantIndex {
				t.Fatalf("status=%d response=%+v", status, response)
			}
			if tc.defect {
				if choices[1].(map[string]any)["index"] != float64(8) || choices[2] != "provider-entry" || response["copilot_usage"].(map[string]any)["quota"] != "preserved" {
					t.Fatalf("provider fields changed: %+v", response)
				}
				if _, exists := response["id"]; exists || len(choices[0].(map[string]any)["message"].(map[string]any)) != 0 {
					t.Fatalf("unrequested fields synthesized: %+v", response)
				}
			}
		})
	}
	t.Run("stream chunks", func(t *testing.T) {
		status, body := cliRequest(t, server.URL+"/v1/chat/completions", map[string]any{
			"model": "fixture/stream", "stream": true,
			"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		})
		records := strings.Split(strings.TrimSuffix(string(body), "\n\n"), "\n\n")
		if status != http.StatusOK || len(records) != len(streamChunks)+2 ||
			records[len(streamChunks)] != "data: provider-heartbeat" ||
			records[len(records)-1] != "data: [DONE]" || strings.Count(string(body), "data: [DONE]\n\n") != 1 {
			t.Fatalf("status=%d stream=%q", status, body)
		}
		for i, raw := range streamChunks {
			var got, want map[string]any
			if !strings.HasPrefix(records[i], "data: ") || json.Unmarshal([]byte(strings.TrimPrefix(records[i], "data: ")), &got) != nil || json.Unmarshal([]byte(raw), &want) != nil {
				t.Fatalf("chunk %d is not JSON: %q", i, records[i])
			}
			if want["object"] == nil || want["object"] == "" {
				want["object"] = "chat.completion.chunk"
			}
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(want)
			if !bytes.Equal(gotJSON, wantJSON) {
				t.Fatalf("chunk %d changed: got=%s want=%s", i, gotJSON, wantJSON)
			}
		}
	})
}

func setupResponsesAPITest(t *testing.T) *httptest.Server {
	t.Helper()
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	router.ResetSavingsState()
	router.ResetTelemetryState()
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	config.Update(func(s *config.Settings) {
		s.APIKey = "test-secret"
		s.AllowUnauthenticatedAPI = false
		s.Providers = map[string]*config.ProviderConfig{"echo": {Type: "echo"}}
		s.Endpoints = map[string]*config.EndpointConfig{}
	})
	providers.ResetProviders()
	server := httptest.NewServer(NewServer())
	t.Cleanup(func() {
		server.Close()
		iam.ResetForTests()
		router.ResetSavingsState()
		router.ResetTelemetryState()
		providers.ResetProviders()
	})
	return server
}

func TestResponsesEndpointReturnsResponsesEnvelope(t *testing.T) {
	server := setupResponsesAPITest(t)
	status, response := jsonRequest(
		t, server.URL+"/v1/responses", http.MethodPost, "test-secret",
		map[string]any{
			"model": "echo/echo-default", "input": "hello",
			"max_output_tokens": 8,
		},
	)
	if status != http.StatusOK || response["object"] != "response" ||
		response["status"] != "completed" {
		t.Fatalf("status=%d response=%+v", status, response)
	}
	if _, ok := response["choices"]; ok {
		t.Fatalf("responses endpoint leaked Chat choices: %+v", response)
	}
	if output, ok := response["output"].([]any); !ok || len(output) != 1 {
		t.Fatalf("output=%+v", response["output"])
	}
	if text, _ := response["output_text"].(string); text == "" {
		t.Fatalf("output_text=%q", text)
	}
}

func TestResponsesEndpointStreamsResponsesEvents(t *testing.T) {
	server := setupResponsesAPITest(t)
	body, _ := json.Marshal(map[string]any{
		"model": "echo/echo-default", "input": "hello", "stream": true,
	})
	request, _ := http.NewRequest(
		http.MethodPost, server.URL+"/v1/responses", bytes.NewReader(body),
	)
	request.Header.Set("Authorization", "Bearer test-secret")
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	text := string(raw)
	if response.StatusCode != http.StatusOK ||
		!strings.Contains(response.Header.Get("Content-Type"), "text/event-stream") ||
		!strings.Contains(text, "event: response.created") ||
		!strings.Contains(text, "event: response.output_item.added") ||
		!strings.Contains(text, "event: response.content_part.added") ||
		!strings.Contains(text, "event: response.output_text.delta") ||
		!strings.Contains(text, "event: response.output_text.done") ||
		!strings.Contains(text, "event: response.completed") ||
		strings.Contains(text, "chat.completion.chunk") {
		t.Fatalf("status=%d content-type=%q body=%q",
			response.StatusCode, response.Header.Get("Content-Type"), text)
	}
}

func TestResponsesFallbackRejectsUnsupportedGuaranteesAndStatefulRoutes(t *testing.T) {
	server := setupResponsesAPITest(t)
	status, _ := jsonRequest(
		t, server.URL+"/v1/responses", http.MethodPost, "test-secret",
		map[string]any{
			"model": "echo/echo-default", "input": "hello",
			"text": map[string]any{
				"format": map[string]any{"type": "json_schema"},
			},
		},
	)
	if status != http.StatusBadRequest {
		t.Fatalf("unsupported fallback field status=%d", status)
	}
	config.Update(func(s *config.Settings) {
		s.Endpoints["multi"] = &config.EndpointConfig{Failover: []config.EndpointMember{
			{Provider: "echo", Model: "echo-default"},
			{Provider: "echo", Model: "echo-strong"},
		}}
		s.Endpoints["echo/stateful"] = &config.EndpointConfig{Failover: []config.EndpointMember{
			{Provider: "echo", Model: "echo-default"},
		}}
	})
	status, _ = jsonRequest(
		t, server.URL+"/v1/responses", http.MethodPost, "test-secret",
		map[string]any{
			"model": "multi", "input": "hello", "previous_response_id": "resp_prior",
		},
	)
	if status != http.StatusBadRequest {
		t.Fatalf("stateful multi-target status=%d", status)
	}
	status, _ = jsonRequest(
		t, server.URL+"/v1/responses", http.MethodPost, "test-secret",
		map[string]any{
			"model": "multi",
			"input": []any{map[string]any{
				"type": "item_reference", "id": "item_prior",
			}},
		},
	)
	if status != http.StatusBadRequest {
		t.Fatalf("item reference multi-target status=%d", status)
	}
	status, _ = jsonRequest(
		t, server.URL+"/v1/responses", http.MethodPost, "test-secret",
		map[string]any{
			"model": "multi",
			"input": []any{map[string]any{
				"type": "reasoning", "id": "rs_prior", "encrypted_content": "opaque",
			}},
		},
	)
	if status != http.StatusBadRequest {
		t.Fatalf("opaque state multi-target status=%d", status)
	}
	for name, request := range map[string]map[string]any{
		"slash category": {
			"model": "echo/stateful", "input": "hello",
			"previous_response_id": "resp_prior",
		},
		"file id": {
			"model": "multi",
			"input": []any{map[string]any{
				"type": "message", "role": "user",
				"content": []any{map[string]any{
					"type": "input_file", "file_id": "file_prior",
				}},
			}},
		},
		"prompt id": {
			"model": "multi", "input": "hello",
			"prompt": map[string]any{"id": "pmpt_prior"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			status, _ := jsonRequest(
				t, server.URL+"/v1/responses", http.MethodPost, "test-secret", request,
			)
			if status != http.StatusBadRequest {
				t.Fatalf("status=%d", status)
			}
		})
	}
}

func TestResponsesIncompleteToolStreamNeverMarksArgumentsDone(t *testing.T) {
	served := &router.Target{Provider: "echo", Model: "echo-default"}
	toolChunk, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{
			"delta": map[string]any{"tool_calls": []any{map[string]any{
				"index": 0, "id": "call_1",
				"function": map[string]any{"name": "lookup", "arguments": `{"q":`},
			}}},
		}},
	})
	finishChunk, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{
			"delta": map[string]any{}, "finish_reason": "length",
		}},
	})
	recorder := httptest.NewRecorder()
	streamChatAsResponseEvents(
		recorder, recorder, &fixtureStreamIter{
			chunks: []string{string(toolChunk), string(finishChunk)},
		}, map[string]any{"input": "hello"}, "route",
		served, &config.Principal{}, time.Now(),
	)
	body := recorder.Body.String()
	if !strings.Contains(body, "event: response.incomplete") ||
		strings.Contains(body, "response.function_call_arguments.done") ||
		strings.Contains(body, `"status":"completed","type":"function_call"`) {
		t.Fatalf("incomplete tool stream=%q", body)
	}
}

func TestNativeTerminalEventPreventsSecondFailureTerminal(t *testing.T) {
	served := &router.Target{Provider: "native", Model: "model"}
	terminal := `{"type":"response.completed","sequence_number":3,"response":{"id":"resp_1","object":"response","status":"completed","model":"model","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`
	recorder := httptest.NewRecorder()
	streamNativeResponseEvents(
		recorder, recorder, &fixtureStreamIter{
			chunks: []string{terminal}, err: errors.New("late transport error"),
		}, map[string]any{"input": "hello"}, "native/model",
		served, &config.Principal{}, time.Now(),
	)
	body := recorder.Body.String()
	if strings.Count(body, "event: response.completed") != 1 ||
		strings.Contains(body, "event: response.failed") {
		t.Fatalf("terminal events=%q", body)
	}
}

func TestResponsesFallbackStreamToolEventsAndFailure(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	router.ResetSavingsState()
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		iam.ResetForTests()
		router.ResetSavingsState()
	})
	served := &router.Target{Provider: "echo", Model: "echo-default"}
	toolChunk, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{
			"delta": map[string]any{"tool_calls": []any{
				map[string]any{
					"index": 0, "id": "call_1",
					"function": map[string]any{"name": "one", "arguments": `{"a":`},
				},
				map[string]any{
					"index": 1, "id": "call_2",
					"function": map[string]any{"name": "two", "arguments": `{"b":`},
				},
			}},
		}},
	})
	finishChunk, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{
			"delta": map[string]any{}, "finish_reason": "tool_calls",
		}},
	})
	recorder := httptest.NewRecorder()
	streamChatAsResponseEvents(
		recorder, recorder, &fixtureStreamIter{
			chunks: []string{string(toolChunk), string(finishChunk)},
		}, map[string]any{"input": "hello"}, "route",
		served, &config.Principal{}, time.Now(),
	)
	body := recorder.Body.String()
	if strings.Count(body, "event: response.output_item.added") != 2 ||
		!strings.Contains(body, `"response_id":"resp_`) ||
		!strings.Contains(body, `"name":"one"`) ||
		!strings.Contains(body, `"name":"two"`) {
		t.Fatalf("tool stream=%q", body)
	}

	recorder = httptest.NewRecorder()
	streamChatAsResponseEvents(
		recorder, recorder, &fixtureStreamIter{
			chunks: []string{`{"choices":[{"delta":{"content":"partial"}}]}`},
			err:    errors.New("stream broke"),
		}, map[string]any{"input": "hello"}, "route",
		served, &config.Principal{}, time.Now(),
	)
	body = recorder.Body.String()
	if !strings.Contains(body, "event: response.failed") ||
		strings.Contains(body, "event: response.completed") {
		t.Fatalf("failed stream=%q", body)
	}
	if !strings.Contains(body, `"code":"server_error"`) ||
		!strings.Contains(body, `"parallel_tool_calls":true`) {
		t.Fatalf("failed stream schema=%q", body)
	}
}

func TestResponsesPreamblePreservesStructuredInstructions(t *testing.T) {
	payload := map[string]any{
		"instructions": []any{map[string]any{"type": "input_text", "text": "existing"}},
		"input":        "hello",
	}

	payload["input"] = prependResponsesDeveloperMessage(payload["input"], "gateway")
	instructions := payload["instructions"].([]any)
	if len(instructions) != 1 {
		t.Fatalf("instructions=%+v", instructions)
	}
	input := payload["input"].([]any)
	if len(input) != 2 || input[0].(map[string]any)["role"] != "developer" {
		t.Fatalf("input=%+v", input)
	}
}

func TestResponsesPreambleIsNotExposedInPublicEnvelope(t *testing.T) {
	server := setupResponsesAPITest(t)
	config.Update(func(settings *config.Settings) {
		settings.GatewayPreamble = "private gateway policy"
	})
	t.Cleanup(func() {
		config.Update(func(settings *config.Settings) {
			settings.GatewayPreamble = ""
		})
	})
	status, response := jsonRequest(
		t, server.URL+"/v1/responses", http.MethodPost, "test-secret",
		map[string]any{
			"model": "echo/echo-default", "input": "hello",
			"instructions": "public instructions",
		},
	)
	if status != http.StatusOK {
		t.Fatalf("status=%d response=%+v", status, response)
	}
	if response["instructions"] != "public instructions" {
		t.Fatalf("instructions=%v", response["instructions"])
	}
	encoded, _ := json.Marshal(response)
	if strings.Contains(string(encoded), "private gateway policy") {
		t.Fatal("gateway preamble leaked into public response")
	}
}

func TestResponsesRejectsBackgroundAndIgnoresToolSchemaFieldNamesForState(t *testing.T) {
	server := setupResponsesAPITest(t)
	status, _ := jsonRequest(
		t, server.URL+"/v1/responses", http.MethodPost, "test-secret",
		map[string]any{
			"model": "echo/echo-default", "input": "hello", "background": true,
		},
	)
	if status != http.StatusBadRequest {
		t.Fatalf("background status=%d", status)
	}
	if responsesRequestHasProviderState(map[string]any{
		"input": "hello",
		"tools": []any{map[string]any{
			"type": "function", "name": "upload",
			"parameters": map[string]any{
				"type": "object", "properties": map[string]any{
					"file_id": map[string]any{"type": "string"},
				},
			},
		}},
	}) {
		t.Fatal("tool schema field name was mistaken for provider state")
	}
	if !responsesRequestHasProviderState(map[string]any{
		"input": "hello",
		"tools": []any{map[string]any{
			"type":             "file_search",
			"vector_store_ids": []any{"vs_private"},
		}},
	}) {
		t.Fatal("provider-owned vector store was not treated as state")
	}
	if !responsesRequestHasProviderState(map[string]any{
		"input": "hello", "store": true,
	}) {
		t.Fatal("stored response was not treated as provider state")
	}
	status, _ = jsonRequest(
		t, server.URL+"/v1/responses", http.MethodPost, "test-secret",
		map[string]any{
			"model": "echo/echo-default", "input": "hello",
			"previous_response_id": "resp_private",
		},
	)
	if status != http.StatusForbidden {
		t.Fatalf("stateful shared credential status=%d", status)
	}
}

func TestResponsesEndpointPreservesNativeResponsesPayload(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	router.ResetSavingsState()
	router.ResetTelemetryState()
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"native-model","supported_endpoints":["/responses"]}]}`))
		case "/responses":
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			input, _ := payload["input"].([]any)
			tools, _ := payload["tools"].([]any)
			if len(input) != 1 || len(tools) != 1 ||
				tools[0].(map[string]any)["type"] != "function" {
				t.Fatalf("native payload=%+v", payload)
			}
			if payload["stream"] == true {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_stream\",\"object\":\"response\",\"status\":\"in_progress\",\"model\":\"native-model\",\"output\":[]}}\n\n"))
				_, _ = w.Write([]byte("data: {\"type\":\"response.reasoning_summary_text.delta\",\"sequence_number\":1,\"output_index\":0,\"summary_index\":0,\"delta\":\"kept\"}\n\n"))
				_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"sequence_number\":2,\"response\":{\"id\":\"resp_stream\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"native-model\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp_native","object":"response","status":"completed","model":"native-model","output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"kept"}]},{"id":"msg_native","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"ok","annotations":[{"type":"url_citation","url":"https://example.test"}]}]}],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	config.Update(func(s *config.Settings) {
		s.APIKey = "test-secret"
		s.Providers = map[string]*config.ProviderConfig{
			"native": {
				Type: "openai_compatible", BaseURL: upstream.URL, APIKey: "fixture",
			},
		}
		s.Endpoints = map[string]*config.EndpointConfig{}
	})
	providers.ResetProviders()
	if models := providers.RefreshCatalog("native"); len(models) != 1 {
		t.Fatalf("catalog=%+v", models)
	}
	server := httptest.NewServer(NewServer())
	defer server.Close()
	t.Cleanup(func() {
		iam.ResetForTests()
		router.ResetSavingsState()
		router.ResetTelemetryState()
		providers.ResetProviders()
	})
	payload := map[string]any{
		"model": "native/native-model",
		"input": []any{map[string]any{
			"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": "hello"}},
		}},
		"tools": []any{map[string]any{
			"type": "function", "name": "lookup",
			"parameters": map[string]any{"type": "object"},
		}},
	}
	status, response := jsonRequest(
		t, server.URL+"/v1/responses", http.MethodPost, "test-secret", payload,
	)
	if status != http.StatusOK || response["object"] != "response" {
		t.Fatalf("status=%d response=%+v", status, response)
	}
	output, _ := response["output"].([]any)
	if len(output) != 2 || output[0].(map[string]any)["type"] != "reasoning" {
		t.Fatalf("native output was collapsed: %+v", output)
	}
	content := output[1].(map[string]any)["content"].([]any)
	annotations := content[0].(map[string]any)["annotations"].([]any)
	if len(annotations) != 1 {
		t.Fatalf("annotations=%+v", annotations)
	}

	payload["stream"] = true
	streamBody, _ := json.Marshal(payload)
	request, _ := http.NewRequest(
		http.MethodPost, server.URL+"/v1/responses", bytes.NewReader(streamBody),
	)
	request.Header.Set("Authorization", "Bearer test-secret")
	request.Header.Set("Content-Type", "application/json")
	streamResponse, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer streamResponse.Body.Close()
	streamRaw, _ := io.ReadAll(streamResponse.Body)
	streamText := string(streamRaw)
	if streamResponse.StatusCode != http.StatusOK ||
		!strings.Contains(streamText, "event: response.reasoning_summary_text.delta") ||
		!strings.Contains(streamText, "\"delta\":\"kept\"") {
		t.Fatalf("native stream status=%d body=%q", streamResponse.StatusCode, streamText)
	}
}
