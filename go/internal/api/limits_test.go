package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"
)

// The limiter reports what a caller has started in the current minute, and
// nothing once the next minute begins.
func TestCallerRateLimiterReportsUseOfTheCurrentMinute(t *testing.T) {
	now := time.Date(2026, time.October, 5, 12, 0, 45, 0, time.UTC)
	limiter := &callerRateLimiter{now: func() time.Time { return now }}
	if used, resets := limiter.used("key:a"); used != 0 || !resets.Equal(time.Date(2026, time.October, 5, 12, 1, 0, 0, time.UTC)) {
		t.Fatalf("before any request: used=%d resets=%v", used, resets)
	}
	for range 2 {
		limiter.admit("key:a", 5)
	}
	limiter.admit("key:b", 5)
	if used, _ := limiter.used("key:a"); used != 2 {
		t.Fatalf("used=%d, want the caller's own 2", used)
	}
	now = now.Add(15 * time.Second)
	if used, resets := limiter.used("key:a"); used != 0 || !resets.Equal(time.Date(2026, time.October, 5, 12, 2, 0, 0, time.UTC)) {
		t.Fatalf("the next minute: used=%d resets=%v", used, resets)
	}
}

// A key's limits are its own, its project's and the per-caller limit, each
// with its usage in the current window and the closest marked, and a request
// a limit refuses is recorded with the limit that refused it. A project's
// limits are its own; a user reads the limits of their own keys only.
func TestKeyLimitsReportUsageAndRefusalsNameTheirLimit(t *testing.T) {
	handler := setupAPISmokeFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"choices": []any{map[string]any{
			"message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop",
		}}})
	}))
	now := time.Now()
	previous := callerRates
	callerRates = &callerRateLimiter{now: func() time.Time { return now }}
	t.Cleanup(func() { callerRates = previous })
	config.Update(func(s *config.Settings) { s.RateLimitPerMinute = 5 })
	token := admissionFixtureKey(t, "limits", iam.KeyPolicy{DailyRequests: 2})
	caller, found, err := iam.ResolveAPIKey(token)
	if err != nil || !found {
		t.Fatalf("resolve found=%v err=%v", found, err)
	}
	if _, err := iam.SetProjectPolicy(caller.ProjectID, iam.KeyPolicy{MonthlyRequests: 50}); err != nil {
		t.Fatal(err)
	}
	chat := map[string]any{"model": "fixture/model", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	for i := range 2 {
		if w := admissionRequest(handler, "/v1/chat/completions", token, nil, chat); w.Code != http.StatusOK {
			t.Fatalf("request %d: status=%d body=%s", i+1, w.Code, w.Body.String())
		}
	}
	if w := admissionRequest(handler, "/v1/chat/completions", token, nil, chat); w.Code != http.StatusTooManyRequests {
		t.Fatalf("request over the key's limit: status=%d body=%s", w.Code, w.Body.String())
	}
	db, err := iam.DB()
	if err != nil {
		t.Fatal(err)
	}
	var code string
	if err := db.QueryRow("SELECT error_code FROM usage_events WHERE status_code=429").Scan(&code); err != nil || code != "quota:key:daily_requests" {
		t.Fatalf("the refusal was recorded as %q, err=%v", code, err)
	}

	reported := time.Now()
	day := reported.UTC().Truncate(24 * time.Hour).Add(24 * time.Hour).Unix()
	month := time.Date(reported.UTC().Year(), reported.UTC().Month()+1, 1, 0, 0, 0, 0, time.UTC).Unix()
	minute := now.Unix()/60*60 + 60
	limits := func(w *httptest.ResponseRecorder) []iam.LimitUsage {
		t.Helper()
		var body struct {
			Limits []iam.LimitUsage `json:"limits"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &body) != nil {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		return body.Limits
	}
	projectLimit := iam.LimitUsage{Scope: "project", Field: "monthly_requests", Metric: "requests", Period: "month", Limit: 50, Used: 2, ResetsAt: month}
	want := []iam.LimitUsage{
		{Scope: "key", Field: "daily_requests", Metric: "requests", Period: "day", Limit: 2, Used: 2, ResetsAt: day, Closest: true},
		projectLimit,
		// The refused request was started, so the per-caller limit counts it.
		{Scope: "caller", Field: "rate_limit_per_minute", Metric: "requests", Period: "minute", Limit: 5, Used: 3, ResetsAt: minute},
	}
	if got := limits(adminGet(handler, "/admin/api/keys/"+caller.KeyID+"/limits")); !reflect.DeepEqual(got, want) {
		t.Fatalf("key limits\n got %+v\nwant %+v", got, want)
	}
	projectLimit.Closest = true
	if got := limits(adminGet(handler, "/admin/api/projects/"+caller.ProjectID+"/limits")); !reflect.DeepEqual(got, []iam.LimitUsage{projectLimit}) {
		t.Fatalf("project limits %+v", got)
	}
	for _, path := range []string{"/admin/api/keys/key-unknown/limits", "/admin/api/projects/project-unknown/limits"} {
		if w := adminGet(handler, path); w.Code != http.StatusNotFound {
			t.Fatalf("%s: status=%d body=%s", path, w.Code, w.Body.String())
		}
	}
	anonymous := httptest.NewRecorder()
	handler.ServeHTTP(anonymous, httptest.NewRequest(http.MethodGet, "/admin/api/keys/"+caller.KeyID+"/limits", nil))
	if anonymous.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: status=%d", anonymous.Code)
	}

	config.Update(func(s *config.Settings) {
		s.SSOEnabled, s.SSOSharedSecret, s.SSOAutoProvision = true, "proxy-secret", true
	})
	server := httptest.NewServer(handler)
	defer server.Close()
	_, me := ssoConnectionRequest(t, server.URL, "limits-owner", http.MethodGet, "/user/api/me", nil)
	user := me["principal"].(map[string]any)["id"].(string)
	if err := iam.SetMembership(caller.ProjectID, user, "member"); err != nil {
		t.Fatal(err)
	}
	own, err := iam.IssueKey(iam.KeyCreate{ProjectID: caller.ProjectID, PrincipalID: user, Name: "own", Policy: iam.KeyPolicy{RPM: 3}})
	if err != nil {
		t.Fatal(err)
	}
	status, body := ssoConnectionRequest(t, server.URL, "limits-owner", http.MethodGet, "/user/api/keys/"+own.ID+"/limits", nil)
	if status != http.StatusOK {
		t.Fatalf("own key: status=%d body=%+v", status, body)
	}
	scopes := []string{}
	for _, raw := range body["limits"].([]any) {
		limit := raw.(map[string]any)
		scopes = append(scopes, limit["scope"].(string)+":"+limit["field"].(string))
	}
	if !reflect.DeepEqual(scopes, []string{"key:rpm", "project:monthly_requests", "caller:rate_limit_per_minute"}) {
		t.Fatalf("own key's limits %v", scopes)
	}
	if status, body := ssoConnectionRequest(t, server.URL, "limits-owner", http.MethodGet, "/user/api/keys/"+caller.KeyID+"/limits", nil); status != http.StatusNotFound {
		t.Fatalf("another principal's key: status=%d body=%+v", status, body)
	}
}
