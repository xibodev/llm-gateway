package providers

import (
	"encoding/json"
	"testing"
)

func TestProviderRegistryIsUniqueAndRunnable(t *testing.T) {
	runtimeTypes := map[string]bool{}
	for _, providerType := range ProviderTypes {
		runtimeTypes[providerType] = true
	}
	seen := map[string]bool{}
	for _, entry := range ProviderRegistry() {
		if entry.ID == "" || entry.Label == "" || entry.RuntimeType == "" {
			t.Fatalf("incomplete registry entry: %+v", entry)
		}
		if seen[entry.ID] {
			t.Fatalf("duplicate registry id %q", entry.ID)
		}
		seen[entry.ID] = true
		if entry.Availability == ProviderAvailable && !runtimeTypes[entry.RuntimeType] {
			t.Fatalf("%s uses unsupported runtime type %q", entry.ID, entry.RuntimeType)
		}
		if entry.ConnectionScope == ConnectionScopePersonal {
			for _, authMethod := range entry.AuthMethods {
				if authMethod == "none" {
					t.Fatalf("%s personal connection has invalid auth methods", entry.ID)
				}
			}
		}
	}
	if len(seen) != 18 {
		t.Fatalf("embedded registry has %d entries, want 18", len(seen))
	}
}

func TestGeminiRegistryTemplate(t *testing.T) {
	entry, ok := RegistryProvider(" GEMINI ")
	if !ok {
		t.Fatal("Gemini registry entry missing")
	}
	if entry.RuntimeType != "openai_compatible" ||
		entry.DefaultBaseURL != "https://generativelanguage.googleapis.com/v1beta/openai" ||
		!entry.RequiresAPIKey || entry.InferAudioCapabilities {
		t.Fatalf("unexpected Gemini registry entry: %+v", entry)
	}
}

func TestLocalAIRegistryEnablesAudioInference(t *testing.T) {
	entry, ok := RegistryProvider("localai")
	if !ok {
		t.Fatal("LocalAI registry entry missing")
	}
	if !entry.InferAudioCapabilities {
		t.Fatalf("LocalAI registry entry must enable audio inference: %+v", entry)
	}
}

func TestVertexRegistryDefaultsLocation(t *testing.T) {
	entry, ok := RegistryProvider("vertex_ai")
	if !ok {
		t.Fatal("Vertex AI registry entry missing")
	}
	if entry.DefaultLocation != "global" {
		t.Fatalf("Vertex default location=%q, want global", entry.DefaultLocation)
	}
}

func TestSubscriptionOAuthProvidersArePersonal(t *testing.T) {
	for _, id := range []string{"github_copilot", "openai_codex"} {
		entry, ok := RegistryProvider(id)
		if !ok {
			t.Fatalf("%s registry entry missing", id)
		}
		if entry.ConnectionScope != ConnectionScopePersonal {
			t.Fatalf("%s scope=%q, want personal", id, entry.ConnectionScope)
		}
	}
}

func TestClaudeCodeIsGatewayClientOnly(t *testing.T) {
	entry, ok := RegistryProvider("claude_code")
	if !ok {
		t.Fatal("Claude Code registry entry missing")
	}
	if !entry.ClientOnly || entry.Availability != ProviderClientOnly ||
		entry.ConnectionScope != ConnectionScopeGatewayClient {
		t.Fatalf("Claude Code classification=%+v", entry)
	}
	for _, method := range entry.AuthMethods {
		if method == "oauth" || method == "oauth_device" {
			t.Fatalf("Claude Code must not advertise an OAuth action: %+v", entry.AuthMethods)
		}
	}
}

func TestRegistryProviderResolvesAliasesAndReturnsCopies(t *testing.T) {
	entry, ok := RegistryProvider(" COPILOT ")
	if !ok || entry.ID != "github_copilot" {
		t.Fatalf("alias lookup=%+v ok=%v", entry, ok)
	}
	if entry.RiskLevel != "yellow" || entry.QuotaAdapter != "github_copilot" {
		t.Fatalf("copilot manifest metadata=%+v", entry)
	}
	entry.AuthMethods[0] = "mutated"
	entry.Aliases[0] = "mutated"

	fresh, ok := RegistryProvider("github_copilot")
	if !ok || fresh.AuthMethods[0] != "oauth_device" || fresh.Aliases[0] != "copilot" {
		t.Fatalf("registry entry was mutated through a caller copy: %+v", fresh)
	}
	if got := CanonicalRegistryID(" CoDeX "); got != "openai_codex" {
		t.Fatalf("canonical codex registry id=%q", got)
	}
	if got := CanonicalRegistryID(" custom-provider "); got != "custom-provider" {
		t.Fatalf("unknown custom registry id=%q", got)
	}
	if _, ok := RegistryProviderByID("copilot"); ok {
		t.Fatal("exact registry lookup must not resolve aliases")
	}
	if exact, ok := RegistryProviderByID("github_copilot"); !ok || exact.ID != "github_copilot" {
		t.Fatalf("exact registry lookup=%+v ok=%v", exact, ok)
	}
	if got := EffectiveRegistryID("openai_codex", "", "openai_compatible"); got != "openai_codex" {
		t.Fatalf("canonical configured provider registry id=%q", got)
	}
	if got := EffectiveRegistryID("codex", "", "openai_compatible"); got != "" {
		t.Fatalf("alias-named custom provider registry id=%q", got)
	}
}

func TestValidateProviderRegistryRejectsAliasCollisions(t *testing.T) {
	entries := []RegistryEntry{
		validRegistryEntry("first"),
		validRegistryEntry("second"),
	}
	entries[0].Aliases = []string{"shared"}
	entries[1].Aliases = []string{"SHARED"}
	if err := validateProviderRegistry(entries); err == nil {
		t.Fatal("duplicate case-insensitive aliases should be rejected")
	}
}

func TestValidateProviderRegistryRejectsInvalidAuthContract(t *testing.T) {
	entry := validRegistryEntry("invalid")
	entry.RequiresAPIKey = true
	entry.AuthMethods = []string{"none"}
	if err := validateProviderRegistry([]RegistryEntry{entry}); err == nil {
		t.Fatal("requires_api_key without api_key auth should be rejected")
	}

	entry = validRegistryEntry("risky")
	entry.RiskLevel = "yellow"
	if err := validateProviderRegistry([]RegistryEntry{entry}); err == nil {
		t.Fatal("non-green provider without a risk notice should be rejected")
	}

	entry = validRegistryEntry("missing-risk")
	entry.RiskLevel = ""
	if err := validateProviderRegistry([]RegistryEntry{entry}); err == nil {
		t.Fatal("missing risk metadata should be rejected")
	}

	entry = validRegistryEntry("oauth-without-adapter")
	entry.AuthMethods = []string{"oauth_device"}
	entry.ConnectionScope = ConnectionScopePersonal
	if err := validateProviderRegistry([]RegistryEntry{entry}); err == nil {
		t.Fatal("available OAuth provider without auth_adapter should be rejected")
	}
}

func TestDecodeProviderRegistryRejectsUnknownFields(t *testing.T) {
	entry := validRegistryEntry("strict")
	raw, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatal(err)
	}
	object["risk_levle"] = "yellow"
	payload, err := json.Marshal([]map[string]any{object})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeProviderRegistry(payload); err == nil {
		t.Fatal("unknown manifest fields should be rejected")
	}
}

func validRegistryEntry(id string) RegistryEntry {
	return RegistryEntry{
		ID: id, Label: id, Description: "test provider", RuntimeType: "openai_compatible",
		Protocol: "openai", Availability: ProviderAvailable, AuthMethods: []string{"api_key"},
		ConnectionScope: ConnectionScopeSystemOrPersonal, DefaultProviderID: id,
		RiskLevel: "green",
	}
}
