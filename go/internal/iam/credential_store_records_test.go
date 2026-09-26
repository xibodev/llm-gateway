package iam

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"testing"
	"time"

	gcpauth "github.com/xibodev/llm-provider-auth/gcp"
	"github.com/xibodev/llm-provider-auth/tokenstore"
	core "github.com/xibodev/llmgw-core"
)

func fullOAuthConnection(t *testing.T, principalID string) ProviderConnection {
	t.Helper()
	connection, err := PutOAuthProviderConnection(OAuthConnectionCreate{
		PrincipalID: principalID, ProviderID: "fixture-codex", Name: "work",
		Kind: "openai_codex_oauth", Source: ConnectionSourceUser, MakeDefault: true,
		AccessToken: "fixture-access-a", RefreshToken: "fixture-refresh-a", IDToken: "fixture-id-a",
		TokenType: "Bearer", ExpiresAt: 1_900_000_000, AccountID: "fixture-account",
		AccountLabel: "fixture-label", ProjectID: "fixture-project", OAuthProfile: "browser_pkce",
		OAuthClientID: "fixture-client", OAuthClientMode: "public",
		OAuthRedirectURI: "http://127.0.0.1/callback", OAuthClientSecret: "fixture-client-secret",
		Status: "active",
	})
	if err != nil {
		t.Fatal(err)
	}
	return connection
}

func TestCredentialStoreRoundTripsTheGatewayEnvelope(t *testing.T) {
	path := credentialStatePath(t)
	ctx := context.Background()
	human, _ := CreatePrincipal("human", "fixture:envelope", "", "Envelope")
	connection := fullOAuthConnection(t, human.ID)
	store := openCredentialStore(t, path, CredentialStoreOptions{})

	record, err := store.Load(ctx, connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantMetadata := map[string]string{
		CredentialMetadataAccountLabel: "fixture-label", CredentialMetadataProjectID: "fixture-project",
		CredentialMetadataOAuthProfile: "browser_pkce", CredentialMetadataOAuthClientID: "fixture-client",
		CredentialMetadataOAuthClientMode: "public", CredentialMetadataOAuthRedirectURI: "http://127.0.0.1/callback",
		CredentialMetadataOAuthClientSecret: "fixture-client-secret", CredentialMetadataOAuthStatus: "active",
	}
	if record.AccessToken != "fixture-access-a" || record.RefreshToken != "fixture-refresh-a" ||
		record.IDToken != "fixture-id-a" || record.TokenType != "Bearer" ||
		!record.Expiry.Equal(time.Unix(1_900_000_000, 0)) || record.AccountID != "fixture-account" ||
		len(record.Metadata) != len(wantMetadata) {
		t.Fatalf("Load mapped the envelope to %s metadata=%d", record, len(record.Metadata))
	}
	for key, value := range wantMetadata {
		if record.Metadata[key] != value {
			t.Fatalf("metadata %s=%q, want %q", key, record.Metadata[key], value)
		}
	}
	_, _, observation, found, err := ProviderConnectionSecretWithObservation(human.ID, "fixture-codex", "")
	if err != nil || !found || observation.ConnectionID != connection.ID ||
		record.Revision != formatRevision(observation.CredentialRevision) {
		t.Fatalf("key/revision %s/%s differ from the gateway observation %+v", connection.ID, record.Revision, observation)
	}

	refreshed := record.Clone()
	refreshed.AccessToken = "fixture-access-b"
	refreshed.Metadata["fixture-extra"] = "kept"
	stored, err := store.ReplaceIfCurrent(ctx, connection.ID, record.Revision, refreshed)
	if err != nil {
		t.Fatal(err)
	}
	envelope, _, found, err := OAuthProviderConnectionSecret(human.ID, "fixture-codex", "work")
	if err != nil || !found {
		t.Fatalf("gateway read after replace: found=%v err=%v", found, err)
	}
	if envelope.AccessToken != "fixture-access-b" || envelope.RefreshToken != "fixture-refresh-a" ||
		envelope.IDToken != "fixture-id-a" || envelope.ExpiresAt != 1_900_000_000 ||
		envelope.AccountID != "fixture-account" || envelope.AccountLabel != "fixture-label" ||
		envelope.ProjectID != "fixture-project" || envelope.OAuthProfile != "browser_pkce" ||
		envelope.OAuthClientID != "fixture-client" || envelope.OAuthClientMode != "public" ||
		envelope.OAuthRedirectURI != "http://127.0.0.1/callback" ||
		envelope.OAuthClientSecret != "fixture-client-secret" || envelope.Status != "active" ||
		envelope.Metadata["fixture-extra"] != "kept" {
		t.Fatal("ReplaceIfCurrent lost an envelope field")
	}
	_, _, observation, _, _ = ProviderConnectionSecretWithObservation(human.ID, "fixture-codex", "")
	if stored.Revision != formatRevision(observation.CredentialRevision) || stored.Revision == record.Revision {
		t.Fatalf("replace revision=%s, gateway revision=%d", stored.Revision, observation.CredentialRevision)
	}
}

func TestCredentialStoreMarksReadsAsUseOnlyWhenAsked(t *testing.T) {
	path := credentialStatePath(t)
	ctx := context.Background()
	human, _ := CreatePrincipal("human", "fixture:mark-used", "", "Mark used")
	connection := fullOAuthConnection(t, human.ID)
	lastUsed := func() int64 {
		t.Helper()
		connections, err := ListProviderConnections(human.ID, "fixture-codex")
		if err != nil || len(connections) != 1 {
			t.Fatalf("connections=%+v err=%v", connections, err)
		}
		return connections[0].LastUsedAt
	}
	clock := newFakeClock()
	if _, err := openCredentialStore(t, path, CredentialStoreOptions{Now: clock.Now}).Load(ctx, connection.ID); err != nil {
		t.Fatal(err)
	}
	if got := lastUsed(); got != 0 {
		t.Fatalf("a plain store marked the read as use: last_used_at=%d", got)
	}
	marking := openCredentialStore(t, path, CredentialStoreOptions{Now: clock.Now, MarkUsed: true})
	if _, err := marking.Load(ctx, connection.ID); err != nil {
		t.Fatal(err)
	}
	if got := lastUsed(); got != clock.Now().Unix() {
		t.Fatalf("last_used_at=%d, want %d", got, clock.Now().Unix())
	}
}

func TestCredentialStoreMapsNonOAuthKinds(t *testing.T) {
	path := credentialStatePath(t)
	ctx := context.Background()
	store := openCredentialStore(t, path, CredentialStoreOptions{})
	if _, err := PutSystemProviderConnection("fixture-openai", "api_key", "fixture-api-key"); err != nil {
		t.Fatal(err)
	}
	system, _, _ := PrincipalBySubject(systemPrincipalSubject)
	apiKey, _, _ := ActiveProviderConnection(system.ID, "fixture-openai")
	record, err := store.Load(ctx, apiKey.ID)
	if err != nil {
		t.Fatal(err)
	}
	credential := core.CredentialFromRecord(apiKey.ID, record)
	if record.TokenType != core.TokenTypeAPIKey || credential.APIKey != "fixture-api-key" ||
		credential.ConnectionID != apiKey.ID {
		t.Fatalf("API key record=%s credential=%+v", record, credential)
	}
	oauth := tokenstore.Record{AccessToken: "fixture-access", TokenType: "Bearer", RefreshToken: "fixture-refresh"}
	if _, err := store.ReplaceIfCurrent(ctx, apiKey.ID, record.Revision, oauth); err == nil ||
		errors.Is(err, tokenstore.ErrConflict) {
		t.Fatalf("an OAuth record replaced an API key: err=%v", err)
	}
	rotated := core.APIKeyRecord("fixture-api-key-2")
	if _, err := store.ReplaceIfCurrent(ctx, apiKey.ID, record.Revision, rotated); err != nil {
		t.Fatal(err)
	}
	if _, err := PutSystemProviderConnection("fixture-anthropic", "setup_token", "fixture-setup-token"); err != nil {
		t.Fatal(err)
	}
	setup, _, _ := ActiveProviderConnection(system.ID, "fixture-anthropic")
	record, err = store.Load(ctx, setup.ID)
	if err != nil || record.TokenType != core.TokenTypeAnthropicSetupToken ||
		record.AccessToken != "fixture-setup-token" {
		t.Fatalf("setup token record=%s err=%v", record, err)
	}
	// Core's Anthropic reads a setup token from Token, under its own type.
	if credential := core.CredentialFromRecord(setup.ID, record); credential.Token != "fixture-setup-token" ||
		credential.APIKey != "" || credential.TokenType != core.TokenTypeAnthropicSetupToken {
		t.Fatalf("setup token credential=%+v", credential)
	}
	// The inverse holds too: a setup-token record replaces the connection's
	// secret, and an API-key record does not.
	if _, err := store.ReplaceIfCurrent(ctx, setup.ID, record.Revision, core.APIKeyRecord("fixture-api-key-3")); err == nil ||
		errors.Is(err, tokenstore.ErrConflict) {
		t.Fatalf("an API key replaced a setup token: err=%v", err)
	}
	replaced := tokenstore.Record{AccessToken: "fixture-setup-token-2", TokenType: core.TokenTypeAnthropicSetupToken}
	if _, err := store.ReplaceIfCurrent(ctx, setup.ID, record.Revision, replaced); err != nil {
		t.Fatal(err)
	}
}

// serviceAccountKeyFixture is a service-account key for a throwaway RSA key,
// so the suite needs no credential.
func serviceAccountKeyFixture(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]string{
		"type": "service_account", "project_id": "fixture-project", "private_key_id": "fixture-key-id",
		"private_key":  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"client_email": "svc@fixture-project.iam.example.test", "token_uri": "https://token.example.test/token",
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// A service-account connection loads as core's service-account kind, with
// the key as the token core's Google exchanges, whatever case its kind was
// stored in; writing the record back keeps the connection's kind.
func TestCredentialStoreLoadsServiceAccountsAsCoreKind(t *testing.T) {
	path := credentialStatePath(t)
	ctx := context.Background()
	store := openCredentialStore(t, path, CredentialStoreOptions{})
	secret := serviceAccountKeyFixture(t)
	for providerID, kind := range map[string]string{"fixture-vertex": gcpauth.CredentialKind, "fixture-vertex-cased": "GCP_Service_Account"} {
		if _, err := PutSystemProviderConnection(providerID, kind, secret); err != nil {
			t.Fatal(err)
		}
		system, _, _ := PrincipalBySubject(systemPrincipalSubject)
		connection, _, _ := ActiveProviderConnection(system.ID, providerID)
		record, err := store.Load(ctx, connection.ID)
		credential := core.CredentialFromRecord(connection.ID, record)
		if err != nil || record.TokenType != core.TokenTypeGCPServiceAccount ||
			credential.Token != secret || credential.APIKey != "" {
			t.Fatalf("kind %q: record type=%q err=%v, want the key under %q", kind, record.TokenType, err, core.TokenTypeGCPServiceAccount)
		}
		if _, err := store.ReplaceIfCurrent(ctx, connection.ID, record.Revision, record); err != nil {
			t.Fatalf("kind %q: the loaded record was refused on write: %v", kind, err)
		}
	}
}
