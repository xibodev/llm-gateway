package providers

import "strings"

const (
	ProviderAvailable  = "available"
	ProviderPlanned    = "planned"
	ProviderClientOnly = "client_only"

	ConnectionScopeSystemOrPersonal = "system_or_personal"
	ConnectionScopePersonal         = "personal"
	ConnectionScopeGatewayClient    = "gateway_client"
)

// RegistryEntry describes a curated provider integration independently from a
// configured provider instance. RuntimeType maps the entry onto the gateway's
// transport/auth implementation.
type RegistryEntry struct {
	ID                     string   `json:"id"`
	Aliases                []string `json:"aliases,omitempty"`
	Label                  string   `json:"label"`
	Description            string   `json:"description"`
	Categories             []string `json:"categories,omitempty"`
	RuntimeType            string   `json:"runtime_type"`
	Protocol               string   `json:"protocol"`
	Availability           string   `json:"availability"`
	AuthMethods            []string `json:"auth_methods"`
	ConnectionScope        string   `json:"connection_scope"`
	DefaultProviderID      string   `json:"default_provider_id"`
	DefaultBaseURL         string   `json:"default_base_url,omitempty"`
	DefaultRegion          string   `json:"default_region,omitempty"`
	DefaultLocation        string   `json:"default_location,omitempty"`
	RequiresBaseURL        bool     `json:"requires_base_url,omitempty"`
	RequiresAPIKey         bool     `json:"requires_api_key,omitempty"`
	SupportsAPIAdaptation  bool     `json:"supports_api_adaptation,omitempty"`
	SupportsModelDiscovery bool     `json:"supports_model_discovery,omitempty"`
	InferAudioCapabilities bool     `json:"infer_audio_capabilities,omitempty"`
	OnboardingFields       []string `json:"onboarding_fields,omitempty"`
	AuthAdapter            string   `json:"auth_adapter,omitempty"`
	QuotaAdapter           string   `json:"quota_adapter,omitempty"`
	RiskLevel              string   `json:"risk_level,omitempty"`
	RiskNotice             string   `json:"risk_notice,omitempty"`
	DocsURL                string   `json:"docs_url,omitempty"`
	ClientOnly             bool     `json:"client_only,omitempty"`
}

func cloneRegistryEntry(entry RegistryEntry) RegistryEntry {
	entry.Aliases = append([]string(nil), entry.Aliases...)
	entry.AuthMethods = append([]string(nil), entry.AuthMethods...)
	entry.Categories = append([]string(nil), entry.Categories...)
	entry.OnboardingFields = append([]string(nil), entry.OnboardingFields...)
	return entry
}

// ProviderRegistry returns a copy so callers cannot mutate global metadata.
func ProviderRegistry() []RegistryEntry {
	out := make([]RegistryEntry, len(providerRegistry))
	for i, entry := range providerRegistry {
		out[i] = cloneRegistryEntry(entry)
	}
	return out
}

// RegistryProvider resolves a curated provider integration by id or alias.
func RegistryProvider(id string) (RegistryEntry, bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "" {
		return RegistryEntry{}, false
	}
	for _, entry := range providerRegistry {
		if entry.ID == id {
			return cloneRegistryEntry(entry), true
		}
		for _, alias := range entry.Aliases {
			if strings.EqualFold(alias, id) {
				return cloneRegistryEntry(entry), true
			}
		}
	}
	return RegistryEntry{}, false
}

// RegistryProviderByID resolves only canonical registry IDs. Use it when a
// configured provider ID must not be interpreted as a registry alias.
func RegistryProviderByID(id string) (RegistryEntry, bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	for _, entry := range providerRegistry {
		if entry.ID == id {
			return cloneRegistryEntry(entry), true
		}
	}
	return RegistryEntry{}, false
}

// CanonicalRegistryID resolves a registry alias while preserving unknown custom
// identifiers in normalized form.
func CanonicalRegistryID(id string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	if entry, ok := RegistryProvider(id); ok {
		return entry.ID
	}
	return id
}

// RegistryRuntimeMatches verifies that a configured runtime can safely execute
// one registry integration. OpenAI-compatible legacy spellings remain accepted.
func RegistryRuntimeMatches(entry RegistryEntry, runtimeType string) bool {
	runtimeType = strings.ToLower(strings.TrimSpace(runtimeType))
	expected := strings.ToLower(strings.TrimSpace(entry.RuntimeType))
	if entry.ID == "openai_codex" && runtimeType == "openai_codex" {
		return true
	}
	if expected == "openai_compatible" {
		return runtimeType == "openai_compatible" || runtimeType == "openai"
	}
	return runtimeType == expected
}

// EffectiveRegistryID resolves the canonical registry identity for one
// configured provider without interpreting custom provider IDs as aliases.
func EffectiveRegistryID(providerID, configuredRegistryID, runtimeType string) string {
	if value := CanonicalRegistryID(configuredRegistryID); value != "" {
		return value
	}
	if entry, ok := RegistryProviderByID(providerID); ok && RegistryRuntimeMatches(entry, runtimeType) {
		return entry.ID
	}
	switch strings.ToLower(strings.TrimSpace(runtimeType)) {
	case "github_copilot":
		return "github_copilot"
	case "openai_codex":
		return "openai_codex"
	default:
		return ""
	}
}
