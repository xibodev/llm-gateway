package iam

import (
	"errors"
	"strings"
	"testing"
)

// A sign-in keeps a principal as its identity provider describes it: values
// it reports replace the stored ones, values it leaves out change nothing,
// and an administrator's name sticks, even against a sign-in that read the
// principal before the rename.
func TestSignInRefreshesANameUntilAnAdministratorNamesThePrincipal(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	ResetForTests()
	t.Cleanup(ResetForTests)
	provisioned, err := CreatePrincipal("human", "authentik:fixture-subject", "", "fixture-subject")
	if err != nil {
		t.Fatal(err)
	}
	if provisioned.NameSetByAdmin {
		t.Fatal("a provisioned principal's name is marked as an administrator's")
	}
	unchanged, err := RefreshPrincipalIdentity(provisioned, " ", "")
	if err != nil || unchanged != provisioned {
		t.Fatalf("a sign-in reporting nothing changed the principal: %+v, %v", unchanged, err)
	}
	named, err := RefreshPrincipalIdentity(provisioned, "ada@example.test", " Ada Lovelace ")
	if err != nil || named.DisplayName != "Ada Lovelace" || named.Email != "ada@example.test" {
		t.Fatalf("refreshed principal = %+v, %v", named, err)
	}
	if stored, _, _ := PrincipalByID(provisioned.ID); stored.DisplayName != "Ada Lovelace" || stored.Email != "ada@example.test" {
		t.Fatalf("stored principal = %+v", stored)
	}
	if kept, err := RefreshPrincipalIdentity(named, "", ""); err != nil || kept.DisplayName != "Ada Lovelace" {
		t.Fatalf("a sign-in reporting nothing renamed the principal: %+v, %v", kept, err)
	}

	before, after, err := RenamePrincipal(provisioned.ID, "  Ada L.  ")
	if err != nil || before.DisplayName != "Ada Lovelace" || after.DisplayName != "Ada L." || !after.NameSetByAdmin {
		t.Fatalf("rename: before=%+v after=%+v err=%v", before, after, err)
	}
	refreshed, err := RefreshPrincipalIdentity(after, "ada@new.example.test", "Someone Else")
	if err != nil || refreshed.DisplayName != "Ada L." || refreshed.Email != "ada@new.example.test" {
		t.Fatalf("a sign-in after the rename: %+v, %v", refreshed, err)
	}
	// named was read before the rename, as a concurrent sign-in would be.
	if raced, err := RefreshPrincipalIdentity(named, "", "Another Name"); err != nil || raced.DisplayName != "Ada L." {
		t.Fatalf("a sign-in that raced the rename: %+v, %v", raced, err)
	}
}

func TestRenamingAPrincipal(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	ResetForTests()
	t.Cleanup(ResetForTests)
	created, err := CreatePrincipalNamedByAdmin("service", "", "", "Batch job")
	if err != nil {
		t.Fatal(err)
	}
	if !created.NameSetByAdmin {
		t.Fatal("an administrator's principal is not marked as named by an administrator")
	}
	if stored, _, _ := PrincipalByID(created.ID); !stored.NameSetByAdmin {
		t.Fatalf("stored principal = %+v", stored)
	}
	system, err := CreatePrincipal("system", "fixture:system", "", "Gateway system")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id, name string
		want     string
	}{
		{created.ID, "  ", "display name is required"},
		{created.ID, strings.Repeat("x", maxPrincipalNameRunes+1), "longer than"},
		{system.ID, "Renamed", "keeps its name"},
	} {
		if _, _, err := RenamePrincipal(tc.id, tc.name); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("RenamePrincipal(%s, %d characters) = %v, want an error containing %q", tc.id, len(tc.name), err, tc.want)
		}
	}
	if _, _, err := RenamePrincipal("prn-missing", "Someone"); !errors.Is(err, ErrPrincipalNotFound) {
		t.Errorf("renaming an unknown principal = %v, want ErrPrincipalNotFound", err)
	}
	if _, after, err := RenamePrincipal(created.ID, strings.Repeat("é", maxPrincipalNameRunes)); err != nil || after.DisplayName != strings.Repeat("é", maxPrincipalNameRunes) {
		t.Errorf("a name of exactly %d characters: %v", maxPrincipalNameRunes, err)
	}
}
