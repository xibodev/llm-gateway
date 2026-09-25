package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/providers"
)

// A Responses request for a Copilot model without native Responses is served
// over Chat, as it always was: Copilot receives the request's
// parallel_tool_calls and the stream_options whose include_usage makes it
// report the stream's usage, which the client then reads.
func TestCopilotResponsesServedOverChatKeepTheirFields(t *testing.T) {
	stateDir := t.TempDir()
	var mu sync.Mutex
	var chatBodies []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			writeJSON(w, http.StatusOK, map[string]any{"data": []any{
				map[string]any{"id": "chat-model", "supported_endpoints": []any{"/chat/completions"}},
			}})
		case "/chat/completions":
			raw, _ := io.ReadAll(r.Body)
			mu.Lock()
			chatBodies = append(chatBodies, string(raw))
			mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n"+
				"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"+
				"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":4,\"total_tokens\":7}}\n\n"+
				"data: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	setupProviderCredentialAPI(t, stateDir)
	previous := *config.Get()
	t.Cleanup(func() {
		config.Update(func(s *config.Settings) {
			s.AllowCopilotProxy, s.GithubCopilotCacheDir = previous.AllowCopilotProxy, previous.GithubCopilotCacheDir
			s.GithubCopilotOAuthToken, s.GithubCopilotUseGhCLI = previous.GithubCopilotOAuthToken, previous.GithubCopilotUseGhCLI
		})
	})
	cacheDir := filepath.Join(stateDir, "cache")
	config.Update(func(s *config.Settings) {
		s.AllowCopilotProxy, s.GithubCopilotCacheDir = true, cacheDir
		s.GithubCopilotOAuthToken, s.GithubCopilotUseGhCLI = "", false
		s.Providers = map[string]*config.ProviderConfig{"copilot": {Type: "github_copilot"}}
		s.Endpoints = map[string]*config.EndpointConfig{}
		s.AnthropicDiscoveryAliases = false
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)
	writeCopilotSession(t, cacheDir, "human-secret", upstream.URL)
	token, _ := issueHumanProviderTestKeyForModels(t, "human-secret", []string{"copilot/chat-model"})
	server := httptest.NewServer(NewServer(Runtime{}))
	defer server.Close()

	raw, _ := json.Marshal(map[string]any{
		"model": "copilot/chat-model", "input": "hi", "stream": true, "parallel_tool_calls": false,
	})
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", bytes.NewReader(raw))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "response.completed") ||
		!strings.Contains(string(body), `"input_tokens":3`) {
		t.Fatalf("status=%d body=%s", response.StatusCode, body)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(chatBodies) != 1 || !strings.Contains(chatBodies[0], `"parallel_tool_calls":false`) ||
		!strings.Contains(chatBodies[0], `"stream_options":{"include_usage":true}`) {
		t.Fatalf("Copilot received %q", chatBodies)
	}
}
