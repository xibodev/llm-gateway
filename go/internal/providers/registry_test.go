package providers

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"testing"

	"llmgw/internal/config"

	anthropicauth "github.com/xibodev/llm-provider-auth/anthropic"
	gcpauth "github.com/xibodev/llm-provider-auth/gcp"
	coreproviders "github.com/xibodev/llmgw-core/providers"
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
	if len(seen) != 25 {
		t.Fatalf("embedded registry has %d entries, want 25", len(seen))
	}
}

func TestAnthropicAndAntigravityRegistryContracts(t *testing.T) {
	anthropic, ok := RegistryProviderByID("anthropic")
	if !ok || !slices.Contains(anthropic.AuthMethods, "api_key") || !slices.Contains(anthropic.AuthMethods, "setup_token") {
		t.Fatalf("anthropic registry=%+v", anthropic)
	}
	antigravity, ok := RegistryProvider("google-antigravity")
	if !ok || antigravity.ID != "google_antigravity" || !slices.Equal(antigravity.AuthMethods, []string{"oauth_browser"}) || antigravity.ConnectionScope != ConnectionScopePersonal || antigravity.RiskLevel != "red" || antigravity.RiskNotice == "" {
		t.Fatalf("antigravity registry=%+v", antigravity)
	}
}

func TestAntigravityCatalogRequiresPrivatePrincipal(t *testing.T) {
	old := config.Get().Providers
	config.Update(func(settings *config.Settings) {
		settings.Providers = map[string]*config.ProviderConfig{
			"antigravity": {Type: "google_antigravity", RegistryID: "google_antigravity"},
		}
	})
	t.Cleanup(func() { config.Update(func(settings *config.Settings) { settings.Providers = old }) })
	if !CatalogRequiresPrincipal("antigravity") {
		t.Fatal("Antigravity catalog was not principal scoped")
	}
}

func TestAnonymousAutomationRegistryIsExplicitAndSafe(t *testing.T) {
	want := map[string]bool{
		"opencode_zen": true, "kilo_code": true, "llm7": true,
		"ovh_ai_endpoints": true, "pollinations": true,
	}
	for _, entry := range ProviderRegistry() {
		if entry.AnonymousAutomation != want[entry.ID] {
			t.Fatalf("anonymous automation eligibility for %q=%v", entry.ID, entry.AnonymousAutomation)
		}
	}
}

func TestOpenCodeZenAPIKeyIsOptionalButOnboardable(t *testing.T) {
	entry, ok := RegistryProvider("opencode_zen")
	if !ok || entry.RequiresAPIKey || !slices.Contains(entry.AuthMethods, "none") ||
		!slices.Contains(entry.AuthMethods, "api_key") || !slices.Contains(entry.OnboardingFields, "api_key") {
		t.Fatalf("OpenCode Zen optional key contract=%+v", entry)
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
	if !slices.Contains(entry.OnboardingFields, "vertex_request_type") {
		t.Fatalf("Vertex onboarding fields omit request type: %+v", entry.OnboardingFields)
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

// TestEffectiveRegistryReproducesSnapshot pins the gateway's effective registry,
// the core manifest plus the gateway overlay, to registry_snapshot.json, which
// the website and documentation checks read. After an intended registry
// change, regenerate the snapshot:
//
//	LLMGW_UPDATE_GOLDEN=1 go test ./internal/providers -run TestEffectiveRegistryReproducesSnapshot
func TestEffectiveRegistryReproducesSnapshot(t *testing.T) {
	got := ProviderRegistry()
	if os.Getenv("LLMGW_UPDATE_GOLDEN") == "1" {
		encoded, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile("registry_snapshot.json", append(encoded, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	payload, err := os.ReadFile("registry_snapshot.json")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := coreproviders.DecodeRegistryEntries(payload)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	want, err := coreproviders.NewRegistry(entries, coreproviders.ValidationOptions{RuntimeTypes: ProviderTypes})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if !reflect.DeepEqual(want.Entries(), got) {
		t.Fatal("the effective registry differs from registry_snapshot.json; the manifest lives in llmgw-core " +
			"and gateway curation in registry_overlay.json; regenerate the snapshot with LLMGW_UPDATE_GOLDEN=1")
	}
}

func TestRegistryOverlayCannotChangeWireFacts(t *testing.T) {
	if _, err := loadProviderRegistry([]byte(`{"override":[{"id":"openai","runtime_type":"anthropic"}]}`)); err == nil {
		t.Fatal("an overlay changed a wire fact")
	}
	if _, err := loadProviderRegistry([]byte(`{"override":[{"id":"not-a-provider","label":"x"}]}`)); err == nil {
		t.Fatal("an overlay overrode an unknown entry")
	}
	unrunnable := `{"add":[{"id":"exotic","label":"Exotic","description":"x","runtime_type":"exotic",` +
		`"protocol":"openai","availability":"available","auth_methods":["api_key"],` +
		`"connection_scope":"system_or_personal","default_provider_id":"exotic","risk_level":"green"}]}`
	if _, err := loadProviderRegistry([]byte(unrunnable)); err == nil {
		t.Fatal("an overlay added an entry this gateway cannot execute")
	}
}

// TestRegistryAuthMethodsMatchAuthLibrary keeps the registry vocabulary and the
// credential kinds the auth library stores spelled identically.
func TestRegistryAuthMethodsMatchAuthLibrary(t *testing.T) {
	if coreproviders.AuthGCPServiceAccount != gcpauth.CredentialKind {
		t.Fatalf("registry %q != auth library %q", coreproviders.AuthGCPServiceAccount, gcpauth.CredentialKind)
	}
	if coreproviders.AuthSetupToken != string(anthropicauth.CredentialSetupToken) {
		t.Fatalf("registry %q != auth library %q", coreproviders.AuthSetupToken, anthropicauth.CredentialSetupToken)
	}
}
