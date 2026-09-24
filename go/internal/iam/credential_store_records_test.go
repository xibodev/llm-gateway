package iam

import (
	"context"
	"errors"
	"testing"
	"time"

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
	if record, err := store.Load(ctx, setup.ID); err != nil || record.TokenType != "setup_token" ||
		record.AccessToken != "fixture-setup-token" {
		t.Fatalf("setup token record=%s err=%v", record, err)
	}
}
