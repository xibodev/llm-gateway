package iam

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// parityWorld holds two identical databases: the gateway's own functions
// write the first through DB, and the store writes the second, a copy.
type parityWorld struct {
	gateway *sql.DB
	store   *CredentialStore
	owners  map[string]Principal
}

func parityTarget(owner Principal) string { return "conn_parity_" + owner.Kind + "_target" }

func newParityWorld(t *testing.T) *parityWorld {
	t.Helper()
	credentialStatePath(t)
	gateway := must(DB())
	w := &parityWorld{gateway: gateway, owners: map[string]Principal{
		"human":   must(CreatePrincipal("human", "fixture:parity-human", "", "Human")),
		"service": must(CreatePrincipal("service", "fixture:parity-service", "", "Service")),
		"system":  must(EnsureSystemPrincipal()),
	}}
	bystander := must(CreatePrincipal("human", "fixture:parity-bystander", "", "Bystander"))
	scopes := []string{"", bystander.ID, "fixture-other-scope"}
	for _, owner := range w.owners {
		prefix := "conn_parity_" + owner.Kind + "_"
		insertParityConnection(t, gateway, prefix+"target", owner.ID, "target", true, 5)
		insertParityConnection(t, gateway, prefix+"older", owner.ID, "older", false, 10)
		insertParityConnection(t, gateway, prefix+"newer", owner.ID, "newer", false, 20)
		scopes = append(scopes, owner.ID)
	}
	insertParityConnection(t, gateway, "conn_parity_bystander", bystander.ID, "target", true, 5)
	seedParityChecks(t, gateway, scopes...)
	copyPath := filepath.Join(t.TempDir(), "gateway.db")
	if _, err := gateway.Exec("VACUUM INTO ?", copyPath); err != nil {
		t.Fatal(err)
	}
	w.store = openCredentialStore(t, copyPath, CredentialStoreOptions{})
	if !reflect.DeepEqual(paritySnapshot(t, gateway), paritySnapshot(t, w.store.db)) {
		t.Fatal("the copied database differs from the original")
	}
	return w
}

// gatewayReplace refreshes the target the way the Codex and Antigravity
// refresh paths call ReplaceOAuthProviderConnectionIfCurrent.
func (w *parityWorld) gatewayReplace(owner Principal, stale bool) error {
	envelope, connection, found, err := OAuthProviderConnectionSecret(owner.ID, parityProvider, "target")
	if err != nil || !found {
		return errors.Join(err, errors.New("fixture connection missing"))
	}
	if stale {
		envelope.AccessToken = "fixture-access-stale"
	}
	_, err = ReplaceOAuthProviderConnectionIfCurrent(connection, envelope, OAuthConnectionCreate{
		PrincipalID: owner.ID, ProviderID: parityProvider, Name: connection.Name, Kind: connection.Kind,
		Source: connection.Source, MakeDefault: connection.IsDefault,
		AccessToken: "fixture-access-refreshed", RefreshToken: "fixture-refresh-refreshed",
		IDToken: envelope.IDToken, TokenType: envelope.TokenType, ExpiresAt: 1_950_000_000,
		AccountID: envelope.AccountID, AccountLabel: envelope.AccountLabel, Status: "active",
		ProjectID: envelope.ProjectID, OAuthProfile: envelope.OAuthProfile,
		OAuthClientID: envelope.OAuthClientID, OAuthClientMode: envelope.OAuthClientMode,
		OAuthRedirectURI: envelope.OAuthRedirectURI, OAuthClientSecret: envelope.OAuthClientSecret,
	})
	return err
}

// storeReplace writes what tokenstore.Coordinator writes for the same
// refresh: the current record with the refreshed tokens merged in.
func (w *parityWorld) storeReplace(owner Principal, stale bool) error {
	current, err := w.store.Load(context.Background(), parityTarget(owner))
	if err != nil {
		return err
	}
	next := current.Clone()
	next.AccessToken, next.RefreshToken = "fixture-access-refreshed", "fixture-refresh-refreshed"
	next.Expiry = time.Unix(1_950_000_000, 0)
	revision := current.Revision
	if stale {
		revision = "2"
	}
	_, err = w.store.ReplaceIfCurrent(context.Background(), parityTarget(owner), revision, next)
	return err
}

func (w *parityWorld) gatewayRevoke(owner Principal, stale bool) (bool, error) {
	envelope, connection, found, err := OAuthProviderConnectionSecret(owner.ID, parityProvider, "target")
	if err != nil || !found {
		return false, errors.Join(err, errors.New("fixture connection missing"))
	}
	if stale {
		envelope.AccessToken = "fixture-access-stale"
	}
	return RevokeOAuthProviderConnectionIfCurrent(connection, envelope)
}

func (w *parityWorld) storeRevoke(owner Principal, stale bool) error {
	current, err := w.store.Load(context.Background(), parityTarget(owner))
	if err != nil {
		return err
	}
	revision := current.Revision
	if stale {
		revision = "2"
	}
	return w.store.RevokeIfCurrent(context.Background(), parityTarget(owner), revision)
}

// gatewaySave logs in again to one connection the way the console's
// re-authorization does, keeping its kind, source and default flag.
func (w *parityWorld) gatewaySave(owner Principal, name string) error {
	connection, found, err := ProviderConnectionByName(owner.ID, parityProvider, name)
	if err != nil || !found {
		return errors.Join(err, errors.New("fixture connection missing"))
	}
	login := parityEnvelope("login")
	_, err = PutOAuthProviderConnection(OAuthConnectionCreate{
		PrincipalID: owner.ID, ProviderID: parityProvider, Name: name, Kind: connection.Kind,
		Source: connection.Source, MakeDefault: connection.IsDefault,
		AccessToken: login.AccessToken, RefreshToken: login.RefreshToken, IDToken: login.IDToken,
		TokenType: login.TokenType, ExpiresAt: login.ExpiresAt, AccountID: login.AccountID,
		AccountLabel: login.AccountLabel, ProjectID: login.ProjectID, OAuthProfile: login.OAuthProfile,
		OAuthClientID: login.OAuthClientID, OAuthClientMode: login.OAuthClientMode,
		OAuthRedirectURI: login.OAuthRedirectURI, OAuthClientSecret: login.OAuthClientSecret,
		Status: login.Status,
	})
	return err
}

func (w *parityWorld) storeSave(owner Principal, name string) error {
	key := "conn_parity_" + owner.Kind + "_" + name
	_, err := w.store.Save(context.Background(), key, envelopeRecord(parityEnvelope("login")))
	return err
}
