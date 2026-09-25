package providers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/xibodev/llm-provider-auth/tokenstore"
	core "github.com/xibodev/llmgw-core"
	coreproviders "github.com/xibodev/llmgw-core/providers"
)

// antigravityEndpoint replaces Google's Cloud Code Assist endpoint of the
// gateway's Antigravity calls. An empty BaseURL keeps Google's, and a nil
// HTTPClient keeps a client that times out after 120 seconds.
type antigravityEndpoint struct {
	BaseURL    string
	HTTPClient *http.Client
}

// antigravityProvider is the gateway's Antigravity facade. Chat goes through
// the core Runtime, which resolves the caller's own connection, keeps its
// token fresh and replays once a request whose token the upstream rejected.
// Image generation and the catalog stay on the gateway's path, because the
// Runtime serves neither: they call core's Antigravity themselves, with a
// credential from the same store refreshed by the same rules, and recover
// from a rejected token the same way; see authorized.
type antigravityProvider struct {
	runtime  *Runtime
	instance string
	caller   core.Caller
	// antigravity is core's Antigravity, used for images and the catalog.
	antigravity *coreproviders.Antigravity
}

// newAntigravityProvider returns the facade of instance for caller.
func (rt *Runtime) newAntigravityProvider(instance string, caller core.Caller) (Provider, error) {
	if strings.TrimSpace(callerPrincipalID(caller)) == "" {
		return nil, &ConfigError{Msg: "google_antigravity: a human principal private connection is required"}
	}
	antigravity, err := rt.newCoreAntigravity()
	if err != nil {
		return nil, err
	}
	return &antigravityProvider{runtime: rt, instance: instance, caller: caller, antigravity: antigravity}, nil
}

// call returns ctx carrying the oauthCall of one operation of this facade.
func (p *antigravityProvider) call(ctx context.Context) (context.Context, *oauthCall) {
	return withOAuthCall(ctx, antigravityVertical, callerPrincipalID(p.caller), p.instance)
}

func (p *antigravityProvider) IsStub() bool { return false }

func (p *antigravityProvider) Complete(model string, messages []Message, kw Kwargs) (map[string]any, error) {
	return p.CompleteContext(context.Background(), model, messages, kw)
}

func (p *antigravityProvider) CompleteWithObservation(
	model string, messages []Message, kw Kwargs,
) (map[string]any, *CredentialObservation, error) {
	return p.CompleteContextWithObservation(context.Background(), model, messages, kw)
}

// CompleteContext sends the body the Antigravity path handed the legacy
// adapter through the core Runtime: the messages and, when set, max_tokens,
// temperature and tools. Core's Antigravity maps it for Cloud Code Assist as
// that adapter did, so the upstream request is unchanged.
func (p *antigravityProvider) CompleteContext(ctx context.Context, model string, messages []Message, kw Kwargs) (map[string]any, error) {
	payload := map[string]any{"messages": messages}
	for _, key := range []string{"max_tokens", "temperature", "tools"} {
		if value := kw[key]; value != nil {
			payload[key] = value
		}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, invocation("google_antigravity: completion failed")
	}
	ctx, call := p.call(ctx)
	response, err := p.runtime.core.Invoke(ctx, p.caller, p.instance, core.Request{
		Surface: core.ModelSurfaceChatCompletions, Model: model, Body: body, ContentType: core.ContentTypeJSON,
	})
	if err != nil {
		return nil, antigravityInvokeFailure(call, err)
	}
	var result map[string]any
	if err := json.Unmarshal(response.Body, &result); err != nil {
		return nil, invocation("google_antigravity: completion failed")
	}
	return result, nil
}

// CompleteContextWithObservation also reports the credential the request
// used; see credentialCollector.
func (p *antigravityProvider) CompleteContextWithObservation(
	ctx context.Context, model string, messages []Message, kw Kwargs,
) (map[string]any, *CredentialObservation, error) {
	ctx, collector := collectCredentials(ctx)
	response, err := p.CompleteContext(ctx, model, messages, kw)
	return response, collector.Observation(), err
}

func (p *antigravityProvider) Stream(string, []Message, Kwargs) (StreamIter, error) {
	return nil, &ConfigError{Msg: "google_antigravity: streaming is not supported"}
}

func (p *antigravityProvider) GenerateImages(model, prompt string, count int) ([]GeneratedImage, map[string]any, error) {
	return p.GenerateImagesContext(context.Background(), model, prompt, count)
}

// GenerateImagesContext generates images on the gateway's path. Core refuses
// a request it cannot serve before it reads the credential, so asking first
// without one sends nothing and refuses such a request before a credential is
// resolved or refreshed for it, as the Antigravity path did.
func (p *antigravityProvider) GenerateImagesContext(ctx context.Context, model, prompt string, count int) ([]GeneratedImage, map[string]any, error) {
	request := core.GenerateImagesRequest{Model: model, Prompt: prompt, Count: count}
	var refusal *core.ProviderError
	if _, err := p.antigravity.GenerateImages(ctx, request, nil); errors.As(err, &refusal) && refusal.Class == core.ProviderErrorInvalidRequest {
		return nil, nil, adaptAntigravityError("image generation", err)
	}
	var result core.GenerateImagesResult
	err := p.authorized(ctx, func(ctx context.Context, credential *core.Credential) (err error) {
		result, err = p.antigravity.GenerateImages(ctx, request, credential)
		return err
	})
	if err != nil {
		return nil, nil, adaptAntigravityError("image generation", err)
	}
	images := make([]GeneratedImage, len(result.Images))
	for index, image := range result.Images {
		images[index] = GeneratedImage{Data: image.Data, MimeType: image.MimeType}
	}
	return images, result.Usage, nil
}

func (p *antigravityProvider) ListModels() []ModelInfo {
	models, _, _ := p.ListModelsWithError()
	return models
}

// ListModelsWithError lists the catalog on the gateway's path and reports the
// credential it used; see credentialCollector.
func (p *antigravityProvider) ListModelsWithError() ([]ModelInfo, *CredentialObservation, error) {
	ctx, collector := collectCredentials(context.Background())
	var models []core.ModelInfo
	err := p.authorized(ctx, func(ctx context.Context, credential *core.Credential) (err error) {
		models, err = p.antigravity.ListModels(ctx, credential)
		return err
	})
	if err != nil {
		return nil, collector.Observation(), adaptAntigravityCatalogError(err)
	}
	rows := make([]ModelInfo, 0, len(models))
	for _, model := range models {
		capabilities := model.Capabilities
		surfaces := []string{"/v1/chat/completions"}
		if capabilities != nil {
			capabilities.Streaming = core.SupportUnsupported
			if capabilities.Operations.Image == core.SupportSupported {
				surfaces = append(surfaces, "/v1/images/generations")
			}
		}
		rows = append(rows, ModelInfo{
			ID: model.ID, Vendor: model.OwnedBy, Label: model.Description,
			TypedCapabilities: capabilities, SupportedSurfaces: surfaces,
		})
	}
	return rows, collector.Observation(), nil
}

// authorized performs operation the way the core Runtime performs one: with
// the caller's connection, resolved through the Runtime's credential store and
// kept fresh by a Coordinator over it, and once more with a refreshed token
// when the upstream rejects the first. A refresh that fails then is returned
// in place of the rejection, as the Antigravity path returned its failed
// recovery.
func (p *antigravityProvider) authorized(ctx context.Context, operation func(context.Context, *core.Credential) error) error {
	ctx, _ = p.call(ctx)
	coordinator, err := p.runtime.antigravityCoordinator(p.instance)
	var key string
	if err == nil {
		key, err = p.runtime.credentials.Resolve(ctx, p.caller, p.instance)
	}
	var record tokenstore.Record
	if err == nil {
		record, err = coordinator.Token(ctx, key)
	}
	if err != nil {
		return err
	}
	if err = operation(ctx, core.CredentialFromRecord(key, record)); !antigravityRejected(err) {
		return err
	}
	if record, err = coordinator.Rejected(ctx, key, record); err != nil {
		return err
	}
	return operation(ctx, core.CredentialFromRecord(key, record))
}

var _ Provider = (*antigravityProvider)(nil)

// antigravityInvokeFailure returns the error the Antigravity path returned
// for err, which the core Runtime returned for a completion. After an
// upstream 401 the Runtime refreshes once and replays; when the refresh
// fails, it returns the 401 without a replay. The Antigravity path reported
// a failed recovery as a failed completion without a status, whatever
// failed, so a 401 the Runtime did not replay is reported that way.
func antigravityInvokeFailure(call *oauthCall, err error) error {
	if antigravityRejected(err) && !call.replayed() {
		return invocation("google_antigravity: completion failed")
	}
	return adaptAntigravityError("completion", err)
}

// antigravityRejected reports an upstream 401, from which the Antigravity
// path recovers with one refresh.
func antigravityRejected(err error) bool {
	var upstream *core.ProviderOperationError
	return errors.As(err, &upstream) && upstream.Failure.StatusCode == http.StatusUnauthorized
}

// adaptAntigravityError maps an Antigravity failure to the gateway error the
// Antigravity path returned: an upstream response keeps its status and
// Retry-After, and any other failure, the credential's included, is the
// operation's failure without a status.
func adaptAntigravityError(operation string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, coreproviders.ErrExperimentalAntigravityStreamingUnsupported) {
		return &ConfigError{Msg: "google_antigravity: streaming is not supported"}
	}
	var upstream *core.ProviderOperationError
	if errors.As(err, &upstream) {
		return invocationStatusRetryAfter("google_antigravity: "+operation+" failed", upstream.Failure.StatusCode, upstream.Failure.RetryAfter)
	}
	return invocation("google_antigravity: " + operation + " failed")
}

func adaptAntigravityCatalogError(err error) error {
	if err == nil {
		return nil
	}
	var upstream *core.ProviderOperationError
	if errors.As(err, &upstream) {
		return catalogError("catalog_failed", "Antigravity model discovery failed.", upstream.Failure.StatusCode)
	}
	return catalogError("catalog_failed", "Antigravity model discovery failed.", 0)
}
