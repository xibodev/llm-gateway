package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

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
	server := httptest.NewServer(NewServer())
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
	server := httptest.NewServer(NewServer())
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
	server := httptest.NewServer(NewServer())
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
	server := httptest.NewServer(NewServer())
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
