package providers

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"llmgw/internal/config"

	core "github.com/xibodev/llmgw-core"
	coreproviders "github.com/xibodev/llmgw-core/providers"
)

const (
	elevenLabsCoreType = "elevenlabs"
	miMoCoreType       = "mimo"
)

// audioCoreVertical serves a direct API-key audio provider through core's
// wire-level audio surfaces. The gateway facade remains catalog-only: chat
// routing must never mistake an audio-only provider for a chat provider.
func (rt *Runtime) audioCoreVertical(providerType string) coreVertical {
	return coreVertical{
		serves: func(_ *config.Settings, _ string, cfg *config.ProviderConfig) bool {
			return strings.EqualFold(strings.TrimSpace(cfg.Type), providerType)
		},
		provider: func(settings *config.Settings, instance string) (core.Provider, error) {
			cfg := settings.Providers[instance]
			client := httpClient(cfg.TimeoutOr(settings.OpenAICompatibleTimeoutSeconds))
			var provider core.Provider
			var err error
			switch providerType {
			case elevenLabsCoreType:
				provider, err = coreproviders.NewElevenLabs(coreproviders.ElevenLabsConfig{
					BaseURL: cfg.BaseURL,
					Client:  client,
				})
			case miMoCoreType:
				provider, err = coreproviders.NewMiMo(coreproviders.MiMoConfig{
					BaseURL: cfg.BaseURL,
					Client:  client,
				})
			default:
				err = fmt.Errorf("unknown audio provider type %q", providerType)
			}
			if err != nil {
				return nil, &ConfigError{Msg: fmt.Sprintf("provider '%s': initialize %s: %v", instance, providerType, err)}
			}
			return provider, nil
		},
		credentials: connectionStore{
			open:       func() (core.CredentialStore, error) { return rt.openCredentials(false) },
			tokenTypes: []string{core.TokenTypeAPIKey},
			refusal:    providerType + ": the resolved connection is not an API key",
		},
	}
}

// audioProviderFacade exposes a static core audio catalog to the gateway's
// catalog service. Its chat methods fail closed and should be unreachable:
// every catalog row advertises only its exact audio surface.
type audioProviderFacade struct {
	providerType string
	models       []ModelInfo
}

var _ Provider = (*audioProviderFacade)(nil)

func (p *audioProviderFacade) Complete(string, []Message, Kwargs) (map[string]any, error) {
	return nil, &ConfigError{Msg: p.providerType + " is an audio-only provider"}
}

func (p *audioProviderFacade) Stream(string, []Message, Kwargs) (StreamIter, error) {
	return nil, &ConfigError{Msg: p.providerType + " is an audio-only provider"}
}

func (p *audioProviderFacade) ListModels() []ModelInfo { return slices.Clone(p.models) }

func (p *audioProviderFacade) IsStub() bool { return false }

func (rt *Runtime) newAudioProviderFacade(
	settings *config.Settings, providerID, providerType string,
) (*audioProviderFacade, error) {
	vertical := rt.verticals[providerType]
	provider, err := vertical.provider(settings, providerID)
	if err != nil {
		return nil, err
	}
	models, err := provider.ListModels(context.Background(), nil)
	if err != nil {
		return nil, &ConfigError{Msg: fmt.Sprintf("provider '%s': list %s models: %v", providerID, providerType, err)}
	}
	return &audioProviderFacade{providerType: providerType, models: gatewayRows(models)}, nil
}

type surfaceDeclarer interface {
	Surfaces(model string) []core.ModelSurface
}

func servesCoreSurface(provider core.Provider, model string, surface core.ModelSurface) bool {
	if declarer, ok := provider.(surfaceDeclarer); ok {
		return slices.Contains(declarer.Surfaces(model), surface)
	}
	return core.ServesNatively(provider, model, surface)
}

// CoreServesSurfaceForPrincipal reports whether the provider's core vertical
// serves surface for model. It resolves no credential and performs no network
// call; the caller argument keeps this API aligned with invocation and leaves
// room for caller-scoped translated declarations.
func (rt *Runtime) CoreServesSurfaceForPrincipal(
	providerID, model string, surface core.ModelSurface, _ core.Caller,
) bool {
	settings, _ := (config.Source{}).Snapshot()
	cfg := settings.Providers[providerID]
	if cfg == nil || cfg.Disabled {
		return false
	}
	_, vertical, ok := rt.vertical(settings, providerID)
	if !ok {
		return false
	}
	provider, err := vertical.provider(settings, providerID)
	return err == nil && servesCoreSurface(provider, model, surface)
}

// InvokeCoreSurfaceForPrincipal invokes a surface only when the configured
// core vertical declares it. handled=false lets legacy native/proxy paths keep
// serving provider types whose core vertical does not own that surface.
func (rt *Runtime) InvokeCoreSurfaceForPrincipal(
	ctx context.Context, providerID string, caller core.Caller, request core.Request,
) (response core.Response, handled bool, err error) {
	settings, _ := (config.Source{}).Snapshot()
	cfg := settings.Providers[providerID]
	if cfg == nil || cfg.Disabled {
		return core.Response{}, false, nil
	}
	verticalName, vertical, ok := rt.vertical(settings, providerID)
	if !ok {
		return core.Response{}, false, nil
	}
	provider, buildErr := vertical.provider(settings, providerID)
	if buildErr != nil {
		return core.Response{}, true, buildErr
	}
	if !servesCoreSurface(provider, request.Model, request.Surface) {
		return core.Response{}, false, nil
	}
	ctx = withCoreOperation(ctx, verticalName, caller)
	response, err = rt.core.Invoke(ctx, caller, providerID, request)
	return response, true, err
}
