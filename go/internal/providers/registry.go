package providers

import (
	"strings"

	coreproviders "github.com/xibodev/llmgw-core/providers"
)

const (
	ProviderAvailable  = coreproviders.ProviderAvailable
	ProviderPlanned    = coreproviders.ProviderPlanned
	ProviderClientOnly = coreproviders.ProviderClientOnly

	ConnectionScopeSystemOrPersonal = coreproviders.ConnectionScopeSystemOrPersonal
	ConnectionScopePersonal         = coreproviders.ConnectionScopePersonal
	ConnectionScopeGatewayClient    = coreproviders.ConnectionScopeGatewayClient
)

// RegistryEntry describes a curated provider integration independently from a
// configured provider instance. RuntimeType maps the entry onto the gateway's
// transport/auth implementation. The type and its manifest are owned by
// llmgw-core; see registry_overlay.go.
type RegistryEntry = coreproviders.RegistryEntry

// ProviderRegistry returns copies so callers cannot mutate registry metadata.
func ProviderRegistry() []RegistryEntry { return providerRegistry.Entries() }

// EffectiveRegistry returns the effective registry itself, for the llmgw-core
// APIs that vet against one. A Registry is immutable, so sharing it is safe.
func EffectiveRegistry() *coreproviders.Registry { return providerRegistry }

// RegistryProvider resolves a curated provider integration by id or alias.
func RegistryProvider(id string) (RegistryEntry, bool) { return providerRegistry.Lookup(id) }

// RegistryProviderByID resolves only canonical registry IDs. Use it when a
// configured provider ID must not be interpreted as a registry alias.
func RegistryProviderByID(id string) (RegistryEntry, bool) { return providerRegistry.ByID(id) }

// CanonicalRegistryID resolves a registry alias while preserving unknown custom
// identifiers in normalized form.
func CanonicalRegistryID(id string) string { return providerRegistry.CanonicalID(id) }

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
