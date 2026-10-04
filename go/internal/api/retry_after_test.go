package api

import (
	"net/http"
	"strconv"
	"testing"
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
