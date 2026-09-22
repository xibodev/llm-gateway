package api

import (
	"database/sql"
	"encoding/json"
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

	core "github.com/xibodev/llmgw-core"
)

func TestOwnerPlaygroundUsesRealRouteAndRecordsKeylessProjectUsage(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	providers.ResetProviders()
	router.ResetSavingsState()
	router.ResetTelemetryState()
	t.Cleanup(func() {
		iam.ResetForTests()
		providers.ResetProviders()
		router.ResetSavingsState()
		router.ResetTelemetryState()
	})
	oldProviders, oldEndpoints := config.Get().Providers, config.Get().Endpoints
	oldSSO, oldSecret, oldAuto := config.Get().SSOEnabled, config.Get().SSOSharedSecret, config.Get().SSOAutoProvision
	t.Cleanup(func() {
		config.Update(func(s *config.Settings) {
			s.Providers, s.Endpoints = oldProviders, oldEndpoints
			s.SSOEnabled, s.SSOSharedSecret, s.SSOAutoProvision = oldSSO, oldSecret, oldAuto
		})
	})
	config.Update(func(s *config.Settings) {
		s.SSOEnabled, s.SSOSharedSecret, s.SSOAutoProvision = true, "proxy-secret", true
		s.Providers = map[string]*config.ProviderConfig{"echo": {Type: "echo"}}
		s.Endpoints = map[string]*config.EndpointConfig{"owner-route": {Failover: []config.EndpointMember{{Provider: "echo", Model: "echo-default"}}}}
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	owner, err := iam.EnsurePrincipalBySubject("human", "authentik:playground-owner", "", "Playground Owner")
	if err != nil {
		t.Fatal(err)
	}
	project, err := iam.CreateProject("playground-project", "Playground Project")
	if err != nil {
		t.Fatal(err)
	}
	if err := iam.SetMembership(project.ID, owner.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := iam.SetProjectPolicy(project.ID, iam.KeyPolicy{DailyRequests: 1}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(NewServer())
	defer server.Close()

	body := map[string]any{
		"project_id": project.ID, "model": "owner-route",
		"messages": []map[string]any{{"role": "user", "content": "hello playground"}},
	}
	status, response := ssoConnectionRequest(t, server.URL, "playground-owner", http.MethodPost, "/user/api/playground", body)
	if status != http.StatusOK {
		t.Fatalf("playground status=%d response=%+v", status, response)
	}
	served := response["served"].(map[string]any)
	if served["provider"] != "echo" || served["model"] != "echo-default" {
		t.Fatalf("served=%+v", served)
	}
	trace := response["fallback_trace"].([]any)
	if len(trace) != 1 || trace[0].(map[string]any)["status"] != "served" {
		t.Fatalf("trace=%+v", trace)
	}
	raw, _ := json.Marshal(response)
	for _, forbidden := range []string{"access_token", "refresh_token", "api_key", "authorization"} {
		if strings.Contains(strings.ToLower(string(raw)), forbidden) {
			t.Fatalf("playground response leaked credential field: %s", raw)
		}
	}

	db, err := iam.DB()
	if err != nil {
		t.Fatal(err)
	}
	var projectID, principalID string
	var keyID sql.NullString
	if err := db.QueryRow("SELECT project_id,principal_id,key_id FROM usage_events WHERE endpoint=?", "playground.chat").Scan(&projectID, &principalID, &keyID); err != nil {
		t.Fatal(err)
	}
	if projectID != project.ID || principalID != owner.ID || keyID.Valid {
		t.Fatalf("usage attribution project=%q principal=%q key=%+v", projectID, principalID, keyID)
	}
	_, dayStart, _ := playgroundQuotaPeriods(time.Now())
	var requests, inputTokens int
	if err := db.QueryRow("SELECT requests,input_tokens FROM project_quota_counters WHERE project_id=? AND period=? AND period_start=?", project.ID, "day", dayStart).Scan(&requests, &inputTokens); err != nil {
		t.Fatal(err)
	}
	if requests != 1 || inputTokens != 1 {
		t.Fatalf("keyless project counters requests=%d input=%d", requests, inputTokens)
	}

	status, denied := ssoConnectionRequest(t, server.URL, "playground-owner", http.MethodPost, "/user/api/playground", body)
	if status != http.StatusTooManyRequests || denied["error"] == nil {
		t.Fatalf("project quota status=%d response=%+v", status, denied)
	}
}

func TestPlaygroundRejectsOtherPrincipalAndExplainsStreamingLimit(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	providers.ResetProviders()
	router.ResetSavingsState()
	router.ResetTelemetryState()
	oldProviders, oldEndpoints := config.Get().Providers, config.Get().Endpoints
	oldSSO, oldSecret, oldAuto := config.Get().SSOEnabled, config.Get().SSOSharedSecret, config.Get().SSOAutoProvision
	t.Cleanup(func() {
		iam.ResetForTests()
		providers.ResetProviders()
		router.ResetSavingsState()
		router.ResetTelemetryState()
		config.Update(func(s *config.Settings) {
			s.Providers, s.Endpoints = oldProviders, oldEndpoints
			s.SSOEnabled, s.SSOSharedSecret, s.SSOAutoProvision = oldSSO, oldSecret, oldAuto
		})
	})
	config.Update(func(s *config.Settings) {
		s.SSOEnabled, s.SSOSharedSecret, s.SSOAutoProvision = true, "proxy-secret", true
		s.Providers = map[string]*config.ProviderConfig{"echo": {Type: "echo"}}
		s.Endpoints = map[string]*config.EndpointConfig{}
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	owner, _ := iam.EnsurePrincipalBySubject("human", "authentik:play-owner", "", "Owner")
	project, _ := iam.CreateProject("play-owner", "Play Owner")
	_ = iam.SetMembership(project.ID, owner.ID, "admin")
	server := httptest.NewServer(NewServer())
	defer server.Close()
	body := map[string]any{"project_id": project.ID, "model": "echo/echo-default", "messages": []map[string]any{{"role": "user", "content": "hello"}}}
	status, denied := ssoConnectionRequest(t, server.URL, "other-owner", http.MethodPost, "/user/api/playground", body)
	if status != http.StatusForbidden || denied["error"] == nil {
		t.Fatalf("other principal status=%d response=%+v", status, denied)
	}
	body["stream"] = true
	status, limited := ssoConnectionRequest(t, server.URL, "play-owner", http.MethodPost, "/user/api/playground", body)
	errorPayload, _ := limited["error"].(map[string]any)
	message, _ := errorPayload["message"].(string)
	if status != http.StatusBadRequest || !strings.Contains(strings.ToLower(message), "streaming") {
		t.Fatalf("streaming limitation status=%d response=%+v", status, limited)
	}
}

func TestPlaygroundPreservesDefinitiveUpstreamErrorWithoutRetry(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			writeJSON(w, http.StatusOK, map[string]any{"data": []any{map[string]any{"id": "unavailable"}}})
			return
		}
		calls.Add(1)
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "Model is unavailable."}})
	}))
	defer upstream.Close()

	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	providers.ResetProviders()
	providers.ResetCircuit("zen")
	router.ResetSavingsState()
	router.ResetTelemetryState()
	t.Cleanup(func() {
		iam.ResetForTests()
		providers.ResetProviders()
		providers.ResetCircuit("zen")
		router.ResetSavingsState()
		router.ResetTelemetryState()
	})
	oldProviders, oldEndpoints := config.Get().Providers, config.Get().Endpoints
	oldSSO, oldSecret, oldAuto := config.Get().SSOEnabled, config.Get().SSOSharedSecret, config.Get().SSOAutoProvision
	oldPolicies := config.Get().Policies
	t.Cleanup(func() {
		config.Update(func(s *config.Settings) {
			s.Providers, s.Endpoints = oldProviders, oldEndpoints
			s.SSOEnabled, s.SSOSharedSecret, s.SSOAutoProvision = oldSSO, oldSecret, oldAuto
			s.Policies = oldPolicies
		})
	})
	config.Update(func(s *config.Settings) {
		s.SSOEnabled, s.SSOSharedSecret, s.SSOAutoProvision = true, "proxy-secret", true
		s.Providers = map[string]*config.ProviderConfig{"zen": {Type: "openai_compatible", BaseURL: upstream.URL, APIKey: "fixture"}}
		s.Endpoints = map[string]*config.EndpointConfig{}
		s.Policies.Defaults.RetryMaxAttempts = 3
		s.Policies.Defaults.RetryInitialBackoffSeconds = 0.001
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	owner, err := iam.EnsurePrincipalBySubject("human", "authentik:unavailable-owner", "", "Unavailable Owner")
	if err != nil {
		t.Fatal(err)
	}
	project, err := iam.CreateProject("unavailable-project", "Unavailable Project")
	if err != nil {
		t.Fatal(err)
	}
	if err := iam.SetMembership(project.ID, owner.ID, "owner"); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(NewServer())
	defer server.Close()
	status, response := ssoConnectionRequest(t, server.URL, "unavailable-owner", http.MethodPost, "/user/api/playground", map[string]any{
		"project_id": project.ID,
		"model":      "zen/unavailable",
		"messages":   []map[string]any{{"role": "user", "content": "hi"}},
	})
	errorPayload, _ := response["error"].(map[string]any)
	message, _ := errorPayload["message"].(string)
	if status != http.StatusBadRequest || !strings.Contains(message, "Model is unavailable") {
		t.Fatalf("status=%d response=%+v", status, response)
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream inference calls=%d want=1", calls.Load())
	}
}

func playgroundQuotaPeriods(now time.Time) (int64, int64, int64) {
	now = now.UTC()
	minute := now.Unix() / 60 * 60
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).Unix()
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).Unix()
	return minute, day, month
}

func TestSafePlaygroundValueRedactsCredentialsAndPreservesTokenUsage(t *testing.T) {
	input := map[string]any{
		"access_token":         "access-secret",
		"refresh_token":        "refresh-secret",
		"session_token":        "session-secret",
		"bearer_token":         "bearer-secret",
		"authorization_header": "authorization-secret",
		"client_secret_value":  "client-secret",
		"private_key":          "private-key-secret",
		"token_count":          13,
		"usage": map[string]any{
			"prompt_tokens":     2,
			"completion_tokens": 3,
			"input_tokens":      5,
			"output_tokens":     7,
			"total_tokens":      12,
			"cached_tokens":     1,
			"reasoning_tokens":  4,
		},
		"nested": map[string]any{
			"password_value": "nested-password",
			"array": []any{
				map[string]any{"cookie_value": "array-cookie", "total_tokens": 9},
				map[string]any{"credential_blob": "array-credential", "input_tokens": 4},
			},
		},
		"headers": map[string]any{
			"Authorization": "header-secret",
			"X-API-Key":     "api-secret",
			"X-Request-ID":  "safe-request-id",
		},
	}
	sanitized := safePlaygroundValue(input).(map[string]any)
	for _, key := range []string{"access_token", "refresh_token", "session_token", "bearer_token", "authorization_header", "client_secret_value", "private_key"} {
		if _, found := sanitized[key]; found {
			t.Fatalf("credential key %q survived sanitization: %+v", key, sanitized)
		}
	}
	if sanitized["token_count"] != 13 {
		t.Fatalf("non-secret token field was removed: %+v", sanitized)
	}
	usage := sanitized["usage"].(map[string]any)
	for key, want := range map[string]int{"prompt_tokens": 2, "completion_tokens": 3, "input_tokens": 5, "output_tokens": 7, "total_tokens": 12, "cached_tokens": 1, "reasoning_tokens": 4} {
		if usage[key] != want {
			t.Fatalf("usage %q=%v want=%d: %+v", key, usage[key], want, usage)
		}
	}
	nested := sanitized["nested"].(map[string]any)
	if _, found := nested["password_value"]; found {
		t.Fatalf("nested credential survived sanitization: %+v", nested)
	}
	array := nested["array"].([]any)
	first := array[0].(map[string]any)
	second := array[1].(map[string]any)
	if _, found := first["cookie_value"]; found {
		t.Fatalf("array credential survived sanitization: %+v", first)
	}
	if _, found := second["credential_blob"]; found {
		t.Fatalf("array credential survived sanitization: %+v", second)
	}
	if first["total_tokens"] != 9 || second["input_tokens"] != 4 {
		t.Fatalf("array usage was removed: first=%+v second=%+v", first, second)
	}
	headers := sanitized["headers"].(map[string]any)
	for _, key := range []string{"Authorization", "X-API-Key"} {
		if _, found := headers[key]; found {
			t.Fatalf("credential header %q survived sanitization: %+v", key, headers)
		}
	}
	if headers["X-Request-ID"] != "safe-request-id" {
		t.Fatalf("safe header was removed: %+v", headers)
	}
	raw, _ := json.Marshal(sanitized)
	for _, secret := range []string{"access-secret", "refresh-secret", "session-secret", "bearer-secret", "authorization-secret", "client-secret", "private-key-secret", "nested-password", "array-cookie", "array-credential", "header-secret", "api-secret"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("credential value leaked: %s", raw)
		}
	}
}

func TestPlaygroundTextSurfaceRoutes(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	providers.ResetProviders()
	router.ResetSavingsState()
	router.ResetTelemetryState()
	oldProviders, oldEndpoints := config.Get().Providers, config.Get().Endpoints
	oldSSO, oldSecret, oldAuto := config.Get().SSOEnabled, config.Get().SSOSharedSecret, config.Get().SSOAutoProvision
	t.Cleanup(func() {
		iam.ResetForTests()
		providers.ResetProviders()
		router.ResetSavingsState()
		router.ResetTelemetryState()
		config.Update(func(s *config.Settings) {
			s.Providers, s.Endpoints = oldProviders, oldEndpoints
			s.SSOEnabled, s.SSOSharedSecret, s.SSOAutoProvision = oldSSO, oldSecret, oldAuto
		})
	})
	config.Update(func(s *config.Settings) {
		s.SSOEnabled, s.SSOSharedSecret, s.SSOAutoProvision = true, "proxy-secret", true
		s.Providers = map[string]*config.ProviderConfig{"echo": {Type: "echo"}}
		s.Endpoints = map[string]*config.EndpointConfig{}
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	owner, _ := iam.EnsurePrincipalBySubject("human", "authentik:surface-owner", "", "Surface Owner")
	project, _ := iam.CreateProject("surface-project", "Surface Project")
	_ = iam.SetMembership(project.ID, owner.ID, "owner")
	server := httptest.NewServer(NewServer())
	defer server.Close()
	status, response := ssoConnectionRequest(t, server.URL, "surface-owner", http.MethodPost, "/user/api/playground/v1/messages", map[string]any{
		"project_id": project.ID, "model": "echo/echo-default", "messages": []map[string]any{{"role": "user", "content": "missing limit"}},
	})
	if status != http.StatusBadRequest || response["error"] == nil {
		t.Fatalf("missing Messages max_tokens status=%d response=%+v", status, response)
	}

	tests := []struct {
		name, path, surface string
		body                map[string]any
	}{
		{"chat", "/user/api/playground/v1/chat/completions", "/v1/chat/completions", map[string]any{"messages": []map[string]any{{"role": "user", "content": "chat"}}}},
		{"responses", "/user/api/playground/v1/responses", "/v1/responses", map[string]any{"input": "responses"}},
		{"messages", "/user/api/playground/v1/messages", "/v1/messages", map[string]any{"messages": []map[string]any{{"role": "user", "content": "messages"}}, "max_tokens": 32}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.body["project_id"] = project.ID
			test.body["model"] = "echo/echo-default"
			status, response := ssoConnectionRequest(t, server.URL, "surface-owner", http.MethodPost, test.path, test.body)
			if status != http.StatusOK {
				t.Fatalf("status=%d response=%+v", status, response)
			}
			if response["surface"] != test.surface || response["raw_response"] == nil {
				t.Fatalf("surface response=%+v", response)
			}
			served := response["served"].(map[string]any)
			if served["provider"] != "echo" || response["transport_mode"] == nil {
				t.Fatalf("metadata response=%+v", response)
			}
		})
	}
}

func TestValidateGeneratedImagesRejectsUnsafeAndIncompleteResults(t *testing.T) {
	valid := providers.GeneratedImage{Data: []byte("image"), MimeType: "image/png"}
	if err := validateGeneratedImages([]providers.GeneratedImage{valid}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		images []providers.GeneratedImage
	}{
		{name: "empty"},
		{name: "active content", images: []providers.GeneratedImage{{Data: []byte("<svg/>"), MimeType: "image/svg+xml"}}},
		{name: "empty bytes", images: []providers.GeneratedImage{{MimeType: "image/png"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := validateGeneratedImages(test.images); err == nil {
				t.Fatal("unsafe image result was accepted")
			}
		})
	}
}

func TestPlaygroundPayloadIsMultimodalBySurface(t *testing.T) {
	responses := map[string]any{"input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": "data:image/png;base64,fixture"}}}}}
	if !playgroundPayloadIsMultimodal(core.ModelSurfaceResponses, responses) {
		t.Fatal("Responses image input was not detected")
	}
	chat := map[string]any{"messages": []any{map[string]any{
		"role": "user", "content": []any{map[string]any{
			"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,fixture"},
		}},
	}}}
	if !playgroundPayloadIsMultimodal(core.ModelSurfaceChatCompletions, chat) {
		t.Fatal("Chat image input was not detected")
	}
}

func TestImageGenerationsRejectsExplicitInvalidCountBeforeRouting(t *testing.T) {
	oldUnauthenticated := config.Get().AllowUnauthenticatedAPI
	config.Update(func(settings *config.Settings) { settings.AllowUnauthenticatedAPI = true })
	t.Cleanup(func() {
		config.Update(func(settings *config.Settings) { settings.AllowUnauthenticatedAPI = oldUnauthenticated })
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"missing","prompt":"draw","n":0}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handleImageGenerations(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "greater than zero") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
