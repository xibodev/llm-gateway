package api

import (
	"strings"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
)

// The completions below are the gateway's own from before its flows ran on
// oauthflow, copied verbatim but for the flow bookkeeping around them. Given
// the provider's result, each is exactly what the gateway stored. The
// differential tests run one against a database and the Service's completion
// against an identical one, and compare what each left behind.

// legacyPollCompletion is pollOAuthFlow once a device poll returned.
func legacyPollCompletion(principal iam.Principal, providerID, kind, name, source string, result providers.ProviderAuthPoll) map[string]any {
	out := safeOAuthPollResponse(result.Status, result.Error)
	if result.Status != "authorized" {
		return out
	}
	connection, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
		PrincipalID: principal.ID, ProviderID: providerID, Name: name, Kind: kind, Source: source,
		MakeDefault: true, AccessToken: result.AccessToken, RefreshToken: result.RefreshToken,
		IDToken: result.IDToken, TokenType: result.TokenType, ExpiresAt: result.ExpiresAt, AccountID: result.AccountID, AccountLabel: result.AccountLabel, ProjectID: result.ProjectID, Status: "active",
		OAuthProfile: result.OAuthProfile, OAuthClientID: result.OAuthClientID,
	})
	if err != nil {
		return safeOAuthPollResponse("error", "Could not store the private OAuth connection.")
	}
	providers.ForgetProviderForPrincipal(providerID, principal.ID)
	providers.ForgetCatalogForPrincipal(providerID, principal.ID)
	out["connection"] = connection
	return out
}

// legacyBrowserFlow is what the gateway kept of a browser or manual flow for
// its completion.
type legacyBrowserFlow struct {
	PrincipalID, ProviderID, Kind, ConnectionName, Source string
	PersistConfig                                         bool
	ManualConfig                                          providers.ProviderAuthManualConfig
}

// legacyCallbackCompletion is handleOAuthBrowserCallback once the exchange
// returned; the flow's recorded result is what the next poll answered.
func legacyCallbackCompletion(flow legacyBrowserFlow, result providers.ProviderAuthPoll, err error) map[string]any {
	response := safeOAuthPollResponse("error", "Browser authorization failed.")
	if err == nil && result.Status == "authorized" {
		connection, storeErr := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
			PrincipalID: flow.PrincipalID, ProviderID: flow.ProviderID, Name: flow.ConnectionName,
			Kind: flow.Kind, Source: flow.Source, MakeDefault: true, AccessToken: result.AccessToken,
			RefreshToken: result.RefreshToken, IDToken: result.IDToken, TokenType: result.TokenType,
			ExpiresAt: result.ExpiresAt, AccountID: result.AccountID, AccountLabel: result.AccountLabel, ProjectID: result.ProjectID, Status: "active",
			OAuthProfile: result.OAuthProfile, OAuthClientID: result.OAuthClientID,
		})
		if storeErr == nil {
			response = map[string]any{"status": "authorized", "connection": connection}
			providers.ForgetProviderForPrincipal(flow.ProviderID, flow.PrincipalID)
			providers.ForgetCatalogForPrincipal(flow.ProviderID, flow.PrincipalID)
		}
	}
	return response
}

// legacyManualCompletion is completeManualOAuthFlow once the exchange
// returned.
func legacyManualCompletion(principal iam.Principal, providerRef, providerID string, flow legacyBrowserFlow, result providers.ProviderAuthPoll, err error) map[string]any {
	failed := func(detail string) map[string]any { return safeOAuthPollResponse("error", detail) }
	if err != nil || result.Status != "authorized" {
		return failed("Manual authorization failed.")
	}
	profile := antigravityManualProfile
	clientID, clientMode, redirectURI, clientSecret := flow.ManualConfig.ClientID, flow.ManualConfig.ClientMode, flow.ManualConfig.RedirectURI, flow.ManualConfig.ClientSecret
	if oauthRegistryIDForRef(providerRef) == "openai_codex" {
		profile = codexBrowserManualProfile
		clientID = strings.TrimSpace(flow.ManualConfig.ClientID)
		clientMode, redirectURI, clientSecret = "public", "http://localhost:1455/auth/callback", ""
	}
	connection, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
		PrincipalID: principal.ID, ProviderID: providerID, Name: flow.ConnectionName,
		Kind: flow.Kind, Source: flow.Source, MakeDefault: true,
		AccessToken: result.AccessToken, RefreshToken: result.RefreshToken, IDToken: result.IDToken,
		TokenType: result.TokenType, ExpiresAt: result.ExpiresAt, AccountID: result.AccountID,
		AccountLabel: result.AccountLabel, ProjectID: result.ProjectID, Status: "active",
		OAuthProfile: profile, OAuthClientID: clientID,
		OAuthClientMode: clientMode, OAuthRedirectURI: redirectURI,
		OAuthClientSecret: clientSecret,
	})
	if err != nil {
		return failed("Could not store the private OAuth connection.")
	}
	if flow.PersistConfig && profile == antigravityManualProfile {
		if err := iam.PutOAuthClientProfile(iam.OAuthClientProfile{
			ProviderID: "google_antigravity", Profile: antigravityManualProfile,
			ClientID: flow.ManualConfig.ClientID, ClientSecret: flow.ManualConfig.ClientSecret,
			ClientMode: flow.ManualConfig.ClientMode, RedirectURI: flow.ManualConfig.RedirectURI,
		}); err != nil {
			envelope, stored, ok, loadErr := iam.OAuthProviderConnectionSecret(principal.ID, providerID, connection.Name)
			if loadErr == nil && ok {
				_, _ = iam.RevokeOAuthProviderConnectionIfCurrent(stored, envelope)
			}
			return failed("Could not store the encrypted consumer_manual OAuth profile.")
		}
	}
	if _, configured := config.Get().Providers[providerID]; !configured {
		if err := ensureOAuthProviderConfig(providerID, providerRef); err != nil {
			envelope, stored, ok, loadErr := iam.OAuthProviderConnectionSecret(principal.ID, providerID, connection.Name)
			if loadErr == nil && ok {
				_, _ = iam.RevokeOAuthProviderConnectionIfCurrent(stored, envelope)
			}
			return failed(err.Error())
		}
	}
	providers.ForgetProviderForPrincipal(providerID, principal.ID)
	providers.ForgetCatalogForPrincipal(providerID, principal.ID)
	return map[string]any{"status": "authorized", "connection": connection}
}
