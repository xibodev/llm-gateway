package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"llmgw/internal/config"

	"github.com/xibodev/llm-provider-auth/tokenstore"
	core "github.com/xibodev/llmgw-core"
	coreproviders "github.com/xibodev/llmgw-core/providers"
)

// googleProvider is the gateway's Google AI Studio and Vertex AI facade.
// Chat and embeddings go through the core Runtime, which resolves the
// caller's credential through Google's store on each request, and core's
// Google, which shapes each request and reads each answer as the gateway's
// transport did. Image generation calls core's Google too, with a credential
// resolved the same way, because the Runtime has no image operation.
//
// Video stays on the gateway's transport, which core does not serve yet, and
// so does the catalog: the gateway's cache, routing and model list read its
// rows as the transport reports them, and core's catalog infers typed
// capabilities for the same models, which would change what they read. Both
// use the credential the provider factory resolved when it built the facade,
// as the transport did.
type googleProvider struct {
	runtime  *Runtime
	instance string
	caller   core.Caller
	// images is the core Google that generates images. The Runtime holds its
	// own for chat and embeddings, so the two keep separate token caches.
	images *coreproviders.Google
	// legacy is the gateway's transport, with the factory's credential.
	legacy GoogleAIProvider
}

var _ Provider = (*googleProvider)(nil)

// newGoogleProvider returns the facade of instance, configured as cfg, for
// caller, over legacy, the transport the factory built with the credential
// it resolved. A configuration core's Google cannot run fails here.
func (rt *Runtime) newGoogleProvider(
	instance string, cfg *config.ProviderConfig, caller core.Caller, legacy GoogleAIProvider,
) (Provider, error) {
	images, err := newCoreGoogle(instance, cfg)
	if err != nil {
		return nil, err
	}
	return &googleProvider{runtime: rt, instance: instance, caller: caller, images: images, legacy: legacy}, nil
}

func (p *googleProvider) IsStub() bool { return false }

// label names the deployment in errors, as the transport named it.
func (p *googleProvider) label() string { return p.legacy.label() }

// googleModelRequired refuses a blank model as the transport's model URL
// did, as a configuration problem found before anything is resolved or
// sent.
func googleModelRequired(model string) error {
	if strings.TrimPrefix(strings.TrimSpace(model), "models/") == "" {
		return &ConfigError{Msg: "a model name is required"}
	}
	return nil
}

func (p *googleProvider) Complete(model string, messages []Message, kw Kwargs) (map[string]any, error) {
	return p.CompleteContext(context.Background(), model, messages, kw)
}

// CompleteContext sends the Chat body core's Google maps, which holds what
// the transport mapped: the messages, max_tokens or, when that is unset, the
// internal _max_output_tokens, and temperature. Core's Google maps it to
// generateContent as the transport did, so the upstream request is unchanged.
func (p *googleProvider) CompleteContext(ctx context.Context, model string, messages []Message, kw Kwargs) (map[string]any, error) {
	if err := googleModelRequired(model); err != nil {
		return nil, err
	}
	payload := map[string]any{"messages": messages}
	maxTokens := kw["max_tokens"]
	if maxTokens == nil {
		maxTokens = kw["_max_output_tokens"]
	}
	if maxTokens != nil {
		payload["max_tokens"] = maxTokens
	}
	if temperature := kw["temperature"]; temperature != nil {
		payload["temperature"] = temperature
	}
	return p.invoke(ctx, core.ModelSurfaceChatCompletions, model, payload)
}

// Stream refuses before anything is resolved or sent, as the transport did.
func (p *googleProvider) Stream(string, []Message, Kwargs) (StreamIter, error) {
	return nil, &ConfigError{Msg: p.label() + ": streaming is not implemented for this provider yet; use a non-streaming request"}
}

// Embed sends the embeddings body core's Google reads. It embeds each input
// with one upstream call, in order, as the transport did.
func (p *googleProvider) Embed(ctx context.Context, model string, input any) (map[string]any, error) {
	if err := googleModelRequired(model); err != nil {
		return nil, err
	}
	return p.invoke(ctx, core.ModelSurfaceEmbeddings, model, map[string]any{"model": model, "input": input})
}

// invoke sends payload on surface through the core Runtime and returns the
// answer core's Google shaped as the transport shaped it.
func (p *googleProvider) invoke(ctx context.Context, surface core.ModelSurface, model string, payload map[string]any) (map[string]any, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	ctx = withCoreOperation(ctx, googleCoreType, p.caller)
	response, err := p.runtime.core.Invoke(ctx, p.caller, p.instance, core.Request{
		Surface: surface, Model: model, Body: body, ContentType: core.ContentTypeJSON,
	})
	if err != nil {
		return nil, p.failure(err)
	}
	var result map[string]any
	if err := json.Unmarshal(response.Body, &result); err != nil {
		return nil, circuitFailureInvocation(p.label() + ": invalid JSON in upstream response")
	}
	return result, nil
}

func (p *googleProvider) GenerateImages(model, prompt string, count int) ([]GeneratedImage, map[string]any, error) {
	return p.GenerateImagesContext(context.Background(), model, prompt, count)
}

// GenerateImagesContext generates images with core's Google. The transport
// refused a blank prompt, then a blank model, before anything was sent, and
// so does the facade, before it resolves a credential.
func (p *googleProvider) GenerateImagesContext(ctx context.Context, model, prompt string, count int) ([]GeneratedImage, map[string]any, error) {
	if strings.TrimSpace(prompt) == "" {
		return nil, nil, &InvocationError{Msg: p.label() + ": a prompt is required"}
	}
	if err := googleModelRequired(model); err != nil {
		return nil, nil, err
	}
	credential, err := p.credential(ctx)
	if err != nil {
		return nil, nil, err
	}
	result, err := p.images.GenerateImages(ctx, core.GenerateImagesRequest{Model: model, Prompt: prompt, Count: count}, credential)
	if err != nil {
		return nil, nil, p.failure(err)
	}
	images := make([]GeneratedImage, len(result.Images))
	for index, image := range result.Images {
		images[index] = GeneratedImage{Data: image.Data, MimeType: image.MimeType}
	}
	return images, result.Usage, nil
}

// credential resolves the credential of an operation the core Runtime does
// not perform, as the Runtime resolves one for this instance: through
// Google's store, with none when none resolves. Neither kind Google takes
// refreshes, so the record is the one the store loads.
func (p *googleProvider) credential(ctx context.Context) (*core.Credential, error) {
	store := p.runtime.verticals[googleCoreType].credentials
	key, err := store.Resolve(ctx, p.caller, p.instance)
	if errors.Is(err, core.ErrNoCredential) {
		return nil, nil
	}
	var record tokenstore.Record
	if err == nil {
		record, err = store.Load(ctx, key)
	}
	if err != nil {
		return nil, p.credentialFailure(err)
	}
	return core.CredentialFromRecord(key, record), nil
}

// credentialFailure is the error of a credential the store could not hand
// out: its own refusal, the operation's context error, or otherwise a load
// failure, which the provider factory reported as a configuration error.
func (p *googleProvider) credentialFailure(err error) error {
	var configErr *ConfigError
	if errors.As(err, &configErr) {
		return configErr
	}
	if isContextError(err) {
		return err
	}
	return &ConfigError{Msg: fmt.Sprintf("provider '%s': load credential: %v", p.instance, err)}
}

// The transport serves video and the catalog, with the factory's credential.

func (p *googleProvider) StartVideo(model, prompt string, parameters map[string]any) (VideoJob, error) {
	return p.legacy.StartVideo(model, prompt, parameters)
}

func (p *googleProvider) StartVideoContext(ctx context.Context, model, prompt string, parameters map[string]any) (VideoJob, error) {
	return p.legacy.StartVideoContext(ctx, model, prompt, parameters)
}

func (p *googleProvider) PollVideo(operation string) (VideoJob, error) {
	return p.legacy.PollVideo(operation)
}

func (p *googleProvider) PollVideoContext(ctx context.Context, operation string) (VideoJob, error) {
	return p.legacy.PollVideoContext(ctx, operation)
}

func (p *googleProvider) ListModels() []ModelInfo { return p.legacy.ListModels() }

func (p *googleProvider) ListModelsWithError() ([]ModelInfo, *CredentialObservation, error) {
	return p.legacy.ListModelsWithError()
}

// failure returns the error the transport returned for what core returned,
// so the router, the resilience wrapper and the API layer decide on it as
// they did, and a client reads the message it read.
//
// A refusal from Google, and an answer without what was asked for, carries
// the transport's message in the cause's Msg. The transport's error held the
// status alone: it sent no Retry-After and had no retry, failover or circuit
// reading of its own, so neither does this one, whatever core reads. A
// request that got no complete answer may repeat, and an answer that is not
// the JSON it should be counts against the provider, each under the
// transport's message. What core refuses before sending anything is a
// configuration error, as the transport's refusals were.
func (p *googleProvider) failure(err error) error {
	var configErr *ConfigError
	if errors.As(err, &configErr) {
		return configErr
	}
	var upstream *coreproviders.InvocationError
	if errors.As(err, &upstream) {
		if upstream.Cause != nil {
			// Only a failed service-account token exchange keeps a cause.
			return vertexTokenFailure(p.instance, upstream.Cause)
		}
		return invocationStatus(upstream.Msg, upstream.Status)
	}
	var failure *core.ProviderError
	if !errors.As(err, &failure) {
		// No provider reported it: the operation's context ended before
		// anything was sent.
		return err
	}
	label := p.label()
	switch failure.Class {
	case core.ProviderErrorTransport:
		// Core names what could not be done; the transport quoted why.
		detail := failure.Message
		if failure.Cause != nil {
			detail = failure.Cause.Error()
		}
		if failure.Message == "the "+label+" response could not be read" {
			return retryableInvocation(label + ": response body transport error: " + detail)
		}
		return retryableInvocation(label + ": " + detail)
	case core.ProviderErrorUpstream:
		// Core words the size limit its own way, and the rest as the
		// transport did.
		if failure.Message == "the "+label+" response exceeds the size limit" {
			return circuitFailureInvocation(label + ": response body exceeded the size limit")
		}
		return circuitFailureInvocation(failure.Message)
	case core.ProviderErrorAuth:
		// The store could not hand the Runtime a credential.
		return p.credentialFailure(failure.Cause)
	}
	return &ConfigError{Msg: failure.Message}
}
