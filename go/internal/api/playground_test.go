package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
	"llmgw/internal/router"
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
	oldProviders, oldCategories := config.Get().Providers, config.Get().Categories
	oldSSO, oldSecret, oldAuto := config.Get().SSOEnabled, config.Get().SSOSharedSecret, config.Get().SSOAutoProvision
	t.Cleanup(func() {
		config.Update(func(s *config.Settings) {
			s.Providers, s.Categories = oldProviders, oldCategories
			s.SSOEnabled, s.SSOSharedSecret, s.SSOAutoProvision = oldSSO, oldSecret, oldAuto
		})
	})
	config.Update(func(s *config.Settings) {
		s.SSOEnabled, s.SSOSharedSecret, s.SSOAutoProvision = true, "proxy-secret", true
		s.Providers = map[string]*config.ProviderConfig{"echo": {Type: "echo"}}
		s.Categories = map[string]*config.CategoryConfig{"owner-route": {Failover: []config.CategoryMember{{Provider: "echo", Model: "echo-default"}}}}
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
	t.Cleanup(iam.ResetForTests)
	config.Update(func(s *config.Settings) {
		s.SSOEnabled, s.SSOSharedSecret, s.SSOAutoProvision = true, "proxy-secret", true
		s.Providers = map[string]*config.ProviderConfig{"echo": {Type: "echo"}}
		s.Categories = map[string]*config.CategoryConfig{}
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
