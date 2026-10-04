package api

import (
	"net/http"
	"strconv"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/providers"
)

// A chain that ends on a throttled or unavailable upstream answers with the
// upstream's status and passes its Retry-After on, on every coding surface,
// streamed or not.
func TestChainFailurePassesRetryAfterOn(t *testing.T) {
	message := []any{map[string]any{"role": "user", "content": "hi"}}
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		for path, body := range map[string]map[string]any{
			"/v1/chat/completions": {"model": "fixture/model", "messages": message},
			"/v1/responses":        {"model": "fixture/model", "input": "hi"},
			"/v1/messages":         {"model": "fixture/model", "max_tokens": 16, "messages": message},
		} {
			for _, stream := range []bool{false, true} {
				t.Run(strconv.Itoa(status)+path+map[bool]string{false: "", true: "/stream"}[stream], func(t *testing.T) {
					handler := setupAPISmokeFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						w.Header().Set("Retry-After", "7")
						writeJSON(w, status, map[string]any{"error": map[string]any{"message": "busy"}})
					}))
					request := map[string]any{"stream": stream}
					for key, value := range body {
						request[key] = value
					}
					response := apiSmokeRequest(handler, path, request)
					if response.Code != status || response.Header().Get("Retry-After") != "7" {
						t.Fatalf("status=%d Retry-After=%q body=%s", response.Code, response.Header().Get("Retry-After"), response.Body.String())
					}
				})
			}
		}
	}
}

// An Anthropic or Azure OpenAI refusal reaches the client with the
// upstream's status and Retry-After, as an OpenAI-compatible one does:
// on Messages and its token count for Anthropic, and on Chat for both,
// streamed or not.
func TestAnthropicAndAzureRefusalsPassRetryAfterOn(t *testing.T) {
	for _, tc := range []struct {
		providerType, path string
		stream             bool
	}{
		{"anthropic", "/v1/messages", false},
		{"anthropic", "/v1/messages", true},
		{"anthropic", "/v1/messages/count_tokens", false},
		{"anthropic", "/v1/chat/completions", false},
		{"anthropic", "/v1/chat/completions", true},
		{"azure_openai", "/v1/chat/completions", false},
		{"azure_openai", "/v1/chat/completions", true},
	} {
		t.Run(tc.providerType+tc.path+map[bool]string{false: "", true: "/stream"}[tc.stream], func(t *testing.T) {
			handler := setupAPISmokeFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", "2")
				writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": map[string]any{"message": "slow down"}})
			}))
			config.Update(func(s *config.Settings) { s.Providers["fixture"].Type = tc.providerType })
			providers.ResetProviders()
			request := map[string]any{
				"model": "fixture/model", "max_tokens": 16, "messages": []any{map[string]any{"role": "user", "content": "hi"}},
			}
			if tc.stream {
				request["stream"] = true
			}
			response := apiSmokeRequest(handler, tc.path, request)
			if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != "2" {
				t.Fatalf("status=%d Retry-After=%q body=%s", response.Code, response.Header().Get("Retry-After"), response.Body.String())
			}
		})
	}
}
