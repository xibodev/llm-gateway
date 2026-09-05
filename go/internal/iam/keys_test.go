package iam

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
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
