package providers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"llmgw/internal/config"

	copilotauth "github.com/xibodev/llm-provider-auth/copilot"
	"github.com/xibodev/llm-provider-auth/tokenstore"
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
	payload["force_api_support"] = adapt
	request, err := copilotRequest(core.ModelSurfaceChatCompletions, model, payload)
	if err != nil {
		return nil, core.Request{}, err
	}
	return withCopilotChat(ctx, copilotChat{adapt: adapt, refuse: func() error {
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
// transport listed it: Copilot's /models with a session for the caller, read
// into the rows /v1/models presents, and once more with a new session when
// Copilot rejects the first. A session that could not be exchanged fails
// without a status, as it did.
func (p *copilotProvider) ListModelsWithError() ([]ModelInfo, *CredentialObservation, error) {
	ctx, collector := collectCredentials(context.Background())
	target, err := p.target(ctx, false)
	if err != nil {
		return nil, copilotObservation(collector), catalogError(
			"catalog_authentication_failed", "Provider authentication failed before catalog access.", 0,
		)
	}
	models, observation, err := OpenAIProvider{auth: target, Timeout: p.timeout}.ListModelsWithError()
	if code, _, status := CatalogFailure(err); code != "catalog_http_error" || status != http.StatusUnauthorized {
		return models, observation, err
	}
	if target, err = p.target(ctx, true); err != nil {
		return nil, observation, catalogError(
			"catalog_refresh_failed", "Provider credential refresh failed.", http.StatusUnauthorized,
		)
	}
	models, observation, err = OpenAIProvider{auth: target, Timeout: p.timeout}.ListModelsWithError()
	if code, _, _ := CatalogFailure(err); code == "catalog_transport_error" {
		err = catalogError(
			"catalog_transport_error", "Provider catalog retry could not reach the upstream service.", 0,
		)
	}
	return models, observation, err
}

// httpTarget is where the gateway proxies a Copilot instance's other
// endpoints, and how it authenticates them, as the transport did.
func (p *copilotProvider) httpTarget() (string, http.Header, bool) {
	target, err := p.target(context.Background(), false)
	return target.base, target.headers, err == nil
}

// copilotTarget is where a Copilot session sends the gateway's own requests:
// the session's API base, with the editor identity the OpenAI transport
// sent, and the credential the session was bought with.
type copilotTarget struct {
	base        string
	headers     http.Header
	observation *CredentialObservation
}

// Prepare and PrepareObserved implement OpenAIAuth for the catalog.
func (t copilotTarget) Prepare() (string, http.Header, error) { return t.base, t.headers.Clone(), nil }

func (t copilotTarget) PrepareObserved() (string, http.Header, *CredentialObservation, error) {
	return t.base, t.headers.Clone(), t.observation, nil
}

// target exchanges the caller's GitHub token, from Copilot's store as the
// Runtime resolves it, on the shared client, or the gateway-wide token for a
// caller without one; force buys a new session. The store refuses a
// principal without a credential, and marks the one it reads used.
func (p *copilotProvider) target(ctx context.Context, force bool) (copilotTarget, error) {
	auth := p.runtime.copilot
	if err := auth.AssertProxyAllowed(); err != nil {
		return copilotTarget{}, err
	}
	store := p.runtime.verticals[copilotCoreType].credentials
	key, err := store.Resolve(ctx, p.caller, p.instance)
	var session *copilotauth.Session
	switch {
	case errors.Is(err, core.ErrNoCredential):
		session, err = auth.GetSession(force)
	case err == nil:
		var record tokenstore.Record
		if record, err = store.Load(ctx, key); err == nil {
			session, err = auth.GetSessionForOAuth(record.AccessToken, force)
		}
	}
	if err != nil {
		return copilotTarget{}, err
	}
	settings := config.Get()
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+session.Token)
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "application/json")
	headers.Set("Copilot-Integration-Id", settings.GithubCopilotIntegrationID)
	headers.Set("Editor-Version", settings.GithubCopilotEditorVersion)
	headers.Set("Editor-Plugin-Version", copilotPluginVersion)
	headers.Set("OpenAI-Intent", "conversation-panel")
	headers.Set("User-Agent", copilotUserAgent)
	return copilotTarget{
		base: session.ChatBaseURL, headers: headers, observation: copilotObservation(credentialCollectorFrom(ctx)),
	}, nil
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
