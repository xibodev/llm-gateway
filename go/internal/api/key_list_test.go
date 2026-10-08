package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
)

// The key listing answers a page of keys, newest first unless asked
// otherwise, with how many the filter selects; a user's lists only the
// user's own keys, and a filter it cannot apply is refused.
func TestKeyListingPagesKeys(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	old := *config.Get()
	t.Cleanup(func() { config.Update(func(s *config.Settings) { *s = old }) })
	config.Update(func(s *config.Settings) {
		s.APIKey, s.AllowUnauthenticatedAPI = "fixture-gateway-token", false
		s.SSOEnabled, s.SSOSharedSecret, s.SSOAutoProvision = true, "proxy-secret", true
	})
	handler := NewServer(Runtime{})
	server := httptest.NewServer(handler)
	defer server.Close()
	_, me := ssoConnectionRequest(t, server.URL, "list-owner", http.MethodGet, "/user/api/me", nil)
	user := me["principal"].(map[string]any)["id"].(string)
	other, err := iam.CreatePrincipal("human", "fixture:list-other", "", "Other")
	if err != nil {
		t.Fatal(err)
	}
	project, err := iam.CreateProject("listing", "Listing")
	if err != nil {
		t.Fatal(err)
	}
	for _, principal := range []string{user, other.ID} {
		if err := iam.SetMembership(project.ID, principal, "member"); err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range []struct{ owner, name string }{{user, "first"}, {other.ID, "second"}, {user, "third"}} {
		if _, err := iam.IssueKey(iam.KeyCreate{ProjectID: project.ID, PrincipalID: key.owner, Name: key.name}); err != nil {
			t.Fatal(err)
		}
	}
	type listing struct {
		Keys []struct {
			Name   string         `json:"name"`
			RPM    *int           `json:"rpm"`
			Policy map[string]any `json:"policy"`
		} `json:"keys"`
		Total int `json:"total"`
	}
	names := func(page listing) []string {
		out := []string{}
		for _, key := range page.Keys {
			out = append(out, key.Name)
		}
		return out
	}
	read := func(w *httptest.ResponseRecorder) listing {
		t.Helper()
		var page listing
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &page) != nil {
			t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
		}
		return page
	}
	page := read(adminGet(handler, "/admin/api/keys?limit=2"))
	if got := names(page); len(got) != 2 || got[0] != "third" || got[1] != "second" || page.Total != 3 {
		t.Fatalf("the first page, newest first: %v of %d", got, page.Total)
	}
	if page.Keys[0].RPM == nil || page.Keys[0].Policy != nil {
		t.Fatalf("an administrator's key row is flat: %+v", page.Keys[0])
	}
	page = read(adminGet(handler, "/admin/api/keys?sort=name&order=asc&offset=1&q=IR"))
	if got := names(page); len(got) != 1 || got[0] != "third" || page.Total != 2 {
		t.Fatalf("searched by name, second of two: %v of %d", got, page.Total)
	}
	for _, query := range []string{"status=lost", "sort=secret", "order=up", "limit=many", "offset=-1"} {
		if w := adminGet(handler, "/admin/api/keys?"+query); w.Code != http.StatusBadRequest {
			t.Fatalf("%s: status=%d", query, w.Code)
		}
	}
	anonymous := httptest.NewRecorder()
	handler.ServeHTTP(anonymous, httptest.NewRequest(http.MethodGet, "/admin/api/keys", nil))
	if anonymous.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: status=%d", anonymous.Code)
	}

	status, body := ssoConnectionRequest(t, server.URL, "list-owner", http.MethodGet, "/user/api/keys?principal_id="+other.ID, nil)
	keys, _ := body["keys"].([]any)
	if status != http.StatusOK || len(keys) != 2 || body["total"] != float64(2) {
		t.Fatalf("a user's listing: status=%d body=%+v", status, body)
	}
	for _, raw := range keys {
		key := raw.(map[string]any)
		if key["principal_id"] != user || key["policy"] == nil {
			t.Fatalf("a user's listing holds %+v", key)
		}
	}
}
