package iam

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestOAuthConnectionsEncryptTokensAndExposeOnlySafeMetadata(t *testing.T) {
	setupConnectionTest(t)
	human, err := CreatePrincipal("human", "authentik:oauth-owner", "", "OAuth Owner")
	if err != nil {
		t.Fatal(err)
	}
	expiresAt := time.Now().Add(time.Hour).Unix()
	connection, err := PutOAuthProviderConnection(OAuthConnectionCreate{
		PrincipalID: human.ID, ProviderID: "copilot", Name: "personal", Kind: "github_oauth",
		Source: ConnectionSourceUser, MakeDefault: true, AccessToken: "fake-access-token",
		RefreshToken: "fake-refresh-token", IDToken: "fake-id-token", ExpiresAt: expiresAt,
		AccountID: "account-123", AccountLabel: "Owner account", Status: "active",
	})
	if err != nil {
		t.Fatal(err)
	}
	if connection.OAuthExpiresAt != expiresAt || connection.OAuthAccountID != "account-123" || connection.OAuthStatus != "active" {
		t.Fatalf("safe OAuth metadata=%+v", connection)
	}

	listed, err := ListProviderConnections(human.ID, "copilot")
	if err != nil || len(listed) != 1 {
		t.Fatalf("list=%+v err=%v", listed, err)
	}
	serialized, _ := json.Marshal(listed[0])
	for _, secret := range []string{"fake-access-token", "fake-refresh-token", "fake-id-token"} {
		if bytes.Contains(serialized, []byte(secret)) {
			t.Fatalf("connection response leaked secret %q: %s", secret, serialized)
		}
	}
	if listed[0].OAuthAccountLabel != "Owner account" || listed[0].OAuthExpiresAt != expiresAt {
		t.Fatalf("listed metadata=%+v", listed[0])
	}

	envelope, _, ok, err := OAuthProviderConnectionSecret(human.ID, "copilot", "personal")
	if err != nil || !ok || envelope.AccessToken != "fake-access-token" || envelope.RefreshToken != "fake-refresh-token" {
		t.Fatalf("envelope=%+v ok=%v err=%v", envelope, ok, err)
	}
	db, err := DB()
	if err != nil {
		t.Fatal(err)
	}
	var ciphertext []byte
	if err := db.QueryRow("SELECT ciphertext FROM provider_connections WHERE id=?", connection.ID).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ciphertext, []byte("fake-access-token")) || bytes.Contains(ciphertext, []byte("fake-refresh-token")) {
		t.Fatalf("OAuth token persisted in plaintext ciphertext=%q", ciphertext)
	}
}

func TestOAuthConnectionRejectsNonHumanAndReadsLegacyRawToken(t *testing.T) {
	setupConnectionTest(t)
	service, err := CreatePrincipal("service", "service:oauth", "", "OAuth Service")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PutOAuthProviderConnection(OAuthConnectionCreate{
		PrincipalID: service.ID, ProviderID: "copilot", Kind: "github_oauth", AccessToken: "token",
	}); err == nil {
		t.Fatal("service principal unexpectedly received OAuth connection")
	}
	human, err := CreatePrincipal("human", "authentik:legacy-oauth", "", "Legacy OAuth")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PutProviderConnection(ProviderConnectionCreate{
		PrincipalID: human.ID, ProviderID: "copilot", Kind: "github_oauth", Secret: "legacy-token",
		Source: ConnectionSourceMigration, MakeDefault: true,
	}); err != nil {
		t.Fatal(err)
	}
	envelope, _, ok, err := OAuthProviderConnectionSecret(human.ID, "copilot", "")
	if err != nil || !ok || envelope.AccessToken != "legacy-token" {
		t.Fatalf("legacy envelope=%+v ok=%v err=%v", envelope, ok, err)
	}
}

func TestOAuthCompareAndSwapDoesNotOverwriteOrRevokeReauthorization(t *testing.T) {
	setupConnectionTest(t)
	human, err := CreatePrincipal("human", "authentik:oauth-cas", "", "OAuth CAS")
	if err != nil {
		t.Fatal(err)
	}
	connection, err := PutOAuthProviderConnection(OAuthConnectionCreate{
		PrincipalID: human.ID, ProviderID: "codex", Kind: "openai_codex_oauth",
		AccessToken: "old-access", RefreshToken: "old-refresh", AccountID: "account-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	expected, _, ok, err := OAuthProviderConnectionSecret(human.ID, "codex", "")
	if err != nil || !ok {
		t.Fatalf("expected connection ok=%v err=%v", ok, err)
	}
	if _, err := PutOAuthProviderConnection(OAuthConnectionCreate{
		PrincipalID: human.ID, ProviderID: "codex", Name: connection.Name, Kind: connection.Kind,
		Source: connection.Source, MakeDefault: connection.IsDefault,
		AccessToken: "replacement-access", RefreshToken: "replacement-refresh", AccountID: "account-b",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := ReplaceOAuthProviderConnectionIfCurrent(connection, expected, OAuthConnectionCreate{
		PrincipalID: human.ID, ProviderID: "codex", Name: connection.Name, Kind: connection.Kind,
		Source: connection.Source, MakeDefault: connection.IsDefault,
		AccessToken: "stale-refreshed-access", RefreshToken: "stale-refreshed-refresh", AccountID: "account-a",
	}); !errors.Is(err, ErrOAuthProviderConnectionChanged) {
		t.Fatalf("replace err=%v want changed sentinel", err)
	}
	revoked, err := RevokeOAuthProviderConnectionIfCurrent(connection, expected)
	if err != nil || revoked {
		t.Fatalf("stale revoke revoked=%v err=%v", revoked, err)
	}
	current, _, ok, err := OAuthProviderConnectionSecret(human.ID, "codex", "")
	if err != nil || !ok || current.AccessToken != "replacement-access" || current.AccountID != "account-b" {
		t.Fatalf("current=%+v ok=%v err=%v", current, ok, err)
	}
}
