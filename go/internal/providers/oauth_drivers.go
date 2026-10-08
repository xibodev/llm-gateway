package providers

import (
	"fmt"
	"strings"

	"github.com/xibodev/llmgw-core/oauthflow"
)

// Metadata keys of the records the OAuth drivers return, for what a sign-in
// reports beyond tokenstore.Record's fields. They are the keys under which
// iam's credential store carries the same envelope fields.
const (
	OAuthMetadataAccountLabel = "account_label"
	OAuthMetadataProjectID    = "project_id"
	OAuthMetadataProfile      = "oauth_profile"
	OAuthMetadataClientID     = "oauth_client_id"
)

// Start parameters that carry a flow's OAuth client to its driver: a manual
// flow's whole client, and the client ID a Codex sign-in names.
const (
	oauthParamClientID     = "client_id"
	oauthParamClientSecret = "client_secret"
	oauthParamClientMode   = "client_mode"
	oauthParamRedirectURI  = "redirect_uri"
)

// WithOAuthClientID returns params naming clientID as the OAuth client the
// flow signs in with. An empty clientID names none, rather than an empty one,
// so the companion daemon signs in with its own client.
func WithOAuthClientID(params map[string]string, clientID string) map[string]string {
	if clientID = strings.TrimSpace(clientID); clientID != "" {
		params[oauthParamClientID] = clientID
	} else {
		delete(params, oauthParamClientID)
	}
	return params
}

type ProviderAuthManualConfig struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	ClientMode   string `json:"client_mode"`
	RedirectURI  string `json:"redirect_uri"`
}

// OAuthParams returns config as the start parameters of a manual flow,
// which its driver reads its OAuth client from.
func (config ProviderAuthManualConfig) OAuthParams() map[string]string {
	return map[string]string{
		oauthParamClientID: config.ClientID, oauthParamClientSecret: config.ClientSecret,
		oauthParamClientMode: config.ClientMode, oauthParamRedirectURI: config.RedirectURI,
	}
}

// OAuthManualConfig reads the OAuth client a manual flow started with back
// from its start parameters.
func OAuthManualConfig(params map[string]string) ProviderAuthManualConfig {
	return ProviderAuthManualConfig{
		ClientID: params[oauthParamClientID], ClientSecret: params[oauthParamClientSecret],
		ClientMode: params[oauthParamClientMode], RedirectURI: params[oauthParamRedirectURI],
	}
}

// OAuthDriver returns the driver that runs method for providerID, an
// instance whose registry auth adapter is adapterID. These are the flows the
// console offers: device authorization for Copilot and Codex, the Codex CLI's
// browser sign-in, whose loopback redirect the owner pastes back, and
// Antigravity's gateway callback and consumer_manual profile. Each calls
// llm-provider-auth exactly as the adapter's own flow methods do.
func (rt *Runtime) OAuthDriver(adapterID, providerID string, method oauthflow.Method) (oauthflow.Driver, error) {
	if isExtensionType(adapterID) {
		client, err := rt.extensionClient()
		if err != nil {
			return nil, err
		}
		return client.OAuthDriver(adapterID), nil
	}
	return nil, fmt.Errorf("provider %q does not offer %s authorization", providerID, method)
}
