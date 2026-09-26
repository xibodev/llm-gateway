package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/providers"
)

// Ollama still finishes a stream with stop after tool calls, as the core
// Runtime relays the daemon's stream the way the transport did. The chat
// layer rewrites that finish to tool_calls, which is what a client reads;
// a completion already finishes with tool_calls.
func TestOllamaToolCallsFinishWithToolCalls(t *testing.T) {
	const call = `{"function":{"name":"add_fixture","arguments":{"a":2,"b":3}}}`
	handler := setupAPISmokeFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if r.URL.Path != "/api/chat" || json.NewDecoder(r.Body).Decode(&body) != nil {
			t.Errorf("unexpected daemon request %s %s", r.Method, r.URL.Path)
		}
		if body["stream"] == true {
			_, _ = fmt.Fprint(w, `{"message":{"tool_calls":[`+call+`]},"done":false}`+"\n"+`{"done":true}`+"\n")
			return
		}
		_, _ = fmt.Fprint(w, `{"message":{"content":"","tool_calls":[`+call+`]},"prompt_eval_count":2,"eval_count":1}`)
	}))
	// The smoke fixture's upstream serves the daemon; its instance becomes
	// an Ollama one at the same root.
	config.Update(func(s *config.Settings) {
		ollama := *s.Providers["fixture"]
		ollama.Type, ollama.APIKey = "ollama", ""
		s.Providers = map[string]*config.ProviderConfig{"fixture": &ollama}
	})
	providers.ResetProviders()
	request := map[string]any{
		"model": "fixture/fixture-model", "messages": []any{map[string]any{"role": "user", "content": "add"}},
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "add_fixture"}}},
	}

	w := apiSmokeRequest(handler, "/v1/chat/completions", request)
	var completion struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &completion) != nil ||
		len(completion.Choices) != 1 || completion.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("completion status=%d body=%s", w.Code, w.Body.String())
	}

	request["stream"] = true
	w = apiSmokeRequest(handler, "/v1/chat/completions", request)
	records := strings.Split(strings.TrimSuffix(w.Body.String(), "\n\n"), "\n\n")
	if w.Code != http.StatusOK || len(records) != 3 || records[2] != "data: [DONE]" {
		t.Fatalf("stream status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(records[0], `"id":"call_0"`) || !strings.Contains(records[1], `"finish_reason":"tool_calls"`) {
		t.Fatalf("stream records=%q, want the tool call and a tool_calls finish", records)
	}
}
