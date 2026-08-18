// Package config is the single source of truth for the gateway's runtime state:
// providers, categories, minted keys, provider secrets, and scalar settings.
//
// Mirrors the Python llmgw.config module. Keys never live in the committed
// config file â€” they live in a 0600 secrets.json / keys.json under the state
// dir (~/.llmgw by default, override with LLMGW_STATE_DIR).
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

const Version = "0.0.1"

// ProviderConfig is one connected upstream.
type ProviderConfig struct {
	Type       string   `yaml:"type" json:"type"`
	RegistryID string   `yaml:"registry_id,omitempty" json:"registry_id,omitempty"`
	BaseURL    string   `yaml:"base_url,omitempty" json:"base_url,omitempty"`
	APIKey     string   `yaml:"api_key,omitempty" json:"-"`
	Region     string   `yaml:"region,omitempty" json:"region,omitempty"`
	Timeout    *float64 `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	// DefaultVoice selects the voice used by speech-synthesis providers when
	// a request names the provider without a specific voice.
	DefaultVoice string `yaml:"default_voice,omitempty" json:"default_voice,omitempty"`
	// Project and Location scope a Vertex AI provider. Location defaults to the
	// multi-region "global" endpoint, which carries the widest model selection;
	// individual models may require a specific region instead.
	Project  string `yaml:"project,omitempty" json:"project,omitempty"`
	Location string `yaml:"location,omitempty" json:"location,omitempty"`
	// Disabled keeps the provider configured but takes it out of service:
	// requests 404, catalogs are not refreshed, and the console shows it as
	// disabled until it is re-enabled.
	Disabled bool `yaml:"disabled,omitempty" json:"disabled,omitempty"`
	// ForceApiSupport opts this provider into experimental API adaptation:
	// when a requested model is reachable only via a different OpenAI-family
	// endpoint (e.g. Responses-only gpt-5.5), translate the request instead of
	// letting it fail. Off by default; adaptation is never native to the provider.
	ForceApiSupport bool `yaml:"force_api_support,omitempty" json:"force_api_support,omitempty"`
}

// CategoryMember is one pinned provider/model target in a failover chain.
type CategoryMember struct {
	Provider string `yaml:"provider" json:"provider"`
	Model    string `yaml:"model" json:"model"`
}

// CategoryConfig is an ordered failover chain of pinned real models.
type CategoryConfig struct {
	Failover []CategoryMember `yaml:"failover" json:"failover"`
}

// ProviderPolicy is the per-provider retry + circuit-breaker policy.
type ProviderPolicy struct {
	RetryMaxAttempts           int     `yaml:"retry_max_attempts" json:"retry_max_attempts"`
	RetryInitialBackoffSeconds float64 `yaml:"retry_initial_backoff_seconds" json:"retry_initial_backoff_seconds"`
	RetryMaxBackoffSeconds     float64 `yaml:"retry_max_backoff_seconds" json:"retry_max_backoff_seconds"`
	RetryBackoffMultiplier     float64 `yaml:"retry_backoff_multiplier" json:"retry_backoff_multiplier"`
	CircuitFailureThreshold    int     `yaml:"circuit_failure_threshold" json:"circuit_failure_threshold"`
	CircuitCooldownSeconds     float64 `yaml:"circuit_cooldown_seconds" json:"circuit_cooldown_seconds"`
}

func defaultPolicy() ProviderPolicy {
	return ProviderPolicy{
		RetryMaxAttempts: 1, RetryInitialBackoffSeconds: 0.5, RetryMaxBackoffSeconds: 8.0,
		RetryBackoffMultiplier: 2.0, CircuitFailureThreshold: 0, CircuitCooldownSeconds: 30.0,
	}
}

// RetryEnabled reports whether retry is configured (>1 attempt).
func (p ProviderPolicy) RetryEnabled() bool { return p.RetryMaxAttempts > 1 }

// CircuitEnabled reports whether the circuit breaker is configured.
func (p ProviderPolicy) CircuitEnabled() bool { return p.CircuitFailureThreshold > 0 }

// TimeoutOr returns the provider's configured timeout, or fallback when unset.
func (c *ProviderConfig) timeoutOr(fallback float64) float64 {
	if c.Timeout != nil {
		return *c.Timeout
	}
	return fallback
}

// TimeoutOr is the exported accessor for the per-provider timeout.
func (c *ProviderConfig) TimeoutOr(fallback float64) float64 { return c.timeoutOr(fallback) }

// BackendPolicies is shared defaults + per-provider overrides.
type BackendPolicies struct {
	Defaults  ProviderPolicy            `yaml:"defaults" json:"defaults"`
	Overrides map[string]ProviderPolicy `yaml:"overrides" json:"overrides"`
}

// SavingsConfig controls the usage/cost ledger.
type SavingsConfig struct {
	Enabled       bool                          `yaml:"enabled" json:"enabled"`
	BaselineModel string                        `yaml:"baseline_model" json:"baseline_model"`
	DBPath        string                        `yaml:"db_path" json:"db_path"`
	PriceCatalog  map[string]map[string]float64 `yaml:"price_catalog" json:"price_catalog"`
}

// Settings is the whole runtime configuration.
type Settings struct {
	// auth / server
	APIKey                  string   `yaml:"api_key"`
	APIKeys                 []string `yaml:"api_keys"`
	AllowUnauthenticatedAPI bool     `yaml:"allow_unauthenticated_api"`
	RateLimitPerMinute      int      `yaml:"rate_limit_per_minute"`
	GatewayPreamble         string   `yaml:"gateway_preamble"`
	// AnthropicDiscoveryAliases makes GET /v1/models also list Claude-family
	// models under their bare id (claude-â€¦ / anthropic-â€¦) so Claude Code's
	// gateway model discovery â€” which ignores ids not beginning with "claude" or
	// "anthropic" â€” surfaces them in the /model picker. On by default.
	AnthropicDiscoveryAliases bool `yaml:"anthropic_discovery_aliases"`
	// AnthropicDiscoveryAllModels extends discovery aliases to NON-Claude models
	// (gpt, gemini, â€¦) by prefixing them "claude-<id>" so they pass Claude Code's
	// filter too. The resolver strips the prefix to route. Off by default â€”
	// non-Claude models go through Anthropicâ†’OpenAI translation.
	AnthropicDiscoveryAllModels bool   `yaml:"anthropic_discovery_all_models"`
	SSOEnabled                  bool   `yaml:"sso_enabled"`
	SSOSharedSecret             string `yaml:"-"`
	SSOAdminGroup               string `yaml:"sso_admin_group"`
	SSOAutoProvision            bool   `yaml:"sso_auto_provision"`
	CredentialEncryptionKey     string `yaml:"-"`

	// core
	Providers  map[string]*ProviderConfig `yaml:"providers"`
	Categories map[string]*CategoryConfig `yaml:"categories"`
	Policies   BackendPolicies            `yaml:"policies"`
	Savings    SavingsConfig              `yaml:"savings"`

	// provider-type defaults
	OpenAICompatibleBaseURL        string  `yaml:"openai_compatible_base_url"`
	OpenAICompatibleAPIKey         string  `yaml:"openai_compatible_api_key"`
	OpenAICompatibleTimeoutSeconds float64 `yaml:"openai_compatible_timeout_seconds"`
	OllamaBaseURL                  string  `yaml:"ollama_base_url"`
	OllamaTimeoutSeconds           float64 `yaml:"ollama_timeout_seconds"`
	LiteLLMTimeoutSeconds          float64 `yaml:"litellm_timeout_seconds"`

	// github copilot
	GithubCopilotOAuthToken     string  `yaml:"github_copilot_oauth_token"`
	GithubCopilotUseGhCLI       bool    `yaml:"github_copilot_use_gh_cli"`
	GithubCopilotCacheDir       string  `yaml:"github_copilot_cache_dir"`
	GithubCopilotTimeoutSeconds float64 `yaml:"github_copilot_timeout_seconds"`
	GithubCopilotEditorVersion  string  `yaml:"github_copilot_editor_version"`
	GithubCopilotIntegrationID  string  `yaml:"github_copilot_integration_id"`
	OpenAICodexClientID         string  `yaml:"openai_codex_client_id"`
	AllowCopilotProxy           bool    `yaml:"allow_copilot_proxy"`
}

// Defaults returns a Settings with the same defaults as the Python model.
func Defaults() *Settings {
	return &Settings{
		Providers:  map[string]*ProviderConfig{},
		Categories: map[string]*CategoryConfig{},
		Policies: BackendPolicies{
			Defaults: ProviderPolicy{
				RetryMaxAttempts: 2, RetryInitialBackoffSeconds: 0.5, RetryMaxBackoffSeconds: 8.0,
				RetryBackoffMultiplier: 2.0, CircuitFailureThreshold: 4, CircuitCooldownSeconds: 30.0,
			},
			Overrides: map[string]ProviderPolicy{},
		},
		Savings:                        SavingsConfig{Enabled: true, PriceCatalog: map[string]map[string]float64{}},
		OpenAICompatibleBaseURL:        "https://api.openai.com/v1",
		OpenAICompatibleTimeoutSeconds: 300.0,
		OllamaBaseURL:                  "http://127.0.0.1:11434",
		OllamaTimeoutSeconds:           30.0,
		LiteLLMTimeoutSeconds:          300.0,
		GithubCopilotUseGhCLI:          true,
		GithubCopilotTimeoutSeconds:    300.0,
		GithubCopilotEditorVersion:     "vscode/1.95.3",
		GithubCopilotIntegrationID:     "vscode-chat",
		AnthropicDiscoveryAliases:      true,
		SSOAdminGroup:                  "llmgw-admin",
		SSOAutoProvision:               true,
	}
}

// ---- singleton + mutex -------------------------------------------------- //

var (
	mu      sync.RWMutex
	current = Defaults()
)

// Get returns a read lock's snapshot pointer. Callers must not mutate without
// Update. For hot-path reads we return the live pointer under the caller's
// responsibility to treat it as read-only.
func Get() *Settings {
	mu.RLock()
	defer mu.RUnlock()
	return current
}

// Update applies fn under the write lock.
func Update(fn func(*Settings)) {
	mu.Lock()
	defer mu.Unlock()
	fn(current)
}

// ---- paths -------------------------------------------------------------- //

func StateDir() string {
	if v := os.Getenv("LLMGW_STATE_DIR"); v != "" {
		return expandUser(v)
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".llmgw")
}

func ConfigFilePath() string {
	if v := os.Getenv("LLMGW_CONFIG"); v != "" {
		return expandUser(v)
	}
	return filepath.Join(StateDir(), "config.yaml")
}

func secretsFilePath() string { return filepath.Join(StateDir(), "secrets.json") }
func keysFilePath() string    { return filepath.Join(StateDir(), "keys.json") }

func expandUser(p string) string {
	if strings.HasPrefix(p, "~") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[1:])
	}
	return p
}

// ---- secrets store ------------------------------------------------------ //

func LoadSecrets() map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile(secretsFilePath())
	if err != nil {
		return out
	}
	var raw map[string]any
	if json.Unmarshal(b, &raw) != nil {
		return out
	}
	for k, v := range raw {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

func writeSecrets(data map[string]string) {
	_ = os.MkdirAll(StateDir(), 0o755)
	b, _ := json.MarshalIndent(data, "", "  ")
	_ = os.WriteFile(secretsFilePath(), b, 0o600)
}

func SaveSecret(providerID, apiKey string) {
	data := LoadSecrets()
	if apiKey != "" {
		data[providerID] = apiKey
	} else {
		delete(data, providerID)
	}
	writeSecrets(data)
}

func DeleteSecret(providerID string) {
	data := LoadSecrets()
	if _, ok := data[providerID]; ok {
		delete(data, providerID)
		writeSecrets(data)
	}
}

var envRef = regexp.MustCompile(`^\$\{ENV:([A-Z][A-Z0-9_]*)\}$`)

func resolveEnv(v string) string {
	if m := envRef.FindStringSubmatch(strings.TrimSpace(v)); m != nil {
		return os.Getenv(m[1])
	}
	return v
}

// ResolveProviderAPIKey: inline ${ENV:} / literal wins, else the secrets store.
func ResolveProviderAPIKey(providerID string, cfg *ProviderConfig) string {
	if cfg != nil && cfg.APIKey != "" {
		if r := resolveEnv(cfg.APIKey); r != "" {
			return r
		}
	}
	return LoadSecrets()[providerID]
}

// ---- YAML load / save --------------------------------------------------- //

func readYAML(path string) map[string]any {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var payload map[string]any
	if yaml.Unmarshal(b, &payload) != nil {
		return nil
	}
	return payload
}

// Load reads config.yaml over the defaults, applies ${ENV:} resolution to
// provider base_url/api_key, layers LLMGW_* env overrides on top, and installs
// it as the singleton.
func Load() *Settings {
	s := Defaults()
	payload := readYAML(ConfigFilePath())
	applyConfig(s, payload)
	applyEnv(s)
	mu.Lock()
	current = s
	mu.Unlock()
	return s
}

func envBool(key string, dst *bool) {
	if v, ok := os.LookupEnv(key); ok {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on":
			*dst = true
		case "0", "false", "no", "off", "":
			*dst = false
		}
	}
}

func envStr(key string, dst *string) {
	if v, ok := os.LookupEnv(key); ok {
		*dst = v
	}
}

func envFloat(key string, dst *float64) {
	if v, ok := os.LookupEnv(key); ok {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			*dst = f
		}
	}
}

// applyEnv layers LLMGW_* environment overrides onto settings, mirroring the
// Python pydantic BaseSettings(env_prefix="LLMGW_").
func applyEnv(s *Settings) {
	envStr("LLMGW_API_KEY", &s.APIKey)
	if v, ok := os.LookupEnv("LLMGW_API_KEYS"); ok && v != "" {
		s.APIKeys = strings.Split(v, ",")
	}
	envBool("LLMGW_ALLOW_UNAUTHENTICATED_API", &s.AllowUnauthenticatedAPI)
	if v, ok := os.LookupEnv("LLMGW_RATE_LIMIT_PER_MINUTE"); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			s.RateLimitPerMinute = n
		}
	}
	envStr("LLMGW_GATEWAY_PREAMBLE", &s.GatewayPreamble)
	envBool("LLMGW_ANTHROPIC_DISCOVERY_ALIASES", &s.AnthropicDiscoveryAliases)
	envBool("LLMGW_ANTHROPIC_DISCOVERY_ALL_MODELS", &s.AnthropicDiscoveryAllModels)
	envBool("LLMGW_SSO_ENABLED", &s.SSOEnabled)
	envStr("LLMGW_SSO_SHARED_SECRET", &s.SSOSharedSecret)
	envStr("LLMGW_SSO_ADMIN_GROUP", &s.SSOAdminGroup)
	envBool("LLMGW_SSO_AUTO_PROVISION", &s.SSOAutoProvision)
	envStr("LLMGW_CREDENTIAL_ENCRYPTION_KEY", &s.CredentialEncryptionKey)
	envStr("LLMGW_OPENAI_COMPATIBLE_BASE_URL", &s.OpenAICompatibleBaseURL)
	envStr("LLMGW_OPENAI_COMPATIBLE_API_KEY", &s.OpenAICompatibleAPIKey)
	envFloat("LLMGW_OPENAI_COMPATIBLE_TIMEOUT_SECONDS", &s.OpenAICompatibleTimeoutSeconds)
	envStr("LLMGW_OLLAMA_BASE_URL", &s.OllamaBaseURL)
	envFloat("LLMGW_OLLAMA_TIMEOUT_SECONDS", &s.OllamaTimeoutSeconds)
	envStr("LLMGW_GITHUB_COPILOT_OAUTH_TOKEN", &s.GithubCopilotOAuthToken)
	envBool("LLMGW_GITHUB_COPILOT_USE_GH_CLI", &s.GithubCopilotUseGhCLI)
	envStr("LLMGW_GITHUB_COPILOT_CACHE_DIR", &s.GithubCopilotCacheDir)
	envFloat("LLMGW_GITHUB_COPILOT_TIMEOUT_SECONDS", &s.GithubCopilotTimeoutSeconds)
	envStr("LLMGW_GITHUB_COPILOT_EDITOR_VERSION", &s.GithubCopilotEditorVersion)
	envStr("LLMGW_GITHUB_COPILOT_INTEGRATION_ID", &s.GithubCopilotIntegrationID)
	envStr("LLMGW_OPENAI_CODEX_CLIENT_ID", &s.OpenAICodexClientID)
	envBool("LLMGW_ALLOW_COPILOT_PROXY", &s.AllowCopilotProxy)
}

func applyConfig(s *Settings, payload map[string]any) {
	if payload == nil {
		return
	}
	// Re-marshal the payload sections into typed structs via yaml round-trip.
	if raw, ok := payload["providers"].(map[string]any); ok {
		s.Providers = map[string]*ProviderConfig{}
		for pid, pc := range raw {
			m, ok := pc.(map[string]any)
			if !ok {
				continue
			}
			cfg := &ProviderConfig{}
			if v, ok := m["type"].(string); ok {
				cfg.Type = v
			}
			if v, ok := m["registry_id"].(string); ok {
				cfg.RegistryID = v
			}
			if v, ok := m["base_url"].(string); ok {
				cfg.BaseURL = resolveEnv(v)
			}
			if v, ok := m["api_key"].(string); ok {
				cfg.APIKey = resolveEnv(v)
			}
			if v, ok := m["region"].(string); ok {
				cfg.Region = v
			}
			if v, ok := m["default_voice"].(string); ok {
				cfg.DefaultVoice = v
			}
			if v, ok := m["project"].(string); ok {
				cfg.Project = v
			}
			if v, ok := m["location"].(string); ok {
				cfg.Location = v
			}
			if v, ok := m["disabled"].(bool); ok {
				cfg.Disabled = v
			}
			if v, ok := toFloat(m["timeout"]); ok {
				cfg.Timeout = &v
			}
			if v, ok := m["force_api_support"].(bool); ok {
				cfg.ForceApiSupport = v
			}
			s.Providers[pid] = cfg
		}
	}
	if raw, ok := payload["categories"].(map[string]any); ok {
		s.Categories = map[string]*CategoryConfig{}
		for name, cc := range raw {
			cat := &CategoryConfig{}
			switch v := cc.(type) {
			case map[string]any:
				if fo, ok := v["failover"].([]any); ok {
					cat.Failover = parseMembers(fo)
				}
			case []any:
				cat.Failover = parseMembers(v)
			}
			s.Categories[name] = cat
		}
	}
	applyScalars(s, payload)
}

func parseMembers(items []any) []CategoryMember {
	var out []CategoryMember
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		prov, _ := m["provider"].(string)
		model, _ := m["model"].(string)
		if prov != "" && model != "" {
			out = append(out, CategoryMember{Provider: prov, Model: model})
		}
	}
	return out
}

func applyScalars(s *Settings, p map[string]any) {
	str := func(k string, dst *string) {
		if v, ok := p[k].(string); ok {
			*dst = v
		}
	}
	b := func(k string, dst *bool) {
		if v, ok := p[k].(bool); ok {
			*dst = v
		}
	}
	f := func(k string, dst *float64) {
		if v, ok := toFloat(p[k]); ok {
			*dst = v
		}
	}
	str("api_key", &s.APIKey)
	if v, ok := p["api_keys"].([]any); ok {
		s.APIKeys = nil
		for _, it := range v {
			if str, ok := it.(string); ok {
				s.APIKeys = append(s.APIKeys, str)
			}
		}
	}
	b("allow_unauthenticated_api", &s.AllowUnauthenticatedAPI)
	if v, ok := toFloat(p["rate_limit_per_minute"]); ok {
		s.RateLimitPerMinute = int(v)
	}
	str("gateway_preamble", &s.GatewayPreamble)
	b("anthropic_discovery_aliases", &s.AnthropicDiscoveryAliases)
	b("anthropic_discovery_all_models", &s.AnthropicDiscoveryAllModels)
	b("sso_enabled", &s.SSOEnabled)
	str("sso_admin_group", &s.SSOAdminGroup)
	b("sso_auto_provision", &s.SSOAutoProvision)
	str("openai_compatible_base_url", &s.OpenAICompatibleBaseURL)
	str("openai_compatible_api_key", &s.OpenAICompatibleAPIKey)
	f("openai_compatible_timeout_seconds", &s.OpenAICompatibleTimeoutSeconds)
	str("ollama_base_url", &s.OllamaBaseURL)
	f("ollama_timeout_seconds", &s.OllamaTimeoutSeconds)
	f("litellm_timeout_seconds", &s.LiteLLMTimeoutSeconds)
	b("github_copilot_use_gh_cli", &s.GithubCopilotUseGhCLI)
	str("github_copilot_cache_dir", &s.GithubCopilotCacheDir)
	f("github_copilot_timeout_seconds", &s.GithubCopilotTimeoutSeconds)
	str("github_copilot_editor_version", &s.GithubCopilotEditorVersion)
	str("github_copilot_integration_id", &s.GithubCopilotIntegrationID)
	str("openai_codex_client_id", &s.OpenAICodexClientID)
	b("allow_copilot_proxy", &s.AllowCopilotProxy)
	// savings block
	if raw, ok := p["savings"].(map[string]any); ok {
		if v, ok := raw["enabled"].(bool); ok {
			s.Savings.Enabled = v
		}
		if v, ok := raw["baseline_model"].(string); ok {
			s.Savings.BaselineModel = v
		}
		if v, ok := raw["db_path"].(string); ok {
			s.Savings.DBPath = v
		}
	}
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case string:
		if f, err := strconv.ParseFloat(n, 64); err == nil {
			return f, true
		}
	}
	return 0, false
}

// configPayload builds the on-disk YAML (providers WITHOUT api keys).
func configPayload(s *Settings) map[string]any {
	providers := map[string]any{}
	for pid, pc := range s.Providers {
		entry := map[string]any{"type": pc.Type}
		if pc.RegistryID != "" {
			entry["registry_id"] = pc.RegistryID
		}
		if pc.BaseURL != "" {
			entry["base_url"] = pc.BaseURL
		}
		if pc.Region != "" {
			entry["region"] = pc.Region
		}
		if pc.DefaultVoice != "" {
			entry["default_voice"] = pc.DefaultVoice
		}
		if pc.Project != "" {
			entry["project"] = pc.Project
		}
		if pc.Location != "" {
			entry["location"] = pc.Location
		}
		if pc.Disabled {
			entry["disabled"] = true
		}
		if pc.Timeout != nil {
			entry["timeout"] = *pc.Timeout
		}
		if pc.ForceApiSupport {
			entry["force_api_support"] = true
		}
		providers[pid] = entry
	}
	categories := map[string]any{}
	for name, cat := range s.Categories {
		fo := []any{}
		for _, m := range cat.Failover {
			fo = append(fo, map[string]any{"provider": m.Provider, "model": m.Model})
		}
		categories[name] = map[string]any{"failover": fo}
	}
	payload := map[string]any{
		"providers":  providers,
		"categories": categories,
		"policies":   s.Policies,
		"savings":    s.Savings,
	}
	if s.OpenAICodexClientID != "" {
		payload["openai_codex_client_id"] = s.OpenAICodexClientID
	}
	return payload
}

// Save persists providers + categories + policies + savings (never keys).
func Save() error {
	mu.RLock()
	payload := configPayload(current)
	mu.RUnlock()
	_ = os.MkdirAll(StateDir(), 0o755)
	b, err := yaml.Marshal(payload)
	if err != nil {
		return err
	}
	path := ConfigFilePath()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
