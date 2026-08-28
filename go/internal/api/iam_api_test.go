package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
	"llmgw/internal/router"
)

func TestIAMAdminAndKeyAuthenticationE2E(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	router.ResetSavingsState()
	router.ResetTelemetryState()
	t.Cleanup(func() {
		iam.ResetForTests()
		router.ResetSavingsState()
		router.ResetTelemetryState()
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	config.Update(func(s *config.Settings) {
		s.APIKey = "admin-secret"
		s.APIKeys = nil
		s.AllowUnauthenticatedAPI = false
		s.Savings.Enabled = false
		s.Providers = map[string]*config.ProviderConfig{"echo": {Type: "echo"}}
		s.Endpoints = map[string]*config.EndpointConfig{}
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)
	server := httptest.NewServer(NewServer())
	defer server.Close()

	admin := func(method, path string, body any) (int, map[string]any) {
		t.Helper()
		return jsonRequest(t, server.URL+path, method, "admin-secret", body)
	}
	status, project := admin("POST", "/admin/api/projects", map[string]any{
		"slug": "project-a", "name": "Project A",
	})
	if status != http.StatusCreated {
		t.Fatalf("create project: %d %+v", status, project)
	}
	status, principal := admin("POST", "/admin/api/principals", map[string]any{
		"kind": "human", "external_subject": "authentik:user-a",
		"email": "a@example.com", "display_name": "User A",
	})
	if status != http.StatusCreated {
		t.Fatalf("create principal: %d %+v", status, principal)
	}
	status, membership := admin("POST", "/admin/api/memberships", map[string]any{
		"project_id": project["id"], "principal_id": principal["id"], "role": "owner",
	})
	if status != http.StatusOK || membership["ok"] != true {
		t.Fatalf("set membership: %d %+v", status, membership)
	}
	status, issued := admin("POST", "/admin/api/keys", map[string]any{
		"project_id": project["id"], "principal_id": principal["id"],
		"name": "laptop", "allowed_models": []string{"echo/echo-default"},
		"rpm": 10, "daily_requests": 20,
	})
	if status != http.StatusOK {
		t.Fatalf("issue key: %d %+v", status, issued)
	}
	token, _ := issued["token"].(string)
	if token == "" {
		t.Fatal("issued key token missing")
	}
	resolved, found, err := iam.ResolveAPIKey(token)
	if err != nil || !found || resolved.KeyID == "" || resolved.ProjectID == "" {
		t.Fatalf("issued key did not resolve with IAM ids: %+v found=%v err=%v", resolved, found, err)
	}
	status, state := admin("GET", "/admin/api/state", nil)
	if status != http.StatusOK {
		t.Fatalf("state: %d %+v", status, state)
	}
	keys := state["keys"].([]any)
	listed := keys[0].(map[string]any)
	if listed["prefix"] == "" || listed["token"] != nil {
		t.Fatalf("listed key leaks token or lacks prefix: %+v", listed)
	}

	status, chat := jsonRequest(t, server.URL+"/v1/chat/completions", "POST", token, map[string]any{
		"model":    "echo/echo-default",
		"messages": []map[string]any{{"role": "user", "content": "hello"}},
	})
	if status != http.StatusOK {
		t.Fatalf("chat with issued key: %d %+v", status, chat)
	}
	if chat["model"] != "echo-default" {
		t.Fatalf("chat model=%v", chat["model"])
	}
	db, err := iam.DB()
	if err != nil {
		t.Fatal(err)
	}
	var usageCount int
	if err := db.QueryRow("SELECT COUNT(*) FROM usage_events").Scan(&usageCount); err != nil {
		t.Fatal(err)
	}
	if usageCount != 1 {
		t.Fatalf("usage_events count=%d, want 1", usageCount)
	}
	key := issued["key"].(map[string]any)
	status, revoked := admin("DELETE", "/admin/api/keys?id="+key["id"].(string), nil)
	if status != http.StatusOK || revoked["ok"] != true {
		t.Fatalf("revoke: %d %+v", status, revoked)
	}
	status, rejected := admin("POST", "/admin/api/keys/update", map[string]any{"id": key["id"], "disabled": false})
	if status != http.StatusBadRequest || rejected["error"] == nil {
		t.Fatalf("revoked admin update: %d %+v", status, rejected)
	}
	status, audit := admin("GET", "/admin/api/audit", nil)
	if status != http.StatusOK {
		t.Fatalf("audit: %d %+v", status, audit)
	}
	if events, ok := audit["events"].([]any); !ok || len(events) < 4 {
		t.Fatalf("audit events=%+v", audit["events"])
	}
	status, _ = jsonRequest(t, server.URL+"/v1/chat/completions", "POST", token, map[string]any{
		"model":    "echo/echo-default",
		"messages": []map[string]any{{"role": "user", "content": "hello"}},
	})
	if status != http.StatusUnauthorized {
		t.Fatalf("revoked key status=%d, want 401", status)
	}
}

func TestProjectPolicyAdminAPIRoundTripReplacement(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	config.Update(func(s *config.Settings) {
		s.APIKey = "admin-secret"
		s.AllowUnauthenticatedAPI = false
	})
	project, err := iam.CreateProject("policy-roundtrip", "Policy Round Trip")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(NewServer())
	defer server.Close()
	path := server.URL + "/admin/api/projects/" + project.ID + "/policy"

	policy := iam.KeyPolicy{
		AllowedModels:       []string{"alpha/model-one", "beta/model-two"},
		AllowedProviders:    []string{"alpha", "beta"},
		RPM:                 101,
		DailyRequests:       202,
		MonthlyRequests:     303,
		DailyInputTokens:    404,
		DailyOutputTokens:   505,
		MonthlyTotalTokens:  606,
		DailyCostMicroUSD:   707,
		MonthlyCostMicroUSD: 808,
		DailyCreditsMilli:   909,
		MonthlyCreditsMilli: 1010,
	}
	assertPolicy := func(label string, got map[string]any, want iam.KeyPolicy) {
		t.Helper()
		payload, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		var response iam.ProjectPolicy
		if err := json.Unmarshal(payload, &response); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(response.KeyPolicy, want) {
			t.Errorf("%s policy = %#v, want %#v", label, response.KeyPolicy, want)
		}
	}

	status, posted := jsonRequest(t, path, http.MethodPost, "admin-secret", policy)
	if status != http.StatusOK {
		t.Fatalf("set project policy: %d %+v", status, posted)
	}
	assertPolicy("POST", posted, policy)
	status, fetched := jsonRequest(t, path, http.MethodGet, "admin-secret", nil)
	if status != http.StatusOK {
		t.Fatalf("get project policy: %d %+v", status, fetched)
	}
	assertPolicy("GET", fetched, policy)

	clear := map[string]any{
		"allowed_models":        []string{},
		"allowed_providers":     []string{},
		"rpm":                   0,
		"daily_requests":        0,
		"monthly_requests":      0,
		"daily_input_tokens":    0,
		"daily_output_tokens":   0,
		"monthly_total_tokens":  0,
		"daily_cost_microusd":   0,
		"monthly_cost_microusd": 0,
		"daily_credits_milli":   0,
		"monthly_credits_milli": 0,
	}
	status, replaced := jsonRequest(t, path, http.MethodPost, "admin-secret", clear)
	if status != http.StatusOK {
		t.Fatalf("clear project policy: %d %+v", status, replaced)
	}
	assertPolicy("replacement POST", replaced, iam.KeyPolicy{})
	status, cleared := jsonRequest(t, path, http.MethodGet, "admin-secret", nil)
	if status != http.StatusOK {
		t.Fatalf("get cleared project policy: %d %+v", status, cleared)
	}
	assertPolicy("GET after replacement", cleared, iam.KeyPolicy{})
}

func jsonRequest(
	t *testing.T, url, method, token string, body any,
) (int, map[string]any) {
	t.Helper()
	var payload []byte
	if body != nil {
		payload, _ = json.Marshal(body)
	}

	req, err := http.NewRequest(method, url, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestFailedRequestRecordedInUsageStats(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	router.ResetSavingsState()
	t.Cleanup(func() {
		iam.ResetForTests()
		router.ResetSavingsState()
	})
	_, _ = iam.Initialize()
	config.Update(func(s *config.Settings) {
		s.AllowUnauthenticatedAPI = false
		s.APIKey = "admin-secret"
		s.Providers = map[string]*config.ProviderConfig{}
		s.Endpoints = map[string]*config.EndpointConfig{}
		s.Savings.Enabled = false
	})
	principal, _ := iam.CreatePrincipal("service", "service:errors", "", "Errors")
	project, _ := iam.CreateProject("errors", "Errors")
	_ = iam.SetMembership(project.ID, principal.ID, "member")
	issued, _ := iam.IssueKey(iam.KeyCreate{
		ProjectID: project.ID, PrincipalID: principal.ID, Name: "errors",
	})
	server := httptest.NewServer(NewServer())
	defer server.Close()
	status, _ := jsonRequest(
		t, server.URL+"/v1/chat/completions", http.MethodPost, issued.Token,
		map[string]any{
			"model":    "missing-model",
			"messages": []map[string]any{{"role": "user", "content": "hi"}},
		},
	)
	if status != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", status)
	}
	stats, err := iam.UsageStats(0)
	if err != nil {
		t.Fatal(err)
	}
	total := stats["totals"].(iam.UsageTotals)
	if total.Requests != 1 || total.Errors != 1 {
		t.Fatalf("failure totals=%+v", total)
	}
}

// A key minted for a human principal inherits that human's private provider
// connections; an auto-created service-principal key does not. Regression for
// the UAT finding that a Copilot-only workspace could not drive a CLI.
func TestCreateKeyBindsToRequestedHumanPrincipal(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	config.Update(func(s *config.Settings) {
		s.APIKey = "admin-secret"
		s.AllowUnauthenticatedAPI = false
	})
	server := httptest.NewServer(NewServer())
	defer server.Close()

	status, principal := jsonRequest(t, server.URL+"/admin/api/principals", http.MethodPost, "admin-secret", map[string]any{
		"kind": "human", "display_name": "Key Owner",
	})
	if status != http.StatusCreated {
		t.Fatalf("create principal: %d %+v", status, principal)
	}
	ownerID, _ := principal["id"].(string)

	status, project := jsonRequest(t, server.URL+"/admin/api/projects", http.MethodPost, "admin-secret", map[string]any{
		"slug": "keys", "name": "Keys",
	})
	if status != http.StatusCreated {
		t.Fatalf("create project: %d %+v", status, project)
	}
	projectID, _ := project["id"].(string)

	// A key's principal must be a project member — the Access page flow.
	status, membership := jsonRequest(t, server.URL+"/admin/api/memberships", http.MethodPost, "admin-secret", map[string]any{
		"project_id": projectID, "principal_id": ownerID, "role": "owner",
	})
	if status != http.StatusOK {
		t.Fatalf("set membership: %d %+v", status, membership)
	}

	status, created := jsonRequest(t, server.URL+"/admin/api/keys", http.MethodPost, "admin-secret", map[string]any{
		"project_id": projectID, "name": "human-key", "principal_id": ownerID,
	})
	if status != http.StatusOK {
		t.Fatalf("create key: %d %+v", status, created)
	}

	keys, err := iam.ListAPIKeys("")
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, key := range keys {
		if key.Name == "human-key" {
			found = true
			if key.PrincipalID != ownerID {
				t.Fatalf("key principal = %q, want the requested human %q", key.PrincipalID, ownerID)
			}
			if key.Kind != "human" {
				t.Fatalf("key principal kind = %q, want human", key.Kind)
			}
		}
	}
	if !found {
		t.Fatalf("human-key not listed")
	}

	// Without principal_id the gateway still falls back to a service identity.
	status, _ = jsonRequest(t, server.URL+"/admin/api/keys", http.MethodPost, "admin-secret", map[string]any{
		"project_id": projectID, "name": "service-key",
	})
	if status != http.StatusOK {
		t.Fatalf("create service key: %d", status)
	}
	keys, _ = iam.ListAPIKeys("")
	for _, key := range keys {
		if key.Name == "service-key" && key.Kind != "service" {
			t.Fatalf("fallback key kind = %q, want service", key.Kind)
		}
	}
}
