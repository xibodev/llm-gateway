package providers

import (
	"fmt"
	"strings"
	"sync"

	"llmgw/internal/config"
	"llmgw/internal/gcpauth"
	"llmgw/internal/iam"
)

var (
	cacheMu    sync.Mutex
	cache      = map[string]Provider{}
	cacheEpoch uint64
)

// ProviderTypes is the set of user-facing provider types (admin UI + validation).
// (echo is an internal test stub, intentionally not listed.)
var ProviderTypes = []string{
	"openai_compatible", "anthropic", "bedrock", "github_copilot", "ollama", "litellm", "edge_tts",
	"ai_studio", "vertex_ai",
}

func instantiate(
	providerID string, cfg *config.ProviderConfig, principal *config.Principal,
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
		apiKey, err := resolveAPIKey(providerID, cfg, principal)
		if err != nil {
			return nil, err
		}
		return NewAIStudio(cfg.BaseURL, apiKey, cfg.TimeoutOr(120)), nil
	case "vertex_ai":
		return newVertexProvider(providerID, cfg, principal)
	case "edge_tts":
		// The access token is optional: a baked-in public default applies.
		// resolveAPIKey still runs so a stored override (config secret or
		// encrypted connection) wins when present.
		token, err := resolveAPIKey(providerID, cfg, principal)
		if err != nil {
			token = ""
		}
		return NewEdgeTTS(cfg.BaseURL, token, cfg.DefaultVoice, cfg.TimeoutOr(60)), nil
	case "openai_compatible", "openai", "litellm":
		if EffectiveRegistryID(providerID, cfg.RegistryID, cfg.Type) == "openai_codex" {
			if principal == nil || strings.TrimSpace(principal.PrincipalID) == "" {
				return nil, &ConfigError{Msg: "openai_codex: a human principal private connection is required"}
			}
			clientID := strings.TrimSpace(config.Get().OpenAICodexClientID)
			if clientID == "" {
				return nil, &ConfigError{Msg: "openai_codex: openai_codex_client_id is required"}
			}
			return CodexProvider{inner: OpenAIProvider{
				auth:    codexAuth{principalID: principal.PrincipalID, providerID: providerID, clientID: clientID},
				Timeout: timeout, providerID: providerID, principal: principal,
			}}, nil
		}
		apiKey, observation, err := resolveAPIKeyObserved(providerID, cfg, principal)
		if err != nil {
			return nil, err
		}
		base := cfg.BaseURL
		if base == "" {
			base = s.OpenAICompatibleBaseURL
		}
		return OpenAIProvider{auth: bearerAuth{
			base: strings.TrimRight(base, "/"), apiKey: apiKey, observation: observation,
		}, Timeout: timeout, forceAdapt: cfg.ForceApiSupport, providerID: providerID, principal: principal}, nil
	case "anthropic":
		apiKey, err := resolveAPIKey(providerID, cfg, principal)
		if err != nil {
			return nil, err
		}
		return AnthropicNativeProvider{BaseURL: cfg.BaseURL, APIKey: apiKey, Timeout: cfg.TimeoutOr(0)}, nil
	case "bedrock":
		apiKey, observation, err := resolveAPIKeyObserved(providerID, cfg, principal)
		if err != nil {
			return nil, err
		}
		bp := buildBedrockProvider(
			cfg.Region, cfg.BaseURL, apiKey, observation, cfg.TimeoutOr(0),
		)
		bp.forceAdapt = cfg.ForceApiSupport
		bp.providerID = providerID
		bp.principal = principal
		return bp, nil
	case "github_copilot":
		return OpenAIProvider{auth: copilotAuth{providerID: providerID, principal: principal}, Timeout: s.GithubCopilotTimeoutSeconds, forceAdapt: cfg.ForceApiSupport, providerID: providerID, principal: principal}, nil
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
func GetProvider(providerID string) (Provider, error) {
	return GetProviderForPrincipal(providerID, nil)
}

// GetProviderForPrincipal returns a provider instance bound to the caller's
// private credential context. A principal-specific cache entry prevents API-key
// and OAuth connections from leaking across callers.
func GetProviderForPrincipal(
	providerID string, principal *config.Principal,
) (Provider, error) {
	// Any provider can carry a personal connection, so a caller-identified
	// instance is never shared: one principal's private credential must not be
	// served to another from cache. providerCacheKey adds the project dimension
	// for service principals resolving shared credential bindings.
	cacheKey := providerCacheKey(providerID, principal)
	for attempt := 0; attempt < 3; attempt++ {
		cacheMu.Lock()
		epoch := cacheEpoch
		if cached, ok := cache[cacheKey]; ok {
			cacheMu.Unlock()
			return cached, nil
		}
		cacheMu.Unlock()

		cfg, ok := config.Get().Providers[providerID]
		if !ok {
			return nil, &ConfigError{Msg: fmt.Sprintf("unknown provider '%s'; add it under 'providers:'", providerID)}
		}
		instance, err := instantiate(providerID, cfg, principal)
		var provider Provider
		if err == nil {
			policy := policyFor(providerID)
			provider = instance
			if policy.RetryEnabled() || policy.CircuitEnabled() {
				provider = &ResilientProvider{inner: instance, name: providerID, policy: policy}
			}
		}

		cacheMu.Lock()
		if epoch != cacheEpoch {
			cacheMu.Unlock()
			continue
		}
		if err != nil {
			cacheMu.Unlock()
			return nil, err
		}
		if cached, ok := cache[cacheKey]; ok {
			cacheMu.Unlock()
			return cached, nil
		}
		cache[cacheKey] = provider
		cacheMu.Unlock()
		return provider, nil
	}
	return nil, &ConfigError{Msg: fmt.Sprintf("provider '%s': configuration changed repeatedly during initialization", providerID)}
}

// AsSpeechSynthesizer reports whether a provider can synthesize speech
// natively, unwrapping resilience decorators to reach the concrete provider.
func AsSpeechSynthesizer(provider Provider) (SpeechSynthesizer, bool) {
	for provider != nil {
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
func SpeechSynthesizerForPrincipal(providerID string, principal *config.Principal) (SpeechSynthesizer, bool) {
	provider, err := GetProviderForPrincipal(providerID, principal)
	if err != nil {
		return nil, false
	}
	return AsSpeechSynthesizer(provider)
}

func resolveAPIKey(
	providerID string, cfg *config.ProviderConfig, principal *config.Principal,
) (string, error) {
	secret, _, err := resolveAPIKeyObserved(providerID, cfg, principal)
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
	providerID string, cfg *config.ProviderConfig, principal *config.Principal,
) (string, *iam.ProviderAccountObservation, error) {
	secret, kind, observation, err := resolveCredentialObserved(providerID, cfg, principal)
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
	providerID string, cfg *config.ProviderConfig, principal *config.Principal,
) (string, string, *iam.ProviderAccountObservation, error) {
	if strings.TrimSpace(config.Get().CredentialEncryptionKey) == "" {
		return config.ResolveProviderAPIKey(providerID, cfg), CredentialKindAPIKey, nil, nil
	}
	if principal != nil && principal.PrincipalID != "" {
		secret, connection, observation, ok, err := iam.ProviderConnectionSecretWithObservation(
			principal.PrincipalID, providerID, "",
		)
		if err != nil {
			return "", "", nil, &ConfigError{Msg: fmt.Sprintf(
				"provider '%s': load personal connection: %v", providerID, err,
			)}
		}
		if ok {
			kind := strings.TrimSpace(connection.Kind)
			if observation.CredentialRevision > 0 {
				return secret, kind, &observation, nil
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
// service-account key is exchanged for a short-lived OAuth2 access token.
func newVertexProvider(
	providerID string, cfg *config.ProviderConfig, principal *config.Principal,
) (Provider, error) {
	secret, kind, _, err := resolveCredentialObserved(providerID, cfg, principal)
	if err != nil {
		return nil, err
	}
	kind = strings.TrimSpace(kind)
	if !strings.EqualFold(kind, CredentialKindServiceAccount) {
		if !strings.EqualFold(kind, CredentialKindAPIKey) {
			return nil, &ConfigError{Msg: fmt.Sprintf(
				"provider '%s': connection kind %q is not usable for vertex_ai", providerID, kind,
			)}
		}
		return NewVertexAI(cfg.BaseURL, secret, cfg.Project, cfg.Location, cfg.TimeoutOr(120)), nil
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
	token, err := gcpauth.AccessToken(credential, gcpauth.CloudPlatformScope)
	if err != nil {
		return nil, &ConfigError{Msg: fmt.Sprintf("provider '%s': %v", providerID, err)}
	}
	return NewVertexAIWithAccessToken(
		cfg.BaseURL, token, project, cfg.Location, cfg.TimeoutOr(120),
	), nil
}

func providerCacheKey(providerID string, principal *config.Principal) string {
	if principal == nil || principal.PrincipalID == "" {
		return providerID
	}
	key := providerID + "@" + principal.PrincipalID
	if principal.PrincipalKind == "service" && principal.ProjectID != "" {
		key += "#" + principal.ProjectID
	}
	return key
}

// ProviderCredentialAuthorized reports whether the caller has an active
// credential context for a provider. Static providers retain their existing
// gateway-managed behavior; principal-scoped providers fail closed.
func ProviderCredentialAuthorized(
	providerID string, principal *config.Principal,
) (bool, error) {
	cfg, ok := config.Get().Providers[providerID]
	if !ok {
		return true, nil
	}
	if !strings.EqualFold(cfg.Type, "github_copilot") ||
		principal == nil || principal.PrincipalID == "" {
		return true, nil
	}
	_, _, found, err := iam.ResolveProviderOAuthCredentialSecretWithObservation(
		principal, providerID,
	)
	return found, err
}

// ListProviderModels returns a fresh catalog for one provider ([] on any failure).
func ListProviderModels(providerID string) []ModelInfo {
	return ListProviderModelsForPrincipal(providerID, nil)
}

func ListProviderModelsForPrincipal(
	providerID string, principal *config.Principal,
) []ModelInfo {
	models, _, _ := ListProviderModelsForPrincipalWithError(providerID, principal)
	return models
}

// ListProviderModelsForPrincipalWithError preserves safe catalog failure
// details for lifecycle checks while the public model-list APIs remain
// backward-compatible and return an empty list on failure.
func ListProviderModelsForPrincipalWithError(
	providerID string, principal *config.Principal,
) ([]ModelInfo, *iam.ProviderAccountObservation, error) {
	if issue := ProviderConfigurationIssue(providerID); issue != "" {
		return nil, nil, catalogError(
			"catalog_configuration_incomplete", issue, 0,
		)
	}
	p, err := GetProviderForPrincipal(providerID, principal)
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
func ResetProviders() {
	cacheMu.Lock()
	cacheEpoch++
	cache = map[string]Provider{}
	cacheMu.Unlock()
}

// ForgetProvider evicts every cached instance for a provider, including
// principal-scoped instances. Call it after a provider is removed.
func ForgetProvider(providerID string) {
	providerID = strings.TrimSpace(providerID)
	if providerID == "" {
		return
	}
	cacheMu.Lock()
	defer cacheMu.Unlock()
	cacheEpoch++
	for key := range cache {
		if key == providerID || strings.HasPrefix(key, providerID+"@") {
			delete(cache, key)
		}
	}
}

// ForgetProviderForPrincipal evicts only one human-owned provider instance.
// It is used after a private credential is created, rotated, or revoked so an
// already-instantiated transport can never retain the previous secret.
func ForgetProviderForPrincipal(providerID, principalID string) {
	providerID = strings.TrimSpace(providerID)
	principalID = strings.TrimSpace(principalID)
	if providerID == "" || principalID == "" {
		return
	}
	cacheMu.Lock()
	cacheEpoch++
	delete(cache, providerID+"@"+principalID)
	cacheMu.Unlock()
}
