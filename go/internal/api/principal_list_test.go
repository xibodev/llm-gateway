package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
)

// principalListingFixture is an administrator handler over a store with
// three people, one of them disabled, a service principal, and a project
// whose members are Ada and the service principal.
func principalListingFixture(t *testing.T) (http.Handler, map[string]string, string) {
	t.Helper()
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	old := *config.Get()
	t.Cleanup(func() { config.Update(func(s *config.Settings) { *s = old }) })
	config.Update(func(s *config.Settings) { s.APIKey, s.AllowUnauthenticatedAPI = "fixture-gateway-token", false })
	ids := map[string]string{}
	for _, principal := range []struct{ kind, subject, name string }{
		{"human", "fixture:ada", "Ada"}, {"human", "fixture:bob", "bob"}, {"human", "fixture:cy", "Cy"}, {"service", "", "Batch"},
	} {
		created, err := iam.CreatePrincipal(principal.kind, principal.subject, "", principal.name)
		if err != nil {
			t.Fatal(err)
		}
		ids[principal.name] = created.ID
	}
	if err := iam.SetPrincipalStatus(ids["Cy"], "disabled"); err != nil {
		t.Fatal(err)
	}
	project, err := iam.CreateProject("listing", "Listing")
	if err != nil {
		t.Fatal(err)
	}
	if err := iam.SetMembership(project.ID, ids["Ada"], "owner"); err != nil {
		t.Fatal(err)
	}
	if err := iam.SetMembership(project.ID, ids["Batch"], "member"); err != nil {
		t.Fatal(err)
	}
	return NewServer(Runtime{}), ids, project.ID
}

// The principal listing answers a page of principals, by name unless asked
// otherwise, with how many the filter selects, chosen by ID, kind, status,
// project and search; a filter it cannot apply is refused, and so is a
// request without administrator credentials.
func TestPrincipalListingPagesPrincipals(t *testing.T) {
	handler, ids, project := principalListingFixture(t)
	read := func(query string) ([]string, int) {
		t.Helper()
		w := adminGet(handler, "/admin/api/principals?"+query)
		var page struct {
			Principals []struct {
				DisplayName string `json:"display_name"`
			} `json:"principals"`
			Total int `json:"total"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &page) != nil {
			t.Fatalf("%s: status=%d body=%s", query, w.Code, w.Body.String())
		}
		names := []string{}
		for _, principal := range page.Principals {
			names = append(names, principal.DisplayName)
		}
		return names, page.Total
	}
	for query, want := range map[string]struct {
		names []string
		total int
	}{
		"kind=human":                                    {[]string{"Ada", "bob", "Cy"}, 3},
		"kind=human&status=active&limit=1":              {[]string{"Ada"}, 2},
		"kind=human&sort=name&order=desc&offset=1":      {[]string{"bob", "Ada"}, 3},
		"kind=human&kind=service&project_id=" + project: {[]string{"Ada", "Batch"}, 2},
		"id=" + ids["bob"] + "&id=" + ids["Cy"]:         {[]string{"bob", "Cy"}, 2},
		"q=" + url.QueryEscape("BAT"):                   {[]string{"Batch"}, 1},
		"kind=human&status=disabled":                    {[]string{"Cy"}, 1},
	} {
		names, total := read(query)
		if !reflect.DeepEqual(names, want.names) || total != want.total {
			t.Fatalf("%s: %v of %d, want %v of %d", query, names, total, want.names, want.total)
		}
	}
	for _, query := range []string{"status=lost", "kind=robot", "sort=secret", "order=up", "limit=many", "offset=-1"} {
		if w := adminGet(handler, "/admin/api/principals?"+query); w.Code != http.StatusBadRequest {
			t.Fatalf("%s: status=%d", query, w.Code)
		}
	}
	anonymous := httptest.NewRecorder()
	handler.ServeHTTP(anonymous, httptest.NewRequest(http.MethodGet, "/admin/api/principals", nil))
	if anonymous.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: status=%d", anonymous.Code)
	}
}

// The administrator state counts the keys and principals the console shows
// in place of their lists, which it no longer carries, and names the
// default owner, the first active person by name, whose catalog the console
// shows until another is chosen.
func TestAdminStateCountsIdentitiesAndNamesTheDefaultOwner(t *testing.T) {
	handler, ids, project := principalListingFixture(t)
	for _, name := range []string{"kept", "revoked"} {
		key, err := iam.IssueKey(iam.KeyCreate{ProjectID: project, PrincipalID: ids["Batch"], Name: name})
		if err != nil {
			t.Fatal(err)
		}
		if name == "revoked" {
			if err := iam.RevokeAPIKey(key.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	w := adminGet(handler, "/admin/api/state")
	var state struct {
		Counts       iam.IdentityCounts `json:"counts"`
		DefaultOwner struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		} `json:"default_owner"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &state) != nil {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if state.Counts.Keys != 2 || state.Counts.ActiveHumans != 2 || state.Counts.ProjectOwners != 1 ||
		state.Counts.ActivePrincipals < 3 || state.Counts.Principals < 4 {
		t.Fatalf("counts=%+v", state.Counts)
	}
	if state.DefaultOwner.ID != ids["Ada"] || state.DefaultOwner.DisplayName != "Ada" {
		t.Fatalf("default owner=%+v", state.DefaultOwner)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	for _, list := range []string{"keys", "principals"} {
		if _, listed := fields[list]; listed {
			t.Fatalf("the state lists %s", list)
		}
	}
}
