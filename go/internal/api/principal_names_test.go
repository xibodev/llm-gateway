package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
)

// A principal SSO provisions takes its name from the identity provider at
// every sign-in, and never from the opaque subject once a name is known, until
// an administrator renames it; the rename is audited and sticks.
func TestSSONamesFollowTheIdentityProviderUntilAnAdministratorRenames(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	old := *config.Get()
	t.Cleanup(func() { config.Update(func(s *config.Settings) { *s = old }) })
	config.Update(func(s *config.Settings) {
		s.APIKey, s.AllowUnauthenticatedAPI = "admin-secret", false
		s.SSOEnabled, s.SSOSharedSecret, s.SSOAutoProvision = true, "proxy-secret", true
	})
	server := httptest.NewServer(NewServer(Runtime{}))
	defer server.Close()
	signIn := func(headers map[string]string) map[string]any {
		t.Helper()
		request, _ := http.NewRequest(http.MethodGet, server.URL+"/user/api/me", nil)
		request.Header.Set(ssoSecretHeader, "proxy-secret")
		request.Header.Set(ssoSubjectHeader, "fixture-subject-7f3a")
		for name, value := range headers {
			request.Header.Set(name, value)
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("sign-in status=%d", response.StatusCode)
		}
		principal, _, err := iam.PrincipalBySubject("authentik:fixture-subject-7f3a")
		if err != nil {
			t.Fatal(err)
		}
		return map[string]any{"name": principal.DisplayName, "email": principal.Email, "id": principal.ID}
	}

	if got := signIn(nil); got["name"] != "fixture-subject-7f3a" {
		t.Fatalf("a sign-in with only the subject named the principal %v", got["name"])
	}
	if got := signIn(map[string]string{ssoNameHeader: "Ada Lovelace", ssoEmailHeader: "ada@example.test"}); got["name"] != "Ada Lovelace" || got["email"] != "ada@example.test" {
		t.Fatalf("a sign-in with a name left the principal %+v", got)
	}
	if got := signIn(nil); got["name"] != "Ada Lovelace" {
		t.Fatalf("a sign-in with only the subject renamed the principal %v", got["name"])
	}
	if got := signIn(map[string]string{ssoUsernameHeader: "ada"}); got["name"] != "ada" {
		t.Fatalf("a sign-in with a username left the principal named %v", got["name"])
	}
	id := signIn(nil)["id"].(string)

	renamed := expectOneAdminEvent(t, "principal.rename", "admin-secret", func() {
		status, body := jsonRequest(t, server.URL+"/admin/api/principals/"+id+"/rename", http.MethodPost, "admin-secret", map[string]any{"display_name": "Ada (research)"})
		if status != http.StatusOK || body["display_name"] != "Ada (research)" || body["name_set_by_admin"] != true {
			t.Fatalf("rename status=%d body=%+v", status, body)
		}
	})
	if renamed.TargetID != id || renamed.Detail["from"] != "ada" || renamed.Detail["to"] != "Ada (research)" {
		t.Fatalf("rename audit target=%s detail=%+v", renamed.TargetID, renamed.Detail)
	}
	if got := signIn(map[string]string{ssoNameHeader: "Ada Lovelace", ssoEmailHeader: "ada@new.example.test"}); got["name"] != "Ada (research)" || got["email"] != "ada@new.example.test" {
		t.Fatalf("a sign-in after the rename left the principal %+v", got)
	}

	for _, tc := range []struct {
		id, name string
		status   int
	}{
		{id, " ", http.StatusBadRequest},
		{"prn-missing", "Someone", http.StatusNotFound},
	} {
		if status, body := jsonRequest(t, server.URL+"/admin/api/principals/"+tc.id+"/rename", http.MethodPost, "admin-secret", map[string]any{"display_name": tc.name}); status != tc.status {
			t.Errorf("rename %s to %q: status=%d body=%+v, want %d", tc.id, tc.name, status, body, tc.status)
		}
	}
	if status, _ := jsonRequest(t, server.URL+"/admin/api/principals/"+id+"/rename", http.MethodPost, "", map[string]any{"display_name": "Mallory"}); status != http.StatusUnauthorized {
		t.Errorf("an unauthenticated rename: status=%d, want 401", status)
	}
}
