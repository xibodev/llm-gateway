package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"testing"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/router"
)

func TestSSOUserSelfServiceKeyLifecycle(t *testing.T) {
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
		s.SSOEnabled = true
		s.SSOSharedSecret = "proxy-secret"
		s.SSOAutoProvision = true
		s.APIKey = "admin-secret"
		s.Providers = map[string]*config.ProviderConfig{}
		s.Endpoints = map[string]*config.EndpointConfig{}
		s.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	})
	principal, _ := iam.EnsurePrincipalBySubject(
		"human", "authentik:user-self", "self@example.com", "Self User",
	)
	project, _ := iam.CreateProject("self-project", "Self Project")
	if err := iam.SetMembership(project.ID, principal.ID, "member"); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(NewServer(Runtime{}))
	defer server.Close()
	origin, _ := url.Parse(server.URL)

	request := func(method, path string, body any) (int, map[string]any) {
		t.Helper()
		var raw []byte
		if body != nil {
			raw, _ = json.Marshal(body)
		}
		req, _ := http.NewRequest(method, server.URL+path, bytes.NewReader(raw))
		req.Header.Set(ssoSecretHeader, "proxy-secret")
		req.Header.Set(ssoSubjectHeader, "user-self")
		req.Header.Set(ssoEmailHeader, "self@example.com")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if mutatingMethod(method) {
			req.Header.Set("Origin", origin.Scheme+"://"+origin.Host)
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

	status, me := request("GET", "/user/api/me", nil)
	if status != http.StatusOK {
		t.Fatalf("me: %d %+v", status, me)
	}
	if len(me["projects"].([]any)) != 1 {
		t.Fatalf("projects=%+v", me["projects"])
	}
	status, issued := request("POST", "/user/api/keys", map[string]any{
		"project_id": project.ID, "name": "laptop", "daily_requests": 10,
	})
	if status != http.StatusOK || issued["token"] == "" {
		t.Fatalf("issue: %d %+v", status, issued)
	}
	key := issued["key"].(map[string]any)
	status, revealed := request("POST", "/user/api/keys/"+key["id"].(string)+"/reveal", nil)
	if status != http.StatusOK || revealed["token"] != issued["token"] {
		t.Fatalf("reveal: %d token_matches=%v", status, revealed["token"] == issued["token"])
	}
	status, me = request("GET", "/user/api/me", nil)
	if status != http.StatusOK || len(me["keys"].([]any)) != 1 {
		t.Fatalf("me after issue: %d %+v", status, me)
	}
	status, revoked := request(
		"DELETE", "/user/api/keys/"+key["id"].(string), nil,
	)
	if status != http.StatusOK || revoked["ok"] != true {
		t.Fatalf("revoke: %d %+v", status, revoked)
	}
}

// The portal's request listing shows only the signed-in user's requests,
// whatever principal the query names, with the administrator's filters and
// paging, and names the providers the user's requests used.
func TestSSOUserListsOnlyTheirOwnRequests(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	old := *config.Get()
	t.Cleanup(func() { config.Update(func(s *config.Settings) { *s = old }) })
	config.Update(func(s *config.Settings) {
		s.SSOEnabled, s.SSOSharedSecret, s.SSOAutoProvision = true, "proxy-secret", true
	})
	server := httptest.NewServer(NewServer(Runtime{}))
	defer server.Close()
	_, me := ssoConnectionRequest(t, server.URL, "requests-user", http.MethodGet, "/user/api/me", nil)
	user := me["principal"].(map[string]any)["id"].(string)
	other, err := iam.CreatePrincipal("human", "fixture:requests-other", "", "Other")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	for _, event := range []iam.UsageEvent{
		{RequestID: "req_mine_ok", Timestamp: now - 90, Endpoint: "openai.chat", StatusCode: 200, Provider: "alpha", RoutedModel: "alpha-model", PrincipalID: user},
		{RequestID: "req_mine_failed", Timestamp: now - 60, Endpoint: "openai.chat", StatusCode: 502, Provider: "beta", RoutedModel: "beta-model", PrincipalID: user},
		{RequestID: "req_theirs", Timestamp: now - 30, Endpoint: "openai.chat", StatusCode: 200, Provider: "gamma", RoutedModel: "gamma-model", PrincipalID: other.ID},
	} {
		if err := iam.RecordUsageEvent(event); err != nil {
			t.Fatal(err)
		}
	}
	list := func(query string) ([]string, map[string]any) {
		t.Helper()
		status, body := ssoConnectionRequest(t, server.URL, "requests-user", http.MethodGet, "/user/api/requests?"+query, nil)
		if status != http.StatusOK {
			t.Fatalf("%s: status=%d body=%+v", query, status, body)
		}
		ids := []string{}
		for _, row := range body["requests"].([]any) {
			ids = append(ids, row.(map[string]any)["request_id"].(string))
		}
		return ids, body
	}
	ids, body := list("")
	if !reflect.DeepEqual(ids, []string{"req_mine_failed", "req_mine_ok"}) {
		t.Fatalf("listed requests=%v, want only the user's, newest first", ids)
	}
	if !reflect.DeepEqual(body["providers"], []any{"alpha", "beta"}) {
		t.Fatalf("providers=%v, want the user's alpha and beta", body["providers"])
	}
	if ids, _ := list("principal_id=" + other.ID); !reflect.DeepEqual(ids, []string{"req_mine_failed", "req_mine_ok"}) {
		t.Fatalf("a query naming another principal listed %v", ids)
	}
	if ids, _ := list("request_id=req_theirs"); len(ids) != 0 {
		t.Fatalf("another principal's request was found: %v", ids)
	}
	if ids, _ := list("status=error"); !reflect.DeepEqual(ids, []string{"req_mine_failed"}) {
		t.Fatalf("failed requests=%v", ids)
	}
	ids, body = list("limit=1")
	next, _ := body["next_before_id"].(float64)
	if !reflect.DeepEqual(ids, []string{"req_mine_failed"}) || next == 0 {
		t.Fatalf("first page=%v next=%v", ids, body["next_before_id"])
	}
	if ids, _ := list("limit=1&before_id=" + strconv.FormatInt(int64(next), 10)); !reflect.DeepEqual(ids, []string{"req_mine_ok"}) {
		t.Fatalf("second page=%v", ids)
	}
	if status, _ := ssoConnectionRequest(t, server.URL, "requests-user", http.MethodGet, "/user/api/requests?status=sometimes", nil); status != http.StatusBadRequest {
		t.Fatalf("an invalid filter: status=%d, want 400", status)
	}
	if status, _ := jsonRequest(t, server.URL+"/user/api/requests", http.MethodGet, "", nil); status != http.StatusUnauthorized {
		t.Fatalf("without a sign-in: status=%d, want 401", status)
	}
}

func TestSSOUserCannotRevealAnotherPrincipalsKey(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	_, _ = iam.Initialize()
	config.Update(func(s *config.Settings) {
		s.SSOEnabled = true
		s.SSOSharedSecret = "proxy-secret"
		s.SSOAutoProvision = true
		s.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	})
	owner, _ := iam.EnsurePrincipalBySubject("human", "authentik:key-owner", "", "Key Owner")
	project, _ := iam.CreateProject("private-key", "Private Key")
	_ = iam.SetMembership(project.ID, owner.ID, "owner")
	issued, _ := iam.IssueKey(iam.KeyCreate{ProjectID: project.ID, PrincipalID: owner.ID, Name: "private"})
	server := httptest.NewServer(NewServer(Runtime{}))
	defer server.Close()
	origin, _ := url.Parse(server.URL)
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/user/api/keys/"+issued.ID+"/reveal", nil)
	req.Header.Set("Origin", origin.Scheme+"://"+origin.Host)
	req.Header.Set(ssoSecretHeader, "proxy-secret")
	req.Header.Set(ssoSubjectHeader, "other-user")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-principal reveal status=%d, want 404", resp.StatusCode)
	}
}

func TestSSOViewerCannotMintKey(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	_, _ = iam.Initialize()
	config.Update(func(s *config.Settings) {
		s.SSOEnabled = true
		s.SSOSharedSecret = "proxy-secret"
		s.SSOAutoProvision = true
	})
	principal, _ := iam.EnsurePrincipalBySubject(
		"human", "authentik:viewer", "", "Viewer",
	)
	project, _ := iam.CreateProject("viewer-project", "Viewer Project")
	_ = iam.SetMembership(project.ID, principal.ID, "viewer")
	server := httptest.NewServer(NewServer(Runtime{}))
	defer server.Close()
	origin, _ := url.Parse(server.URL)
	body, _ := json.Marshal(map[string]any{"project_id": project.ID, "name": "denied"})
	req, _ := http.NewRequest(
		"POST", server.URL+"/user/api/keys", bytes.NewReader(body),
	)
	req.Header.Set("Origin", origin.Scheme+"://"+origin.Host)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(ssoSecretHeader, "proxy-secret")
	req.Header.Set(ssoSubjectHeader, "viewer")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}

	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer issue status=%d, want 403", resp.StatusCode)
	}
}

func TestSSOUserHonorsDisabledAutoProvision(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	_, _ = iam.Initialize()
	config.Update(func(s *config.Settings) {
		s.SSOEnabled = true
		s.SSOSharedSecret = "proxy-secret"
		s.SSOAutoProvision = false
	})
	server := httptest.NewServer(NewServer(Runtime{}))
	defer server.Close()
	request := func() int {
		req, _ := http.NewRequest("GET", server.URL+"/user/api/me", nil)
		req.Header.Set(ssoSecretHeader, "proxy-secret")
		req.Header.Set(ssoSubjectHeader, "preprovisioned")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if got := request(); got != http.StatusForbidden {
		t.Fatalf("unprovisioned status=%d, want 403", got)
	}
	if _, err := iam.CreatePrincipal(
		"human", "authentik:preprovisioned", "", "Preprovisioned",
	); err != nil {
		t.Fatal(err)
	}
	if got := request(); got != http.StatusOK {
		t.Fatalf("preprovisioned status=%d, want 200", got)
	}
}

// The portal's usage report breaks the signed-in user's usage down by
// provider, model, key and project, and lists every provider that usage
// names for the filter, whichever one the request names. No other
// principal's usage shows.
func TestSSOUserUsageBreaksDownTheirOwnUsage(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	old := *config.Get()
	t.Cleanup(func() { config.Update(func(s *config.Settings) { *s = old }) })
	config.Update(func(s *config.Settings) {
		s.SSOEnabled, s.SSOSharedSecret, s.SSOAutoProvision = true, "proxy-secret", true
	})
	server := httptest.NewServer(NewServer(Runtime{}))
	defer server.Close()
	status, me := ssoConnectionRequest(t, server.URL, "usage-user", http.MethodGet, "/user/api/me", nil)
	if status != http.StatusOK {
		t.Fatalf("me: %d %+v", status, me)
	}
	user := me["principal"].(map[string]any)["id"].(string)
	other, err := iam.CreatePrincipal("human", "fixture:usage-other", "", "Other")
	if err != nil {
		t.Fatal(err)
	}
	project, err := iam.CreateProject("usage-project", "Usage Project")
	if err != nil {
		t.Fatal(err)
	}
	if err := iam.SetMembership(project.ID, user, "member"); err != nil {
		t.Fatal(err)
	}
	key, err := iam.IssueKey(iam.KeyCreate{ProjectID: project.ID, PrincipalID: user, Name: "usage key"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	for _, event := range []iam.UsageEvent{
		{Timestamp: now - 90, Endpoint: "openai.chat", StatusCode: 200, Provider: "alpha", RoutedModel: "alpha-model", PrincipalID: user, ProjectID: project.ID, KeyID: key.ID},
		{Timestamp: now - 60, Endpoint: "openai.chat", StatusCode: 502, Provider: "beta", RoutedModel: "beta-model", PrincipalID: user},
		{Timestamp: now - 30, Endpoint: "openai.chat", StatusCode: 200, Provider: "gamma", RoutedModel: "gamma-model", PrincipalID: other.ID},
	} {
		if err := iam.RecordUsageEvent(event); err != nil {
			t.Fatal(err)
		}
	}
	status, report := ssoConnectionRequest(t, server.URL, "usage-user", http.MethodGet, "/user/api/usage?provider=alpha", nil)
	if status != http.StatusOK {
		t.Fatalf("usage: %d %+v", status, report)
	}
	values := func(rows any, field string) []string {
		out := []string{}
		list, _ := rows.([]any)
		for _, row := range list {
			out = append(out, row.(map[string]any)[field].(string))
		}
		return out
	}
	controlPlane, _ := report["control_plane"].(map[string]any)
	groups, _ := controlPlane["groups"].(map[string]any)
	if groups == nil {
		t.Fatalf("the usage report has no breakdown: %+v", report)
	}
	for _, breakdown := range []struct {
		group, field string
		want         []string
	}{
		{"provider", "provider", []string{"alpha"}},
		{"model", "model", []string{"alpha-model"}},
		{"key", "key_id", []string{key.ID}},
		{"project", "project_id", []string{project.ID}},
		{"principal", "principal_id", []string{user}},
	} {
		if got := values(groups[breakdown.group], breakdown.field); !reflect.DeepEqual(got, breakdown.want) {
			t.Errorf("%s breakdown = %v, want %v", breakdown.group, got, breakdown.want)
		}
	}
	if got := report["providers"]; !reflect.DeepEqual(got, []any{"alpha", "beta"}) {
		t.Errorf("providers = %v, want the user's alpha and beta", got)
	}
}
