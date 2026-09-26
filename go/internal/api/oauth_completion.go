package api

import (
	"context"
	"errors"
	"strings"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"

	"github.com/xibodev/llmgw-core/oauthflow"
)

// storeOAuthConnection is the Service's credential key hook. A key cannot
// name the principal a new connection belongs to, so the hook stores the
// connection itself, with the iam calls and inputs the gateway has always
// used, and returns its ID as the key. A device flow's connection is named
// by the poll that completes it; a browser or manual flow's by its start.
func storeOAuthConnection(ctx context.Context, done oauthflow.Completion) (string, error) {
	completion := oauthCompletionFrom(ctx)
	if completion == nil {
		return "", errors.New("an OAuth flow can only complete in a gateway route")
	}
	principalID, _ := iam.CallerPrincipalID(done.Caller)
	params, record := done.Params, done.Record
	// The drivers carry, to the second, the expiry the gateway has always
	// stored: 0 for a device grant's token that names none, the zero time's
	// second for a code grant's.
	create := iam.OAuthConnectionCreate{
		PrincipalID: principalID, ProviderID: done.Instance, Name: params[oauthParamConnectionName],
		Kind: params[oauthParamKind], Source: params[oauthParamSource], MakeDefault: true,
		AccessToken: record.AccessToken, RefreshToken: record.RefreshToken, IDToken: record.IDToken,
		TokenType: record.TokenType, ExpiresAt: record.Expiry.Unix(), AccountID: record.AccountID,
		AccountLabel: record.Metadata[providers.OAuthMetadataAccountLabel],
		ProjectID:    record.Metadata[providers.OAuthMetadataProjectID], Status: "active",
		OAuthProfile:  record.Metadata[providers.OAuthMetadataProfile],
		OAuthClientID: record.Metadata[providers.OAuthMetadataClientID],
	}
	if done.Method == oauthflow.MethodDevice {
		create.Name, create.Source = completion.name, completion.source
	}
	manual := done.Method == oauthflow.MethodManual
	client := providers.OAuthManualConfig(params)
	if manual {
		// A manual connection keeps the client it was granted to, so it can
		// refresh with that client whatever is configured later.
		create.OAuthProfile, create.OAuthClientID = antigravityManualProfile, client.ClientID
		create.OAuthClientMode, create.OAuthRedirectURI, create.OAuthClientSecret = client.ClientMode, client.RedirectURI, client.ClientSecret
		if oauthRegistryIDForRef(completion.providerRef) == "openai_codex" {
			create.OAuthProfile, create.OAuthClientID = codexBrowserManualProfile, strings.TrimSpace(client.ClientID)
			create.OAuthClientMode, create.OAuthRedirectURI, create.OAuthClientSecret = "public", codexBrowserLoopbackRedirectURI, ""
		}
	}
	connection, err := iam.PutOAuthProviderConnection(create)
	if err != nil {
		return "", completion.fail("Could not store the private OAuth connection.")
	}
	if manual {
		if detail := finishManualOAuthConnection(completion.providerRef, params[oauthParamPersistProfile] == "true", create, client); detail != "" {
			revokeOAuthConnection(principalID, done.Instance, connection.Name)
			return "", completion.fail(detail)
		}
	}
	providers.ForgetProviderForPrincipal(done.Instance, principalID)
	providers.ForgetCatalogForPrincipal(done.Instance, principalID)
	completion.connection = &connection
	return connection.ID, nil
}

// finishManualOAuthConnection saves what a manual sign-in settled besides the
// connection: the consumer_manual client an administrator entered, and the
// provider, which a manual start leaves unconfigured until the owner signs
// in. It returns what the owner is told when either fails.
func finishManualOAuthConnection(providerRef string, persistProfile bool, create iam.OAuthConnectionCreate, client providers.ProviderAuthManualConfig) string {
	if persistProfile && create.OAuthProfile == antigravityManualProfile {
		if err := iam.PutOAuthClientProfile(iam.OAuthClientProfile{
			ProviderID: "google_antigravity", Profile: antigravityManualProfile,
			ClientID: client.ClientID, ClientSecret: client.ClientSecret,
			ClientMode: client.ClientMode, RedirectURI: client.RedirectURI,
		}); err != nil {
			return "Could not store the encrypted consumer_manual OAuth profile."
		}
	}
	if _, configured := config.Get().Providers[create.ProviderID]; !configured {
		if err := ensureOAuthProviderConfig(create.ProviderID, providerRef); err != nil {
			return err.Error()
		}
	}
	return ""
}

// revokeOAuthConnection withdraws a connection whose completion failed after
// it was stored, unless another sign-in replaced it meanwhile.
func revokeOAuthConnection(principalID, providerID, name string) {
	envelope, stored, ok, err := iam.OAuthProviderConnectionSecret(principalID, providerID, name)
	if err == nil && ok {
		_, _ = iam.RevokeOAuthProviderConnectionIfCurrent(stored, envelope)
	}
}

// oauthConnection loads a connection a flow stored, as the connection list
// shows it.
func oauthConnection(principalID, providerID, id string) (iam.ProviderConnection, bool) {
	connections, err := iam.ListProviderConnections(principalID, providerID)
	if err != nil {
		return iam.ProviderConnection{}, false
	}
	for _, connection := range connections {
		if connection.ID == id {
			return connection, true
		}
	}
	return iam.ProviderConnection{}, false
}
