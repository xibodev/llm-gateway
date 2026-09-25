package providers

import (
	"context"
	"errors"
	"testing"

	"llmgw/internal/config"

	core "github.com/xibodev/llmgw-core"
	"github.com/xibodev/llmgw-core/catalog"
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

	owner := core.Caller{ID: "prn_owner", Kind: core.CallerHuman}
	Current().catalogs.store(catalogCacheKey("copilot", owner), []ModelInfo{{ID: "gpt-5-mini"}, {ID: "gpt-4o"}})

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

// refreshCatalog refreshes the catalog of key through the Runtime's catalog
// service, running during before its discovery lists any rows.
func refreshCatalog(runtime *Runtime, key string, during func()) error {
	_, err := runtime.catalogService.Refresh(context.Background(), catalog.Request{
		Key: catalogKeyOf(key),
		Discover: func(context.Context) ([]core.ModelInfo, error) {
			during()
			return []core.ModelInfo{{ID: "discovered"}}, nil
		},
	})
	return err
}

func TestCatalogInvalidationRejectsInFlightStaleWrite(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	// A fresh Runtime loads the catalog from this test's state directory.
	runtime := InstallForTests(t)

	if err := refreshCatalog(runtime, "copilot@owner", func() {
		ForgetCatalogForPrincipal("copilot", "owner")
	}); !errors.Is(err, catalog.ErrStateChanged) {
		t.Fatalf("invalidated catalog accepted an in-flight stale write: err=%v", err)
	}
	if models, _ := CatalogCachedForPrincipal(
		"copilot", core.Caller{ID: "owner", Kind: core.CallerHuman},
	); len(models) != 0 {
		t.Fatalf("stale catalog was restored: %+v", models)
	}
}

func TestCatalogProviderPersistenceRebasesWithoutWeakeningHardFence(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	// A fresh Runtime loads the catalog from this test's state directory.
	runtime := InstallForTests(t)

	persisted := func() { runtime.forgetCatalogAfterProviderPersistence("antigravity", "owner") }
	if err := refreshCatalog(runtime, "antigravity@owner", persisted); err != nil {
		t.Fatalf("credential refresh fenced the operation that performed it: %v", err)
	}
	if entry, ok := runtime.catalogs.entry("antigravity@owner"); !ok || len(entry.Models) != 1 {
		t.Fatalf("the operation's catalog was not stored: %+v", entry)
	}

	for _, mutation := range []struct {
		name       string
		invalidate func()
	}{
		{name: "revoke", invalidate: func() { ForgetCatalogForPrincipal("antigravity", "owner") }},
		{name: "reauthorization", invalidate: func() { ForgetCatalogForPrincipal("antigravity", "owner") }},
		{name: "provider config", invalidate: func() { ForgetCatalog("antigravity") }},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			if err := refreshCatalog(runtime, "antigravity@owner", func() {
				persisted()
				mutation.invalidate()
			}); !errors.Is(err, catalog.ErrStateChanged) {
				t.Fatalf("hard invalidation was mistaken for in-operation provider persistence: err=%v", err)
			}
		})
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
	Current().catalogs.store("vertex_ai", []ModelInfo{{ID: "gemini-test"}})
	Current().catalogs.store("azure", []ModelInfo{{ID: "azure-test"}})

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
