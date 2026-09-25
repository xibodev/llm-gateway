package providers

import (
	"context"
	"errors"

	core "github.com/xibodev/llmgw-core"
	coreproviders "github.com/xibodev/llmgw-core/providers"
)

// coreDeclarer is a facade whose inference a core provider serves. That
// provider's declarations, which core.PreservesWireFor reads, say which
// surfaces the facade forwards in their own wire, for the credential its
// requests carry.
type coreDeclarer interface {
	coreDeclaration() (core.Provider, *core.Credential)
}

// WireDeclaration returns the core provider whose declarations say which
// surfaces provider forwards in their own wire, and the credential its
// requests carry, for core's transport planning. A facade core serves
// returns its core provider. Any other declares through
// PreservesWireNativeSurface, as Copilot's does, or preserves nothing, as
// core's providers of the remaining types declare. A nil provider, one that
// could not be built, declares nothing, so planning offers it no interface.
func WireDeclaration(provider Provider) (core.Provider, *core.Credential) {
	if provider == nil {
		return nil, nil
	}
	for current := provider; current != nil; {
		switch declared := current.(type) {
		case coreDeclarer:
			return declared.coreDeclaration()
		case WireNativePreservationProvider:
			return gatewayDeclaration{declared}, nil
		}
		unwrapper, ok := current.(interface{ Unwrap() Provider })
		if !ok {
			break
		}
		current = unwrapper.Unwrap()
	}
	return gatewayDeclaration{}, nil
}

var errDeclarationOnly = errors.New("providers: a wire declaration performs no operation")

// gatewayDeclaration is a gateway declaration as a core provider: it serves
// natively, and preserves, exactly the chat surfaces declarer preserves for
// a model, and nothing without a declarer. It only declares.
type gatewayDeclaration struct {
	declarer WireNativePreservationProvider
}

var _ core.WirePreserver = gatewayDeclaration{}

func (d gatewayDeclaration) NativeSurfaces(model string) []core.ModelSurface {
	var surfaces []core.ModelSurface
	for _, surface := range []core.ModelSurface{
		core.ModelSurfaceChatCompletions, core.ModelSurfaceResponses, core.ModelSurfaceMessages,
	} {
		if d.PreservesWire(model, surface) {
			surfaces = append(surfaces, surface)
		}
	}
	return surfaces
}

func (d gatewayDeclaration) PreservesWire(model string, surface core.ModelSurface) bool {
	return d.declarer != nil && d.declarer.PreservesWireNativeSurface(model, surface)
}

func (gatewayDeclaration) Invoke(context.Context, core.Request) (core.Response, error) {
	return core.Response{}, errDeclarationOnly
}

func (gatewayDeclaration) Stream(context.Context, core.Request) (core.StreamIter, error) {
	return nil, errDeclarationOnly
}

func (gatewayDeclaration) ListModels(context.Context, *core.Credential) ([]core.ModelInfo, error) {
	return nil, errDeclarationOnly
}

// coreDeclaration is core's OpenAICompatible over the caller's catalog.
func (p *openAICompatibleProvider) coreDeclaration() (core.Provider, *core.Credential) {
	if p.declared == nil {
		return gatewayDeclaration{}, nil
	}
	return p.declared, nil
}

// coreDeclaration is core's Zen over the caller's catalog, with the key the
// facade was built with: anonymous access preserves nothing.
func (p *zenProvider) coreDeclaration() (core.Provider, *core.Credential) {
	if p.declared == nil {
		return gatewayDeclaration{}, nil
	}
	return p.declared, &core.Credential{APIKey: p.apiKey}
}

// coreDeclaration is core's Codex, which preserves Responses alone.
func (p CodexProvider) coreDeclaration() (core.Provider, *core.Credential) {
	if p.catalog == nil {
		return gatewayDeclaration{}, nil
	}
	return p.catalog, nil
}

// coreDeclaration is core's Anthropic, which preserves Messages whatever it
// is configured with, so the default configuration declares for any.
func (AnthropicNativeProvider) coreDeclaration() (core.Provider, *core.Credential) {
	anthropic, err := coreproviders.NewAnthropic(coreproviders.AnthropicConfig{})
	if err != nil {
		return gatewayDeclaration{}, nil
	}
	return anthropic, nil
}
