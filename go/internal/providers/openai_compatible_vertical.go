package providers

import (
	"context"
	"fmt"
	"strings"

	"llmgw/internal/config"

	core "github.com/xibodev/llmgw-core"
	coreproviders "github.com/xibodev/llmgw-core/providers"
)

// openAICompatibleCoreType is the key in coreVerticals of the instances
// core's OpenAICompatible serves: the openai_compatible, openai and litellm
// instances neither Codex nor OpenCode Zen serves, the registry's anonymous
// catalogs and keyless local servers among them. The facade names it on
// each operation, so the core Runtime's credential store loads their keys
// from their store.
const openAICompatibleCoreType = "openai_compatible"

// openAICompatibleCoreVertical is an OpenAI-compatible upstream as the core
// Runtime serves it: core's OpenAICompatible over the caller's cached
// catalog, and the key the provider factory resolves for the instance, or
// none. Keys never refresh, so there is no refresh.
//
// Its store is a connectionStore of API keys, the one kind the factory
// accepts. When none resolves, the request is sent without Authorization,
// as the transport sent the factory's empty key: a keyless local server
// and an anonymous registry entry are served that way. A resolved key is
// normalized as the transport normalized it: a "Bearer " prefix is dropped,
// "free" and "none" send no Authorization, and "public" is the bearer.
func (rt *Runtime) openAICompatibleCoreVertical() coreVertical {
	return coreVertical{
		serves: openAICompatibleInstance,
		provider: func(settings *config.Settings, instance string) (core.Provider, error) {
			return rt.newOpenAICoreProvider(openAICompatibleCore(settings, instance, settings.Providers[instance]))
		},
		credentials: connectionStore{
			open:       func() (core.CredentialStore, error) { return rt.openCredentials(false) },
			tokenTypes: []string{core.TokenTypeAPIKey},
			refusal:    "openai_compatible: the resolved connection is not an API key",
		},
	}
}

// openAICompatibleInstance reports whether a configured provider is served
// by core's OpenAICompatible: an OpenAI-compatible instance that neither
// Codex nor OpenCode Zen serves, as the provider factory told them apart.
func openAICompatibleInstance(settings *config.Settings, instance string, cfg *config.ProviderConfig) bool {
	switch strings.ToLower(strings.TrimSpace(cfg.Type)) {
	case "openai_compatible", "openai", "litellm":
		if cfg != nil && strings.EqualFold(cfg.RegistryID, "openai_codex") {
			return false
		}
		return true
	}
	return false
}

// openAICompatibleCore is how an OpenAI-compatible instance's core provider
// is built: at the instance's base URL, for its registry entry, with its
// configured adaptation, and with requests that time out as the transport's
// did. Core's catalog client is never used, since the catalog stays on the
// gateway's path.
func openAICompatibleCore(settings *config.Settings, instance string, cfg *config.ProviderConfig) openAICoreSpec {
	return openAICoreSpec{
		instance: instance,
		config: coreproviders.OpenAICompatibleConfig{
			BaseURL: openAICompatibleBase(settings, cfg), RegistryID: EffectiveRegistryID(instance, cfg.RegistryID, cfg.Type),
			ForceAPISupport: cfg.ForceApiSupport, Client: httpClient(cfg.TimeoutOr(settings.OpenAICompatibleTimeoutSeconds)),
		},
		build: func(config coreproviders.OpenAICompatibleConfig) (*coreproviders.OpenAICompatible, error) {
			provider, err := coreproviders.NewOpenAICompatible(config)
			if err != nil {
				return nil, &ConfigError{Msg: fmt.Sprintf("provider '%s': initialize OpenAI-compatible: %v", instance, err)}
			}
			return provider, nil
		},
	}
}

// openAICoreSpec is how the core provider of an instance of an OpenAI-wire
// type is built: the configuration its operations share, and the
// constructor of its type.
type openAICoreSpec struct {
	instance string
	config   coreproviders.OpenAICompatibleConfig
	build    func(coreproviders.OpenAICompatibleConfig) (*coreproviders.OpenAICompatible, error)
}

// bind builds the core provider over the catalog the gateway cached for
// caller: Responses is native for a model whose row lists it, and
// adaptation reads the row's surfaces and reasoning efforts, as the
// transport's plan read them. The lookup never discovers, so a request adds
// no catalog call; the facade refreshes a stale catalog first where the
// transport did.
func (s openAICoreSpec) bind(rt *Runtime, caller core.Caller) (*coreproviders.OpenAICompatible, error) {
	config := s.config
	config.Models = func(model string) (core.ModelInfo, bool) {
		row, ok := rt.CatalogCachedLookupForPrincipal(s.instance, model, caller)
		if !ok {
			return core.ModelInfo{}, false
		}
		return core.ModelInfo{ID: row.ID, SupportedAPIs: row.SupportedSurfaces, LegacyCapabilities: row.Capabilities}, true
	}
	return s.build(config)
}

// newOpenAICoreProvider returns the provider the core Runtime keeps for the
// instance spec builds, once its configuration builds one.
func (rt *Runtime) newOpenAICoreProvider(spec openAICoreSpec) (core.Provider, error) {
	if _, err := spec.bind(rt, core.Caller{}); err != nil {
		return nil, err
	}
	return openAICoreProvider{runtime: rt, spec: spec}, nil
}

// openAICoreProvider is core's OpenAICompatible as the core Runtime serves
// an instance of an OpenAI-wire type. The Runtime keeps one provider per
// instance, while core's reads the catalog of one caller, so each operation
// binds a core provider to the catalog of the caller its context names. A
// bound provider costs its configuration, and every one shares the
// instance's client.
type openAICoreProvider struct {
	runtime *Runtime
	spec    openAICoreSpec
}

var _ core.Provider = openAICoreProvider{}

func (p openAICoreProvider) bind(ctx context.Context) (*coreproviders.OpenAICompatible, error) {
	return p.spec.bind(p.runtime, coreOperationFrom(ctx).caller)
}

// NativeSurfaces implements core.Provider over the shared catalog. An
// operation's provider reads its caller's.
func (p openAICoreProvider) NativeSurfaces(model string) []core.ModelSurface {
	provider, err := p.bind(context.Background())
	if err != nil {
		return nil
	}
	return provider.NativeSurfaces(model)
}

func (p openAICoreProvider) Invoke(ctx context.Context, request core.Request) (core.Response, error) {
	provider, err := p.bind(ctx)
	if err != nil {
		return core.Response{}, err
	}
	return provider.Invoke(ctx, request)
}

func (p openAICoreProvider) Stream(ctx context.Context, request core.Request) (core.StreamIter, error) {
	provider, err := p.bind(ctx)
	if err != nil {
		return nil, err
	}
	return provider.Stream(ctx, request)
}

func (p openAICoreProvider) ListModels(ctx context.Context, credential *core.Credential) ([]core.ModelInfo, error) {
	provider, err := p.bind(ctx)
	if err != nil {
		return nil, err
	}
	return provider.ListModels(ctx, credential)
}
