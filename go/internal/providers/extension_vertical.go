package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"

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

// extensionSurfaces are the surfaces every extension-served provider
// accepts. The gateway does not ask the daemon which surfaces each provider
// serves, so the daemon itself refuses one a provider lacks.
func extensionSurfaces() []core.ModelSurface {
	return []core.ModelSurface{
		core.ModelSurfaceChatCompletions,
		core.ModelSurfaceResponses,
		core.ModelSurfaceMessages,
		core.ModelSurfaceAudioSpeech,
		core.ModelSurfaceImages,
	}
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

func (rt *Runtime) isExtensionEnabled() bool {
	val := strings.ToLower(strings.TrimSpace(os.Getenv("LLMGW_EXTENSION_ENABLED")))
	if val == "1" || val == "true" || val == "yes" || val == "on" {
		return true
	}
	return strings.TrimSpace(os.Getenv("LLMGW_EXTENSION_URL")) != ""
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
	url := strings.TrimSpace(os.Getenv("LLMGW_EXTENSION_URL"))
	if url == "" {
		url = defaultExtensionURL
	}
	secret := strings.TrimSpace(os.Getenv("LLMGW_EXTENSION_SECRET"))

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
			return extension.NewProvider(client, extension.ProviderInfo{ID: providerType, Surfaces: extensionSurfaces()}), nil
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
}

func (rt *Runtime) newExtensionFacade(instance, providerID string, caller core.Caller) Provider {
	return &ExtensionProviderFacade{
		runtime:    rt,
		instance:   instance,
		providerID: providerID,
		caller:     caller,
	}
}

func (f *ExtensionProviderFacade) IsStub() bool {
	return false
}

// daemonField reports whether a request field goes to the daemon. A field
// prefixed "_" is one of the gateway's own controls, such as the failover
// budget, the affinity key or the adaptation switch: it is not part of the
// client's request, and the daemon does not read it, so it stays here. The
// daemon reads one: _max_output_tokens, the output limit a Responses request
// carries through the Chat fallback, which it turns into the provider's own
// limit where the provider has one.
func daemonField(key string) bool {
	return !strings.HasPrefix(key, "_") || key == "_max_output_tokens"
}

func (f *ExtensionProviderFacade) Complete(model string, messages []Message, kw Kwargs) (map[string]any, error) {
	resp, _, err := f.CompleteContextWithObservation(context.Background(), model, messages, kw)
	return resp, err
}

func (f *ExtensionProviderFacade) CompleteWithObservation(model string, messages []Message, kw Kwargs) (map[string]any, *CredentialObservation, error) {
	return f.CompleteContextWithObservation(context.Background(), model, messages, kw)
}

func (f *ExtensionProviderFacade) CompleteContextWithObservation(ctx context.Context, model string, messages []Message, kw Kwargs) (map[string]any, *CredentialObservation, error) {
	payload := map[string]any{
		"model":    model,
		"messages": messages,
	}
	for k, v := range kw {
		if daemonField(k) {
			payload[k] = v
		}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal request: %w", err)
	}
	req := core.Request{
		Surface:     core.ModelSurfaceChatCompletions,
		Model:       model,
		Body:        raw,
		ContentType: core.ContentTypeJSON,
	}
	ctx, collector := collectCredentials(ctx)
	resp, err := f.runtime.core.Invoke(ctx, f.caller, f.instance, req)
	if err != nil {
		return nil, collector.Observation(), extensionFailure(ctx, f.instance, err)
	}
	var out map[string]any
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return nil, collector.Observation(), circuitFailureInvocation(f.instance + ": the companion daemon's answer is not a JSON object")
	}
	return out, collector.Observation(), nil
}

func (f *ExtensionProviderFacade) Stream(model string, messages []Message, kw Kwargs) (StreamIter, error) {
	return f.StreamContext(context.Background(), model, messages, kw)
}

func (f *ExtensionProviderFacade) StreamContext(ctx context.Context, model string, messages []Message, kw Kwargs) (StreamIter, error) {
	payload := map[string]any{
		"model":    model,
		"messages": messages,
		"stream":   true,
	}
	for k, v := range kw {
		if daemonField(k) {
			payload[k] = v
		}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	req := core.Request{
		Surface:     core.ModelSurfaceChatCompletions,
		Model:       model,
		Body:        raw,
		ContentType: core.ContentTypeJSON,
	}
	iter, err := f.runtime.core.Stream(ctx, f.caller, f.instance, req)
	if err != nil {
		return nil, extensionFailure(ctx, f.instance, err)
	}
	return &relayedStream{inner: iter, prefix: f.instance}, nil
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
	resp, err := client.Invoke(ctx, f.providerID, core.Request{
		Surface: core.ModelSurfaceAudioSpeech,
		Model:   voice,
		Body:    []byte(text),
	})
	if err != nil {
		return nil, "", extensionFailure(ctx, f.instance, err)
	}
	return resp.Body, "audio/mpeg", nil
}
