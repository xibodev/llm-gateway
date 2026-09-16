package providers

import (
	"testing"

	"llmgw/internal/config"
)

// Catalogs are cached per principal so credentials never leak between callers,
// but the operator-wide provider hub must still see that a provider has a
// synced catalog. Without this the hub reports "0 models · unknown" for a
// provider that just synced hundreds.
func TestCatalogSnapshotSeesPrincipalScopedEntries(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	ResetProviders()
	t.Cleanup(ResetProviders)
	ForgetCatalog("copilot")
	ForgetCatalog("other")

	owner := &config.Principal{PrincipalID: "prn_owner", PrincipalKind: "human"}
	storeEntry(catalogCacheKey("copilot", owner), []ModelInfo{{ID: "gpt-5-mini"}, {ID: "gpt-4o"}})

	if models, _ := CatalogCached("copilot"); len(models) != 0 {
		t.Fatalf("unscoped cache should not expose a principal's catalog, got %d", len(models))
	}
	models, refreshed := CatalogSnapshot("copilot")
	if len(models) != 2 {
		t.Fatalf("snapshot models = %d, want 2", len(models))
	}
	if refreshed.IsZero() {
		t.Fatal("snapshot lost the refresh timestamp")
	}
	if models, _ := CatalogSnapshot("other"); len(models) != 0 {
		t.Fatalf("snapshot leaked across providers: %d", len(models))
	}
}

func TestCatalogInvalidationRejectsInFlightStaleWrite(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	catMu.Lock()
	catData = nil
	catGeneration = nil
	catLoaded = false
	catMu.Unlock()
	t.Cleanup(func() {
		catMu.Lock()
		catData = nil
		catGeneration = nil
		catLoaded = false
		catMu.Unlock()
	})

	key := "copilot@owner"
	generation := catalogGenerationFor(key)
	ForgetCatalogForPrincipal("copilot", "owner")
	if storeEntryIfGeneration(key, []ModelInfo{{ID: "stale"}}, generation) {
		t.Fatal("invalidated catalog accepted an in-flight stale write")
	}
	if models, _ := CatalogCachedForPrincipal(
		"copilot", &config.Principal{PrincipalID: "owner", PrincipalKind: "human"},
	); len(models) != 0 {
		t.Fatalf("stale catalog was restored: %+v", models)
	}
}

func TestIncompleteProviderConfigurationHidesCachedCatalog(t *testing.T) {
	oldProviders := config.Get().Providers
	t.Cleanup(func() {
		config.Update(func(settings *config.Settings) {
			settings.Providers = oldProviders
		})
	})
	config.Update(func(settings *config.Settings) {
		settings.Providers = map[string]*config.ProviderConfig{
			"vertex_ai": {Type: "vertex_ai", RegistryID: "vertex_ai"},
			"azure":     {Type: "azure_openai", RegistryID: "azure_openai"},
		}
	})
	storeEntry("vertex_ai", []ModelInfo{{ID: "gemini-test"}})
	storeEntry("azure", []ModelInfo{{ID: "azure-test"}})

	if issue := ProviderConfigurationIssue("vertex_ai"); issue == "" {
		t.Fatal("Vertex without a project should report incomplete configuration")
	}
	if models, _ := CatalogSnapshot("vertex_ai"); len(models) != 0 {
		t.Fatalf("incomplete Vertex provider exposed cached models: %+v", models)
	}
	if issue := ProviderConfigurationIssue("azure"); issue == "" {
		t.Fatal("Azure without a base URL should report incomplete configuration")
	}
	if models, _ := CatalogSnapshot("azure"); len(models) != 0 {
		t.Fatalf("incomplete Azure provider exposed cached models: %+v", models)
	}

	config.Update(func(settings *config.Settings) {
		settings.Providers["vertex_ai"].Project = "project-a"
		settings.Providers["azure"].BaseURL = "https://example.openai.azure.com"
	})
	if issue := ProviderConfigurationIssue("vertex_ai"); issue != "" {
		t.Fatalf("configured Vertex issue=%q", issue)
	}
	if models, _ := CatalogSnapshot("vertex_ai"); len(models) != 1 {
		t.Fatalf("configured Vertex catalog models=%+v", models)
	}
	if issue := ProviderConfigurationIssue("azure"); issue != "" {
		t.Fatalf("configured Azure issue=%q", issue)
	}
	if models, _ := CatalogSnapshot("azure"); len(models) != 1 {
		t.Fatalf("configured Azure catalog models=%+v", models)
	}
}
