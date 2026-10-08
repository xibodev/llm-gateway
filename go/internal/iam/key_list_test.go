package iam

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

// A page of keys is chosen by status, owner, project, route grant and search,
// in the order asked for, and counts every key its filter selects.
func TestListAPIKeysPageFiltersSortsAndPages(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	ResetForTests()
	t.Cleanup(ResetForTests)
	ada, err := CreatePrincipal("human", "fixture:ada", "", "Ada")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := CreatePrincipal("human", "fixture:bob", "", "Bob")
	if err != nil {
		t.Fatal(err)
	}
	research, err := CreateProject("research", "Research")
	if err != nil {
		t.Fatal(err)
	}
	ops, err := CreateProject("ops", "Operations")
	if err != nil {
		t.Fatal(err)
	}
	for _, membership := range []struct{ project, principal string }{{research.ID, ada.ID}, {ops.ID, bob.ID}} {
		if err := SetMembership(membership.project, membership.principal, "member"); err != nil {
			t.Fatal(err)
		}
	}
	issue := func(project, principal, name string, expiresAt int64, policy KeyPolicy) string {
		t.Helper()
		key, err := IssueKey(KeyCreate{ProjectID: project, PrincipalID: principal, Name: name, ExpiresAt: expiresAt, Policy: policy})
		if err != nil {
			t.Fatal(err)
		}
		return key.ID
	}
	now := time.Now()
	issue(research.ID, ada.ID, "notebook", 0, KeyPolicy{AllowedRoutes: []string{"coding"}})
	paused := issue(research.ID, ada.ID, "paused", 0, KeyPolicy{})
	disabled := "disabled"
	if err := UpdateAPIKey(paused, KeyUpdate{Status: &disabled, Admin: true}); err != nil {
		t.Fatal(err)
	}
	issue(research.ID, ada.ID, "lapsed", now.Add(-time.Minute).Unix(), KeyPolicy{})
	retired := issue(ops.ID, bob.ID, "retired", 0, KeyPolicy{})
	if err := RevokeAPIKey(retired); err != nil {
		t.Fatal(err)
	}
	issue(ops.ID, bob.ID, "Batch_Job", 0, KeyPolicy{AllowedRoutes: []string{"coding", "fast"}})

	ids := func(filter KeyListFilter) ([]string, int) {
		t.Helper()
		keys, total, err := ListAPIKeysPage(filter)
		if err != nil {
			t.Fatalf("%+v: %v", filter, err)
		}
		out := []string{}
		for _, key := range keys {
			out = append(out, key.Name)
		}
		return out, total
	}
	for name, check := range map[string]struct {
		filter KeyListFilter
		want   []string
	}{
		"usable, newest first":           {KeyListFilter{Descending: true}, []string{"Batch_Job", "paused", "notebook"}},
		"ended":                          {KeyListFilter{Status: "ended", Sort: "name"}, []string{"lapsed", "retired"}},
		"active":                         {KeyListFilter{Status: "active", Sort: "name"}, []string{"Batch_Job", "notebook"}},
		"disabled":                       {KeyListFilter{Status: "disabled"}, []string{"paused"}},
		"expired":                        {KeyListFilter{Status: "expired"}, []string{"lapsed"}},
		"revoked":                        {KeyListFilter{Status: "revoked"}, []string{"retired"}},
		"an owner's":                     {KeyListFilter{Status: "all", PrincipalID: bob.ID, Sort: "name"}, []string{"Batch_Job", "retired"}},
		"a project's":                    {KeyListFilter{Status: "all", ProjectID: research.ID, Sort: "name", Descending: true}, []string{"paused", "notebook", "lapsed"}},
		"a route's":                      {KeyListFilter{Route: "coding", Sort: "name"}, []string{"Batch_Job", "notebook"}},
		"a search, ignoring case":        {KeyListFilter{Status: "all", Search: "batch_"}, []string{"Batch_Job"}},
		"a search of the owner":          {KeyListFilter{Status: "all", Search: "BOB", Sort: "name"}, []string{"Batch_Job", "retired"}},
		"a search's wildcard is literal": {KeyListFilter{Status: "all", Search: "%"}, []string{}},
	} {
		got, total := ids(check.filter)
		if !reflect.DeepEqual(got, check.want) || total != len(check.want) {
			t.Fatalf("%s: got %v of %d, want %v", name, got, total, check.want)
		}
	}
	page, total := ids(KeyListFilter{Status: "all", Sort: "name", Limit: 2, Offset: 2})
	if !reflect.DeepEqual(page, []string{"notebook", "paused"}) || total != 5 {
		t.Fatalf("second page %v of %d", page, total)
	}
	var invalid *InvalidFilterError
	if _, _, err := ListAPIKeysPage(KeyListFilter{Sort: "secret"}); !errors.As(err, &invalid) {
		t.Fatalf("an unknown sort: %v", err)
	}
	if _, _, err := ListAPIKeysPage(KeyListFilter{Status: "lost"}); !errors.As(err, &invalid) {
		t.Fatalf("an unknown status: %v", err)
	}
}
