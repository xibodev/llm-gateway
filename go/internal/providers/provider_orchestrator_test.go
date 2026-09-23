package providers

import (
	"testing"

	core "github.com/xibodev/llmgw-core"
)

func TestGatewayProviderOrchestratorRegistersOnlyReviewedAnonymousProfiles(t *testing.T) {
	if _, err := NewGatewayProviderOrchestrator(AnonymousProviderProfiles()); err != nil {
		t.Fatalf("reviewed profiles: %v", err)
	}
	for _, profile := range []AnonymousProviderProfile{
		{RegistryID: "openai_codex", ProviderID: "codex", RuntimeType: "openai_compatible"},
		{RegistryID: "custom_openai", ProviderID: "custom", RuntimeType: "openai_compatible"},
	} {
		if _, err := NewGatewayProviderOrchestrator([]AnonymousProviderProfile{profile}); err == nil {
			t.Fatalf("profile %q entered anonymous orchestration", profile.RegistryID)
		}
	}
}

func TestAnonymousProbeSelectorUsesEveryDiscoveredModel(t *testing.T) {
	profile := AnonymousProviderProfile{
		RegistryID: "llm7", ProviderID: "fixture",
	}
	targets := anonymousProbeSelector(profile)(
		core.ProviderConnection{ProviderID: "fixture", Kind: core.ProviderConnectionAnonymous, AuthKind: core.ProviderAuthAnonymous},
		[]core.ModelInfo{{ID: "paid"}, {ID: "codestral-latest"}},
	)
	if len(targets) != 2 || targets[0] != (core.Target{Provider: "fixture", Model: "paid"}) ||
		targets[1] != (core.Target{Provider: "fixture", Model: "codestral-latest"}) {
		t.Fatalf("targets=%+v", targets)
	}
}

func TestAnonymousProbeSelectorDoesNotVerifySiblingsFromOneTarget(t *testing.T) {
	profile := AnonymousProviderProfile{RegistryID: "fixture", ProviderID: "fixture"}
	targets := anonymousProbeSelector(profile)(core.ProviderConnection{}, []core.ModelInfo{
		{ID: "working"}, {ID: "broken"}, {ID: "working"},
	})
	if len(targets) != 2 || targets[0].Model != "working" || targets[1].Model != "broken" {
		t.Fatalf("per-model targets=%+v", targets)
	}
}

func TestGatewayProviderOrchestratorKeepsBespokeTransportsInApplication(t *testing.T) {
	for _, registryID := range []string{"opencode_zen", "pollinations"} {
		if !usesApplicationAnonymousAdapter(registryID) {
			t.Fatalf("%s did not retain its application adapter", registryID)
		}
	}
	for _, registryID := range []string{"llm7", "kilo_code", "ovh_ai_endpoints"} {
		if usesApplicationAnonymousAdapter(registryID) {
			t.Fatalf("%s did not use the core anonymous adapter", registryID)
		}
	}
}
