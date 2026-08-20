package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
	"llmgw/internal/router"
)

// The canonical route is /endpoints.
func TestUpsertEndpointRoute(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	router.ResetSavingsState()
	router.ResetTelemetryState()
	t.Cleanup(func() {
		iam.ResetForTests()
		router.ResetSavingsState()
		router.ResetTelemetryState()
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	config.Update(func(s *config.Settings) {
		s.APIKey = "admin-secret"
		s.AllowUnauthenticatedAPI = false
		s.Providers = map[string]*config.ProviderConfig{"echo": {Type: "echo"}}
		s.Endpoints = map[string]*config.EndpointConfig{}
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)

	server := httptest.NewServer(NewServer())
	defer server.Close()

	status, saved := jsonRequest(t, server.URL+"/admin/api/endpoints", http.MethodPost, "admin-secret", map[string]any{
		"name":     "coding",
		"failover": []map[string]any{{"provider": "echo", "model": "echo-strong"}},
	})
	if status != http.StatusOK || saved["ok"] != true {
		t.Fatalf("upsert endpoint status=%d payload=%+v", status, saved)
	}
	chain := config.Get().Endpoints["coding"]
	if chain == nil || len(chain.Failover) != 1 || chain.Failover[0].Provider != "echo" || chain.Failover[0].Model != "echo-strong" {
		t.Fatalf("endpoint chain not stored in config: %+v", chain)
	}

	status, deleted := jsonRequest(t, server.URL+"/admin/api/endpoints/coding", http.MethodDelete, "admin-secret", nil)
	if status != http.StatusOK || deleted["ok"] != true {
		t.Fatalf("delete endpoint status=%d payload=%+v", status, deleted)
	}
	if _, exists := config.Get().Endpoints["coding"]; exists {
		t.Fatalf("endpoint chain still present after delete: %+v", config.Get().Endpoints)
	}
}

// The old route keeps working — it is published and clients use it.
func TestLegacyCategoriesRouteStillWorks(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	router.ResetSavingsState()
	router.ResetTelemetryState()
	t.Cleanup(func() {
		iam.ResetForTests()
		router.ResetSavingsState()
		router.ResetTelemetryState()
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	config.Update(func(s *config.Settings) {
		s.APIKey = "admin-secret"
		s.AllowUnauthenticatedAPI = false
		s.Providers = map[string]*config.ProviderConfig{"echo": {Type: "echo"}}
		s.Endpoints = map[string]*config.EndpointConfig{}
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)

	server := httptest.NewServer(NewServer())
	defer server.Close()

	status, saved := jsonRequest(t, server.URL+"/admin/api/categories", http.MethodPost, "admin-secret", map[string]any{
		"name":     "coding",
		"failover": []map[string]any{{"provider": "echo", "model": "echo-strong"}},
	})
	if status != http.StatusOK || saved["ok"] != true {
		t.Fatalf("upsert via legacy route status=%d payload=%+v", status, saved)
	}
	chain := config.Get().Endpoints["coding"]
	if chain == nil || len(chain.Failover) != 1 || chain.Failover[0].Provider != "echo" || chain.Failover[0].Model != "echo-strong" {
		t.Fatalf("legacy route did not have the same effect on config: %+v", chain)
	}

	status, deleted := jsonRequest(t, server.URL+"/admin/api/categories/coding", http.MethodDelete, "admin-secret", nil)
	if status != http.StatusOK || deleted["ok"] != true {
		t.Fatalf("delete via legacy route status=%d payload=%+v", status, deleted)
	}
	if _, exists := config.Get().Endpoints["coding"]; exists {
		t.Fatalf("endpoint chain still present after legacy delete: %+v", config.Get().Endpoints)
	}
}

// Both routes must write the same store; a chain created on one is visible on the other.
func TestBothRoutesShareOneStore(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	router.ResetSavingsState()
	router.ResetTelemetryState()
	t.Cleanup(func() {
		iam.ResetForTests()
		router.ResetSavingsState()
		router.ResetTelemetryState()
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	config.Update(func(s *config.Settings) {
		s.APIKey = "admin-secret"
		s.AllowUnauthenticatedAPI = false
		s.Providers = map[string]*config.ProviderConfig{"echo": {Type: "echo"}}
		s.Endpoints = map[string]*config.EndpointConfig{}
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)

	server := httptest.NewServer(NewServer())
	defer server.Close()

	// Create via the legacy /categories route.
	status, saved := jsonRequest(t, server.URL+"/admin/api/categories", http.MethodPost, "admin-secret", map[string]any{
		"name":     "coding",
		"failover": []map[string]any{{"provider": "echo", "model": "echo-strong"}},
	})
	if status != http.StatusOK || saved["ok"] != true {
		t.Fatalf("create via legacy route status=%d payload=%+v", status, saved)
	}

	// It must be visible directly in the shared config store...
	chain := config.Get().Endpoints["coding"]
	if chain == nil || len(chain.Failover) != 1 || chain.Failover[0].Model != "echo-strong" {
		t.Fatalf("chain created via legacy route not visible in store: %+v", config.Get().Endpoints)
	}

	// ...and a duplicate create through the canonical /endpoints route must
	// hit the same collision check that the legacy route enforces, proving
	// both routes see the same underlying data rather than separate stores.
	status, duplicate := jsonRequest(t, server.URL+"/admin/api/endpoints", http.MethodPost, "admin-secret", map[string]any{
		"name":     "CODING",
		"failover": []map[string]any{{"provider": "echo", "model": "echo-small"}},
	})
	if status != http.StatusBadRequest || duplicate["error"] == nil {
		t.Fatalf("canonical route did not see the chain created via the legacy route: status=%d payload=%+v", status, duplicate)
	}

	// Deleting through the canonical route must remove what the legacy route created.
	status, deleted := jsonRequest(t, server.URL+"/admin/api/endpoints/coding", http.MethodDelete, "admin-secret", nil)
	if status != http.StatusOK || deleted["ok"] != true {
		t.Fatalf("delete via canonical route status=%d payload=%+v", status, deleted)
	}
	if _, exists := config.Get().Endpoints["coding"]; exists {
		t.Fatalf("chain created via legacy route survived delete via canonical route: %+v", config.Get().Endpoints)
	}
}
