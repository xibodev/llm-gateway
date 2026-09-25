package providers

import (
	"context"
	"errors"
	"maps"
	"strings"

	"llmgw/internal/config"
	"llmgw/internal/iam"

	"github.com/xibodev/llm-provider-auth/tokenstore"
	core "github.com/xibodev/llmgw-core"
	coreproviders "github.com/xibodev/llmgw-core/providers"
)

func errNoAntigravityConnection() error {
	return &ConfigError{Msg: "google_antigravity: this principal has no active private Antigravity connection"}
}

func errNoAntigravityRefreshToken() error {
	return errors.New("Antigravity refresh token is unavailable")
}

func errAntigravityReauthorize() error {
	return errors.New("Antigravity OAuth client profile is unavailable; reauthorize this connection")
}

func errAntigravityProfileUnavailable() error {
	return errors.New("Antigravity OAuth client profile is unavailable")
}

// antigravityInstance reports whether a configured provider is served by
// Antigravity.
func antigravityInstance(cfg *config.ProviderConfig) bool {
	return strings.EqualFold(strings.TrimSpace(cfg.Type), "google_antigravity")
}

// antigravityRefresh returns how a Coordinator refreshes a credential of the
// Antigravity instance under settings: core's Antigravity refresh, which never
// revokes a connection, as the Antigravity path never did, with the runtime
// OAuth client the gateway signs in with, plus the rules of that path core
// lacks.
func (rt *Runtime) antigravityRefresh(settings *config.Settings, instance string) tokenstore.RefreshFunc {
	oauth := rt.antigravityOAuthConfig(settings, "")
	publicClientID := ""
	if cfg := settings.Providers[instance]; cfg != nil {
		publicClientID = cfg.PublicOAuthClientID
	}
	refresh := coreproviders.NewAntigravityRefresh(oauth)
	return func(ctx context.Context, current tokenstore.Record) (tokenstore.Record, error) {
		if err := antigravityGrantRefusal(oauth.ClientID, publicClientID, current.Metadata); err != nil {
			return tokenstore.Record{}, err
		}
		return refresh(ctx, current)
	}
}

// antigravityGrantRefusal refuses the grants the Antigravity path refused to
// refresh, which core's refresh would send to a client they may not belong
// to. Core refreshes a record without an OAuth profile, or with the runtime
// profile but no client, with the configured client; such a connection
// predates stored client profiles and must be authorized again. It refreshes
// a public client's record with the client the record names; the gateway
// requires that client to be the one the provider still configures. And it
// reports a consumer's malformed client in its own words.
func antigravityGrantRefusal(configuredClientID, publicClientID string, metadata map[string]string) error {
	clientID := strings.TrimSpace(metadata[core.CredentialMetadataOAuthClientID])
	switch metadata[core.CredentialMetadataOAuthProfile] {
	case "":
		return errAntigravityReauthorize()
	case coreproviders.AntigravityOAuthProfileRuntimeSecret:
		if clientID == "" {
			return errAntigravityReauthorize()
		}
		if strings.TrimSpace(configuredClientID) != clientID {
			return errAntigravityProfileUnavailable()
		}
	case coreproviders.AntigravityOAuthProfilePublicPKCE:
		if public := strings.TrimSpace(publicClientID); public == "" || public != clientID {
			return errAntigravityProfileUnavailable()
		}
	case coreproviders.AntigravityOAuthProfileConsumerManual:
		_, err := antigravityManualConfig(ProviderAuthManualConfig{
			ClientID: metadata[core.CredentialMetadataOAuthClientID], ClientSecret: metadata[core.CredentialMetadataOAuthClientSecret],
			ClientMode: metadata[core.CredentialMetadataOAuthClientMode], RedirectURI: metadata[core.CredentialMetadataOAuthRedirectURI],
		})
		return err
	default:
		return errors.New("Antigravity OAuth client profile is unsupported")
	}
	return nil
}

// antigravityCoordinator refreshes the credentials of the Antigravity
// instance outside the core Runtime: for image generation and the catalog,
// which the Runtime does not serve, and for the console. Its refreshes take
// the credential store's lease, so they serialize with the Runtime's own
// coordinators in this process and every other.
func (rt *Runtime) antigravityCoordinator(instance string) (*tokenstore.Coordinator, error) {
	return tokenstore.NewCoordinator(rt.credentials, rt.antigravityRefresh(config.Get(), instance))
}

// storeAntigravityProject is core Antigravity's ProjectResolved hook. It
// stores the Code Assist project an operation discovered for credential as
// the Antigravity path stored it: only while the connection still holds the
// token the project was discovered with and names no project, and with a
// write fenced on the revision that read returned, so a sign-in meanwhile,
// which may act for another account, is never overwritten. A failed write
// is dropped; the next operation discovers the project again.
func (rt *Runtime) storeAntigravityProject(ctx context.Context, credential *core.Credential, projectID string) {
	projectID = strings.TrimSpace(projectID)
	if credential == nil || projectID == "" {
		return
	}
	record, err := rt.credentials.Load(ctx, credential.ConnectionID)
	if err != nil || record.AccessToken != credential.Token ||
		strings.TrimSpace(record.Metadata[core.CredentialMetadataProjectID]) != "" {
		return
	}
	record.Metadata = maps.Clone(record.Metadata)
	if record.Metadata == nil {
		record.Metadata = map[string]string{}
	}
	record.Metadata[core.CredentialMetadataProjectID] = projectID
	_, _ = rt.credentials.ReplaceIfCurrent(ctx, credential.ConnectionID, record.Revision, record)
}

// RefreshAntigravityOAuthConnection is the console's "refresh now". It runs
// the refresh the Antigravity operations run, through a Coordinator over the
// credential store, so it takes the same lease and compare-and-swap, and it
// refreshes even a token that has not expired: the Coordinator's Rejected
// refreshes whenever the record it is handed is still current.
func RefreshAntigravityOAuthConnection(
	ctx context.Context, principalID, providerID, name string,
) (iam.OAuthTokenEnvelope, iam.ProviderConnection, error) {
	if err := Current().refreshAntigravityConnection(ctx, principalID, providerID, name); err != nil {
		return iam.OAuthTokenEnvelope{}, iam.ProviderConnection{}, err
	}
	envelope, connection, ok, err := iam.OAuthProviderConnectionSecret(principalID, providerID, name)
	if err == nil && !ok {
		err = errors.New("Antigravity connection is no longer active")
	}
	if err != nil {
		return iam.OAuthTokenEnvelope{}, iam.ProviderConnection{}, err
	}
	return envelope, connection, nil
}

func (rt *Runtime) refreshAntigravityConnection(ctx context.Context, principalID, providerID, name string) error {
	envelope, connection, ok, err := iam.OAuthProviderConnectionSecret(principalID, providerID, name)
	if err != nil || !ok {
		return errors.New("Antigravity connection is unavailable")
	}
	if strings.TrimSpace(envelope.RefreshToken) == "" {
		return errNoAntigravityRefreshToken()
	}
	ctx, _ = withOAuthCall(ctx, antigravityVertical, principalID, providerID)
	coordinator, err := rt.antigravityCoordinator(providerID)
	var record tokenstore.Record
	if err == nil {
		record, err = rt.credentials.Load(ctx, connection.ID)
	}
	if err == nil {
		_, err = coordinator.Rejected(ctx, connection.ID, record)
	}
	return err
}
