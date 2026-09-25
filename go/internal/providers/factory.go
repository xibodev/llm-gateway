package providers

import (
	"errors"
	"fmt"
	"strings"
	"sync"

	"llmgw/internal/config"
	"llmgw/internal/iam"

	anthropicauth "github.com/xibodev/llm-provider-auth/anthropic"
	gcpauth "github.com/xibodev/llm-provider-auth/gcp"
	core "github.com/xibodev/llmgw-core"
)

// providerCache holds the provider instances built for each caller scope.
// Every eviction advances epoch, so an instance built from settings or
// credentials that changed during the build is never stored.
type providerCache struct {
	mu        sync.Mutex
	instances map[string]Provider
	epoch     uint64
}

// ProviderTypes is the set of user-facing provider types (admin UI + validation).
// (echo is an internal test stub, intentionally not listed.) It is constant
// after init: nothing writes it.
var ProviderTypes = []string{
	"openai_compatible", "anthropic", "bedrock", "github_copilot", "ollama", "litellm", "edge_tts",
	"ai_studio", "vertex_ai", "azure_openai", "google_antigravity",
}

func (rt *Runtime) instantiate(
	providerID string, cfg *config.ProviderConfig, caller core.Caller,
) (Provider, error) {
	ptype := strings.ToLower(strings.TrimSpace(cfg.Type))
	if cfg.Disabled {
		return nil, &ConfigError{Msg: "provider '" + providerID + "' is disabled — re-enable it from the provider detail page"}
	}
	s := config.Get()
	timeout := s.OpenAICompatibleTimeoutSeconds
	if cfg.Timeout != nil {
		timeout = *cfg.Timeout
	}
	switch ptype {
	case "ai_studio":
		apiKey, err := resolveAPIKey(providerID, cfg, caller)
		if err != nil {
			return nil, err
		}
		return NewAIStudio(cfg.BaseURL, apiKey, cfg.TimeoutOr(120)), nil
	case "vertex_ai":
		return newVertexProvider(&rt.gcpTokens, providerID, cfg, caller)
	case "edge_tts":
		// The access token is optional: a baked-in public default applies.
		// resolveAPIKey still runs so a stored override (config secret or
		// encrypted connection) wins when present.
		token, err := resolveAPIKey(providerID, cfg, caller)
		if err != nil {
			token = ""
		}
		return NewEdgeTTS(cfg.BaseURL, token, cfg.DefaultVoice, cfg.TimeoutOr(60)), nil
	case "openai_compatible", "openai", "litellm":
		if codexInstance(providerID, cfg) {
			if strings.TrimSpace(callerPrincipalID(caller)) == "" {
				return nil, &ConfigError{Msg: "openai_codex: a human principal private connection is required"}
			}
			return rt.newCodexProvider(providerID, caller, timeout, nil, "")
		}
		base := openAICompatibleBase(s, cfg)
		if zenInstance(s, providerID, cfg) {
			return rt.newZenProvider(providerID, cfg, caller, base, timeout)
		}
		registryID := EffectiveRegistryID(providerID, cfg.RegistryID, cfg.Type)
		apiKey, observation, err := resolveAPIKeyObserved(providerID, cfg, caller)
		if err != nil {
			return nil, err
		}
		registryEntry, _ := RegistryProviderByID(registryID)
		anonymous := registryEntry.AnonymousAutomation && AnonymousAPIKey(apiKey)
		return OpenAIProvider{
			auth:    bearerAuth{base: strings.TrimRight(base, "/"), apiKey: apiKey, observation: observation},
			Timeout: timeout, forceAdapt: cfg.ForceApiSupport,
			providerID: providerID, caller: caller, registryID: registryID,
			anonymous: anonymous,
		}, nil
	case "azure_openai":
		// Normalised, not merely checked for emptiness: the catalog derives the
		// deployments route from scheme+host alone while inference appends to
		// base_url verbatim, so a bare resource endpoint used to pass setup,
		// populate the catalog, and 404 every completion.
		baseURL, err := azureInferenceBaseURL(cfg.BaseURL)
		if err != nil {
			return nil, &ConfigError{Msg: fmt.Sprintf("provider '%s': %v", providerID, err)}
		}
		apiKey, observation, err := resolveAPIKeyObserved(providerID, cfg, caller)
		if err != nil {
			return nil, err
		}
		// The core Runtime resolves each request's key itself; the facade
		// keeps the one resolved here, and its observation, for the catalog.
		return AzureOpenAIProvider{
			BaseURL:     baseURL,
			APIKey:      apiKey,
			Timeout:     timeout,
			observation: observation,
			runtime:     rt,
			instance:    providerID,
			caller:      caller,
		}, nil
	case "anthropic":
		credential, kind, _, err := resolveCredentialObserved(providerID, cfg, caller)
		if err != nil {
			return nil, err
		}
		// The core Runtime resolves each request's credential itself; the
		// facade keeps the one resolved here for the catalog.
		provider := AnthropicNativeProvider{BaseURL: cfg.BaseURL, Timeout: cfg.TimeoutOr(0), runtime: rt, instance: providerID, caller: caller}
		if strings.TrimSpace(credential) == "" {
			return provider, nil
		}
		if kind != CredentialKindAPIKey && kind != string(anthropicauth.CredentialSetupToken) {
			return nil, &ConfigError{Msg: fmt.Sprintf("provider '%s': connection kind %q is not usable for anthropic", providerID, kind)}
		}
		auth, err := anthropicauth.NewHeaderSource(credential)
		if err != nil {
			return nil, &ConfigError{Msg: fmt.Sprintf("provider '%s': %v", providerID, err)}
		}
		provider.Auth = auth
		return provider, nil
	case "google_antigravity":
		return rt.newAntigravityProvider(providerID, caller)
	case "bedrock":
		apiKey, observation, err := resolveAPIKeyObserved(providerID, cfg, caller)
		if err != nil {
			return nil, err
		}
		bp := buildBedrockProvider(
			cfg.Region, cfg.BaseURL, apiKey, observation, cfg.TimeoutOr(0),
		)
		bp.forceAdapt = cfg.ForceApiSupport
		bp.providerID = providerID
		bp.caller = caller
		return bp, nil
	case "github_copilot":
		return rt.newCopilotProvider(providerID, cfg, caller, cfg.TimeoutOr(s.GithubCopilotTimeoutSeconds)), nil
	case "ollama":
		base := cfg.BaseURL
		if base == "" {
			base = s.OllamaBaseURL
		}
		return OllamaProvider{BaseURL: base, Timeout: cfg.TimeoutOr(s.OllamaTimeoutSeconds)}, nil
	case "echo":
		return EchoProvider{}, nil
	}
	return nil, &ConfigError{Msg: fmt.Sprintf("provider '%s': unknown type '%s'", providerID, cfg.Type)}
}

func policyFor(providerID string) config.ProviderPolicy {
	p := config.Get().Policies
	if override, ok := p.Overrides[providerID]; ok {
		return override
	}
	return p.Defaults
}

// GetProvider builds (and caches) the provider instance for an id.
func (rt *Runtime) GetProvider(providerID string) (Provider, error) {
	return rt.GetProviderForPrincipal(providerID, gatewayCaller())
}

// GetProviderForPrincipal returns a provider instance bound to the caller's
// private credential context. A caller-specific cache entry prevents API-key
// and OAuth connections from leaking across callers.
func (rt *Runtime) GetProviderForPrincipal(
	providerID string, caller core.Caller,
) (Provider, error) {
	// Any provider can carry a personal connection, so a caller-identified
	// instance is never shared: one principal's private credential must not be
	// served to another from cache. providerCacheKey adds the project dimension
	// for service principals resolving shared credential bindings.
	cacheKey := providerCacheKey(providerID, caller)
	cache := &rt.instances
	for attempt := 0; attempt < 3; attempt++ {
		// A cached transport must not bypass a provider being taken offline.
		if cfg := config.Get().Providers[providerID]; cfg != nil && cfg.Disabled {
			return nil, &ConfigError{Msg: "provider is disabled"}
		}
		cache.mu.Lock()
		epoch := cache.epoch
		if cached, ok := cache.instances[cacheKey]; ok {
			cache.mu.Unlock()
			return cached, nil
		}
		cache.mu.Unlock()

		cfg, ok := config.Get().Providers[providerID]
		if !ok {
			return nil, &ConfigError{Msg: fmt.Sprintf("unknown provider '%s'; add it under 'providers:'", providerID)}
		}
		instance, err := rt.instantiate(providerID, cfg, caller)
		var provider Provider
		if err == nil {
			policy := policyFor(providerID)
			provider = instance
			if policy.RetryEnabled() || policy.CircuitEnabled() {
				provider = &ResilientProvider{inner: instance, name: providerID, policy: policy}
			}
		}

		cache.mu.Lock()
		if epoch != cache.epoch {
			cache.mu.Unlock()
			continue
		}
		if err != nil {
			cache.mu.Unlock()
			return nil, err
		}
		if cached, ok := cache.instances[cacheKey]; ok {
			cache.mu.Unlock()
			return cached, nil
		}
		cache.instances[cacheKey] = provider
		cache.mu.Unlock()
		return provider, nil
	}
	return nil, &ConfigError{Msg: fmt.Sprintf("provider '%s': configuration changed repeatedly during initialization", providerID)}
}

// AsSpeechSynthesizer reports whether a provider can synthesize speech
// natively. Resilience decorators implement the capability and enforce policy.
func AsSpeechSynthesizer(provider Provider) (SpeechSynthesizer, bool) {
	for provider != nil {
		if resilient, ok := provider.(*ResilientProvider); ok {
			if _, supported := AsSpeechSynthesizer(resilient.inner); !supported {
				return nil, false
			}
			return resilient, true
		}
		if synthesizer, ok := provider.(SpeechSynthesizer); ok {
			return synthesizer, true
		}
		unwrapper, ok := provider.(interface{ Unwrap() Provider })
		if !ok {
			return nil, false
		}
		provider = unwrapper.Unwrap()
	}
	return nil, false
}

// SpeechSynthesizerForPrincipal resolves a provider and reports whether it can
// synthesize speech natively (e.g. edge_tts) rather than via HTTP proxying.
func (rt *Runtime) SpeechSynthesizerForPrincipal(providerID string, caller core.Caller) (SpeechSynthesizer, bool) {
	provider, err := rt.GetProviderForPrincipal(providerID, caller)
	if err != nil {
		return nil, false
	}
	return AsSpeechSynthesizer(provider)
}

func resolveAPIKey(
	providerID string, cfg *config.ProviderConfig, caller core.Caller,
) (string, error) {
	secret, _, err := resolveAPIKeyObserved(providerID, cfg, caller)
	return secret, err
}

// CredentialKindAPIKey and CredentialKindServiceAccount are the stored
// connection kinds this package understands. The kind travels with the secret
// because a service-account key is not usable as an API key and must never be
// silently presented as one.
const (
	CredentialKindAPIKey         = "api_key"
	CredentialKindServiceAccount = gcpauth.CredentialKind
)

// resolveAPIKeyObserved resolves a credential and requires it to be an API key.
func resolveAPIKeyObserved(
	providerID string, cfg *config.ProviderConfig, caller core.Caller,
) (string, *CredentialObservation, error) {
	secret, kind, observation, err := resolveCredentialObserved(providerID, cfg, caller)
	if err != nil {
		return "", nil, err
	}
	if !strings.EqualFold(strings.TrimSpace(kind), CredentialKindAPIKey) {
		return "", nil, &ConfigError{Msg: fmt.Sprintf(
			"provider '%s': connection kind %q is not an API key", providerID, kind,
		)}
	}
	return secret, observation, nil
}

// resolveCredentialObserved applies the credential resolution order and reports
// the stored kind alongside the secret.
func resolveCredentialObserved(
	providerID string, cfg *config.ProviderConfig, caller core.Caller,
) (string, string, *CredentialObservation, error) {
	if strings.TrimSpace(config.Get().CredentialEncryptionKey) == "" {
		return config.ResolveProviderAPIKey(providerID, cfg), CredentialKindAPIKey, nil, nil
	}
	if principalID := callerPrincipalID(caller); principalID != "" {
		secret, connection, observation, ok, err := iam.ProviderConnectionSecretWithObservation(
			principalID, providerID, "",
		)
		if err != nil {
			return "", "", nil, &ConfigError{Msg: fmt.Sprintf(
				"provider '%s': load personal connection: %v", providerID, err,
			)}
		}
		if ok {
			kind := strings.TrimSpace(connection.Kind)
			if observation.CredentialRevision > 0 {
				return secret, kind, credentialObservation(&observation), nil
			}
			return secret, kind, nil, nil
		}
	}
	secret, connection, ok, err := iam.SystemProviderConnectionSecret(providerID)
	if err != nil {
		return "", "", nil, &ConfigError{Msg: fmt.Sprintf(
			"provider '%s': load system connection: %v", providerID, err,
		)}
	}
	if ok {
		return secret, strings.TrimSpace(connection.Kind), nil, nil
	}
	return config.ResolveProviderAPIKey(providerID, cfg), CredentialKindAPIKey, nil, nil
}

// newVertexProvider builds the Vertex provider for whichever credential kind is
// stored. An API key keeps the existing x-goog-api-key path untouched; a
// service-account key is exchanged for a short-lived OAuth2 access token,
// cached in tokens.
func newVertexProvider(
	tokens *gcpauth.TokenCache, providerID string, cfg *config.ProviderConfig, caller core.Caller,
) (Provider, error) {
	requestType := strings.ToLower(strings.TrimSpace(cfg.VertexRequestType))
	switch requestType {
	case "", "default", "paygo", "dedicated":
	default:
		return nil, &ConfigError{Msg: fmt.Sprintf(
			"provider '%s': vertex_request_type must be default, paygo, or dedicated", providerID,
		)}
	}
	secret, kind, _, err := resolveCredentialObserved(providerID, cfg, caller)
	if err != nil {
		return nil, err
	}
	kind = strings.TrimSpace(kind)
	// Fail here rather than sending an unauthenticated request. Vertex answers a
	// missing credential with 401, which reads as "credential rejected" and points
	// at a bad key instead of an absent one.
	if strings.TrimSpace(secret) == "" {
		return nil, &ConfigError{Msg: fmt.Sprintf(
			"provider '%s': no credential configured — vertex_ai needs an API key or a "+
				"Google service account key; add one before sending requests",
			providerID,
		)}
	}
	if !strings.EqualFold(kind, CredentialKindServiceAccount) {
		if !strings.EqualFold(kind, CredentialKindAPIKey) {
			return nil, &ConfigError{Msg: fmt.Sprintf(
				"provider '%s': connection kind %q is not usable for vertex_ai", providerID, kind,
			)}
		}
		provider := NewVertexAI(cfg.BaseURL, secret, cfg.Project, cfg.Location, cfg.TimeoutOr(120)).withVertexRequestType(requestType)
		return provider, nil
	}

	credential, err := gcpauth.Parse([]byte(secret))
	if err != nil {
		return nil, &ConfigError{Msg: fmt.Sprintf("provider '%s': %v", providerID, err)}
	}
	// The key names the project it belongs to, so an unset project is filled in
	// from it and a contradicting one fails here rather than as an opaque 403.
	project := strings.TrimSpace(cfg.Project)
	switch {
	case project == "":
		project = credential.ProjectID()
	case !strings.EqualFold(project, credential.ProjectID()):
		return nil, &ConfigError{Msg: fmt.Sprintf(
			"provider '%s': configured project %q does not match the service account project %q",
			providerID, project, credential.ProjectID(),
		)}
	}
	tokenSource := func() (string, error) {
		token, tokenErr := tokens.AccessToken(credential, gcpauth.CloudPlatformScope)
		if tokenErr != nil {
			var exchangeErr *gcpauth.TokenError
			if errors.As(tokenErr, &exchangeErr) {
				message := fmt.Sprintf("provider '%s': service account token refresh failed", providerID)
				if exchangeErr.StatusCode != 0 {
					return "", failoverInvocationStatus(message, exchangeErr.StatusCode)
				}
				if exchangeErr.Code == "transport" {
					return "", retryableInvocation(message)
				}
			}
			return "", invocation(fmt.Sprintf("provider '%s': service account token refresh failed", providerID))
		}
		return token, nil
	}
	provider := NewVertexAIWithTokenSource(
		cfg.BaseURL, project, cfg.Location, cfg.TimeoutOr(120), tokenSource,
	).withVertexRequestType(requestType)
	return provider, nil
}

// providerCacheKey scopes an instance to the principal whose credentials it
// holds: the gateway-wide key for shared-only callers, the principal for a
// human or system principal, and the principal in its project for a service,
// whose credential bindings are per project.
func providerCacheKey(providerID string, caller core.Caller) string {
	principalID, kind := callerPrincipal(caller)
	if principalID == "" {
		return providerID
	}
	key := providerID + "@" + principalID
	if kind == "service" && caller.ProjectID != "" {
		key += "#" + caller.ProjectID
	}
	return key
}

// ProviderCredentialAuthorized reports whether the caller has an active
// credential context for a provider. Static providers retain their existing
// gateway-managed behavior; principal-scoped providers fail closed.
func ProviderCredentialAuthorized(
	providerID string, caller core.Caller,
) (bool, error) {
	cfg, ok := config.Get().Providers[providerID]
	if !ok {
		return true, nil
	}
	privateCatalog := CatalogRequiresPrincipal(providerID)
	if !strings.EqualFold(cfg.Type, "github_copilot") && !privateCatalog {
		return true, nil
	}
	if callerPrincipalID(caller) == "" {
		return !privateCatalog, nil
	}
	_, _, found, err := iam.ResolveCallerOAuthCredentialSecretWithObservation(
		caller, providerID,
	)
	if err != nil {
		return false, err
	}
	return found, nil
}

// AdaptsChatToNativeResponses reports whether a provider serves Chat
// Completions for a Responses-native model by translating to its Responses
// transport. Routing may then offer Chat and Messages for such models, which
// the published model list already advertises as emulated (llm-gateway#76).
func AdaptsChatToNativeResponses(providerID string, cfg *config.ProviderConfig) bool {
	if cfg == nil {
		return false
	}
	switch EffectiveRegistryID(providerID, cfg.RegistryID, cfg.Type) {
	case "opencode_zen", "openai_codex":
		return true
	}
	return isZenBaseURL(cfg.BaseURL)
}

// AnonymousZenForPrincipal reports the effective OpenCode Zen access mode
// without exposing the resolved credential.
func AnonymousZenForPrincipal(providerID string, caller core.Caller) (bool, error) {
	cfg, ok := config.Get().Providers[providerID]
	if !ok || cfg == nil {
		return false, &ConfigError{Msg: fmt.Sprintf("provider '%s': not configured", providerID)}
	}
	registryID := EffectiveRegistryID(providerID, cfg.RegistryID, cfg.Type)
	if registryID != "opencode_zen" && !isZenBaseURL(cfg.BaseURL) {
		return false, nil
	}
	key, _, err := resolveAPIKeyObserved(providerID, cfg, caller)
	if err != nil {
		return false, err
	}
	return AnonymousAPIKey(key), nil
}

// ListProviderModels returns a fresh catalog for one provider ([] on any failure).
func (rt *Runtime) ListProviderModels(providerID string) []ModelInfo {
	return rt.ListProviderModelsForPrincipal(providerID, gatewayCaller())
}

func (rt *Runtime) ListProviderModelsForPrincipal(
	providerID string, caller core.Caller,
) []ModelInfo {
	models, _, _ := rt.ListProviderModelsForPrincipalWithError(providerID, caller)
	return models
}

// ListProviderModelsForPrincipalWithError preserves safe catalog failure
// details for lifecycle checks while the public model-list APIs remain
// backward-compatible and return an empty list on failure.
func (rt *Runtime) ListProviderModelsForPrincipalWithError(
	providerID string, caller core.Caller,
) ([]ModelInfo, *CredentialObservation, error) {
	if issue := ProviderConfigurationIssue(providerID); issue != "" {
		return nil, nil, catalogError(
			"catalog_configuration_incomplete", issue, 0,
		)
	}
	p, err := rt.GetProviderForPrincipal(providerID, caller)
	if err != nil {
		return nil, nil, catalogError(
			"catalog_provider_unavailable",
			"Provider could not be initialized for catalog access.",
			0,
		)
	}
	return listModelsWithError(p)
}

// ResetProviders clears the instance cache (after a config change).
func (rt *Runtime) ResetProviders() {
	rt.instances.mu.Lock()
	rt.instances.epoch++
	rt.instances.instances = map[string]Provider{}
	rt.instances.mu.Unlock()
}

// ForgetProvider evicts every cached instance for a provider, including
// principal-scoped instances. Call it after a provider is removed.
func (rt *Runtime) ForgetProvider(providerID string) {
	providerID = strings.TrimSpace(providerID)
	if providerID == "" {
		return
	}
	rt.instances.mu.Lock()
	defer rt.instances.mu.Unlock()
	rt.instances.epoch++
	for key := range rt.instances.instances {
		if key == providerID || strings.HasPrefix(key, providerID+"@") {
			delete(rt.instances.instances, key)
		}
	}
}

// ForgetProviderForPrincipal evicts only one human-owned provider instance.
// It is used after a private credential is created, rotated, or revoked so an
// already-instantiated transport can never retain the previous secret.
func (rt *Runtime) ForgetProviderForPrincipal(providerID, principalID string) {
	providerID = strings.TrimSpace(providerID)
	principalID = strings.TrimSpace(principalID)
	if providerID == "" || principalID == "" {
		return
	}
	rt.instances.mu.Lock()
	rt.instances.epoch++
	delete(rt.instances.instances, providerID+"@"+principalID)
	rt.instances.mu.Unlock()
}
