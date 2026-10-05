// Package config is the single source of truth for the gateway's runtime state:
// providers, endpoints, minted keys, provider secrets, and scalar settings.
//
// Mirrors the Python llmgw.config module. Keys never live in the committed
// config file â€” they live in a 0600 secrets.json / keys.json under the state
// dir (~/.llmgw by default, override with LLMGW_STATE_DIR).
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"gopkg.in/yaml.v3"
)

// ProviderConfig is one connected upstream.
type ProviderConfig struct {
	Type       string `yaml:"type" json:"type"`
	RegistryID string `yaml:"registry_id,omitempty" json:"registry_id,omitempty"`
	BaseURL    string `yaml:"base_url,omitempty" json:"base_url,omitempty"`
	APIKey     string `yaml:"api_key,omitempty" json:"-"`
	// FileAPIKey is api_key as the configuration file writes it, a literal
	// or an ${ENV:NAME} reference. A save keeps the file's api_key only while
	// this is set, so clearing it, as a key entered in the console does,
	// removes the configured key instead of letting it win after a restart.
	FileAPIKey          string   `yaml:"-" json:"-"`
	Region              string   `yaml:"region,omitempty" json:"region,omitempty"`
	Timeout             *float64 `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	PublicOAuthClientID string   `yaml:"public_oauth_client_id,omitempty" json:"public_oauth_client_id,omitempty"`
	// DefaultVoice selects the voice used by speech-synthesis providers when
	// a request names the provider without a specific voice.
	DefaultVoice string `yaml:"default_voice,omitempty" json:"default_voice,omitempty"`
	// Project and Location scope a Vertex AI provider. Location defaults to the
	// multi-region "global" endpoint, which carries the widest model selection;
	// individual models may require a specific region instead.
	Project  string `yaml:"project,omitempty" json:"project,omitempty"`
	Location string `yaml:"location,omitempty" json:"location,omitempty"`
	// VertexRequestType optionally selects Google's request accounting path.
	// Empty preserves the provider default; dedicated requires provisioned throughput.
	VertexRequestType string `yaml:"vertex_request_type,omitempty" json:"vertex_request_type,omitempty"`
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

// EndpointMember is one pinned provider/model target in a failover chain.
type EndpointMember struct {
	Provider        string `yaml:"provider" json:"provider"`
	Model           string `yaml:"model" json:"model"`
	AllowUnverified bool   `yaml:"allow_unverified,omitempty" json:"allow_unverified,omitempty"`
}

// EndpointConfig is an ordered failover chain of pinned real models. A client
// requests the endpoint name (e.g. "smart") and the gateway cascades through
// this chain until one pinned provider/model succeeds.
type EndpointConfig struct {
	Failover []EndpointMember `yaml:"failover" json:"failover"`
}

// Deprecated: use EndpointConfig. Retained until client evidence and a
// documented release boundary make removal safe.
type CategoryConfig = EndpointConfig

// Deprecated: use EndpointMember. See CategoryConfig.
type CategoryMember = EndpointMember

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
	Defaults       ProviderPolicy            `yaml:"defaults" json:"defaults"`
	Overrides      map[string]ProviderPolicy `yaml:"overrides" json:"overrides"`
	OverrideFields map[string]map[string]any `yaml:"-" json:"-"`
}

func (p BackendPolicies) ConfiguredOverrides() map[string]any {
	configured := map[string]any{}
	for providerID, policy := range p.Overrides {
		if fields, ok := p.OverrideFields[providerID]; ok {
			configured[providerID] = cloneStringAnyMap(fields)
		} else {
			configured[providerID] = policy
		}
	}
	return configured
}

// SavingsConfig controls the usage/cost ledger. Unset fields stay out of a
// saved file, so a save does not add empty keys under an operator's savings
// block.
type SavingsConfig struct {
	Enabled       bool                          `yaml:"enabled" json:"enabled"`
	BaselineModel string                        `yaml:"baseline_model,omitempty" json:"baseline_model"`
	DBPath        string                        `yaml:"db_path,omitempty" json:"db_path"`
	PriceCatalog  map[string]map[string]float64 `yaml:"price_catalog,omitempty" json:"price_catalog"`
}

// Settings is the whole runtime configuration.
type Settings struct {
	// auth / server
	APIKey                  string   `yaml:"api_key"`
	APIKeys                 []string `yaml:"api_keys"`
	AllowUnauthenticatedAPI bool     `yaml:"allow_unauthenticated_api"`
	// AdminKeysOnDataPlane lets the static administrator keys authenticate
	// /v1 requests. It is deprecated: the next major release refuses them
	// there by default.
	AdminKeysOnDataPlane bool   `yaml:"admin_keys_on_data_plane"`
	RateLimitPerMinute   int    `yaml:"rate_limit_per_minute"`
	GatewayPreamble      string `yaml:"gateway_preamble"`
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
	Providers map[string]*ProviderConfig `yaml:"providers"`
	Endpoints map[string]*EndpointConfig `yaml:"endpoints"`
	Policies  BackendPolicies            `yaml:"policies"`
	Savings   SavingsConfig              `yaml:"savings"`

	// provider-type defaults
	OpenAICompatibleBaseURL        string  `yaml:"openai_compatible_base_url"`
	OpenAICompatibleAPIKey         string  `yaml:"openai_compatible_api_key"`
	OpenAICompatibleTimeoutSeconds float64 `yaml:"openai_compatible_timeout_seconds"`
	OllamaBaseURL                  string  `yaml:"ollama_base_url"`
	OllamaTimeoutSeconds           float64 `yaml:"ollama_timeout_seconds"`
	LiteLLMTimeoutSeconds          float64 `yaml:"litellm_timeout_seconds"`

	// github copilot
	GithubCopilotOAuthToken       string  `yaml:"github_copilot_oauth_token"`
	GithubCopilotUseGhCLI         bool    `yaml:"github_copilot_use_gh_cli"`
	GithubCopilotCacheDir         string  `yaml:"github_copilot_cache_dir"`
	GithubCopilotTimeoutSeconds   float64 `yaml:"github_copilot_timeout_seconds"`
	GithubCopilotEditorVersion    string  `yaml:"github_copilot_editor_version"`
	GithubCopilotIntegrationID    string  `yaml:"github_copilot_integration_id"`
	OpenAICodexClientID           string  `yaml:"openai_codex_client_id"`
	GoogleAntigravityClientID     string  `yaml:"-"`
	GoogleAntigravityClientSecret string  `yaml:"-"`
	GoogleAntigravityOAuthProfile string  `yaml:"-"`
	GoogleAntigravityClientMode   string  `yaml:"-"`
	GoogleAntigravityRedirectURI  string  `yaml:"-"`
	OAuthPublicBaseURL            string  `yaml:"-"`
	AllowCopilotProxy             bool    `yaml:"allow_copilot_proxy"`
}

// Defaults returns a Settings with the same defaults as the Python model.
func Defaults() *Settings {
	return &Settings{
		Providers: map[string]*ProviderConfig{},
		Endpoints: map[string]*EndpointConfig{},
		Policies: BackendPolicies{
			Defaults: ProviderPolicy{
				RetryMaxAttempts: 2, RetryInitialBackoffSeconds: 0.5, RetryMaxBackoffSeconds: 8.0,
				RetryBackoffMultiplier: 2.0, CircuitFailureThreshold: 4, CircuitCooldownSeconds: 30.0,
			},
			Overrides:      map[string]ProviderPolicy{},
			OverrideFields: map[string]map[string]any{},
		},
		Savings:                        SavingsConfig{Enabled: false, PriceCatalog: map[string]map[string]float64{}},
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
		AdminKeysOnDataPlane:           true,
		SSOAdminGroup:                  "llmgw-admin",
		SSOAutoProvision:               true,
	}
}

// ---- published settings ------------------------------------------------- //

// published pairs one settings value with the generation it was published
// under. It is replaced as a whole, so a single atomic load always yields a
// settings value and the generation that belongs to it.
type published struct {
	settings   *Settings
	generation uint64
}

var (
	// writerMu serializes writers. Each writer deep-copies the published
	// settings, edits its private copy and publishes that under the next
	// generation, so a published value is never written again. Readers
	// never take the mutex: request paths range over Providers and other
	// maps without a lock, and an in-place write would race them into
	// Go's unrecoverable concurrent map access fault.
	writerMu sync.Mutex
	state    atomic.Pointer[published]
)

// ErrRestoreSuperseded is returned by the restore function of UpdateAndSave
// when another change was published after the one it would undo.
var ErrRestoreSuperseded = errors.New("configuration changed after this update; restore skipped")

func init() {
	// Generation 1 matches llmgw-core's reference settings source; any
	// increase after it tells a consumer the settings changed.
	state.Store(&published{settings: Defaults(), generation: 1})
}

// publishLocked installs next under the generation after the current one.
// The caller holds writerMu and must not write to next afterwards.
func publishLocked(next *Settings) uint64 {
	generation := state.Load().generation + 1
	state.Store(&published{settings: next, generation: generation})
	return generation
}

// Get returns the current settings without locking. The value is immutable
// once published: writers publish a new value instead of editing this one,
// so a caller may keep and range over it while settings change, and a kept
// value does not see later changes. Callers must treat it as read-only and
// change settings through Update or UpdateAndSave.
func Get() *Settings {
	return state.Load().settings
}

// Snapshot returns the current settings and the generation they were
// published under. Both come from one atomic load, so they always belong
// together. The generation increases with every published change.
func Snapshot() (*Settings, uint64) {
	current := state.Load()
	return current.settings, current.generation
}

// Generation returns the generation of the current settings.
func Generation() uint64 {
	return state.Load().generation
}

// Source exposes the process-wide settings as a snapshot source, such as
// llmgw-core's runtime.SettingsSource, without this package depending on
// the consumer.
type Source struct{}

// Snapshot returns the current settings and their generation.
func (Source) Snapshot() (*Settings, uint64) { return Snapshot() }

// Provider returns an isolated copy of one provider's configuration for
// background workers that must not retain the published settings.
func Provider(id string) (*ProviderConfig, bool) {
	provider := Get().Providers[id]
	if provider == nil {
		return nil, false
	}
	return cloneProvider(provider), true
}

// Update applies fn to a private deep copy of the current settings and
// publishes the copy under a new generation. fn edits the copy, never a
// value a reader may hold, and must not keep it: once Update returns, the
// copy is published and must not change.
func Update(fn func(*Settings)) {
	writerMu.Lock()
	defer writerMu.Unlock()
	next := cloneSettings(state.Load().settings)
	fn(next)
	publishLocked(next)
}

// UpdateAndSave applies fn to a private deep copy of the current settings,
// persists the copy, and only then publishes it under a new generation. A
// failed fn or save publishes nothing, so the live configuration is intact.
//
// The returned restore function republishes the previous settings under
// another new generation and persists them. It is a compare-and-swap: if
// any change was published after this one, restore leaves memory and disk
// untouched and returns ErrRestoreSuperseded, because rolling back would
// silently discard that newer change. Callers already report a failed
// restore as a rollback that did not complete.
func UpdateAndSave(fn func(*Settings) error) (func() error, error) {
	writerMu.Lock()
	defer writerMu.Unlock()
	previous := state.Load().settings
	next := cloneSettings(previous)
	if err := fn(next); err != nil {
		return nil, err
	}
	path := ConfigFilePath()
	original, err := readConfigFile(path)
	if err != nil {
		return nil, fmt.Errorf("configuration not saved: %w", err)
	}
	if err := writeSettings(path, original, next); err != nil {
		return nil, err
	}
	generation := publishLocked(next)
	return func() error {
		writerMu.Lock()
		defer writerMu.Unlock()
		if state.Load().generation != generation {
			return ErrRestoreSuperseded
		}
		// Memory is restored even when the write fails, so the running
		// gateway holds the settings the caller meant to keep; the error
		// tells the caller the file may still hold the change.
		err := restoreFile(path, original, previous)
		publishLocked(cloneSettings(previous))
		return err
	}, nil
}

// restoreFile writes previous back over the file an update replaced. It
// builds on original, the content the update read, so whatever the update
// dropped along with an entry, such as a deleted provider's api_key, comes
// back with it. Like every writer it refuses to replace a file that no
// longer parses.
func restoreFile(path string, original []byte, previous *Settings) error {
	if _, err := readYAML(path); err != nil {
		return fmt.Errorf("configuration not saved: %w", err)
	}
	return writeSettings(path, original, previous)
}

// cloneSettings deep-copies every reference a writer could reach, so edits
// to the copy never show through a published value.
func cloneSettings(source *Settings) *Settings {
	next := *source
	next.APIKeys = append([]string(nil), source.APIKeys...)
	next.Providers = make(map[string]*ProviderConfig, len(source.Providers))
	for id, provider := range source.Providers {
		next.Providers[id] = cloneProvider(provider)
	}
	next.Endpoints = make(map[string]*EndpointConfig, len(source.Endpoints))
	for name, endpoint := range source.Endpoints {
		if endpoint == nil {
			next.Endpoints[name] = nil
			continue
		}
		copy := *endpoint
		copy.Failover = append([]EndpointMember(nil), endpoint.Failover...)
		next.Endpoints[name] = &copy
	}
	next.Policies.Overrides = make(map[string]ProviderPolicy, len(source.Policies.Overrides))
	for id, policy := range source.Policies.Overrides {
		next.Policies.Overrides[id] = policy
	}
	next.Policies.OverrideFields = make(map[string]map[string]any, len(source.Policies.OverrideFields))
	for id, fields := range source.Policies.OverrideFields {
		next.Policies.OverrideFields[id] = cloneStringAnyMap(fields)
	}
	next.Savings.PriceCatalog = make(map[string]map[string]float64, len(source.Savings.PriceCatalog))
	for model, prices := range source.Savings.PriceCatalog {
		copy := make(map[string]float64, len(prices))
		for unit, price := range prices {
			copy[unit] = price
		}
		next.Savings.PriceCatalog[model] = copy
	}
	return &next
}

// cloneProvider copies one provider, including the timeout it points to.
func cloneProvider(provider *ProviderConfig) *ProviderConfig {
	if provider == nil {
		return nil
	}
	copy := *provider
	if provider.Timeout != nil {
		timeout := *provider.Timeout
		copy.Timeout = &timeout
	}
	return &copy
}

// AddProviderIfMissing persists one provider without overwriting an instance
// another administrator or automation run created concurrently. It adds the
// one entry to the file and leaves the rest of the file as it is.
func AddProviderIfMissing(id string, provider *ProviderConfig) (bool, error) {
	writerMu.Lock()
	defer writerMu.Unlock()
	if provider == nil {
		return false, fmt.Errorf("provider is required")
	}
	current := state.Load().settings
	if current.Providers[id] != nil {
		return false, nil
	}
	path := ConfigFilePath()
	raw, err := readConfigFile(path)
	if err != nil {
		return false, fmt.Errorf("configuration not saved: %w", err)
	}
	document, _, err := parseYAML(path, raw)
	if err != nil {
		return false, fmt.Errorf("configuration not saved: %w", err)
	}
	root := rootMapping(document)
	providers := unaliased(mappingValue(root, "providers"))
	switch {
	case providers == nil || providers.Tag == "!!null":
		providers = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	case providers.Kind != yaml.MappingNode:
		return false, fmt.Errorf("existing providers configuration has an invalid shape")
	case mappingValue(providers, id) != nil:
		return false, nil
	}
	entry, err := encodeNode(providerConfigPayload(provider))
	if err != nil {
		return false, err
	}
	providers.Content = append(providers.Content, keyNode(id), entry)
	setMappingValue(root, "providers", providers)
	if err := writeDocument(path, document); err != nil {
		return false, err
	}
	next := cloneSettings(current)
	next.Providers[id] = cloneProvider(provider)
	publishLocked(next)
	return true, nil
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

// secretsMu serializes the read-modify-write cycles on secrets.json, so two
// concurrent saves cannot each write back a copy that lacks the other's key.
// Readers need no lock: a write replaces the file in one rename.
var secretsMu sync.Mutex

// LoadSecrets returns the stored plaintext keys; a missing or unreadable file
// reads as empty.
func LoadSecrets() map[string]string {
	out, err := readSecrets()
	if err != nil {
		return map[string]string{}
	}
	return out
}

// readSecrets is LoadSecrets that reports why a present file could not be
// read, so a writer never replaces a file whose keys it did not see.
func readSecrets() (map[string]string, error) {
	out := map[string]string{}
	b, err := os.ReadFile(secretsFilePath())
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	if json.Unmarshal(b, &raw) != nil {
		// The decoder's message can quote the file, and the file holds keys.
		return nil, errors.New("secrets.json is not a JSON object")
	}
	for k, v := range raw {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out, nil
}

// writeSecrets replaces secrets.json through a synced temporary file in the
// same directory, so a crash or a full disk leaves the previous file or the
// new one, never a truncated mix of both.
func writeSecrets(data map[string]string) error {
	path := secretsFilePath()
	// An empty store is removed rather than kept as an empty file, so a state
	// directory whose keys all moved into the encrypted store has no secrets
	// file left.
	if len(data) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".secrets-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// SaveSecret stores a provider's plaintext key, or removes it when apiKey is
// empty.
func SaveSecret(providerID, apiKey string) error {
	secretsMu.Lock()
	defer secretsMu.Unlock()
	data, err := readSecrets()
	if err != nil {
		return err
	}
	if apiKey != "" {
		data[providerID] = apiKey
	} else {
		delete(data, providerID)
	}
	return writeSecrets(data)
}

// DeleteSecret removes a provider's plaintext key; a missing key is not an
// error.
func DeleteSecret(providerID string) error {
	secretsMu.Lock()
	defer secretsMu.Unlock()
	data, err := readSecrets()
	if err != nil {
		return err
	}
	if _, ok := data[providerID]; !ok {
		return nil
	}
	delete(data, providerID)
	return writeSecrets(data)
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

// readConfigFile returns the content of the configuration file at path, nil
// when there is no file.
func readConfigFile(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read configuration: %w", err)
	}
	return raw, nil
}

// readYAML reads and parses one configuration file. A missing file yields
// no payload and no error, because the gateway then runs on its defaults; a
// file that exists but cannot be read or parsed is an error.
func readYAML(path string) (map[string]any, error) {
	raw, err := readConfigFile(path)
	if err != nil || raw == nil {
		return nil, err
	}
	_, payload, err := parseYAML(path, raw)
	return payload, err
}

// parseYAML parses configuration content read from path into its document
// and the payload the loader applies. An empty or null document is the
// defaults. Errors name the file and the position, and never quote a value:
// a configuration file can hold credentials, and yaml's own message for a
// document that is not a mapping quotes its start.
func parseYAML(path string, raw []byte) (*yaml.Node, map[string]any, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(raw, &document); err != nil {
		return nil, nil, fmt.Errorf("parse configuration %s: %w", path, err)
	}
	if len(document.Content) == 0 || document.Content[0].Tag == "!!null" {
		return &document, nil, nil
	}
	if root := document.Content[0]; root.Kind != yaml.MappingNode {
		return nil, nil, fmt.Errorf("parse configuration %s: line %d, column %d: settings must be a mapping", path, root.Line, root.Column)
	}
	var payload map[string]any
	if err := document.Decode(&payload); err != nil {
		return nil, nil, fmt.Errorf("parse configuration %s: %w", path, err)
	}
	// A setting the loader rejects makes the file as unusable as a syntax
	// error does, so startup and every writer refuse it the same way.
	if err := applyConfig(Defaults(), payload); err != nil {
		return nil, nil, fmt.Errorf("parse configuration %s: %w", path, err)
	}
	return &document, payload, nil
}

// Load reads config.yaml over the defaults, applies ${ENV:} resolution to
// provider base_url/api_key, layers LLMGW_* env overrides on top, and
// publishes the result under a new generation. A missing file means the
// defaults. A file that cannot be read or parsed, or a configured seed that
// cannot be used, is returned as an error and nothing is published: running
// on the defaults instead would look like a healthy start while every
// configured provider and endpoint is missing. It holds the writer mutex
// while it reads, so a concurrent UpdateAndSave cannot land between the read
// and the publish and be lost. The returned value is the published one and
// is as read-only as Get's.
func Load() (*Settings, error) {
	writerMu.Lock()
	defer writerMu.Unlock()
	if err := seedConfigIfMissing(); err != nil {
		return nil, err
	}
	payload, err := readYAML(ConfigFilePath())
	if err != nil {
		return nil, err
	}
	s := Defaults()
	if err := applyConfig(s, payload); err != nil {
		return nil, fmt.Errorf("parse configuration %s: %w", ConfigFilePath(), err)
	}
	applyEnv(s)
	publishLocked(s)
	return s, nil
}

// seedConfigIfMissing copies LLMGW_CONFIG_SEED to the configuration path
// when that path does not exist yet. A seed that cannot be read or parsed is
// refused before anything is copied, so it never becomes a configuration
// file that every later start refuses as well.
func seedConfigIfMissing() error {
	seed := strings.TrimSpace(os.Getenv("LLMGW_CONFIG_SEED"))
	target := ConfigFilePath()
	if seed == "" || filepath.Clean(expandUser(seed)) == filepath.Clean(target) {
		return nil
	}
	if _, err := os.Stat(target); err == nil || !os.IsNotExist(err) {
		return nil
	}
	raw, err := os.ReadFile(expandUser(seed))
	if err != nil {
		return fmt.Errorf("read configuration seed: %w", err)
	}
	if _, _, err := parseYAML(expandUser(seed), raw); err != nil {
		return err
	}
	copyFailed := func(err error) error {
		return fmt.Errorf("copy configuration seed to %s: %w", target, err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return copyFailed(err)
	}
	temp, err := os.CreateTemp(filepath.Dir(target), ".config-seed-*.tmp")
	if err != nil {
		return copyFailed(err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if _, err := temp.Write(raw); err != nil {
		_ = temp.Close()
		return copyFailed(err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return copyFailed(err)
	}
	if err := temp.Close(); err != nil {
		return copyFailed(err)
	}
	if err := os.Rename(tempPath, target); err != nil {
		return copyFailed(err)
	}
	return nil
}

// ReadFile parses one configuration file without changing the process-wide
// settings or applying environment overrides. Maintenance uses it to derive
// destinations from the configuration stored inside a backup archive. A
// file that cannot be parsed is an error rather than the defaults, because
// default destinations would quietly put restored state where the restored
// configuration does not look for it.
func ReadFile(path string) (*Settings, error) {
	payload, err := readYAML(path)
	if err != nil {
		return nil, err
	}
	s := Defaults()
	if err := applyConfig(s, payload); err != nil {
		return nil, fmt.Errorf("parse configuration %s: %w", path, err)
	}
	return s, nil
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
	envBool("LLMGW_ADMIN_KEYS_ON_DATA_PLANE", &s.AdminKeysOnDataPlane)
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
	envStr("LLMGW_GOOGLE_ANTIGRAVITY_CLIENT_ID", &s.GoogleAntigravityClientID)
	envStr("LLMGW_GOOGLE_ANTIGRAVITY_CLIENT_SECRET", &s.GoogleAntigravityClientSecret)
	envStr("LLMGW_GOOGLE_ANTIGRAVITY_OAUTH_PROFILE", &s.GoogleAntigravityOAuthProfile)
	envStr("LLMGW_GOOGLE_ANTIGRAVITY_CLIENT_MODE", &s.GoogleAntigravityClientMode)
	envStr("LLMGW_GOOGLE_ANTIGRAVITY_REDIRECT_URI", &s.GoogleAntigravityRedirectURI)
	envStr("LLMGW_OAUTH_PUBLIC_BASE_URL", &s.OAuthPublicBaseURL)
	envBool("LLMGW_ALLOW_COPILOT_PROXY", &s.AllowCopilotProxy)
}

func applyConfig(s *Settings, payload map[string]any) error {
	if payload == nil {
		return nil
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
			if v, ok := m["public_oauth_client_id"].(string); ok {
				cfg.PublicOAuthClientID = v
			}
			if v, ok := m["base_url"].(string); ok {
				cfg.BaseURL = resolveEnv(v)
			}
			if v, ok := m["api_key"].(string); ok {
				cfg.APIKey = resolveEnv(v)
				cfg.FileAPIKey = v
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
			if v, ok := m["vertex_request_type"].(string); ok {
				cfg.VertexRequestType = v
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
	// endpoints: is canonical. categories: is the pre-rename key; it is read
	// only as a fallback so config files written before the rename keep
	// loading. When both are present endpoints: wins outright rather than
	// merging, so a mid-migration operator gets a predictable result instead
	// of one that depends on map iteration order.
	if raw, ok := payload["endpoints"].(map[string]any); ok {
		s.Endpoints = parseEndpoints(raw)
	} else if raw, ok := payload["categories"].(map[string]any); ok {
		s.Endpoints = parseEndpoints(raw)
	}
	if raw, ok := payload["policies"].(map[string]any); ok {
		defaults := s.Policies.Defaults
		if values, ok := raw["defaults"].(map[string]any); ok {
			defaults = mergeProviderPolicy(defaults, values)
		}
		overrides := map[string]ProviderPolicy{}
		overrideFields := map[string]map[string]any{}
		if values, ok := raw["overrides"].(map[string]any); ok {
			for providerID, value := range values {
				if fields, ok := value.(map[string]any); ok {
					overrides[providerID] = mergeProviderPolicy(defaults, fields)
					overrideFields[providerID] = cloneStringAnyMap(fields)
				}
			}
		}
		s.Policies = BackendPolicies{Defaults: defaults, Overrides: overrides, OverrideFields: overrideFields}
	}
	applyScalars(s, payload)
	if savings, ok := payload["savings"].(map[string]any); ok {
		catalog, err := parsePriceCatalog(savings["price_catalog"])
		if err != nil {
			return err
		}
		s.Savings.PriceCatalog = catalog
	}
	return nil
}

// parsePriceCatalog reads savings.price_catalog: for each model ID, the
// input and output price in US dollars per million tokens, the unit the
// usage cost is computed in. A price that is missing, not a number, or
// negative is an error rather than a zero, because every usage record would
// then carry a wrong cost without a word, and so would cost quotas.
func parsePriceCatalog(raw any) (map[string]map[string]float64, error) {
	catalog := map[string]map[string]float64{}
	if raw == nil {
		return catalog, nil
	}
	models, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("savings.price_catalog must map model IDs to prices")
	}
	for model, entry := range models {
		fields, ok := entry.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("savings.price_catalog: model %q needs input and output prices", model)
		}
		prices := map[string]float64{}
		for _, field := range []string{"input", "output"} {
			price, ok := toFloat(fields[field])
			if !ok || price < 0 || math.IsNaN(price) || math.IsInf(price, 0) {
				return nil, fmt.Errorf("savings.price_catalog: model %q: %s must be a number of US dollars per million tokens, zero or more", model, field)
			}
			prices[field] = price
		}
		catalog[model] = prices
	}
	return catalog, nil
}

// cloneStringAnyMap deep-copies decoded YAML. The policy fields the gateway
// reads are scalars, but a nested mapping or sequence would otherwise be
// shared between a published value and a writer's copy.
func cloneStringAnyMap(source map[string]any) map[string]any {
	cloned := make(map[string]any, len(source))
	for key, value := range source {
		cloned[key] = cloneAny(value)
	}
	return cloned
}

// cloneAny copies the containers a YAML decode produces and returns every
// other value as is: decoded scalars carry no shared mutable state.
func cloneAny(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		if typed == nil {
			return typed
		}
		return cloneStringAnyMap(typed)
	case map[any]any:
		if typed == nil {
			return typed
		}
		cloned := make(map[any]any, len(typed))
		for key, item := range typed {
			cloned[key] = cloneAny(item)
		}
		return cloned
	case []any:
		if typed == nil {
			return typed
		}
		cloned := make([]any, len(typed))
		for index, item := range typed {
			cloned[index] = cloneAny(item)
		}
		return cloned
	default:
		return value
	}
}

func mergeProviderPolicy(base ProviderPolicy, fields map[string]any) ProviderPolicy {
	if value, ok := toFloat(fields["retry_max_attempts"]); ok {
		base.RetryMaxAttempts = int(value)
	}
	if value, ok := toFloat(fields["retry_initial_backoff_seconds"]); ok {
		base.RetryInitialBackoffSeconds = value
	}
	if value, ok := toFloat(fields["retry_max_backoff_seconds"]); ok {
		base.RetryMaxBackoffSeconds = value
	}
	if value, ok := toFloat(fields["retry_backoff_multiplier"]); ok {
		base.RetryBackoffMultiplier = value
	}
	if value, ok := toFloat(fields["circuit_failure_threshold"]); ok {
		base.CircuitFailureThreshold = int(value)
	}
	if value, ok := toFloat(fields["circuit_cooldown_seconds"]); ok {
		base.CircuitCooldownSeconds = value
	}
	return base
}

func parseEndpoints(raw map[string]any) map[string]*EndpointConfig {
	out := map[string]*EndpointConfig{}
	for name, cc := range raw {
		ep := &EndpointConfig{}
		switch v := cc.(type) {
		case map[string]any:
			if fo, ok := v["failover"].([]any); ok {
				ep.Failover = parseMembers(fo)
			}
		case []any:
			ep.Failover = parseMembers(v)
		}
		out[name] = ep
	}
	return out
}

func parseMembers(items []any) []EndpointMember {
	var out []EndpointMember
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		prov, _ := m["provider"].(string)
		model, _ := m["model"].(string)
		allowUnverified, _ := m["allow_unverified"].(bool)
		if prov != "" && model != "" {
			out = append(out, EndpointMember{Provider: prov, Model: model, AllowUnverified: allowUnverified})
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
	b("admin_keys_on_data_plane", &s.AdminKeysOnDataPlane)
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
		providers[pid] = providerConfigPayload(pc)
	}
	// Always write the canonical endpoints: key so a save quietly migrates a
	// config file that was still on the pre-rename categories: key.
	endpoints := map[string]any{}
	for name, ep := range s.Endpoints {
		fo := []any{}
		for _, m := range ep.Failover {
			member := map[string]any{"provider": m.Provider, "model": m.Model}
			if m.AllowUnverified {
				member["allow_unverified"] = true
			}
			fo = append(fo, member)
		}
		endpoints[name] = map[string]any{"failover": fo}
	}
	payload := map[string]any{
		"providers": providers,
		"endpoints": endpoints,
		"policies": map[string]any{
			"defaults":  s.Policies.Defaults,
			"overrides": s.Policies.ConfiguredOverrides(),
		},
		"savings": s.Savings,
	}
	if s.OpenAICodexClientID != "" {
		payload["openai_codex_client_id"] = s.OpenAICodexClientID
	}
	return payload
}

// Save persists the sections the gateway manages from the current settings
// (see mergeManagedSettings). It holds the writer mutex so the file cannot be
// written between another writer's save and publish.
func Save() error {
	writerMu.Lock()
	defer writerMu.Unlock()
	path := ConfigFilePath()
	raw, err := readConfigFile(path)
	if err != nil {
		return fmt.Errorf("configuration not saved: %w", err)
	}
	return writeSettings(path, raw, state.Load().settings)
}

// providerField is one provider key a save writes: its value, and whether it
// is set. A save removes an unset key from the file.
type providerField struct {
	key   string
	value any
	set   bool
}

// providerFields lists every provider key a save manages. api_key is not
// among them: a save never writes one, and keeps the file's as written,
// ${ENV:NAME} reference or literal, while the provider still carries it (see
// FileAPIKey), because a key entered in the console goes to the credential
// store.
func providerFields(pc *ProviderConfig) []providerField {
	var timeout any
	if pc.Timeout != nil {
		timeout = *pc.Timeout
	}
	return []providerField{
		{"type", pc.Type, true},
		{"registry_id", pc.RegistryID, pc.RegistryID != ""},
		{"public_oauth_client_id", pc.PublicOAuthClientID, pc.PublicOAuthClientID != ""},
		{"base_url", pc.BaseURL, pc.BaseURL != ""},
		{"region", pc.Region, pc.Region != ""},
		{"default_voice", pc.DefaultVoice, pc.DefaultVoice != ""},
		{"project", pc.Project, pc.Project != ""},
		{"location", pc.Location, pc.Location != ""},
		{"vertex_request_type", pc.VertexRequestType, pc.VertexRequestType != ""},
		{"disabled", true, pc.Disabled},
		{"timeout", timeout, pc.Timeout != nil},
		{"force_api_support", true, pc.ForceApiSupport},
	}
}

func providerConfigPayload(pc *ProviderConfig) map[string]any {
	entry := map[string]any{}
	for _, field := range providerFields(pc) {
		if field.set {
			entry[field.key] = field.value
		}
	}
	return entry
}

// writeSettings writes s over the file at path, whose content is raw,
// keeping everything in raw that a save does not manage.
func writeSettings(path string, raw []byte, s *Settings) error {
	document, _, err := parseYAML(path, raw)
	if err != nil {
		// A file the loader cannot parse holds an edit the operator has yet
		// to fix; replacing it would discard that edit along with the rest.
		return fmt.Errorf("configuration not saved: %w", err)
	}
	if err := mergeManagedSettings(rootMapping(document), s); err != nil {
		return fmt.Errorf("configuration not saved: %w", err)
	}
	return writeDocument(path, document)
}

// mergeManagedSettings writes the sections a save manages, the keys of
// configPayload, from s into root, the file's top-level mapping. Every other
// key stays as the file has it, and so does every node that already holds
// what a save would write there, comments included.
func mergeManagedSettings(root *yaml.Node, s *Settings) error {
	// A save moves a file still on the pre-rename categories: key to
	// endpoints:, in place when there is no endpoints: key, because the
	// loader then read the endpoints from categories:.
	if index := keyIndex(root, "categories"); index >= 0 {
		if keyIndex(root, "endpoints") < 0 {
			root.Content[index].Value = "endpoints"
		} else {
			deleteMappingKey(root, "categories")
		}
	}
	providers, err := mergeProviders(unaliased(mappingValue(root, "providers")), s.Providers)
	if err != nil {
		return err
	}
	setMappingValue(root, "providers", providers)
	payload := configPayload(s)
	for _, key := range []string{"endpoints", "policies", "savings", "openai_codex_client_id"} {
		value, managed := payload[key]
		if !managed {
			deleteMappingKey(root, key)
			continue
		}
		merged, err := mergeValue(unaliased(mappingValue(root, key)), value)
		if err != nil {
			return err
		}
		setMappingValue(root, key, merged)
	}
	return nil
}

// mergeProviders returns the providers mapping holding exactly configured. An
// entry the file already has keeps the keys a save does not manage, such as
// api_key, and a base_url ${ENV:NAME} reference that still resolves to the
// configured URL.
func mergeProviders(existing *yaml.Node, configured map[string]*ProviderConfig) (*yaml.Node, error) {
	return mergeEntries(existing, slices.Sorted(maps.Keys(configured)), func(id string, entry *yaml.Node) (*yaml.Node, error) {
		provider := configured[id]
		if entry == nil || entry.Kind != yaml.MappingNode {
			return encodeNode(providerConfigPayload(provider))
		}
		for _, field := range providerFields(provider) {
			current := mappingValue(entry, field.key)
			if field.key == "base_url" && keepsEnvReference(current, provider.BaseURL) {
				continue
			}
			if !field.set {
				deleteMappingKey(entry, field.key)
				continue
			}
			merged, err := mergeValue(current, field.value)
			if err != nil {
				return nil, err
			}
			setMappingValue(entry, field.key, merged)
		}
		if provider.FileAPIKey == "" {
			deleteMappingKey(entry, "api_key")
		}
		return entry, nil
	})
}

// keepsEnvReference reports whether node is an ${ENV:NAME} reference that
// still resolves to value, which a save then leaves as written rather than
// replacing it with what it resolves to.
func keepsEnvReference(node *yaml.Node, value string) bool {
	return node != nil && node.Kind == yaml.ScalarNode &&
		envRef.MatchString(strings.TrimSpace(node.Value)) && resolveEnv(node.Value) == value
}

// mergeValue returns a node holding value, built on existing, the node the
// file holds there, if any (see mergeNode).
func mergeValue(existing *yaml.Node, value any) (*yaml.Node, error) {
	desired, err := encodeNode(value)
	if err != nil {
		return nil, err
	}
	return mergeNode(existing, desired)
}

// mergeNode returns existing itself when it already holds the data desired
// holds, whatever its style, and otherwise merges mappings key by key so the
// unchanged entries keep their nodes. A node it replaces passes its comments
// to its replacement.
func mergeNode(existing, desired *yaml.Node) (*yaml.Node, error) {
	switch {
	case existing == nil:
		return desired, nil
	case sameData(existing, desired):
		return existing, nil
	case existing.Kind == yaml.MappingNode && desired.Kind == yaml.MappingNode:
		keys := make([]string, 0, len(desired.Content)/2)
		for index := 0; index+1 < len(desired.Content); index += 2 {
			keys = append(keys, desired.Content[index].Value)
		}
		return mergeEntries(existing, keys, func(key string, entry *yaml.Node) (*yaml.Node, error) {
			return mergeNode(entry, mappingValue(desired, key))
		})
	}
	desired.HeadComment, desired.LineComment, desired.FootComment = existing.HeadComment, existing.LineComment, existing.FootComment
	return desired, nil
}

// mergeEntries makes mapping hold exactly keys, each with the value merge
// returns for it from the value the mapping holds, nil for a new key. Keys
// the mapping already has keep their place and key node, and with it their
// comments; new keys follow in the order given.
func mergeEntries(mapping *yaml.Node, keys []string, merge func(key string, value *yaml.Node) (*yaml.Node, error)) (*yaml.Node, error) {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		mapping = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	}
	if len(mapping.Content) == 0 {
		// An empty mapping is written {}; the entries it gains read better
		// as a block.
		mapping.Style = 0
	}
	missing := make(map[string]bool, len(keys))
	for _, key := range keys {
		missing[key] = true
	}
	content := make([]*yaml.Node, 0, 2*len(keys))
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		key := mapping.Content[index]
		if !missing[key.Value] {
			continue
		}
		value, err := merge(key.Value, mapping.Content[index+1])
		if err != nil {
			return nil, err
		}
		content = append(content, key, value)
		delete(missing, key.Value)
	}
	for _, key := range keys {
		if !missing[key] {
			continue
		}
		value, err := merge(key, nil)
		if err != nil {
			return nil, err
		}
		content = append(content, keyNode(key), value)
	}
	mapping.Content = content
	return mapping, nil
}

// sameData reports whether two nodes decode to the same data.
func sameData(a, b *yaml.Node) bool {
	var left, right any
	if a.Decode(&left) != nil || b.Decode(&right) != nil {
		return false
	}
	return reflect.DeepEqual(left, right)
}

// rootMapping returns the top-level mapping of a document parseYAML
// accepted, creating it in an empty or null document.
func rootMapping(document *yaml.Node) *yaml.Node {
	if document.Kind != yaml.DocumentNode {
		*document = yaml.Node{Kind: yaml.DocumentNode}
	}
	if len(document.Content) == 0 || document.Content[0].Kind != yaml.MappingNode {
		document.Content = []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}
	}
	return document.Content[0]
}

// keyIndex returns the index of key's key node in mapping, or -1.
func keyIndex(mapping *yaml.Node, key string) int {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return -1
	}
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			return index
		}
	}
	return -1
}

// mappingValue returns the value node of key in mapping, or nil.
func mappingValue(mapping *yaml.Node, key string) *yaml.Node {
	if index := keyIndex(mapping, key); index >= 0 {
		return mapping.Content[index+1]
	}
	return nil
}

// setMappingValue sets key in mapping to value, in place when the mapping
// already has the key.
func setMappingValue(mapping *yaml.Node, key string, value *yaml.Node) {
	if index := keyIndex(mapping, key); index >= 0 {
		mapping.Content[index+1] = value
		return
	}
	mapping.Content = append(mapping.Content, keyNode(key), value)
}

// deleteMappingKey removes key and its value from mapping.
func deleteMappingKey(mapping *yaml.Node, key string) {
	if index := keyIndex(mapping, key); index >= 0 {
		mapping.Content = append(mapping.Content[:index], mapping.Content[index+2:]...)
	}
}

func keyNode(key string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
}

func encodeNode(value any) (*yaml.Node, error) {
	node := &yaml.Node{}
	if err := node.Encode(value); err != nil {
		return nil, err
	}
	return node, nil
}

// unaliased returns node with every alias in it replaced by a copy of what
// the alias names. A save edits the sections it manages in place, and an
// edit must not reach another value through a shared anchor.
func unaliased(node *yaml.Node) *yaml.Node {
	if node == nil {
		return nil
	}
	if node.Kind == yaml.AliasNode && node.Alias != nil {
		return copyNode(node.Alias)
	}
	for index, child := range node.Content {
		node.Content[index] = unaliased(child)
	}
	return node
}

// copyNode deep-copies node, expanding its aliases and dropping its anchors,
// so the copy shares nothing with the original.
func copyNode(node *yaml.Node) *yaml.Node {
	if node.Kind == yaml.AliasNode && node.Alias != nil {
		return copyNode(node.Alias)
	}
	copied := *node
	copied.Anchor = ""
	copied.Content = make([]*yaml.Node, len(node.Content))
	for index, child := range node.Content {
		copied.Content[index] = copyNode(child)
	}
	return &copied
}

// writeDocument encodes document and writes it to path once the loader
// accepts the result, so a save never leaves a file the next start refuses.
func writeDocument(path string, document *yaml.Node) error {
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if err := encoder.Encode(document); err != nil {
		return fmt.Errorf("configuration not saved: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return fmt.Errorf("configuration not saved: %w", err)
	}
	if _, _, err := parseYAML(path, out.Bytes()); err != nil {
		return fmt.Errorf("configuration not saved: %w", err)
	}
	if err := writeConfigFile(path, out.Bytes()); err != nil {
		return fmt.Errorf("configuration not saved: write %s: %w", path, err)
	}
	return nil
}

// writeConfigFile replaces the file at path with data atomically, so a crash
// leaves either the old file or the new one.
func writeConfigFile(path string, data []byte) error {
	if err := os.MkdirAll(StateDir(), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
