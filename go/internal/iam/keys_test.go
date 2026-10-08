package iam

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"llmgw/internal/config"
)

func TestIssueResolveAndDisableHashedKey(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LLMGW_STATE_DIR", dir)
	ResetForTests()
	t.Cleanup(ResetForTests)

	principal, err := CreatePrincipal("human", "authentik:user-1", "user@example.com", "User One")
	if err != nil {
		t.Fatal(err)
	}
	project, err := CreateProject("Example Project", "Example Project")
	if err != nil {
		t.Fatal(err)
	}
	if err := SetMembership(project.ID, principal.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	issued, err := IssueKey(KeyCreate{
		ProjectID: project.ID, PrincipalID: principal.ID, Name: "laptop",
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
		Policy: KeyPolicy{
			AllowedModels: []string{"claude-smart"}, RPM: 9, DailyRequests: 100,
			MonthlyTotalTokens: 1_000_000,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(issued.Token, "llmgw_") || issued.Token == issued.Prefix {
		t.Fatalf("issued token/prefix invalid: token=%q prefix=%q", issued.Token, issued.Prefix)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), issued.Token) {
		t.Fatal("raw API token was stored in gateway.db")
	}
	resolved, ok, err := ResolveAPIKey(issued.Token)
	if err != nil || !ok {
		t.Fatalf("resolve: ok=%v err=%v", ok, err)
	}
	if resolved.PrincipalID != principal.ID || resolved.ProjectID != project.ID ||
		resolved.Role != "owner" || resolved.Project != "example-project" {
		t.Fatalf("resolved = %+v", resolved)
	}
	if resolved.RPM != 9 || resolved.DailyRequests != 100 {
		t.Fatalf("resolved policy = %+v", resolved)
	}
	keys, err := ListAPIKeys(project.ID)
	if err != nil || len(keys) != 1 {
		t.Fatalf("keys=%+v err=%v", keys, err)
	}
	if keys[0].Prefix != issued.Prefix {
		t.Fatalf("listed prefix=%q want %q", keys[0].Prefix, issued.Prefix)
	}

	disabled := "disabled"
	if err := UpdateAPIKey(issued.ID, KeyUpdate{Status: &disabled}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := ResolveAPIKey(issued.Token); err != nil || ok {
		t.Fatalf("disabled key resolved: ok=%v err=%v", ok, err)
	}
}

// An expired key stays expired: a later expiry, or none, would make it
// authenticate again, so a new one is refused, and a new key replaces it.
// An active key's expiry still changes, and an expired key's other settings
// still save with its expiry as it is.
func TestAnExpiredKeyKeepsItsExpiry(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	ResetForTests()
	t.Cleanup(ResetForTests)
	principal, err := CreatePrincipal("human", "fixture:expiry", "", "Expiry Owner")
	if err != nil {
		t.Fatal(err)
	}
	project, err := CreateProject("expiry-project", "Expiry Project")
	if err != nil {
		t.Fatal(err)
	}
	if err := SetMembership(project.ID, principal.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	issue := func(expiresAt int64) IssuedKey {
		t.Helper()
		key, err := IssueKey(KeyCreate{ProjectID: project.ID, PrincipalID: principal.ID, ExpiresAt: expiresAt})
		if err != nil {
			t.Fatal(err)
		}
		return key
	}
	expired := issue(time.Now().Add(-time.Minute).Unix())
	active := issue(time.Now().Add(time.Hour).Unix())

	for _, expiresAt := range []int64{time.Now().Add(time.Hour).Unix(), 0} {
		for _, update := range []KeyUpdate{
			{ExpiresAt: &expiresAt, Admin: true},
			{ExpiresAt: &expiresAt, OwnerPrincipalID: principal.ID},
		} {
			if err := UpdateAPIKey(expired.ID, update); !errors.Is(err, ErrAPIKeyExpired) {
				t.Fatalf("a new expiry %d for an expired key: err=%v, want it refused", expiresAt, err)
			}
		}
	}
	if _, ok, err := ResolveAPIKey(expired.Token); err != nil || ok {
		t.Fatalf("the expired key authenticates: ok=%v err=%v", ok, err)
	}
	unchanged, disabled := expired.ExpiresAt, "disabled"
	if err := UpdateAPIKey(expired.ID, KeyUpdate{ExpiresAt: &unchanged, Status: &disabled, Admin: true}); err != nil {
		t.Fatalf("an expired key's other settings with its expiry as it is: %v", err)
	}

	later := time.Now().Add(2 * time.Hour).Unix()
	if err := UpdateAPIKey(active.ID, KeyUpdate{ExpiresAt: &later, OwnerPrincipalID: principal.ID}); err != nil {
		t.Fatalf("an active key's new expiry: %v", err)
	}
	keys, err := ListAPIKeys(project.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range keys {
		if key.ID == active.ID && key.ExpiresAt != later {
			t.Fatalf("active key expires at %d, want %d", key.ExpiresAt, later)
		}
	}
	if _, ok, err := ResolveAPIKey(active.Token); err != nil || !ok {
		t.Fatalf("the active key does not authenticate: ok=%v err=%v", ok, err)
	}
}

func TestIssueAndRevealEncryptedAPIKey(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	ResetForTests()
	oldKey := config.Get().CredentialEncryptionKey
	config.Update(func(s *config.Settings) {
		s.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	})
	t.Cleanup(func() {
		ResetForTests()
		config.Update(func(s *config.Settings) { s.CredentialEncryptionKey = oldKey })
	})

	principal, err := CreatePrincipal("human", "authentik:reveal", "", "Reveal Owner")
	if err != nil {
		t.Fatal(err)
	}
	project, err := CreateProject("reveal-project", "Reveal Project")
	if err != nil {
		t.Fatal(err)
	}
	if err := SetMembership(project.ID, principal.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	issued, err := IssueKey(KeyCreate{ProjectID: project.ID, PrincipalID: principal.ID, Name: "laptop"})
	if err != nil {
		t.Fatal(err)
	}
	if !issued.Revealable {
		t.Fatal("new key is not marked revealable")
	}
	revealed, found, err := RevealAPIKey(issued.ID)
	if err != nil || !found || revealed != issued.Token {
		t.Fatalf("reveal: found=%v matches=%v err=%v", found, revealed == issued.Token, err)
	}
	raw, err := os.ReadFile(filepath.Join(config.StateDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), issued.Token) {
		t.Fatal("raw API token was stored in plaintext")
	}

	config.Update(func(s *config.Settings) {
		s.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	})
	if _, found, err := RevealAPIKey(issued.ID); err == nil || !found {
		t.Fatalf("wrong encryption key: found=%v err=%v", found, err)
	}
}

func TestHashOnlyAPIKeyIsNotRevealable(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	ResetForTests()
	oldKey := config.Get().CredentialEncryptionKey
	config.Update(func(s *config.Settings) { s.CredentialEncryptionKey = "" })
	t.Cleanup(func() {
		ResetForTests()
		config.Update(func(s *config.Settings) { s.CredentialEncryptionKey = oldKey })
	})

	principal, _ := CreatePrincipal("service", "service:legacy-reveal", "", "Legacy")
	project, _ := CreateProject("legacy-reveal", "Legacy Reveal")
	_ = SetMembership(project.ID, principal.ID, "member")
	issued, err := IssueKey(KeyCreate{ProjectID: project.ID, PrincipalID: principal.ID, Name: "legacy"})
	if err != nil {
		t.Fatal(err)
	}
	if issued.Revealable {
		t.Fatal("hash-only key is marked revealable")
	}
	if _, found, err := RevealAPIKey(issued.ID); !found || !errors.Is(err, ErrAPIKeyNotRevealable) {
		t.Fatalf("hash-only reveal: found=%v err=%v", found, err)
	}
}

func TestIssueKeyRequiresProjectMembership(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	ResetForTests()
	t.Cleanup(ResetForTests)

	principal, _ := CreatePrincipal("service", "service:worker", "", "Worker")
	project, _ := CreateProject("project", "Project")
	if _, err := IssueKey(KeyCreate{
		ProjectID: project.ID, PrincipalID: principal.ID, Name: "worker",
	}); err == nil {
		t.Fatal("key issuance without membership should fail")
	}
}

func TestRevokedKeyCannotChangeStatusOrBecomeValidAgain(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	ResetForTests()
	t.Cleanup(ResetForTests)

	principal, err := CreatePrincipal("human", "authentik:revoked-key", "", "Revoked Key")
	if err != nil {
		t.Fatal(err)
	}
	project, err := CreateProject("revoked-key-project", "Revoked Key Project")
	if err != nil {
		t.Fatal(err)
	}
	if err := SetMembership(project.ID, principal.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	issued, err := IssueKey(KeyCreate{ProjectID: project.ID, PrincipalID: principal.ID, Name: "laptop"})
	if err != nil {
		t.Fatal(err)
	}
	activePolicy := KeyPolicy{RPM: 7}
	if err := UpdateAPIKey(issued.ID, KeyUpdate{Policy: &activePolicy}); err != nil {
		t.Fatal(err)
	}
	disabled, active := "disabled", "active"
	if err := UpdateAPIKey(issued.ID, KeyUpdate{Status: &disabled}); err != nil {
		t.Fatal(err)
	}
	disabledPolicy := KeyPolicy{DailyRequests: 7}
	if err := UpdateAPIKey(issued.ID, KeyUpdate{Policy: &disabledPolicy}); err != nil {
		t.Fatal(err)
	}
	if err := UpdateAPIKey(issued.ID, KeyUpdate{Status: &active}); err != nil {
		t.Fatalf("disabled key should be reversible: %v", err)
	}
	if err := RevokeAPIKey(issued.ID); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{active, disabled} {
		if err := UpdateAPIKey(issued.ID, KeyUpdate{Status: &status}); err == nil {
			t.Fatalf("revoked key unexpectedly changed to %q", status)
		}
	}
	key, found, err := APIKeyByID(issued.ID)
	if err != nil || !found || key.Status != "revoked" || key.Policy.DailyRequests != 7 {
		t.Fatalf("key=%+v found=%v err=%v", key, found, err)
	}
	if _, ok, err := ResolveAPIKey(issued.Token); err != nil || ok {
		t.Fatalf("revoked key resolved: ok=%v err=%v", ok, err)
	}
}

func TestStaleAPIKeyUpdateCannotOverwriteRevocation(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	ResetForTests()
	t.Cleanup(ResetForTests)

	principal, err := CreatePrincipal("human", "authentik:stale-key", "", "Stale Key")
	if err != nil {
		t.Fatal(err)
	}
	project, err := CreateProject("stale-key-project", "Stale Key Project")
	if err != nil {
		t.Fatal(err)
	}
	if err := SetMembership(project.ID, principal.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	issued, err := IssueKey(KeyCreate{ProjectID: project.ID, PrincipalID: principal.ID, Name: "laptop"})
	if err != nil {
		t.Fatal(err)
	}

	stale, found, err := APIKeyByID(issued.ID)
	if err != nil || !found {
		t.Fatalf("stale key=%+v found=%v err=%v", stale, found, err)
	}
	stale.Policy = KeyPolicy{RPM: 25}
	if err := RevokeAPIKey(issued.ID); err != nil {
		t.Fatal(err)
	}
	if err := saveAPIKey(issued.ID, stale); err == nil {
		t.Fatal("stale update unexpectedly overwrote a revocation")
	}

	key, found, err := APIKeyByID(issued.ID)
	if err != nil || !found || key.Status != "revoked" {
		t.Fatalf("key=%+v found=%v err=%v", key, found, err)
	}
	if _, ok, err := ResolveAPIKey(issued.Token); err != nil || ok {
		t.Fatalf("revoked key resolved: ok=%v err=%v", ok, err)
	}
}

func TestRemovingMembershipRevokesTheMembersKeysInThatProject(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	ResetForTests()
	t.Cleanup(ResetForTests)

	member, err := CreatePrincipal("human", "authentik:removed-member", "", "Removed Member")
	if err != nil {
		t.Fatal(err)
	}
	colleague, err := CreatePrincipal("human", "authentik:remaining-member", "", "Remaining Member")
	if err != nil {
		t.Fatal(err)
	}
	project, err := CreateProject("membership-removal", "Membership Removal")
	if err != nil {
		t.Fatal(err)
	}
	other, err := CreateProject("membership-kept", "Membership Kept")
	if err != nil {
		t.Fatal(err)
	}
	for _, membership := range []struct{ projectID, principalID string }{
		{project.ID, member.ID}, {project.ID, colleague.ID}, {other.ID, member.ID},
	} {
		if err := SetMembership(membership.projectID, membership.principalID, "member"); err != nil {
			t.Fatal(err)
		}
	}
	issue := func(projectID, principalID string) IssuedKey {
		t.Helper()
		issued, err := IssueKey(KeyCreate{ProjectID: projectID, PrincipalID: principalID})
		if err != nil {
			t.Fatal(err)
		}
		return issued
	}
	active, disabled := issue(project.ID, member.ID), issue(project.ID, member.ID)
	disabledStatus := "disabled"
	if err := UpdateAPIKey(disabled.ID, KeyUpdate{Status: &disabledStatus}); err != nil {
		t.Fatal(err)
	}
	unrelated := []IssuedKey{issue(other.ID, member.ID), issue(project.ID, colleague.ID)}

	if err := RemoveMembership(project.ID, member.ID); err != nil {
		t.Fatal(err)
	}
	if err := SetMembership(project.ID, member.ID, "member"); err != nil {
		t.Fatal(err)
	}
	for _, issued := range []IssuedKey{active, disabled} {
		if _, ok, err := ResolveAPIKey(issued.Token); err != nil || ok {
			t.Fatalf("key of a removed membership resolved after re-adding: ok=%v err=%v", ok, err)
		}
		key, found, err := APIKeyByID(issued.ID)
		if err != nil || !found || key.Status != "revoked" {
			t.Fatalf("key=%+v found=%v err=%v, want revoked", key, found, err)
		}
	}
	for _, issued := range unrelated {
		if _, ok, err := ResolveAPIKey(issued.Token); err != nil || !ok {
			t.Fatalf("unrelated key stopped resolving: ok=%v err=%v", ok, err)
		}
	}
}

// Only a revoked or expired key is deleted. It leaves every listing and
// lookup, never authenticates and loses its encrypted copy, while its row
// stays, so its usage still names it.
func TestDeletedAPIKeyLeavesAHistoryTombstone(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	ResetForTests()
	oldKey := config.Get().CredentialEncryptionKey
	config.Update(func(s *config.Settings) {
		s.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	})
	t.Cleanup(func() {
		ResetForTests()
		config.Update(func(s *config.Settings) { s.CredentialEncryptionKey = oldKey })
	})
	project, err := CreateProject("tombstone-project", "Tombstone Project")
	if err != nil {
		t.Fatal(err)
	}
	principal := func(subject string) Principal {
		t.Helper()
		created, err := CreatePrincipal("human", subject, "", subject)
		if err != nil {
			t.Fatal(err)
		}
		if err := SetMembership(project.ID, created.ID, "member"); err != nil {
			t.Fatal(err)
		}
		return created
	}
	owner, other := principal("fixture:tombstone-owner"), principal("fixture:tombstone-other")
	now := time.Now().Unix()
	issue := func(principalID, name string, expiresAt int64, revoke bool) IssuedKey {
		t.Helper()
		key, err := IssueKey(KeyCreate{ProjectID: project.ID, PrincipalID: principalID, Name: name, ExpiresAt: expiresAt})
		if err != nil {
			t.Fatal(err)
		}
		if revoke {
			if err := RevokeAPIKey(key.ID); err != nil {
				t.Fatal(err)
			}
		}
		return key
	}
	live := issue(owner.ID, "live", 0, false)
	revoked := issue(owner.ID, "revoked", 0, true)
	expired := issue(owner.ID, "expired", now-60, false)
	others := issue(other.ID, "others", 0, true)
	if err := RecordUsageEvent(UsageEvent{
		Timestamp: now - 30, Endpoint: "openai.chat", StatusCode: 200, Provider: "echo", RoutedModel: "echo-default",
		ProjectID: project.ID, PrincipalID: owner.ID, KeyID: revoked.ID,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := DeleteAPIKey(live.ID, ""); !errors.Is(err, ErrAPIKeyLive) {
		t.Fatalf("deleting a live key: err=%v, want it refused", err)
	}
	if _, err := DeleteAPIKey(others.ID, owner.ID); !errors.Is(err, ErrAPIKeyNotFound) {
		t.Fatalf("deleting another principal's key: err=%v, want not found", err)
	}
	for _, key := range []IssuedKey{revoked, expired} {
		deleted, err := DeleteAPIKey(key.ID, owner.ID)
		if err != nil || deleted.ID != key.ID || deleted.Name != key.Name {
			t.Fatalf("delete %s: key=%+v err=%v", key.Name, deleted, err)
		}
		if _, err := DeleteAPIKey(key.ID, ""); !errors.Is(err, ErrAPIKeyNotFound) {
			t.Fatalf("delete %s twice: err=%v, want not found", key.Name, err)
		}
		if _, found, err := APIKeyByID(key.ID); err != nil || found {
			t.Fatalf("a deleted key is found: found=%v err=%v", found, err)
		}
		if _, found, err := RevealAPIKey(key.ID); err != nil || found {
			t.Fatalf("a deleted key is revealed: found=%v err=%v", found, err)
		}
		if _, ok, err := ResolveAPIKey(key.Token); err != nil || ok {
			t.Fatalf("a deleted key authenticates: ok=%v err=%v", ok, err)
		}
	}
	ids := func(keys []APIKey) []string {
		out := []string{}
		for _, key := range keys {
			out = append(out, key.ID)
		}
		slices.Sort(out)
		return out
	}
	listed, err := ListAPIKeys(project.ID)
	if want := []string{live.ID, others.ID}; err != nil || !slices.Equal(ids(listed), slices.Sorted(slices.Values(want))) {
		t.Fatalf("listed keys=%v err=%v, want the live and the other principal's", ids(listed), err)
	}
	mine, err := ListPrincipalAPIKeys(owner.ID)
	if err != nil || !slices.Equal(ids(mine), []string{live.ID}) {
		t.Fatalf("owner's keys=%v err=%v, want only the live one", ids(mine), err)
	}
	db, err := DB()
	if err != nil {
		t.Fatal(err)
	}
	var ciphertext []byte
	if err := db.QueryRow("SELECT secret_ciphertext FROM api_keys WHERE id=?", revoked.ID).Scan(&ciphertext); err != nil || len(ciphertext) != 0 {
		t.Fatalf("a deleted key kept its encrypted copy: %d bytes, err=%v", len(ciphertext), err)
	}

	stats, err := UsageStatsFor(UsageTimeSeriesFilter{From: now - 3600})
	if err != nil {
		t.Fatal(err)
	}
	if groups := stats["groups"].(map[string][]UsageGroup)["key"]; len(groups) != 1 || groups[0].KeyName != "revoked" {
		t.Fatalf("key usage=%+v, want it named after the deleted key", groups)
	}
	events, _, err := ListUsageEvents(UsageEventFilter{})
	if err != nil || len(events) != 1 || events[0].KeyName != "revoked" {
		t.Fatalf("recorded requests=%+v err=%v, want them named after the deleted key", events, err)
	}
}
