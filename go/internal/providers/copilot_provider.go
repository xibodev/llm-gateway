package providers

import (
	"context"
	"encoding/json"
	"net/http"

	"llmgw/internal/config"

	core "github.com/xibodev/llmgw-core"
)

// copilotProvider is the gateway's GitHub Copilot facade. Chat, Responses and
// both streams go through the core Runtime, which resolves the caller's GitHub
// token through Copilot's store on each request, and core's Copilot, which
// exchanges it for a session and shapes each request and reads each response
// as the OpenAI transport did. What the gateway did above that transport
// stays here: the adaptation switch each request carries, the Chat fallback
// of a model without native Responses, and the relay of stream records as the
// data events the API layer reads. The catalog and the endpoints the gateway
// proxies stay on the gateway's path: core's catalog rows carry none of the
// untyped capabilities /v1/models presents, and core proxies nothing.
type copilotProvider struct {
	runtime    *Runtime
	instance   string
	caller     core.Caller
	timeout    float64
	forceAdapt bool
}

var _ Provider = (*copilotProvider)(nil)

// newCopilotProvider returns the facade of the Copilot instance for caller,
// whose catalog requests time out after timeout seconds.
func (rt *Runtime) newCopilotProvider(
	instance string, cfg *config.ProviderConfig, caller core.Caller, timeout float64,
) *copilotProvider {
	return &copilotProvider{
		runtime: rt, instance: instance, caller: caller, timeout: timeout, forceAdapt: copilotForceAdapt(cfg),
	}
}

func (p *copilotProvider) IsStub() bool { return false }

// PreservesWireNativeSurface declares Chat and Responses as the OpenAI
// transport declared them for Copilot.
func (p *copilotProvider) PreservesWireNativeSurface(_ string, surface core.ModelSurface) bool {
	return surface == core.ModelSurfaceChatCompletions || surface == core.ModelSurfaceResponses
}

func (p *copilotProvider) Complete(model string, messages []Message, kw Kwargs) (map[string]any, error) {
	return p.CompleteContext(context.Background(), model, messages, kw)
}

func (p *copilotProvider) CompleteContext(ctx context.Context, model string, messages []Message, kw Kwargs) (map[string]any, error) {
	response, _, err := p.CompleteContextWithObservation(ctx, model, messages, kw)
	return response, err
}

func (p *copilotProvider) CompleteWithObservation(
	model string, messages []Message, kw Kwargs,
) (map[string]any, *CredentialObservation, error) {
	return p.CompleteContextWithObservation(context.Background(), model, messages, kw)
}

// CompleteContextWithObservation serves Chat. For a model core serves over
// Responses, core marks the answer as the transport marked it, which the API
// layer reports in the adapted header.
func (p *copilotProvider) CompleteContextWithObservation(
	ctx context.Context, model string, messages []Message, kw Kwargs,
) (map[string]any, *CredentialObservation, error) {
	ctx, collector := collectCredentials(ctx)
	ctx, request, err := p.chat(ctx, model, messages, kw, false)
	if err != nil {
		return nil, nil, err
	}
	response, err := p.invoke(ctx, request)
	return response, copilotObservation(collector), err
}

func (p *copilotProvider) Stream(model string, messages []Message, kw Kwargs) (StreamIter, error) {
	return p.StreamContext(context.Background(), model, messages, kw)
}

func (p *copilotProvider) StreamContext(ctx context.Context, model string, messages []Message, kw Kwargs) (StreamIter, error) {
	ctx, request, err := p.chat(ctx, model, messages, kw, true)
	if err != nil {
		return nil, err
	}
	return p.stream(ctx, request)
}

// chat returns the Chat request of the payload the OpenAI transport built,
// with force_api_support, which turns core's adaptation on or off for the
// request as the transport's plan was, and ctx naming what its body cannot
// say; see copilotChat. core renames max_tokens for a reasoning model and
// serves a Responses-only model over Responses, as the plan did.
func (p *copilotProvider) chat(
	ctx context.Context, model string, messages []Message, kw Kwargs, stream bool,
) (context.Context, core.Request, error) {
	kw = withOpenAIOutputLimit(kw)
	adapt := adaptEnabled(kw, p.forceAdapt)
	payload := buildOpenAIPayload(model, messages, stream, kw)
	fields := copilotDroppedFields(payload)
	payload["force_api_support"] = adapt
	request, err := copilotRequest(core.ModelSurfaceChatCompletions, model, payload)
	if err != nil {
		return nil, core.Request{}, err
	}
	return withCopilotChat(ctx, copilotChat{adapt: adapt, fields: fields, refuse: func() error {
		conversion := chatToResponsesWithReport(model, messages, kw)
		if err := RejectMaterialLossExceptThoughtSignatures(conversion.Report); err != nil {
			return &ConfigError{Msg: err.Error()}
		}
		return nil
	}}), request, nil
}

func (p *copilotProvider) CompleteResponses(
	model string, payload map[string]any,
) (map[string]any, *CredentialObservation, error) {
	return p.CompleteResponsesContext(context.Background(), model, payload)
}

// CompleteResponsesContext refuses a model without native Responses with
// ErrResponsesUnsupported before anything is sent, on which the router serves
// the request over Chat, and so does a Responses endpoint Copilot answers 404
// or 405. core routes by the catalog the provider lists on the request path,
// as the transport routed by the catalog it fetched there.
func (p *copilotProvider) CompleteResponsesContext(
	ctx context.Context, model string, payload map[string]any,
) (map[string]any, *CredentialObservation, error) {
	ctx, collector := collectCredentials(ctx)
	request, err := copilotRequest(core.ModelSurfaceResponses, model, payload)
	if err != nil {
		return nil, nil, err
	}
	response, err := p.invoke(ctx, request)
	return response, copilotObservation(collector), err
}

func (p *copilotProvider) StreamResponses(
	model string, payload map[string]any,
) (StreamIter, *CredentialObservation, error) {
	return p.StreamResponsesContext(context.Background(), model, payload)
}

func (p *copilotProvider) StreamResponsesContext(
	ctx context.Context, model string, payload map[string]any,
) (StreamIter, *CredentialObservation, error) {
	ctx, collector := collectCredentials(ctx)
	request, err := copilotRequest(core.ModelSurfaceResponses, model, payload)
	if err != nil {
		return nil, nil, err
	}
	stream, err := p.stream(ctx, request)
	return stream, copilotObservation(collector), err
}

func (p *copilotProvider) ListModels() []ModelInfo {
	models, _, _ := p.ListModelsWithError()
	return models
}

// ListModelsWithError lists the catalog on the gateway's path, as the OpenAI
// transport listed it, with the rows /v1/models presents.
func (p *copilotProvider) ListModelsWithError() ([]ModelInfo, *CredentialObservation, error) {
	catalog := OpenAIProvider{auth: copilotAuth{providerID: p.instance, caller: p.caller}, Timeout: p.timeout}
	return catalog.ListModelsWithError()
}

// httpTarget is where the gateway proxies a Copilot instance's other
// endpoints, and how it authenticates them, as the transport did.
func (p *copilotProvider) httpTarget() (string, http.Header, bool) {
	base, headers, err := copilotAuth{providerID: p.instance, caller: p.caller}.Prepare()
	return base, headers, err == nil
}

func copilotRequest(surface core.ModelSurface, model string, payload map[string]any) (core.Request, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return core.Request{}, invocation("github_copilot: request encoding failed")
	}
	return core.Request{Surface: surface, Model: model, Body: body, ContentType: core.ContentTypeJSON}, nil
}

// invoke sends request through the core Runtime, naming Copilot for the
// credential store. Core's Copilot re-encodes the body as the transport
// encoded one.
func (p *copilotProvider) invoke(ctx context.Context, request core.Request) (map[string]any, error) {
	ctx = withCoreOperation(ctx, copilotCoreType, p.caller)
	response, err := p.runtime.core.Invoke(ctx, p.caller, p.instance, request)
	if err != nil {
		return nil, p.failure(ctx, err)
	}
	var result map[string]any
	if err := json.Unmarshal(response.Body, &result); err != nil {
		return nil, circuitFailureInvocation("github_copilot: invalid JSON in upstream response")
	}
	return result, nil
}

func (p *copilotProvider) stream(ctx context.Context, request core.Request) (StreamIter, error) {
	ctx = withCoreOperation(ctx, copilotCoreType, p.caller)
	stream, err := p.runtime.core.Stream(ctx, p.caller, p.instance, request)
	if err != nil {
		return nil, p.failure(ctx, err)
	}
	return &copilotCoreStream{provider: p, ctx: ctx, inner: stream}, nil
}

// copilotObservation is the credential an operation used, as the resolver
// reported it: a personal connection that has account state, and none for a
// legacy, bound or gateway-wide credential, whose revision is 0.
func copilotObservation(collector *credentialCollector) *CredentialObservation {
	observation := collector.Observation()
	if observation == nil || observation.CredentialRevision <= 0 {
		return nil
	}
	return observation
}
