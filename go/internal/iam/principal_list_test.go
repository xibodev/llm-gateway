package iam

import (
	"errors"
	"reflect"
	"testing"
)

// A page of principals is chosen by ID, kind, status, project and search, in
// the order asked for, and counts every principal its filter selects. Names
// sort without letter case, and principals that tie stay in name order.
func TestListPrincipalsPageFiltersSortsAndPages(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	ResetForTests()
	t.Cleanup(ResetForTests)
	create := func(kind, subject, email, name string) Principal {
		t.Helper()
		principal, err := CreatePrincipal(kind, subject, email, name)
		if err != nil {
			t.Fatal(err)
		}
		return principal
	}
	ada := create("human", "fixture:ada", "ada@example.test", "Ada")
	bob := create("human", "fixture:bob", "", "bob")
	cy := create("human", "fixture:cy", "cy@example.test", "Cy")
	batch := create("service", "", "", "Batch_Runner")
	if err := SetPrincipalStatus(cy.ID, "disabled"); err != nil {
		t.Fatal(err)
	}
	research, err := CreateProject("research", "Research")
	if err != nil {
		t.Fatal(err)
	}
	for _, principal := range []Principal{ada, batch} {
		if err := SetMembership(research.ID, principal.ID, "member"); err != nil {
			t.Fatal(err)
		}
	}
	names := func(filter PrincipalListFilter) ([]string, int) {
		t.Helper()
		principals, total, err := ListPrincipalsPage(filter)
		if err != nil {
			t.Fatalf("%+v: %v", filter, err)
		}
		out := []string{}
		for _, principal := range principals {
			if principal.Kind != "system" {
				out = append(out, principal.DisplayName)
			}
		}
		return out, total
	}
	for name, check := range map[string]struct {
		filter PrincipalListFilter
		want   []string
		total  int
	}{
		"people, by name":                {PrincipalListFilter{Kinds: []string{"human"}}, []string{"Ada", "bob", "Cy"}, 3},
		"people, by name, descending":    {PrincipalListFilter{Kinds: []string{"human"}, Descending: true}, []string{"Cy", "bob", "Ada"}, 3},
		"active people":                  {PrincipalListFilter{Kinds: []string{"human"}, Status: "active"}, []string{"Ada", "bob"}, 2},
		"disabled":                       {PrincipalListFilter{Status: "disabled"}, []string{"Cy"}, 1},
		"people and services":            {PrincipalListFilter{Kinds: []string{"human", "service"}, Status: "active"}, []string{"Ada", "Batch_Runner", "bob"}, 3},
		"a project's members":            {PrincipalListFilter{ProjectID: research.ID}, []string{"Ada", "Batch_Runner"}, 2},
		"by ID":                          {PrincipalListFilter{IDs: []string{bob.ID, " ", cy.ID}}, []string{"bob", "Cy"}, 2},
		"a search of names":              {PrincipalListFilter{Search: "BATCH_"}, []string{"Batch_Runner"}, 1},
		"a search of emails":             {PrincipalListFilter{Search: "example.test"}, []string{"Ada", "Cy"}, 2},
		"a search of subjects":           {PrincipalListFilter{Search: "fixture:b"}, []string{"bob"}, 1},
		"a search's wildcard is literal": {PrincipalListFilter{Search: "%"}, []string{}, 0},
		"by email":                       {PrincipalListFilter{Kinds: []string{"human"}, Sort: "email"}, []string{"bob", "Ada", "Cy"}, 3},
	} {
		got, total := names(check.filter)
		if !reflect.DeepEqual(got, check.want) || total != check.total {
			t.Fatalf("%s: got %v of %d, want %v of %d", name, got, total, check.want, check.total)
		}
	}
	page, total := names(PrincipalListFilter{Kinds: []string{"human", "service"}, Limit: 2, Offset: 2})
	if !reflect.DeepEqual(page, []string{"bob", "Cy"}) || total != 4 {
		t.Fatalf("second page %v of %d", page, total)
	}
	var invalid *InvalidFilterError
	for _, filter := range []PrincipalListFilter{{Sort: "secret"}, {Status: "lost"}, {Kinds: []string{"robot"}}} {
		if _, _, err := ListPrincipalsPage(filter); !errors.As(err, &invalid) {
			t.Fatalf("%+v: %v", filter, err)
		}
	}
}

// The identity counts stand in for the lists the console no longer loads:
// every key that is not deleted, every principal, the active people and
// service principals a project can take, the active people, and those of
// them who own or administer an active project.
func TestCountIdentities(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	ResetForTests()
	t.Cleanup(ResetForTests)
	before, err := CountIdentities()
	if err != nil {
		t.Fatal(err)
	}
	ada, err := CreatePrincipal("human", "fixture:ada", "", "Ada")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := CreatePrincipal("human", "fixture:bob", "", "Bob")
	if err != nil {
		t.Fatal(err)
	}
	batch, err := CreatePrincipal("service", "", "", "Batch")
	if err != nil {
		t.Fatal(err)
	}
	if err := SetPrincipalStatus(bob.ID, "disabled"); err != nil {
		t.Fatal(err)
	}
	research, err := CreateProject("research", "Research")
	if err != nil {
		t.Fatal(err)
	}
	if err := SetMembership(research.ID, ada.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if err := SetMembership(research.ID, bob.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if err := SetMembership(research.ID, batch.ID, "member"); err != nil {
		t.Fatal(err)
	}
	if _, err := IssueKey(KeyCreate{ProjectID: research.ID, PrincipalID: batch.ID, Name: "kept"}); err != nil {
		t.Fatal(err)
	}
	revoked, err := IssueKey(KeyCreate{ProjectID: research.ID, PrincipalID: batch.ID, Name: "revoked"})
	if err != nil {
		t.Fatal(err)
	}
	if err := RevokeAPIKey(revoked.ID); err != nil {
		t.Fatal(err)
	}
	got, err := CountIdentities()
	if err != nil {
		t.Fatal(err)
	}
	want := IdentityCounts{
		Keys: before.Keys + 2, Principals: before.Principals + 3, ActivePrincipals: before.ActivePrincipals + 2,
		ActiveHumans: before.ActiveHumans + 1, ProjectOwners: before.ProjectOwners + 1,
	}
	if got != want {
		t.Fatalf("counts = %+v, want %+v", got, want)
	}
}

// A listed membership names its principal, so the console can show and pick
// a project's members without the list of every principal.
func TestMembershipsNameTheirPrincipals(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	ResetForTests()
	t.Cleanup(ResetForTests)
	ada, err := CreatePrincipal("human", "fixture:ada", "", "Ada")
	if err != nil {
		t.Fatal(err)
	}
	if err := SetPrincipalStatus(ada.ID, "disabled"); err != nil {
		t.Fatal(err)
	}
	research, err := CreateProject("research", "Research")
	if err != nil {
		t.Fatal(err)
	}
	if err := SetMembership(research.ID, ada.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	want := Membership{ProjectID: research.ID, PrincipalID: ada.ID, Role: "admin", PrincipalName: "Ada", PrincipalKind: "human", PrincipalStatus: "disabled"}
	for name, list := range map[string]func() ([]Membership, error){
		"all":         ListAllMemberships,
		"a project's": func() ([]Membership, error) { return ListMemberships(research.ID) },
		"a person's":  func() ([]Membership, error) { return ListPrincipalMemberships(ada.ID) },
	} {
		memberships, err := list()
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, membership := range memberships {
			if membership.PrincipalID == ada.ID {
				membership.CreatedAt = 0
				found = membership == want
			}
		}
		if !found {
			t.Fatalf("%s: %+v, want %+v", name, memberships, want)
		}
	}
}
