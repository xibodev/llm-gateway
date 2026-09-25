package providers

import (
	"context"
	"fmt"
	"strings"
	"time"

	antigravityauth "github.com/xibodev/llm-provider-auth/antigravity"
	"github.com/xibodev/llm-provider-auth/tokenstore"
	"github.com/xibodev/llmgw-core/oauthflow"
)

// codeFlowTTL is how long a browser or manual sign-in stays valid.
const codeFlowTTL = 10 * time.Minute

// codexBrowserDriver runs the official Codex CLI's browser sign-in. Its
// registered redirect is a loopback the gateway does not serve, so the owner
// pastes the redirect back: oauthflow.MethodManual. The client is the one
// the start parameters captured, or the configured one.
type codexBrowserDriver struct{}

var _ oauthflow.CodeDriver = codexBrowserDriver{}

// Start implements oauthflow.Driver.
func (codexBrowserDriver) Start(_ context.Context, request oauthflow.StartRequest) (oauthflow.Authorization, error) {
	captured := OAuthManualConfig(request.Params)
	clientID := strings.TrimSpace(captured.ClientID)
	if clientID == "" {
		clientID = EffectiveCodexClientID()
	}
	redirectURI := strings.TrimSpace(captured.RedirectURI)
	if redirectURI == "" {
		redirectURI = codexBrowserRedirectURI
	}
	authorization, err := codexBrowserConfig(clientID).AuthorizationURL(redirectURI)
	if err != nil {
		return oauthflow.Authorization{}, err
	}
	return oauthflow.Authorization{
		AuthorizationURL: authorization.URL, ExpiresIn: codeFlowTTL,
		Secrets: oauthflow.Secrets{
			Verifier: authorization.CodeVerifier, State: authorization.State, RedirectURI: redirectURI,
			DriverData: map[string]string{"client_id": clientID},
		},
	}, nil
}

// Exchange implements oauthflow.CodeDriver with the client that started the
// flow, at the redirect it named.
func (codexBrowserDriver) Exchange(ctx context.Context, flow oauthflow.Flow, code string) (tokenstore.Record, error) {
	clientID := flow.Secrets.DriverData["client_id"]
	tokens, err := codexBrowserConfig(clientID).Exchange(ctx, code, flow.Secrets.Verifier, flow.Secrets.RedirectURI)
	if err != nil {
		return tokenstore.Record{}, err
	}
	accountID, accountLabel := codexIDTokenIdentity(tokens.IDToken)
	return tokenstore.Record{
		AccessToken: tokens.AccessToken, RefreshToken: tokens.RefreshToken, IDToken: tokens.IDToken,
		TokenType: tokens.TokenType, Expiry: tokens.ExpiresAt, AccountID: accountID,
		Metadata: map[string]string{
			OAuthMetadataAccountLabel: accountLabel,
			OAuthMetadataProfile:      codexOAuthProfileBrowser, OAuthMetadataClientID: clientID,
		},
	}, nil
}

// antigravityBrowserDriver runs Antigravity's sign-in through the gateway's
// callback with the client the instance's settings name. The exchange is
// refused when that client changed since the start, because the code belongs
// to the one that started.
type antigravityBrowserDriver struct{ adapter googleAntigravityAuthAdapter }

var _ oauthflow.CodeDriver = antigravityBrowserDriver{}

// Start implements oauthflow.Driver at the request's redirect.
func (d antigravityBrowserDriver) Start(_ context.Context, request oauthflow.StartRequest) (oauthflow.Authorization, error) {
	oauth := d.adapter.config(request.RedirectURI)
	authorization, err := oauth.AuthorizationURL()
	if err != nil {
		return oauthflow.Authorization{}, err
	}
	return oauthflow.Authorization{
		AuthorizationURL: authorization.URL, ExpiresIn: codeFlowTTL,
		Secrets: oauthflow.Secrets{
			Verifier: authorization.CodeVerifier, State: authorization.State, RedirectURI: request.RedirectURI,
			DriverData: map[string]string{
				"oauth_profile": antigravityOAuthProfile(oauth.ClientAuthMode), "oauth_client_id": oauth.ClientID,
			},
		},
	}, nil
}

// Exchange implements oauthflow.CodeDriver and discovers the account's
// project, which a failed discovery leaves empty.
func (d antigravityBrowserDriver) Exchange(ctx context.Context, flow oauthflow.Flow, code string) (tokenstore.Record, error) {
	if flow.Secrets.Verifier == "" || flow.Secrets.RedirectURI == "" {
		return tokenstore.Record{}, fmt.Errorf("browser OAuth state is unavailable")
	}
	oauth := d.adapter.config(flow.Secrets.RedirectURI)
	profile, clientID := flow.Secrets.DriverData["oauth_profile"], flow.Secrets.DriverData["oauth_client_id"]
	if profile != "" && (antigravityOAuthProfile(oauth.ClientAuthMode) != profile || strings.TrimSpace(oauth.ClientID) != strings.TrimSpace(clientID)) {
		return tokenstore.Record{}, fmt.Errorf("browser OAuth client profile is no longer available")
	}
	return antigravityRecord(ctx, oauth, flow.Secrets.Verifier, code, antigravityOAuthProfile(oauth.ClientAuthMode))
}

// antigravityManualDriver runs Antigravity's consumer_manual profile: a
// client and fixed redirect an administrator configured, which the gateway
// does not serve, so the owner pastes the code back. The client comes in
// the start parameters, where the Service keeps it for the exchange.
type antigravityManualDriver struct {
	config func(ProviderAuthManualConfig) (antigravityauth.Config, error)
}

var _ oauthflow.CodeDriver = antigravityManualDriver{}

// Start implements oauthflow.Driver.
func (d antigravityManualDriver) Start(_ context.Context, request oauthflow.StartRequest) (oauthflow.Authorization, error) {
	oauth, err := d.config(OAuthManualConfig(request.Params))
	if err != nil {
		return oauthflow.Authorization{}, err
	}
	authorization, err := oauth.AuthorizationURL()
	if err != nil {
		return oauthflow.Authorization{}, err
	}
	return oauthflow.Authorization{
		AuthorizationURL: authorization.URL, ExpiresIn: codeFlowTTL,
		Secrets: oauthflow.Secrets{
			Verifier: authorization.CodeVerifier, State: authorization.State, RedirectURI: oauth.RedirectURI,
		},
	}, nil
}

// Exchange implements oauthflow.CodeDriver.
func (d antigravityManualDriver) Exchange(ctx context.Context, flow oauthflow.Flow, code string) (tokenstore.Record, error) {
	oauth, err := d.config(OAuthManualConfig(flow.Secrets.Params))
	if err != nil {
		return tokenstore.Record{}, err
	}
	return antigravityRecord(ctx, oauth, flow.Secrets.Verifier, code, antigravityOAuthProfileConsumerManual)
}

// antigravityRecord exchanges code and discovers the account's project.
func antigravityRecord(
	ctx context.Context, oauth antigravityauth.Config, verifier, code, profile string,
) (tokenstore.Record, error) {
	tokens, err := oauth.Exchange(ctx, code, verifier)
	if err != nil {
		return tokenstore.Record{}, err
	}
	account, _ := oauth.DiscoverAccount(ctx, tokens.AccessToken)
	return tokenstore.Record{
		AccessToken: tokens.AccessToken, RefreshToken: tokens.RefreshToken, IDToken: tokens.IDToken,
		TokenType: tokens.TokenType, Expiry: tokens.ExpiresAt,
		Metadata: map[string]string{
			OAuthMetadataProjectID: account.ProjectID,
			OAuthMetadataProfile:   profile, OAuthMetadataClientID: oauth.ClientID,
		},
	}, nil
}
