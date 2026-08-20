package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/router"
)

func TestAdminRejectsProjectKeyAndAcceptsVerifiedSSO(t *testing.T) {
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
		s.AllowUnauthenticatedAPI = false
		s.SSOEnabled = true
		s.SSOSharedSecret = "proxy-secret"
		s.SSOAdminGroup = "llmgw-admin"
		s.SSOAutoProvision = true
		s.Providers = map[string]*config.ProviderConfig{}
		s.Endpoints = map[string]*config.EndpointConfig{}
	})
	principal, _ := iam.CreatePrincipal("service", "service:test", "", "Test Service")
	project, _ := iam.CreateProject("test", "Test")
	_ = iam.SetMembership(project.ID, principal.ID, "member")
	issued, err := iam.IssueKey(iam.KeyCreate{
		ProjectID: project.ID, PrincipalID: principal.ID, Name: "service",
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(NewServer())
	defer server.Close()

	req, _ := http.NewRequest("GET", server.URL+"/admin/api/state", nil)
	req.Header.Set("Authorization", "Bearer "+issued.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("project key admin status=%d, want 401", resp.StatusCode)
	}

	req, _ = http.NewRequest("GET", server.URL+"/admin/api/state", nil)
	req.Header.Set(ssoSecretHeader, "proxy-secret")
	req.Header.Set(ssoSubjectHeader, "user-123")
	req.Header.Set(ssoUsernameHeader, "alex")
	req.Header.Set(ssoEmailHeader, "alex@example.com")
	req.Header.Set(ssoGroupsHeader, "developers|llmgw-admin")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("SSO admin status=%d, want 200", resp.StatusCode)
	}
	if _, ok, err := iam.PrincipalBySubject("authentik:user-123"); err != nil || !ok {
		t.Fatalf("SSO principal not provisioned: ok=%v err=%v", ok, err)
	}

	projectBody, _ := json.Marshal(map[string]any{"slug": "sso-project", "name": "SSO Project"})
	req, _ = http.NewRequest(
		"POST", server.URL+"/admin/api/projects", bytes.NewReader(projectBody),
	)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(ssoSecretHeader, "proxy-secret")
	req.Header.Set(ssoSubjectHeader, "user-123")
	req.Header.Set(ssoGroupsHeader, "llmgw-admin")
	resp, _ = http.DefaultClient.Do(req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("SSO mutation without Origin status=%d, want 403", resp.StatusCode)
	}
	parsed, _ := url.Parse(server.URL)
	req, _ = http.NewRequest(
		"POST", server.URL+"/admin/api/projects", bytes.NewReader(projectBody),
	)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://"+parsed.Host)
	req.Header.Set(ssoSecretHeader, "proxy-secret")
	req.Header.Set(ssoSubjectHeader, "user-123")
	req.Header.Set(ssoGroupsHeader, "llmgw-admin")
	resp, _ = http.DefaultClient.Do(req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-scheme SSO mutation status=%d, want 403", resp.StatusCode)
	}
	req, _ = http.NewRequest(
		"POST", server.URL+"/admin/api/projects", bytes.NewReader(projectBody),
	)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", parsed.Scheme+"://"+parsed.Host)
	req.Header.Set(ssoSecretHeader, "proxy-secret")
	req.Header.Set(ssoSubjectHeader, "user-123")
	req.Header.Set(ssoGroupsHeader, "llmgw-admin")
	resp, _ = http.DefaultClient.Do(req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("same-origin SSO mutation status=%d, want 201", resp.StatusCode)
	}

	req, _ = http.NewRequest("GET", server.URL+"/admin/api/state", nil)
	req.Header.Set(ssoSecretHeader, "proxy-secret")
	req.Header.Set(ssoSubjectHeader, "user-456")
	req.Header.Set(ssoGroupsHeader, "developers")
	resp, _ = http.DefaultClient.Do(req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin SSO status=%d, want 403", resp.StatusCode)
	}

	provisioned, _, _ := iam.PrincipalBySubject("authentik:user-123")
	if err := iam.SetPrincipalStatus(provisioned.ID, "disabled"); err != nil {
		t.Fatal(err)
	}
	req, _ = http.NewRequest("GET", server.URL+"/admin/api/state", nil)
	req.Header.Set(ssoSecretHeader, "proxy-secret")
	req.Header.Set(ssoSubjectHeader, "user-123")
	req.Header.Set(ssoGroupsHeader, "llmgw-admin")
	resp, _ = http.DefaultClient.Do(req)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("disabled SSO admin status=%d, want 403", resp.StatusCode)
	}
}
