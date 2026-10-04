package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
	"llmgw/internal/router"
)

func TestAnthropicErrorTypeFollowsTheStatus(t *testing.T) {
	for status, want := range map[int]string{
		400: "invalid_request_error", 405: "invalid_request_error", 413: "invalid_request_error", 422: "invalid_request_error",
		401: "authentication_error", 403: "permission_error", 404: "not_found_error", 429: "rate_limit_error",
		529: "overloaded_error", 500: "api_error", 502: "api_error", 503: "api_error", 504: "api_error",
	} {
		if got := anthropicErrorType(status); got != want {
			t.Errorf("status %d: type=%q, want %q", status, got, want)
		}
	}
}

// An upstream refusal on a Messages route keeps its status and Retry-After
// in Anthropic's envelope, so an Anthropic client retries it as it would
// Anthropic's own.
func TestMessagesRouteUpstreamErrorKeepsStatusAndRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		status    int
		errorType string
	}{
		{http.StatusTooManyRequests, "rate_limit_error"},
		{529, "overloaded_error"},
		{http.StatusServiceUnavailable, "api_error"},
	} {
		recorder := httptest.NewRecorder()
		writeUpstreamError(anthropicErrorWriter{recorder}, &providers.InvocationError{
			Msg: "upstream body must not be exposed", Status: tc.status, RetryAfter: "7",
		})
		want := fmt.Sprintf(`{"error":{"message":"Upstream provider request failed.","type":%q},"type":"error"}`+"\n", tc.errorType)
		if recorder.Code != tc.status || recorder.Header().Get("Retry-After") != "7" || recorder.Body.String() != want {
			t.Fatalf("status=%d Retry-After=%q body=%s", recorder.Code, recorder.Header().Get("Retry-After"), recorder.Body.String())
		}
	}
}

// Every error on the Messages routes, from authentication, body decoding,
// routing or the upstream, reaches the client in Anthropic's envelope with
// its HTTP status and, where Anthropic's own envelope has it, the request's
// ID; the OpenAI-shaped routes keep their own envelope.
func TestMessagesRoutesAnswerErrorsInAnthropicsEnvelope(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"data":[],"has_more":false}`)
			return
		}
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		status := http.StatusServiceUnavailable
		if body.Model == "overloaded" {
			status = 529
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
	}))
	t.Cleanup(upstream.Close)
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	router.ResetSavingsState()
	router.ResetTelemetryState()
	old := *config.Get()
	config.Update(func(s *config.Settings) {
		s.APIKey = "gateway-token"
		s.APIKeys = nil
		s.AllowUnauthenticatedAPI = false
		s.Providers = map[string]*config.ProviderConfig{"claude": {Type: "anthropic", BaseURL: upstream.URL, APIKey: "fixture"}}
		s.Endpoints = map[string]*config.EndpointConfig{}
		s.Policies.Defaults = config.ProviderPolicy{RetryMaxAttempts: 1}
	})
	providers.ResetProviders()
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	gateway := httptest.NewServer(NewServer(Runtime{}))
	t.Cleanup(func() {
		gateway.Close()
		providers.ResetProviders()
		iam.ResetForTests()
		router.ResetSavingsState()
		router.ResetTelemetryState()
		config.Update(func(s *config.Settings) { *s = old })
	})
	send := func(path, token, body string) (*http.Response, map[string]any) {
		t.Helper()
		request, _ := http.NewRequest(http.MethodPost, gateway.URL+path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var envelope map[string]any
		if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
			t.Fatal(err)
		}
		return response, envelope
	}
	request := func(model string) string {
		return `{"model":"` + model + `","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	}
	for _, path := range []string{"/v1/messages", "/messages", "/v1/messages/count_tokens", "/messages/count_tokens"} {
		for _, tc := range []struct {
			name, token, body, errorType string
			status                       int
		}{
			{"invalid key", "wrong", request("claude/model"), "authentication_error", http.StatusUnauthorized},
			{"invalid body", "gateway-token", `{"model":`, "invalid_request_error", http.StatusUnprocessableEntity},
			{"missing model", "gateway-token", `{"messages":[]}`, "invalid_request_error", http.StatusUnprocessableEntity},
			{"unknown model", "gateway-token", request("missing/model"), "not_found_error", http.StatusNotFound},
			{"overloaded upstream", "gateway-token", request("claude/overloaded"), "overloaded_error", 529},
			{"failed upstream", "gateway-token", request("claude/down"), "api_error", http.StatusServiceUnavailable},
		} {
			t.Run(path+" "+tc.name, func(t *testing.T) {
				response, envelope := send(path, tc.token, tc.body)
				errorBody, _ := envelope["error"].(map[string]any)
				message, _ := errorBody["message"].(string)
				if response.StatusCode != tc.status || envelope["type"] != "error" || len(envelope) != 3 ||
					envelope["request_id"] != response.Header.Get("X-Request-Id") ||
					errorBody["type"] != tc.errorType || len(errorBody) != 2 || message == "" {
					t.Fatalf("status=%d envelope=%v", response.StatusCode, envelope)
				}
				if tc.status == http.StatusUnauthorized && response.Header.Get("WWW-Authenticate") != "Bearer" {
					t.Fatalf("WWW-Authenticate=%q", response.Header.Get("WWW-Authenticate"))
				}
			})
		}
	}
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		response, envelope := send(path, "gateway-token", `{"model":"missing/model","input":"hi","messages":[]}`)
		errorBody, _ := envelope["error"].(map[string]any)
		if response.StatusCode != http.StatusNotFound || len(envelope) != 1 || errorBody["type"] != "invalid_request_error" || errorBody["code"] != "404" {
			t.Fatalf("%s: status=%d envelope=%v", path, response.StatusCode, envelope)
		}
	}
}
