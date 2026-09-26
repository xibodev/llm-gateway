package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"llmgw/internal/config"

	"github.com/xibodev/llm-translate"
	core "github.com/xibodev/llmgw-core"
	coreproviders "github.com/xibodev/llmgw-core/providers"
)

// openAICompatibleProvider is the gateway's facade for an upstream that
// speaks the OpenAI wire: an openai_compatible, openai or litellm instance
// that neither Codex nor OpenCode Zen serves, or a Bedrock instance. Chat,
// Responses and both streams go through the core Runtime, which resolves the
// caller's key through the type's store on each request, and core's
// OpenAICompatible, which shapes each request and reads each answer as the
// gateway's OpenAI transport did, adaptation and the Responses parameter
// retry included. What the gateway did above that transport stays here: the
// adaptation switch each request carries, the Chat fallback of a model
// without native Responses, and the relay of stream records as the data
// events the API layer reads. The router still serves a Chat-only model's
// Responses over this facade's Chat with its own conversion, streamed or
// not: core's translation.Adapter cannot stream Responses over Chat.
//
// The catalog, and the endpoints the gateway proxies, stay on the gateway's
// path with the factory's key, as Copilot's and keyed Zen's do: the gateway
// never asks the core Runtime for a catalog, and core's rows would have to
// be read back into the untyped rows /v1/models presents.
type openAICompatibleProvider struct {
	runtime  *Runtime
	instance string
	caller   core.Caller
	// vertical is the type's key in coreVerticals, and label the name core
	// gives the upstream in the errors it reports.
	vertical, label string
	forceAdapt      bool
	// openAIEntry is an instance of the openai registry entry, whose every
	// model serves Responses natively.
	openAIEntry bool
	// catalog lists with the key the provider factory resolved when it
	// built the facade, and the facade reports that key's observation, as
	// the transport reported the credential it held; a request uses what
	// the store resolves for it.
	catalog openAICatalog
	// declared is core's provider over the caller's catalog. It sends
	// nothing: it says which surfaces the facade preserves.
	declared *coreproviders.OpenAICompatible
}

var _ Provider = (*openAICompatibleProvider)(nil)

// newOpenAICompatibleProvider returns the facade of an OpenAI-compatible
// instance for caller. Its catalog is at the instance's base URL, bounded by
// its timeout as the transport bounded it, and an anonymous registry entry
// lists only anonymous models without a key.
func (rt *Runtime) newOpenAICompatibleProvider(
	instance string, cfg *config.ProviderConfig, caller core.Caller, settings *config.Settings,
) (*openAICompatibleProvider, error) {
	spec := openAICompatibleCore(settings, instance, cfg)
	entry, _ := RegistryProviderByID(spec.config.RegistryID)
	catalog := openAICatalog{
		base:    strings.TrimRight(openAICompatibleBase(settings, cfg), "/"),
		timeout: cfg.TimeoutOr(settings.OpenAICompatibleTimeoutSeconds), registryID: spec.config.RegistryID,
	}
	return rt.newOpenAIFacade(settings, openAICompatibleCoreType, "OpenAI-compatible", spec, cfg, caller, catalog, entry.AnonymousAutomation)
}

// newBedrockProvider returns the facade of a Bedrock instance for caller.
// Its catalog is at its endpoint and bounded by its timeout, as the
// transport's was, which never timed out when none was configured.
func (rt *Runtime) newBedrockProvider(
	settings *config.Settings, instance string, cfg *config.ProviderConfig, caller core.Caller,
) (*openAICompatibleProvider, error) {
	catalog := openAICatalog{base: bedrockBase(cfg), timeout: cfg.TimeoutOr(0)}
	return rt.newOpenAIFacade(settings, bedrockCoreType, "Bedrock", bedrockCore(instance, cfg), cfg, caller, catalog, false)
}

// newOpenAIFacade builds the facade of the instance spec builds, with the
// API key the factory resolves for an OpenAI-compatible instance. A key of
// another kind is refused here, as the factory refused it, and so is a
// configuration core cannot build a provider for.
func (rt *Runtime) newOpenAIFacade(
	settings *config.Settings, vertical, label string, spec openAICoreSpec, cfg *config.ProviderConfig, caller core.Caller,
	catalog openAICatalog, anonymousEntry bool,
) (*openAICompatibleProvider, error) {
	apiKey, observation, err := resolveAPIKeyObserved(settings, spec.instance, cfg, caller)
	if err != nil {
		return nil, err
	}
	declared, err := spec.bind(rt, caller)
	if err != nil {
		return nil, err
	}
	catalog.header, catalog.observation = openAIHeader(apiKey), observation
	catalog.anonymous = anonymousEntry && AnonymousAPIKey(apiKey)
	return &openAICompatibleProvider{
		runtime: rt, instance: spec.instance, caller: caller, vertical: vertical, label: label,
		forceAdapt: cfg.ForceApiSupport, openAIEntry: spec.config.RegistryID == "openai",
		catalog: catalog, declared: declared,
	}, nil
}

func (p *openAICompatibleProvider) IsStub() bool { return false }

// PreservesWireNativeSurface is core's declaration for the facade's caller:
// Chat is forwarded as sent unless the configured adaptation serves the
// model over Responses, and Responses is forwarded for a model whose cached
// row lists it, or for every model of the openai entry.
func (p *openAICompatibleProvider) PreservesWireNativeSurface(model string, surface core.ModelSurface) bool {
	return p.declared != nil && core.PreservesWire(p.declared, model, surface)
}

func (p *openAICompatibleProvider) ListModels() []ModelInfo {
	models, _, _ := p.ListModelsWithError()
	return models
}

// ListModelsWithError lists the catalog on the gateway's path with the
// factory's key, as the transport listed it.
func (p *openAICompatibleProvider) ListModelsWithError() ([]ModelInfo, *CredentialObservation, error) {
	return p.catalog.list()
}

// httpTarget is where the gateway proxies the instance's other endpoints,
// and how it authenticates them, as the transport did.
func (p *openAICompatibleProvider) httpTarget() (string, http.Header) {
	return p.catalog.base, p.catalog.header.Clone()
}

func (p *openAICompatibleProvider) Complete(model string, messages []Message, kw Kwargs) (map[string]any, error) {
	return p.CompleteContext(context.Background(), model, messages, kw)
}

func (p *openAICompatibleProvider) CompleteContext(ctx context.Context, model string, messages []Message, kw Kwargs) (map[string]any, error) {
	response, _, err := p.CompleteContextWithObservation(ctx, model, messages, kw)
	return response, err
}

func (p *openAICompatibleProvider) CompleteWithObservation(
	model string, messages []Message, kw Kwargs,
) (map[string]any, *CredentialObservation, error) {
	return p.CompleteContextWithObservation(context.Background(), model, messages, kw)
}

// CompleteContextWithObservation serves Chat. For a model core serves over
// Responses, core marks the answer as the transport marked it, which the API
// layer reports in the adapted header.
func (p *openAICompatibleProvider) CompleteContextWithObservation(
	ctx context.Context, model string, messages []Message, kw Kwargs,
) (map[string]any, *CredentialObservation, error) {
	request, operation, err := p.chat(model, messages, kw, openAIChatCompletion)
	if err != nil {
		return nil, p.catalog.observation, err
	}
	response, err := p.invoke(ctx, request, operation)
	return response, p.catalog.observation, err
}

func (p *openAICompatibleProvider) Stream(model string, messages []Message, kw Kwargs) (StreamIter, error) {
	return p.StreamContext(context.Background(), model, messages, kw)
}

func (p *openAICompatibleProvider) StreamContext(ctx context.Context, model string, messages []Message, kw Kwargs) (StreamIter, error) {
	request, operation, err := p.chat(model, messages, kw, openAIChatStream)
	if err != nil {
		return nil, err
	}
	return p.stream(ctx, request, operation)
}

// chat returns the core request of a Chat call: the options with the model,
// the messages and force_api_support, which turns core's adaptation on or
// off for the request as the transport's plan was, so that core also marks
// an answer it serves over Responses, as the transport marked every one.
// The operation is native's, or Chat over Responses' when the plan serves
// the model there, which it reads from the catalog it refreshes when stale.
func (p *openAICompatibleProvider) chat(
	model string, messages []Message, kw Kwargs, native openAIOperation,
) (core.Request, openAIOperation, error) {
	adapt := adaptEnabled(kw, p.forceAdapt)
	payload := make(map[string]any, len(kw)+3)
	for key, value := range kw {
		payload[key] = value
	}
	payload["model"], payload["messages"], payload["force_api_support"] = model, messages, adapt
	request, err := openAIRequest(core.ModelSurfaceChatCompletions, model, payload)
	if adapt && p.overResponses(model) {
		return request, openAIChatOverResponses, err
	}
	return request, native, err
}

// overResponses reports a model whose catalog row, which this refreshes when
// stale as the transport's plan did, lists Responses but not Chat
// Completions: one adaptation serves over Responses. Core reads the row the
// refresh cached.
func (p *openAICompatibleProvider) overResponses(model string) bool {
	row, ok := p.runtime.CatalogLookupForPrincipal(p.instance, model, p.caller)
	return ok && translate.PreferredEndpoint(row.SupportedSurfaces) == "responses"
}

func (p *openAICompatibleProvider) CompleteResponses(
	model string, payload map[string]any,
) (map[string]any, *CredentialObservation, error) {
	return p.CompleteResponsesContext(context.Background(), model, payload)
}

// CompleteResponsesContext refuses a model without native Responses with
// ErrResponsesUnsupported before anything is sent, on which the router
// serves the request over Chat, and so does a Responses endpoint the
// upstream answers 404 or 405.
func (p *openAICompatibleProvider) CompleteResponsesContext(
	ctx context.Context, model string, payload map[string]any,
) (map[string]any, *CredentialObservation, error) {
	if !p.nativeResponses(model) {
		return nil, nil, ErrResponsesUnsupported
	}
	request, err := openAIRequest(core.ModelSurfaceResponses, model, payload)
	if err != nil {
		return nil, p.catalog.observation, err
	}
	response, err := p.invoke(ctx, request, openAIResponsesCompletion)
	return response, p.catalog.observation, err
}

func (p *openAICompatibleProvider) StreamResponses(
	model string, payload map[string]any,
) (StreamIter, *CredentialObservation, error) {
	return p.StreamResponsesContext(context.Background(), model, payload)
}

func (p *openAICompatibleProvider) StreamResponsesContext(
	ctx context.Context, model string, payload map[string]any,
) (StreamIter, *CredentialObservation, error) {
	if !p.nativeResponses(model) {
		return nil, nil, ErrResponsesUnsupported
	}
	request, err := openAIRequest(core.ModelSurfaceResponses, model, payload)
	if err != nil {
		return nil, p.catalog.observation, err
	}
	stream, err := p.stream(ctx, request, openAIResponsesStream)
	return stream, p.catalog.observation, err
}

// nativeResponses reports a model with native Responses as the transport
// read one: every model of the openai entry, and a model whose catalog row,
// which this refreshes when stale, lists Responses. Core reads the row the
// refresh cached.
func (p *openAICompatibleProvider) nativeResponses(model string) bool {
	if p.openAIEntry {
		return true
	}
	row, ok := p.runtime.CatalogLookupForPrincipal(p.instance, model, p.caller)
	return ok && listsResponses(row.SupportedSurfaces)
}

// openAIRequest is the core request of payload on surface. Core decodes the
// body and encodes what it sends as the transport encoded a body, so the
// upstream request is unchanged.
func openAIRequest(surface core.ModelSurface, model string, payload map[string]any) (core.Request, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return core.Request{}, invocation("openai: request encoding failed")
	}
	return core.Request{Surface: surface, Model: model, Body: body, ContentType: core.ContentTypeJSON}, nil
}
