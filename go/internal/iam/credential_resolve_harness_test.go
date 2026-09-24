package iam

import (
	"context"
	"errors"
	"strings"
	"testing"

	"llmgw/internal/config"

	core "github.com/xibodev/llmgw-core"
)

// referenceConnectionResolve is providers.resolveCredentialObserved written
// against this package's API, which is all it calls; the providers package
// cannot be imported here. It returns the secret, its kind and the source key.
func referenceConnectionResolve(principal *config.Principal, providerID string) (string, string, string, error) {
	cfg := config.Get().Providers[providerID]
	configured := func() (string, string, string, error) {
		secret := config.ResolveProviderAPIKey(providerID, cfg)
		return secret, "api_key", ConfiguredCredentialKeyPrefix + providerID, nil
	}
	if strings.TrimSpace(config.Get().CredentialEncryptionKey) == "" {
		return configured()
	}
	if principal != nil && principal.PrincipalID != "" {
		secret, connection, _, ok, err := ProviderConnectionSecretWithObservation(principal.PrincipalID, providerID, "")
		if err != nil || ok {
			return secret, strings.TrimSpace(connection.Kind), connection.ID, err
		}
	}
	secret, connection, ok, err := SystemProviderConnectionSecret(providerID)
	if err != nil || ok {
		return secret, strings.TrimSpace(connection.Kind), connection.ID, err
	}
	return configured()
}

type resolveCase struct {
	name       string
	caller     core.Caller
	providerID string
	precedence CredentialPrecedence
	// want pins the resolved key, so the differential check cannot pass by
	// both sides resolving nothing. "" means no credential.
	want string
}

func (c resolveCase) principal() *config.Principal {
	return callerPrincipal(c.caller)
}

// assertResolvesLikeGateway checks that Resolve names, and Load yields, the
// credential the gateway's own resolver hands out for the same principal.
func assertResolvesLikeGateway(t *testing.T, store *CredentialStore, c resolveCase) {
	t.Helper()
	ctx := context.Background()
	key, err := store.Resolve(ctx, c.caller, c.providerID)
	var wantSecret, wantKind, wantKey string
	var wantErr error
	switch c.precedence {
	case ConnectionPrecedence:
		wantSecret, wantKind, wantKey, wantErr = referenceConnectionResolve(c.principal(), c.providerID)
		if isOAuthCredentialKind(wantKind) && wantErr == nil {
			envelope, decodeErr := decodeOAuthEnvelope(wantSecret)
			wantSecret, wantErr = envelope.AccessToken, decodeErr
		}
	case OAuthPrecedence:
		var observation *ProviderAccountObservation
		var ok bool
		wantSecret, observation, ok, wantErr = ResolveProviderOAuthCredentialSecretWithObservation(c.principal(), c.providerID)
		wantKey = "?"
		if observation != nil {
			wantKey = observation.ConnectionID
		}
		if !ok {
			wantSecret = ""
		}
	}
	if wantErr != nil {
		if err == nil || errors.Is(err, core.ErrNoCredential) {
			t.Fatalf("%s: gateway failed with %v, Resolve returned key=%q err=%v", c.name, wantErr, key, err)
		}
		return
	}
	if strings.TrimSpace(wantSecret) == "" {
		if !errors.Is(err, core.ErrNoCredential) || c.want != "" {
			t.Fatalf("%s: gateway resolves nothing, Resolve returned key=%q err=%v want=%q", c.name, key, err, c.want)
		}
		return
	}
	if err != nil {
		t.Fatalf("%s: Resolve: %v", c.name, err)
	}
	if key != c.want || (wantKey != "?" && key != wantKey) {
		t.Fatalf("%s: key=%q, want %q (gateway source %q)", c.name, key, c.want, wantKey)
	}
	record, err := store.Load(ctx, key)
	if err != nil || record.AccessToken != wantSecret {
		t.Fatalf("%s: Load(%q) err=%v, secret differs from the gateway's", c.name, key, err)
	}
	if c.precedence == ConnectionPrecedence {
		assertRecordKind(t, c.name, record.TokenType, wantKind)
	}
}

func assertRecordKind(t *testing.T, name, tokenType, kind string) {
	t.Helper()
	switch {
	case strings.EqualFold(kind, core.TokenTypeAPIKey):
		if tokenType != core.TokenTypeAPIKey {
			t.Fatalf("%s: API key loaded as token type %q", name, tokenType)
		}
	case isOAuthCredentialKind(kind):
		if tokenType == core.TokenTypeAPIKey {
			t.Fatalf("%s: OAuth credential loaded as an API key", name)
		}
	case tokenType != kind:
		t.Fatalf("%s: kind %q loaded as token type %q", name, kind, tokenType)
	}
}

// setProviderConfig configures one provider for the test and removes it after.
func setProviderConfig(t *testing.T, providerID string, provider *config.ProviderConfig) {
	t.Helper()
	config.Update(func(s *config.Settings) {
		if s.Providers == nil {
			s.Providers = map[string]*config.ProviderConfig{}
		}
		s.Providers[providerID] = provider
	})
	t.Cleanup(func() { config.Update(func(s *config.Settings) { delete(s.Providers, providerID) }) })
}
