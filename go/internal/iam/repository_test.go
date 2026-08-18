package iam

import (
	"sync"
	"testing"
)

func TestPrincipalProjectMembershipLifecycle(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	ResetForTests()
	t.Cleanup(ResetForTests)

	principal, err := EnsurePrincipalBySubject(
		"human", "authentik:abc", "a@example.com", "A User",
	)
	if err != nil {
		t.Fatal(err)
	}

	again, err := EnsurePrincipalBySubject(
		"human", "authentik:abc", "changed@example.com", "Changed",
	)
	if err != nil || again.ID != principal.ID {
		t.Fatalf("ensure principal: %+v err=%v", again, err)
	}
	project, err := EnsureProject("My Project", "My Project")
	if err != nil {
		t.Fatal(err)
	}
	if project.Slug != "my-project" {
		t.Fatalf("slug=%q", project.Slug)
	}
	if err := SetMembership(project.ID, principal.ID, "member"); err != nil {
		t.Fatal(err)
	}
	if err := SetMembership(project.ID, principal.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	memberships, err := ListMemberships(project.ID)
	if err != nil || len(memberships) != 1 || memberships[0].Role != "owner" {
		t.Fatalf("memberships=%+v err=%v", memberships, err)
	}
	if err := SetPrincipalStatus(principal.ID, "disabled"); err != nil {
		t.Fatal(err)
	}
	got, ok, err := PrincipalByID(principal.ID)
	if err != nil || !ok || got.Status != "disabled" {
		t.Fatalf("principal=%+v ok=%v err=%v", got, ok, err)
	}
}

func TestConcurrentEnsurePrincipalIsIdempotent(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	ResetForTests()
	t.Cleanup(ResetForTests)
	const count = 12
	ids := make(chan string, count)
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			principal, err := EnsurePrincipalBySubject(
				"human", "authentik:race", "race@example.com", "Race User",
			)
			if err != nil {
				errs <- err
				return
			}
			ids <- principal.ID
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent ensure: %v", err)
	}
	first := ""
	for id := range ids {
		if first == "" {
			first = id
		} else if id != first {
			t.Fatalf("different principal ids: %q vs %q", first, id)
		}
	}
}
