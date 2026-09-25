package providers

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"llmgw/internal/config"

	core "github.com/xibodev/llmgw-core"
	coreproviders "github.com/xibodev/llmgw-core/providers"
)

// zenCoreType is OpenCode Zen's key in coreVerticals. The facade names it on
// each operation, so the core Runtime's credential store loads Zen's keys
// from Zen's store.
const zenCoreType = "opencode_zen"

// zenCoreVertical is OpenCode Zen as the core Runtime serves it: core's Zen
// over the caller's cached catalog, and the credential the provider factory
// resolves for an OpenAI-compatible instance, or none. API keys never
// refresh, so there is no refresh.
//
// Zen's store is a connectionStore of API keys. When none resolves, core's
// Zen sends the request anonymously, as the transport sent the factory's
// empty key. A configured anonymous sentinel resolves as the configured key,
// which core's Zen reads as anonymous access too.
func (rt *Runtime) zenCoreVertical() coreVertical {
	return coreVertical{
		serves:   zenInstance,
		provider: rt.coreZen,
		credentials: connectionStore{
			open:       func() (core.CredentialStore, error) { return rt.openCredentials(false) },
			tokenTypes: []string{core.TokenTypeAPIKey},
			refusal:    "opencode_zen: the resolved connection is not an API key",
		},
	}
}

// zenInstance reports whether a configured provider is served by OpenCode
// Zen: an OpenAI-compatible instance, other than Codex, that names Zen's
// registry entry or Zen's endpoint, as the provider factory recognized one.
func zenInstance(settings *config.Settings, instance string, cfg *config.ProviderConfig) bool {
	switch strings.ToLower(strings.TrimSpace(cfg.Type)) {
	case "openai_compatible", "openai", "litellm":
	default:
		return false
	}
	return !codexInstance(instance, cfg) &&
		(EffectiveRegistryID(instance, cfg.RegistryID, cfg.Type) == "opencode_zen" ||
			isZenBaseURL(openAICompatibleBase(settings, cfg)))
}

// openAICompatibleBase is the base URL of an OpenAI-compatible instance: its
// own, or the configured default.
func openAICompatibleBase(settings *config.Settings, cfg *config.ProviderConfig) string {
	if cfg.BaseURL != "" {
		return cfg.BaseURL
	}
	return settings.OpenAICompatibleBaseURL
}

// coreZen builds the core provider of a Zen instance, whose requests time
// out as the OpenAI-compatible transport's did.
func (rt *Runtime) coreZen(settings *config.Settings, instance string) (core.Provider, error) {
	cfg := settings.Providers[instance]
	timeout := settings.OpenAICompatibleTimeoutSeconds
	if cfg.Timeout != nil {
		timeout = *cfg.Timeout
	}
	provider := zenCoreProvider{
		runtime: rt, instance: instance, base: openAICompatibleBase(settings, cfg), client: httpClient(timeout),
	}
	if _, err := provider.bind(context.Background()); err != nil {
		return nil, zenConfigError(instance, err)
	}
	return provider, nil
}

func zenConfigError(instance string, err error) error {
	return &ConfigError{Msg: fmt.Sprintf("provider '%s': initialize OpenCode Zen: %v", instance, err)}
}

// zenCoreProvider is core's Zen as the core Runtime serves a Zen instance.
// The Runtime keeps one provider per instance, while core's Zen reads the
// catalog of one caller, so each operation binds a core Zen to the catalog
// of the caller its context names. A bound Zen costs its configuration, and
// every one shares the instance's client.
type zenCoreProvider struct {
	runtime  *Runtime
	instance string
	base     string
	client   *http.Client
}

var _ core.Provider = zenCoreProvider{}

func (p zenCoreProvider) bind(ctx context.Context) (*coreproviders.Zen, error) {
	return p.runtime.newCoreZen(p.instance, p.base, coreOperationFrom(ctx).caller, p.client)
}

// NativeSurfaces implements core.Provider over the shared catalog. An
// operation's Zen reads its caller's.
func (p zenCoreProvider) NativeSurfaces(model string) []core.ModelSurface {
	zen, err := p.bind(context.Background())
	if err != nil {
		return nil
	}
	return zen.NativeSurfaces(model)
}

func (p zenCoreProvider) Invoke(ctx context.Context, request core.Request) (core.Response, error) {
	zen, err := p.bind(ctx)
	if err != nil {
		return core.Response{}, err
	}
	return zen.Invoke(ctx, request)
}

func (p zenCoreProvider) Stream(ctx context.Context, request core.Request) (core.StreamIter, error) {
	zen, err := p.bind(ctx)
	if err != nil {
		return nil, err
	}
	return zen.Stream(ctx, request)
}

func (p zenCoreProvider) ListModels(ctx context.Context, credential *core.Credential) ([]core.ModelInfo, error) {
	zen, err := p.bind(ctx)
	if err != nil {
		return nil, err
	}
	return zen.ListModels(ctx, credential)
}

// newCoreZen builds core's Zen for instance at base over the catalog the
// gateway cached for caller: Zen serves a model on the surfaces its row
// lists, and anonymously only a row the catalog marked free. The lookup never
// discovers, so a request adds no catalog call. client sends the requests; a
// Zen that sends none may leave it nil.
func (rt *Runtime) newCoreZen(instance, base string, caller core.Caller, client *http.Client) (*coreproviders.Zen, error) {
	return coreproviders.NewZen(coreproviders.ZenConfig{
		BaseURL: base, Client: client,
		Models: func(model string) (core.ModelInfo, bool) {
			row, ok := rt.CatalogCachedLookupForPrincipal(instance, model, caller)
			if !ok {
				return core.ModelInfo{}, false
			}
			info := core.ModelInfo{ID: row.ID, SupportedAPIs: row.SupportedSurfaces}
			if row.Free {
				info.Tags = []string{coreproviders.ModelTagFree}
			}
			return info, true
		},
	})
}
