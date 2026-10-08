package api

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"
)

// Keys are deleted in bulk, by an administrator or by the user who owns
// them, and only revoked or expired ones: a live key, another user's key
// and an unknown one are refused, each with its reason. Every deletion is
// audited with the key's name, and the deleted keys leave the key lists.
func TestDeletingKeysTakesOnlyRevokedOrExpiredOnes(t *testing.T) {
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
	_, me := ssoConnectionRequest(t, server.URL, "key-owner", http.MethodGet, "/user/api/me", nil)
	user := me["principal"].(map[string]any)["id"].(string)
	other, err := iam.CreatePrincipal("human", "fixture:key-other", "", "Other")
	if err != nil {
		t.Fatal(err)
	}
	project, err := iam.CreateProject("deletion-project", "Deletion Project")
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range []string{user, other.ID} {
		if err := iam.SetMembership(project.ID, member, "member"); err != nil {
			t.Fatal(err)
		}
	}
	issue := func(principalID, name string, expiresAt int64, revoke bool) string {
		t.Helper()
		key, err := iam.IssueKey(iam.KeyCreate{ProjectID: project.ID, PrincipalID: principalID, Name: name, ExpiresAt: expiresAt})
		if err != nil {
			t.Fatal(err)
		}
		if revoke {
			if err := iam.RevokeAPIKey(key.ID); err != nil {
				t.Fatal(err)
			}
		}
		return key.ID
	}
	live := issue(user, "live", 0, false)
	revoked := issue(user, "revoked", 0, true)
	expired := issue(user, "expired", time.Now().Add(-time.Minute).Unix(), false)
	othersRevoked := issue(other.ID, "others-revoked", 0, true)
	outcome := func(status int, body map[string]any) ([]string, map[string]string) {
		t.Helper()
		if status != http.StatusOK {
			t.Fatalf("delete status=%d body=%+v", status, body)
		}
		deleted := []string{}
		for _, id := range body["deleted"].([]any) {
			deleted = append(deleted, id.(string))
		}
		refused := map[string]string{}
		for _, raw := range body["refused"].([]any) {
			refusal := raw.(map[string]any)
			refused[refusal["id"].(string)] = refusal["error"].(string)
		}
		return deleted, refused
	}

	deleted, refused := outcome(ssoConnectionRequest(t, server.URL, "key-owner", http.MethodPost, "/user/api/keys/delete",
		map[string]any{"ids": []string{revoked, live, othersRevoked, revoked}}))
	if !slices.Equal(deleted, []string{revoked}) || refused[live] != iam.ErrAPIKeyLive.Error() ||
		refused[othersRevoked] != iam.ErrAPIKeyNotFound.Error() || len(refused) != 2 {
		t.Fatalf("portal deletion: deleted=%v refused=%v", deleted, refused)
	}
	deleted, refused = outcome(jsonRequest(t, server.URL+"/admin/api/keys/delete", http.MethodPost, "admin-secret",
		map[string]any{"ids": []string{expired, othersRevoked, "key-missing"}}))
	if !slices.Equal(deleted, []string{expired, othersRevoked}) || refused["key-missing"] != iam.ErrAPIKeyNotFound.Error() || len(refused) != 1 {
		t.Fatalf("administrator deletion: deleted=%v refused=%v", deleted, refused)
	}
	if status, body := jsonRequest(t, server.URL+"/admin/api/keys/delete", http.MethodPost, "admin-secret", map[string]any{"ids": []string{}}); status != http.StatusBadRequest {
		t.Fatalf("a deletion naming no key: status=%d body=%+v", status, body)
	}

	names := map[string]string{}
	for _, event := range auditEventsWithAction(t, "api_key.delete") {
		names[event.TargetID] = event.Detail["name"].(string)
	}
	if want := map[string]string{revoked: "revoked", expired: "expired", othersRevoked: "others-revoked"}; len(names) != 3 ||
		names[revoked] != want[revoked] || names[expired] != want[expired] || names[othersRevoked] != want[othersRevoked] {
		t.Fatalf("deletion audit names=%v, want %v", names, want)
	}
	_, listing := jsonRequest(t, server.URL+"/admin/api/keys?status=all", http.MethodGet, "admin-secret", nil)
	listed := []string{}
	for _, key := range listing["keys"].([]any) {
		listed = append(listed, key.(map[string]any)["id"].(string))
	}
	if !slices.Equal(listed, []string{live}) {
		t.Fatalf("listed keys=%v, want only the live one", listed)
	}
}
