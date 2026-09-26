package providers

import (
	"testing"

	"llmgw/internal/config"

	core "github.com/xibodev/llmgw-core"
)

func TestInstantiateUsesExtensionForCodexRegistry(t *testing.T) {
	t.Setenv("LLMGW_EXTENSION_ENABLED", "1")
	t.Setenv("LLMGW_EXTENSION_URL", "http://extension.example.test:18888")
	runtime := newRuntime(func(bool) (core.CredentialStore, error) {
		return core.NewMemoryCredentialStore(), nil
	})
	settings := config.Defaults()
	cfg := &config.ProviderConfig{Type: "openai_compatible", RegistryID: "openai_codex"}
	settings.Providers = map[string]*config.ProviderConfig{"codex": cfg}

	provider, err := runtime.instantiate(settings, "codex", cfg, core.Caller{})
	if err != nil {
		t.Fatalf("instantiate Codex: %v", err)
	}
	facade, ok := provider.(*ExtensionProviderFacade)
	if !ok {
		t.Fatalf("provider type = %T, want *ExtensionProviderFacade", provider)
	}
	if facade.providerID != ExtensionTypeCodex {
		t.Fatalf("extension provider id = %q, want %q", facade.providerID, ExtensionTypeCodex)
	}
}

func TestOnlyEdgeTTSExtensionIsSpeechSynthesizer(t *testing.T) {
	tests := map[string]bool{
		ExtensionTypeCodex:   false,
		ExtensionTypeEdgeTTS: true,
	}
	for providerID, want := range tests {
		t.Run(providerID, func(t *testing.T) {
			_, got := AsSpeechSynthesizer(&ExtensionProviderFacade{providerID: providerID})
			if got != want {
				t.Fatalf("speech synthesizer = %v, want %v", got, want)
			}
		})
	}
}
