package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"llmgw/internal/config"

	"github.com/xibodev/llm-provider-auth/tokenstore"
	core "github.com/xibodev/llmgw-core"
	"github.com/xibodev/llmgw-core/extension"
)

// Extension-served provider types
const (
	ExtensionTypeCopilot        = "github_copilot"
	ExtensionTypeCodex          = "openai_codex"
	ExtensionTypeAntigravity    = "google_antigravity"
	ExtensionTypeEdgeTTS        = "edge_tts"
	ExtensionTypeZenAnonymous   = "opencode_zen_anonymous"
	ExtensionTypeAnthropicSetup = "anthropic_setup_token"
)

// defaultExtensionURL is where the extension daemon listens unless
// LLMGW_EXTENSION_URL names another address.
const defaultExtensionURL = "http://127.0.0.1:18888"

// extensionSurfaces are the surfaces the daemon serves for providerType, the
// only ones the extension client sends it; the facade serves natively what
// they list beyond Chat. Copilot and anonymous OpenCode Zen serve Responses
// only for some models, which the daemon chooses in a way the gateway cannot
// see, so they list Chat alone, over which the router serves Responses. The
// daemon refuses images for Antigravity, which lists Chat alone too.
func extensionSurfaces(providerType string) []core.ModelSurface {
	switch providerType {
	case ExtensionTypeCodex:
		return []core.ModelSurface{core.ModelSurfaceChatCompletions, core.ModelSurfaceResponses}
	case ExtensionTypeCopilot, ExtensionTypeAntigravity, ExtensionTypeZenAnonymous:
		return []core.ModelSurface{core.ModelSurfaceChatCompletions}
	case ExtensionTypeEdgeTTS:
		return []core.ModelSurface{core.ModelSurfaceAudioSpeech}
	case ExtensionTypeAnthropicSetup:
		return []core.ModelSurface{core.ModelSurfaceMessages}
	}
	return nil
}

func isExtensionType(ptype string) bool {
	switch strings.ToLower(strings.TrimSpace(ptype)) {
	case ExtensionTypeCopilot, ExtensionTypeCodex, ExtensionTypeAntigravity,
		ExtensionTypeEdgeTTS, ExtensionTypeZenAnonymous, ExtensionTypeAnthropicSetup, "extension":
		return true
	default:
		return false
	}
}

// CompanionDaemonProviders returns, sorted, the IDs of the providers
// settings configures that the companion daemon serves: those of its types,
// and Codex by its registry entry, as the provider factory chooses them.
func CompanionDaemonProviders(settings *config.Settings) []string {
	if settings == nil {
		return nil
	}
	var served []string
	for id, cfg := range settings.Providers {
		if CompanionDaemonType(cfg) != "" {
			served = append(served, id)
		}
	}
	slices.Sort(served)
	return served
}

// CompanionDaemonType returns the provider type the companion daemon serves
// cfg as, or "" for a provider it does not serve: a type of its own, or Codex
// by its registry entry, as the provider factory chooses them.
func CompanionDaemonType(cfg *config.ProviderConfig) string {
	if cfg == nil {
		return ""
	}
	if strings.EqualFold(strings.TrimSpace(cfg.RegistryID), ExtensionTypeCodex) {
		return ExtensionTypeCodex
	}
	if providerType := strings.ToLower(strings.TrimSpace(cfg.Type)); isExtensionType(providerType) {
		return providerType
	}
	return ""
}

// CompanionDaemonAddress returns the address the gateway reaches the
// companion daemon at, and whether LLMGW_EXTENSION_URL names it rather than
// the default.
func CompanionDaemonAddress() (string, bool) {
	if url := strings.TrimSpace(os.Getenv("LLMGW_EXTENSION_URL")); url != "" {
		return url, true
	}
	return defaultExtensionURL, false
}

// CompanionDaemonInfo asks the companion daemon which providers it serves,
// through the client every daemon-served request shares.
func (rt *Runtime) CompanionDaemonInfo(ctx context.Context) (extension.InfoResponse, error) {
	client, err := rt.extensionClient()
	if err != nil {
		return extension.InfoResponse{}, err
	}
	return client.Info(ctx)
}

// CompanionDaemonSecretSet reports whether LLMGW_EXTENSION_SECRET names the
// shared secret the gateway sends the companion daemon.
func CompanionDaemonSecretSet() bool {
	return extensionSecret() != ""
}

func extensionSecret() string {
	return strings.TrimSpace(os.Getenv("LLMGW_EXTENSION_SECRET"))
}

// extensionClients holds the client of the extension daemon the environment
// names. Each llmgw-core extension client owns its connection pool, so every
// extension-served request shares this one, which is rebuilt only when the
// address or the secret changes.
type extensionClients struct {
	mu     sync.Mutex
	url    string
	secret string
	client *extension.Client
}

// extensionClient returns the client of the daemon at LLMGW_EXTENSION_URL,
// authenticated with LLMGW_EXTENSION_SECRET. An address the client cannot
// use is a configuration error, which permits failover without marking the
// provider unhealthy.
func (rt *Runtime) extensionClient() (*extension.Client, error) {
	url, _ := CompanionDaemonAddress()
	secret := extensionSecret()

	cache := &rt.extensions
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.client != nil && cache.url == url && cache.secret == secret {
		return cache.client, nil
	}
	client, err := extension.NewClient(extension.Config{BaseURL: url, Secret: secret})
	if err != nil {
		// The message names the variable but not its value, which may
		// carry credentials.
		return nil, core.NewConfigurationError(
			"LLMGW_EXTENSION_URL is not a valid extension daemon address: it must be an absolute http or https URL without credentials, query or fragment", err)
	}
	cache.url, cache.secret, cache.client = url, secret, client
	return client, nil
}

// extensionCoreVertical returns the coreVertical of providerType, which the
// local extension daemon serves. The Runtime builds its verticals when it is
// created, where an invalid LLMGW_EXTENSION_URL could not be reported, so
// the shared client is looked up when a provider is built or a credential
// refreshed instead.
func (rt *Runtime) extensionCoreVertical(providerType string) coreVertical {
	isOAuth := providerType == ExtensionTypeCodex || providerType == ExtensionTypeCopilot || providerType == ExtensionTypeAntigravity
	return coreVertical{
		serves: func(_ *config.Settings, _ string, cfg *config.ProviderConfig) bool {
			if cfg == nil {
				return false
			}
			t := strings.ToLower(strings.TrimSpace(cfg.Type))
			r := strings.ToLower(strings.TrimSpace(cfg.RegistryID))
			if providerType == ExtensionTypeCodex {
				return t == ExtensionTypeCodex || r == ExtensionTypeCodex
			}
			return t == providerType
		},
		provider: func(_ *config.Settings, _ string) (core.Provider, error) {
			client, err := rt.extensionClient()
			if err != nil {
				return nil, err
			}
			return extension.NewProvider(client, extension.ProviderInfo{ID: providerType, Surfaces: extensionSurfaces(providerType)}), nil
		},
		refresh: func(_ *config.Settings, _ string) tokenstore.RefreshFunc {
			return func(ctx context.Context, current tokenstore.Record) (tokenstore.Record, error) {
				client, err := rt.extensionClient()
				if err != nil {
					return tokenstore.Record{}, err
				}
				return client.Refresh(ctx, providerType, current)
			}
		},
		credentials: extensionCredentials{
			open: func() (core.CredentialStore, error) { return rt.openCredentials(isOAuth) },
		},
	}
}

type extensionCredentials struct {
	open func() (core.CredentialStore, error)
}

func (c extensionCredentials) Resolve(ctx context.Context, caller core.Caller, instance string) (string, error) {
	store, err := c.open()
	if err != nil {
		return "", err
	}
	return store.Resolve(ctx, caller, instance)
}

func (c extensionCredentials) Load(ctx context.Context, key string) (tokenstore.Record, error) {
	store, err := c.open()
	if err != nil {
		return tokenstore.Record{}, err
	}
	return store.Load(ctx, key)
}

func (c extensionCredentials) Save(ctx context.Context, key string, record tokenstore.Record) (tokenstore.Record, error) {
	store, err := c.open()
	if err != nil {
		return tokenstore.Record{}, err
	}
	return store.Save(ctx, key, record)
}

func (c extensionCredentials) ReplaceIfCurrent(ctx context.Context, key, revision string, record tokenstore.Record) (tokenstore.Record, error) {
	store, err := c.open()
	if err != nil {
		return tokenstore.Record{}, err
	}
	return store.ReplaceIfCurrent(ctx, key, revision, record)
}

func (c extensionCredentials) RevokeIfCurrent(ctx context.Context, key, revision string) error {
	store, err := c.open()
	if err != nil {
		return err
	}
	return store.RevokeIfCurrent(ctx, key, revision)
}

func (c extensionCredentials) Lease(ctx context.Context, key string) (func(), error) {
	store, err := c.open()
	if err != nil {
		return nil, err
	}
	return store.Lease(ctx, key)
}

// ExtensionProviderFacade wraps an extension-served provider for the gateway's router.
type ExtensionProviderFacade struct {
	runtime    *Runtime
	instance   string
	providerID string
	caller     core.Caller
	// timeout is how long the daemon waits for the provider's answer to
	// begin, and then between two reads of it; see extensionTimeout.
	timeout time.Duration
}

// defaultExtensionTimeoutSeconds is the timeout of a provider the companion
// daemon serves that sets none.
const defaultExtensionTimeoutSeconds = 300.0

// extensionTimeout is the timeout of instance cfg, which the daemon serves as
// a provider of providerType: its own, or a GitHub Copilot provider's
// setting, or the default. Every invoke and stream carries it, so the daemon
// waits for the provider as long as other provider types wait for theirs,
// and the client sets no bound of its own: without it, an invoke is cut
// after the client's default however long the provider keeps answering.
func extensionTimeout(settings *config.Settings, providerType string, cfg *config.ProviderConfig) time.Duration {
	seconds := cfg.TimeoutOr(0)
	if seconds <= 0 && providerType == ExtensionTypeCopilot && settings != nil {
		seconds = settings.GithubCopilotTimeoutSeconds
	}
	if !(seconds > 0) {
		seconds = defaultExtensionTimeoutSeconds
	}
	if seconds >= float64(math.MaxInt64)/float64(time.Second) {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(seconds * float64(time.Second))
}

// newExtensionFacade returns the facade of instance, which the daemon serves
// as a provider of type providerID with timeout. A provider the daemon serves
// over Messages gets the facade that answers Messages natively.
func (rt *Runtime) newExtensionFacade(instance, providerID string, caller core.Caller, timeout time.Duration) Provider {
	facade := &ExtensionProviderFacade{
		runtime:    rt,
		instance:   instance,
		providerID: providerID,
		caller:     caller,
		timeout:    timeout,
	}
	if facade.serves(core.ModelSurfaceMessages) {
		return &extensionMessagesFacade{extensionChat: facade, facade: facade}
	}
	return facade
}

func (f *ExtensionProviderFacade) IsStub() bool {
	return false
}

// daemonField reports whether a request field goes to the daemon. A field
// prefixed "_" is one of the gateway's own controls, such as the failover
// budget, the affinity key or the adaptation switch: it is not part of the
// client's request, and the daemon does not read it, so it stays here. The
// daemon reads two: a Chat body's _max_output_tokens, the output limit a
// Responses request carries through the Chat fallback, which it turns into
// the provider's own limit where the provider has one, and a Messages body's
// _llmgw_preamble, the gateway's preamble, which it puts before the system
// prompt.
func daemonField(key string) bool {
	switch key {
	case "_max_output_tokens", "_llmgw_preamble":
		return true
	}
	return !strings.HasPrefix(key, "_")
}

func (f *ExtensionProviderFacade) Complete(model string, messages []Message, kw Kwargs) (map[string]any, error) {
	resp, _, err := f.CompleteContextWithObservation(context.Background(), model, messages, kw)
	return resp, err
}

func (f *ExtensionProviderFacade) CompleteWithObservation(model string, messages []Message, kw Kwargs) (map[string]any, *CredentialObservation, error) {
	return f.CompleteContextWithObservation(context.Background(), model, messages, kw)
}

func (f *ExtensionProviderFacade) CompleteContextWithObservation(ctx context.Context, model string, messages []Message, kw Kwargs) (map[string]any, *CredentialObservation, error) {
	return f.answer(f.invoke(ctx, core.ModelSurfaceChatCompletions, model, chatPayload(model, messages, kw, false)))
}

func (f *ExtensionProviderFacade) Stream(model string, messages []Message, kw Kwargs) (StreamIter, error) {
	return f.StreamContext(context.Background(), model, messages, kw)
}

func (f *ExtensionProviderFacade) StreamContext(ctx context.Context, model string, messages []Message, kw Kwargs) (StreamIter, error) {
	return f.stream(ctx, core.ModelSurfaceChatCompletions, model, chatPayload(model, messages, kw, true))
}

func (f *ExtensionProviderFacade) CompleteResponses(model string, payload map[string]any) (map[string]any, *CredentialObservation, error) {
	return f.CompleteResponsesContext(context.Background(), model, payload)
}

// CompleteResponsesContext sends Responses as they are to a provider whose
// daemon serves them, which keeps what the Chat fallback cannot carry, such
// as encrypted reasoning. Any other provider refuses with
// ErrResponsesUnsupported before anything is sent, on which the router
// serves the request over Chat.
func (f *ExtensionProviderFacade) CompleteResponsesContext(ctx context.Context, model string, payload map[string]any) (map[string]any, *CredentialObservation, error) {
	if !f.serves(core.ModelSurfaceResponses) {
		return nil, nil, ErrResponsesUnsupported
	}
	return f.answer(f.invoke(ctx, core.ModelSurfaceResponses, model, payload))
}

func (f *ExtensionProviderFacade) StreamResponses(model string, payload map[string]any) (StreamIter, *CredentialObservation, error) {
	return f.StreamResponsesContext(context.Background(), model, payload)
}

func (f *ExtensionProviderFacade) StreamResponsesContext(ctx context.Context, model string, payload map[string]any) (StreamIter, *CredentialObservation, error) {
	if !f.serves(core.ModelSurfaceResponses) {
		return nil, nil, ErrResponsesUnsupported
	}
	stream, err := f.stream(ctx, core.ModelSurfaceResponses, model, payload)
	return stream, nil, err
}

// serves reports whether the daemon serves surface for the facade's provider.
func (f *ExtensionProviderFacade) serves(surface core.ModelSurface) bool {
	return slices.Contains(extensionSurfaces(f.providerID), surface)
}

// chatPayload is the Chat body of messages and kw.
func chatPayload(model string, messages []Message, kw Kwargs, stream bool) map[string]any {
	payload := map[string]any{"model": model, "messages": messages}
	if stream {
		payload["stream"] = true
	}
	for k, v := range kw {
		payload[k] = v
	}
	return payload
}

// request is the core request of payload on surface, without the fields that
// stay in the gateway.
func (f *ExtensionProviderFacade) request(surface core.ModelSurface, model string, payload map[string]any) (core.Request, error) {
	body := make(map[string]any, len(payload))
	for key, value := range payload {
		if daemonField(key) {
			body[key] = value
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return core.Request{}, fmt.Errorf("marshal request: %w", err)
	}
	return core.Request{Surface: surface, Model: model, Body: raw, ContentType: core.ContentTypeJSON}, nil
}

// invoke performs one non-streaming operation of payload on surface.
func (f *ExtensionProviderFacade) invoke(ctx context.Context, surface core.ModelSurface, model string, payload map[string]any) (core.Response, *CredentialObservation, error) {
	request, err := f.request(surface, model, payload)
	if err != nil {
		return core.Response{}, nil, err
	}
	ctx, collector := collectCredentials(extension.WithTimeout(ctx, f.timeout))
	response, err := f.runtime.core.Invoke(ctx, f.caller, f.instance, request)
	if err != nil {
		return core.Response{}, collector.Observation(), extensionFailure(ctx, f.instance, err)
	}
	return response, collector.Observation(), nil
}

// answer decodes what invoke returned. An answer the gateway cannot read
// counts against the circuit, as an upstream's does, but is not repeated.
func (f *ExtensionProviderFacade) answer(response core.Response, observation *CredentialObservation, err error) (map[string]any, *CredentialObservation, error) {
	if err != nil {
		return nil, observation, err
	}
	var result map[string]any
	if json.Unmarshal(response.Body, &result) != nil {
		return nil, observation, f.unreadable()
	}
	return result, observation, nil
}

func (f *ExtensionProviderFacade) unreadable() error {
	return circuitFailureInvocation(f.instance + ": the companion daemon's answer is not a JSON object")
}

// stream opens one streaming operation of payload on surface. A stream of
// Responses events ends with a terminal event, which the API layer checks.
func (f *ExtensionProviderFacade) stream(ctx context.Context, surface core.ModelSurface, model string, payload map[string]any) (StreamIter, error) {
	request, err := f.request(surface, model, payload)
	if err != nil {
		return nil, err
	}
	iter, err := f.runtime.core.Stream(extension.WithTimeout(ctx, f.timeout), f.caller, f.instance, request)
	if err != nil {
		return nil, extensionFailure(ctx, f.instance, err)
	}
	return &relayedStream{inner: iter, prefix: f.instance, responses: surface == core.ModelSurfaceResponses}, nil
}

func (f *ExtensionProviderFacade) ListModels() []ModelInfo {
	models, _, _ := f.ListModelsWithError()
	return models
}

func (f *ExtensionProviderFacade) ListModelsWithError() ([]ModelInfo, *CredentialObservation, error) {
	ctx, collector := collectCredentials(context.Background())
	cred, err := f.runtime.coreCredential(ctx, f.providerID, f.caller, f.instance)
	if err != nil {
		return nil, nil, err
	}
	client, err := f.runtime.extensionClient()
	if err != nil {
		return nil, collector.Observation(), err
	}
	models, err := client.ListModels(ctx, f.providerID, cred)
	if err != nil {
		return nil, collector.Observation(), err
	}
	return gatewayRows(models), collector.Observation(), nil
}

func (f *ExtensionProviderFacade) DefaultVoice() string {
	return "en-US-ChristopherNeural"
}

func (f *ExtensionProviderFacade) Synthesize(voice, text, speed string) ([]byte, string, error) {
	ctx := context.Background()
	client, err := f.runtime.extensionClient()
	if err != nil {
		return nil, "", extensionFailure(ctx, f.instance, err)
	}
	resp, err := client.Invoke(extension.WithTimeout(ctx, f.timeout), f.providerID, core.Request{
		Surface: core.ModelSurfaceAudioSpeech,
		Model:   voice,
		Body:    []byte(text),
	})
	if err != nil {
		return nil, "", extensionFailure(ctx, f.instance, err)
	}
	return resp.Body, "audio/mpeg", nil
}

// extensionChat is what the Messages facade keeps of the facade: Chat and
// the catalog. It leaves out speech, which the daemon does not serve the
// provider, so the gateway does not take the facade for a speech synthesizer.
type extensionChat interface {
	Provider
	detailedCompleter
	detailedContextCompleter
	ContextStreamProvider
	detailedModelLister
}

// extensionMessagesFacade is the facade of a provider the daemon serves over
// Messages. It is a type of its own because the router sends Messages as
// they are only to a facade whose type answers them. Its Chat, which the
// daemon does not serve for the provider, is refused before anything is
// sent, and the refusal leaves an endpoint to its next member.
type extensionMessagesFacade struct {
	extensionChat
	facade *ExtensionProviderFacade
}

func (f *extensionMessagesFacade) CompleteAnthropicMessages(model string, payload map[string]any) (map[string]any, error) {
	return f.CompleteAnthropicMessagesContext(context.Background(), model, payload)
}

// CompleteAnthropicMessagesContext passes a Messages payload through with
// the preamble the API layer set, and decodes the answer with its numbers
// kept as written, as the gateway's own Anthropic facade does.
func (f *extensionMessagesFacade) CompleteAnthropicMessagesContext(ctx context.Context, model string, payload map[string]any) (map[string]any, error) {
	response, _, err := f.facade.invoke(ctx, core.ModelSurfaceMessages, model, payload)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	decoder := json.NewDecoder(bytes.NewReader(response.Body))
	decoder.UseNumber()
	if decoder.Decode(&result) != nil {
		return nil, f.facade.unreadable()
	}
	return result, nil
}
