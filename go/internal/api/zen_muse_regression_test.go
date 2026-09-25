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

	corezen "github.com/xibodev/llmgw-core/providers/zen"
)

func TestAnonymousZenMuseChatFacadePreservesV066MultiTurnContract(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	old := *config.Get()
	iam.ResetForTests()
	router.ResetSavingsState()
	router.ResetTelemetryState()
	t.Cleanup(func() {
		providers.ResetProviders()
		iam.ResetForTests()
		router.ResetSavingsState()
		router.ResetTelemetryState()
		config.Update(func(settings *config.Settings) { *settings = old })
	})

	var payloads []map[string]any
	var headers []http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/responses" {
			t.Errorf("path=%q want /responses", request.URL.Path)
			http.NotFound(w, request)
			return
		}
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Error(err)
			return
		}
		payloads = append(payloads, payload)
		headers = append(headers, request.Header.Clone())
		if len(payloads) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = fmt.Fprint(w, `{"error":{"message":"retry"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"private reasoning\"}\n\n"+
			"data: {\"type\":\"response.function_call_arguments.delta\",\"delta\":\"{\\\"secret\\\":true}\"}\n\n"+
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"Muse answer\"}\n\n"+
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_muse\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"muse-spark-fixture\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"Muse answer\"}]}]}}\n\n")
	}))
	defer upstream.Close()

	config.Update(func(settings *config.Settings) {
		*settings = *config.Defaults()
		settings.APIKey = "fixture-gateway-token"
		settings.AllowUnauthenticatedAPI = false
		settings.Providers = map[string]*config.ProviderConfig{
			"zen": {Type: "openai_compatible", RegistryID: "opencode_zen", BaseURL: upstream.URL},
		}
		settings.Endpoints = map[string]*config.EndpointConfig{}
		settings.Policies.Defaults.RetryMaxAttempts = 2
		settings.Policies.Defaults.RetryInitialBackoffSeconds = 0
		settings.Policies.Defaults.CircuitFailureThreshold = 0
	})
	providers.ResetProviders()

	recorder := apiSmokeRequest(NewServer(Runtime{}), "/v1/chat/completions", map[string]any{
		"model": "zen/muse-spark-fixture",
		"messages": []any{
			map[string]any{"role": "user", "content": "turn one"},
			map[string]any{"role": "assistant", "content": "turn two"},
			map[string]any{"role": "user", "content": "turn three"},
		},
		"max_tokens": 2048,
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if len(payloads) != 2 {
		t.Fatalf("requests=%d payloads=%+v", len(payloads), payloads)
	}
	for index, payload := range payloads {
		if payload["stream"] != true || payload["max_output_tokens"] != float64(2048) {
			t.Fatalf("payload %d stream/max_output_tokens=%+v", index, payload)
		}
		reasoning, _ := payload["reasoning"].(map[string]any)
		if reasoning["effort"] != "minimal" {
			t.Fatalf("payload %d reasoning=%+v", index, reasoning)
		}
		input, _ := payload["input"].([]any)
		if len(input) != 3 {
			t.Fatalf("payload %d history=%+v", index, input)
		}
		tools, _ := payload["tools"].([]any)
		var names []string
		for _, raw := range tools {
			tool, _ := raw.(map[string]any)
			name, _ := tool["name"].(string)
			names = append(names, name)
		}
		if !reflect.DeepEqual(names, []string{"bash", "read"}) || payload["tool_choice"] != "auto" {
			t.Fatalf("payload %d tools=%v choice=%v", index, names, payload["tool_choice"])
		}
	}
	for _, key := range []string{"x-opencode-session", "x-opencode-request"} {
		if headers[0].Get(key) == "" || headers[0].Get(key) != headers[1].Get(key) {
			t.Fatalf("%s changed across retry: %q -> %q", key, headers[0].Get(key), headers[1].Get(key))
		}
	}
	if headers[0].Get("x-opencode-project") != "global" || headers[0].Get("x-opencode-client") != "cli" || headers[0].Get("User-Agent") != corezen.AnonymousUserAgent {
		t.Fatalf("anonymous fingerprint=%v", headers[0])
	}
	if !strings.Contains(recorder.Body.String(), "Muse answer") || strings.Contains(recorder.Body.String(), "private reasoning") || strings.Contains(recorder.Body.String(), "secret") || strings.Contains(recorder.Body.String(), `"content":"completed"`) {
		t.Fatalf("response=%s", recorder.Body.String())
	}
}
