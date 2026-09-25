package providers

import (
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"

	core "github.com/xibodev/llmgw-core"
)

// Transport planning reads each facade's declarations through
// WireDeclaration and core.PreservesWireFor, so response labels and
// /v1/models stay as they were only while that answers exactly as
// PreservesWireNativeSurface does for every facade, cached rows included.
func TestWireDeclarationAnswersAsTheFacadeDeclares(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	runtime := InstallForTests(t)
	old := *config.Get()
	config.Update(func(s *config.Settings) {
		s.CredentialEncryptionKey = ""
		s.Providers = map[string]*config.ProviderConfig{
			"openai":        {Type: "openai_compatible", BaseURL: "https://fixture.invalid/v1", APIKey: "fixture"},
			"openai-entry":  {Type: "openai_compatible", RegistryID: "openai", APIKey: "fixture"},
			"adapting":      {Type: "openai_compatible", BaseURL: "https://fixture.invalid/v1", APIKey: "fixture", ForceApiSupport: true},
			"zen":           {Type: "openai_compatible", RegistryID: "opencode_zen", APIKey: "fixture"},
			"zen-anonymous": {Type: "openai_compatible", RegistryID: "opencode_zen"},
			"codex":         {Type: "openai_compatible", RegistryID: "openai_codex"},
			"anthropic":     {Type: "anthropic", APIKey: "fixture"},
			"copilot":       {Type: "github_copilot"},
			"azure":         {Type: "azure_openai", BaseURL: "https://fixture.openai.azure.com", APIKey: "fixture"},
			"echo":          {Type: "echo"},
		}
	})
	t.Cleanup(func() { config.Update(func(s *config.Settings) { *s = old }) })
	owner := core.Caller{ID: "owner", Kind: core.CallerHuman}
	rows := []ModelInfo{
		{ID: "chat-model", SupportedSurfaces: []string{"/chat/completions"}},
		{ID: "responses-model", SupportedSurfaces: []string{"/responses"}},
		{ID: "both-model", SupportedSurfaces: []string{"/chat/completions", "/responses"}},
	}
	for _, instance := range []string{"openai", "adapting", "zen", "zen-anonymous"} {
		runtime.catalogs.store(catalogCacheKey(instance, owner), rows)
	}
	for instance := range config.Get().Providers {
		provider, err := runtime.GetProviderForPrincipal(instance, owner)
		if err != nil {
			t.Fatalf("%s: %v", instance, err)
		}
		declared, credential := WireDeclaration(provider)
		for _, model := range []string{"chat-model", "responses-model", "both-model", "uncached-model", "muse-spark-fixture"} {
			for _, surface := range []core.ModelSurface{
				core.ModelSurfaceChatCompletions, core.ModelSurfaceResponses, core.ModelSurfaceMessages,
				core.ModelSurfaceEmbeddings, "",
			} {
				want := PreservesWireNativeSurface(provider, model, surface)
				if got := core.PreservesWireFor(declared, credential, model, surface); got != want {
					t.Errorf("%s %s %q: core declares %v, the facade %v", instance, model, surface, got, want)
				}
			}
		}
	}
	if declared, _ := WireDeclaration(nil); declared != nil {
		t.Fatalf("a provider that could not be built declared %T", declared)
	}
}
