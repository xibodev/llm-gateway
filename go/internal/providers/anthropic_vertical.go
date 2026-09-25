package providers

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"llmgw/internal/config"

	core "github.com/xibodev/llmgw-core"
	coreproviders "github.com/xibodev/llmgw-core/providers"
)

// anthropicCoreType is Anthropic's key in coreVerticals. The facade names it
// on each operation, so the core Runtime's credential store loads Anthropic's
// credentials from Anthropic's store.
const anthropicCoreType = "anthropic"

// anthropicCoreVertical is Anthropic as the core Runtime serves it: core's
// Anthropic on the Messages surface, and the credential the provider factory
// resolves for an Anthropic instance, an API key or a setup token, or none
// for an endpoint that needs none. Neither kind refreshes, so there is no
// refresh.
//
// Its store is a connectionStore of the kinds the factory accepts. A
// setup-token connection loads as core.TokenTypeAnthropicSetupToken, which
// core's Anthropic sends as the OAuth bearer with Anthropic's beta marker. A
// configured key loads as an API key, which core's Anthropic, like the
// transport, still sends as a setup token when it has a setup token's
// prefix. When none resolves, the request is sent without a credential, as
// the transport sent it.
func (rt *Runtime) anthropicCoreVertical() coreVertical {
	return coreVertical{
		serves: func(_ *config.Settings, _ string, cfg *config.ProviderConfig) bool {
			return strings.ToLower(strings.TrimSpace(cfg.Type)) == "anthropic"
		},
		provider: func(settings *config.Settings, instance string) (core.Provider, error) {
			cfg := settings.Providers[instance]
			return newCoreAnthropic(instance, cfg.BaseURL, cfg.TimeoutOr(0))
		},
		credentials: connectionStore{
			open:       func() (core.CredentialStore, error) { return rt.openCredentials(false) },
			tokenTypes: []string{core.TokenTypeAPIKey, core.TokenTypeAnthropicSetupToken},
			refusal:    "anthropic: the resolved connection is neither an API key nor a setup token",
		},
	}
}

// newCoreAnthropic builds core's Anthropic at baseURL, whose requests time
// out as the transport's did: after timeout, or a minute when it is not
// positive. Core's catalog client is never used, since the catalog stays on
// the gateway's path.
func newCoreAnthropic(instance, baseURL string, timeout float64) (*coreproviders.Anthropic, error) {
	anthropic, err := coreproviders.NewAnthropic(coreproviders.AnthropicConfig{
		BaseURL: baseURL, Client: httpClient(anthropicTimeout(timeout)),
	})
	if err != nil {
		return nil, &ConfigError{Msg: fmt.Sprintf("provider '%s': initialize Anthropic: %v", instance, err)}
	}
	return anthropic, nil
}

func anthropicTimeout(timeout float64) float64 {
	if timeout > 0 {
		return timeout
	}
	return 60
}

// coreCredential resolves, through the store of vertical, the credential the
// core Runtime would send for caller on instance, for an operation the
// Runtime does not perform. Nil means none resolves, and the request goes
// without one, as the Runtime sends it. The vertical's credentials never
// refresh, so the stored record is the credential.
func (rt *Runtime) coreCredential(ctx context.Context, vertical string, caller core.Caller, instance string) (*core.Credential, error) {
	store := rt.verticals[vertical].credentials
	key, err := store.Resolve(ctx, caller, instance)
	if errors.Is(err, core.ErrNoCredential) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	record, err := store.Load(ctx, key)
	if err != nil {
		return nil, err
	}
	return core.CredentialFromRecord(key, record), nil
}
