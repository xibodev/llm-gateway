package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"llmgw/internal/config"

	core "github.com/xibodev/llmgw-core"
	coreproviders "github.com/xibodev/llmgw-core/providers"
)

// zenProvider is the gateway's OpenCode Zen facade. Chat, Responses and both
// streams go through the core Runtime, which resolves the caller's
// credential through Zen's store on each request, and core's Zen, which
// shapes each request and reads each response as the OpenAI-compatible
// transport did. What the gateway did above that transport stays here: the
// forced-adaptation plan, the marker of Chat served over Responses, the Chat
// fallback of a model without native Responses, and the relay of stream
// records as the data events the API layer reads. The catalog stays on the
// gateway's path, because the anonymous catalog needs no vertical: core's
// zen.Client already discovers it there.
type zenProvider struct {
	runtime    *Runtime
	instance   string
	caller     core.Caller
	base       string
	timeout    float64
	forceAdapt bool
	// apiKey and observation are the credential the provider factory
	// resolved when it built the facade, and anonymous says it is none or an
	// anonymous sentinel. The catalog and the declarations follow them, as
	// they followed the credential the transport held; a request uses what
	// the store resolves for it.
	apiKey      string
	anonymous   bool
	observation *CredentialObservation
	// metadataURL replaces the models.dev catalog; only tests set it.
	metadataURL string
	// declared is core's Zen over the caller's catalog. It sends nothing: it
	// says how the operations of this facade are served.
	declared *coreproviders.Zen
}

var _ Provider = (*zenProvider)(nil)

// newZenProvider returns the facade of the Zen instance at base for caller,
// with the credential the factory resolves for an OpenAI-compatible instance.
func (rt *Runtime) newZenProvider(
	instance string, cfg *config.ProviderConfig, caller core.Caller, base string, timeout float64,
) (*zenProvider, error) {
	apiKey, observation, err := resolveAPIKeyObserved(instance, cfg, caller)
	if err != nil {
		return nil, err
	}
	declared, err := rt.newCoreZen(instance, base, caller, nil)
	if err != nil {
		return nil, zenConfigError(instance, err)
	}
	return &zenProvider{
		runtime: rt, instance: instance, caller: caller, base: base, timeout: timeout,
		forceAdapt: cfg.ForceApiSupport, apiKey: apiKey, anonymous: AnonymousAPIKey(apiKey),
		observation: observation, declared: declared,
	}, nil
}

func (p *zenProvider) IsStub() bool { return false }

// PreservesWireNativeSurface is core's declaration for the facade's
// credential: anonymous access reshapes every request, and a key preserves a
// model's one native surface.
func (p *zenProvider) PreservesWireNativeSurface(model string, surface core.ModelSurface) bool {
	return p.declared.PreservesWireFor(&core.Credential{APIKey: p.apiKey}, model, surface)
}

// overResponses reports a model core serves Chat for over Responses: one its
// cached catalog row lists Responses alone for, or, with no row, a Muse model.
func (p *zenProvider) overResponses(model string) bool {
	surfaces := p.declared.NativeSurfaces(model)
	return len(surfaces) > 0 && surfaces[0] == core.ModelSurfaceResponses
}

// nativeResponses reports a model with native Responses as the transport
// read one: core routes Responses for it by the cached catalog, or the
// catalog, which this refreshes when stale, lists Responses for it.
func (p *zenProvider) nativeResponses(model string) bool {
	if slices.Contains(p.declared.NativeSurfaces(model), core.ModelSurfaceResponses) {
		return true
	}
	info, ok := p.runtime.CatalogLookupForPrincipal(p.instance, model, p.caller)
	return ok && listsResponses(info.SupportedSurfaces)
}

// chatPlan applies the forced-adaptation plan the transport applied to a
// Chat request it served natively. The plan reads the catalog, which it
// refreshes when stale, and core routes by what it cached, so a model the
// plan finds Responses-only is served over Responses. chatPlan reports
// whether core serves the request over Responses.
func (p *zenProvider) chatPlan(model string, kw Kwargs) (Kwargs, bool) {
	if !p.overResponses(model) && adaptEnabled(kw, p.forceAdapt) {
		plan := adaptPlanFor(p.runtime.CatalogLookupForPrincipal(p.instance, model, p.caller))
		if plan.renameMaxTokens {
			kw = withRenamedMaxTokens(kw)
		}
	}
	return kw, p.overResponses(model)
}

func (p *zenProvider) ListModels() []ModelInfo {
	models, _, _ := p.ListModelsWithError()
	return models
}

// ListModelsWithError lists the catalog on the gateway's path with the
// factory's credential: without a key the verified anonymous catalog, and
// with one Zen's /models, read as the OpenAI-compatible transport reads a
// catalog.
func (p *zenProvider) ListModelsWithError() ([]ModelInfo, *CredentialObservation, error) {
	base := strings.TrimRight(p.base, "/")
	if p.anonymous {
		models, err := anonymousZenModels(base, p.metadataURL, p.timeout)
		return models, nil, err
	}
	catalog := openAICatalog{base: base, header: openAIHeader(p.apiKey), observation: p.observation, timeout: p.timeout}
	return catalog.list()
}

// httpTarget is where the gateway proxies a Zen instance's other endpoints,
// and how it authenticates them, as the transport did: with the key, or the
// public bearer without one.
func (p *zenProvider) httpTarget() (string, http.Header) {
	key := normalizeBearerKey(p.apiKey)
	if key == "" {
		key = "public"
	}
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	headers.Set("Authorization", "Bearer "+key)
	return strings.TrimRight(p.base, "/"), headers
}

// operation returns ctx carrying the invocation identity every request of
// the operation sends, a new one unless the caller set one, and naming Zen
// and the facade's caller for the store and the provider the core Runtime
// calls.
func (p *zenProvider) operation(ctx context.Context) (context.Context, error) {
	ctx, err := ensureZenInvocation(ctx)
	if err != nil {
		return nil, err
	}
	return withCoreOperation(ctx, zenCoreType, p.caller), nil
}

func (p *zenProvider) Complete(model string, messages []Message, kw Kwargs) (map[string]any, error) {
	return p.CompleteContext(context.Background(), model, messages, kw)
}

func (p *zenProvider) CompleteContext(ctx context.Context, model string, messages []Message, kw Kwargs) (map[string]any, error) {
	response, _, err := p.CompleteContextWithObservation(ctx, model, messages, kw)
	return response, err
}

func (p *zenProvider) CompleteWithObservation(
	model string, messages []Message, kw Kwargs,
) (map[string]any, *CredentialObservation, error) {
	return p.CompleteContextWithObservation(context.Background(), model, messages, kw)
}

// CompleteContextWithObservation marks a completion core served over
// Responses, which the API layer reports in the adapted header.
func (p *zenProvider) CompleteContextWithObservation(
	ctx context.Context, model string, messages []Message, kw Kwargs,
) (map[string]any, *CredentialObservation, error) {
	kw, overResponses := p.chatPlan(model, kw)
	response, err := p.invoke(ctx, core.ModelSurfaceChatCompletions, model, zenChatPayload(model, messages, kw))
	if err != nil {
		return nil, p.observation, err
	}
	if overResponses {
		response["forced_support"] = map[string]any{"req_api": "chat", "resp_api": "responses"}
	}
	return response, p.observation, nil
}

func (p *zenProvider) Stream(model string, messages []Message, kw Kwargs) (StreamIter, error) {
	return p.StreamContext(context.Background(), model, messages, kw)
}

func (p *zenProvider) StreamContext(ctx context.Context, model string, messages []Message, kw Kwargs) (StreamIter, error) {
	kw, _ = p.chatPlan(model, kw)
	return p.stream(ctx, core.ModelSurfaceChatCompletions, model, zenChatPayload(model, messages, kw))
}

func (p *zenProvider) CompleteResponses(
	model string, payload map[string]any,
) (map[string]any, *CredentialObservation, error) {
	return p.CompleteResponsesContext(context.Background(), model, payload)
}

// CompleteResponsesContext refuses a model without native Responses with
// ErrResponsesUnsupported, on which the router serves the request over Chat,
// and so does a Responses endpoint Zen answers 404 or 405.
func (p *zenProvider) CompleteResponsesContext(
	ctx context.Context, model string, payload map[string]any,
) (map[string]any, *CredentialObservation, error) {
	if !p.nativeResponses(model) {
		return nil, nil, ErrResponsesUnsupported
	}
	response, err := p.invoke(ctx, core.ModelSurfaceResponses, model, payload)
	return response, p.observation, responsesFallback(err)
}

func (p *zenProvider) StreamResponses(
	model string, payload map[string]any,
) (StreamIter, *CredentialObservation, error) {
	return p.StreamResponsesContext(context.Background(), model, payload)
}

func (p *zenProvider) StreamResponsesContext(
	ctx context.Context, model string, payload map[string]any,
) (StreamIter, *CredentialObservation, error) {
	if !p.nativeResponses(model) {
		return nil, nil, ErrResponsesUnsupported
	}
	stream, err := p.stream(ctx, core.ModelSurfaceResponses, model, payload)
	return stream, p.observation, responsesFallback(err)
}

// responsesFallback reports a Responses endpoint Zen does not route as
// ErrResponsesUnsupported.
func responsesFallback(err error) error {
	if status := UpstreamStatus(err); status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
		return ErrResponsesUnsupported
	}
	return err
}

// zenChatPayload is the Chat body core's Zen reads: the options, the model
// and the messages.
func zenChatPayload(model string, messages []Message, kw Kwargs) map[string]any {
	payload := make(map[string]any, len(kw)+2)
	for key, value := range kw {
		payload[key] = value
	}
	payload["model"], payload["messages"] = model, messages
	return payload
}

func zenRequest(surface core.ModelSurface, model string, payload map[string]any) (core.Request, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return core.Request{}, invocation("opencode_zen: request encoding failed")
	}
	return core.Request{Surface: surface, Model: model, Body: body, ContentType: core.ContentTypeJSON}, nil
}

// invoke sends payload through the core Runtime. Core's Zen re-encodes it
// as the transport encoded a body, so the upstream request is unchanged.
func (p *zenProvider) invoke(ctx context.Context, surface core.ModelSurface, model string, payload map[string]any) (map[string]any, error) {
	request, err := zenRequest(surface, model, payload)
	if err != nil {
		return nil, err
	}
	if ctx, err = p.operation(ctx); err != nil {
		return nil, err
	}
	response, err := p.runtime.core.Invoke(ctx, p.caller, p.instance, request)
	if err != nil {
		return nil, p.failure(ctx, err)
	}
	var result map[string]any
	if err := json.Unmarshal(response.Body, &result); err != nil {
		return nil, circuitFailureInvocation("opencode_zen: invalid JSON in upstream response")
	}
	return result, nil
}

func (p *zenProvider) stream(ctx context.Context, surface core.ModelSurface, model string, payload map[string]any) (StreamIter, error) {
	request, err := zenRequest(surface, model, payload)
	if err != nil {
		return nil, err
	}
	if ctx, err = p.operation(ctx); err != nil {
		return nil, err
	}
	stream, err := p.runtime.core.Stream(ctx, p.caller, p.instance, request)
	if err != nil {
		return nil, p.failure(ctx, err)
	}
	return &zenCoreStream{provider: p, ctx: ctx, inner: stream}, nil
}
