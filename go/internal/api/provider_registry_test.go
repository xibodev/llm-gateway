package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
	"llmgw/internal/router"
)

func TestAdminCreatesProviderFromRegistry(t *testing.T) {
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
		s.Providers = map[string]*config.ProviderConfig{}
		s.Endpoints = map[string]*config.EndpointConfig{}
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)

	server := httptest.NewServer(NewServer())
	defer server.Close()
	status, created := jsonRequest(t, server.URL+"/admin/api/providers", http.MethodPost, "admin-secret", map[string]any{
		"registry_id": "gemini",
		"api_key":     "gemini-test-secret",
	})
	if status != http.StatusOK || created["id"] != "gemini" {
		t.Fatalf("create registry provider: %d %+v", status, created)
	}
	cfg := config.Get().Providers["gemini"]
	if cfg == nil || cfg.RegistryID != "gemini" || cfg.Type != "openai_compatible" ||
		cfg.BaseURL != "https://generativelanguage.googleapis.com/v1beta/openai" {
		t.Fatalf("Gemini config=%+v", cfg)
	}
	if got := config.LoadSecrets()["gemini"]; got != "gemini-test-secret" {
		t.Fatalf("Gemini secret=%q", got)
	}
	status, created = jsonRequest(t, server.URL+"/admin/api/providers", http.MethodPost, "admin-secret", map[string]any{
		"registry_id": "codex",
	})
	if status != http.StatusOK || created["id"] != "codex" {
		t.Fatalf("create aliased Codex provider: %d %+v", status, created)
	}
	codex := config.Get().Providers["codex"]
	if codex == nil || codex.RegistryID != "openai_codex" {
		t.Fatalf("Codex registry id was not canonicalized: %+v", codex)
	}
	status, _ = jsonRequest(t, server.URL+"/admin/api/providers", http.MethodPost, "admin-secret", map[string]any{
		"registry_id": "vertex_ai",
		"api_key":     "vertex-test-secret",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("Vertex without project status=%d, want 400", status)
	}
	status, created = jsonRequest(t, server.URL+"/admin/api/providers", http.MethodPost, "admin-secret", map[string]any{
		"registry_id": "vertex_ai",
		"api_key":     "vertex-test-secret",
		"project":     "project-a",
	})
	if status != http.StatusOK || created["id"] != "vertex_ai" {
		t.Fatalf("create Vertex provider: %d %+v", status, created)
	}
	vertex := config.Get().Providers["vertex_ai"]
	if vertex == nil || vertex.Project != "project-a" || vertex.Location != "global" {
		t.Fatalf("Vertex config=%+v", vertex)
	}

	status, state := jsonRequest(t, server.URL+"/admin/api/state", http.MethodGet, "admin-secret", nil)
	if status != http.StatusOK {
		t.Fatalf("state: %d %+v", status, state)
	}
	registry, ok := state["provider_registry"].([]any)
	if !ok || len(registry) < 10 {
		t.Fatalf("provider registry missing from state: %#v", state["provider_registry"])
	}
	statuses, ok := state["provider_statuses"].([]any)
	if !ok || len(statuses) < 10 {
		t.Fatalf("provider snapshots missing from state: %#v", state["provider_statuses"])
	}
	serialized, _ := json.Marshal(state)
	if strings.Contains(string(serialized), "gemini-test-secret") {
		t.Fatalf("provider state leaked a credential: %s", serialized)
	}
	if strings.Contains(string(serialized), "vertex-test-secret") {
		t.Fatalf("provider state leaked the Vertex credential: %s", serialized)
	}
	if !strings.Contains(string(serialized), `"project":"project-a"`) ||
		!strings.Contains(string(serialized), `"location":"global"`) {
		t.Fatalf("provider state omitted safe Vertex setup fields: %s", serialized)
	}
}

func TestAdminRejectsPlannedRegistryProvider(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	router.ResetSavingsState()
	router.ResetTelemetryState()
	t.Cleanup(func() {
		router.ResetSavingsState()
		router.ResetTelemetryState()
	})
	config.Update(func(s *config.Settings) {
		s.APIKey = "admin-secret"
		s.AllowUnauthenticatedAPI = false
		s.Providers = map[string]*config.ProviderConfig{}
	})
	server := httptest.NewServer(NewServer())
	defer server.Close()
	status, _ := jsonRequest(t, server.URL+"/admin/api/providers", http.MethodPost, "admin-secret", map[string]any{
		"registry_id": "claude_code",
	})
	if status != http.StatusConflict {
		t.Fatalf("planned provider status=%d, want %d", status, http.StatusConflict)
	}
}

func TestProviderStatusKeepsAliasNamedCustomProviderSeparate(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	settings := config.Defaults()
	settings.Providers = map[string]*config.ProviderConfig{
		"claude": {Type: "anthropic", BaseURL: "https://example.invalid"},
	}
	snapshots, err := providerStatusSnapshots(
		settings, map[string]string{"claude": "fake-key"}, nil, "",
	)
	if err != nil {
		t.Fatal(err)
	}
	var custom, client map[string]any
	for _, snapshot := range snapshots {
		switch snapshot["id"] {
		case "claude":
			custom = snapshot
		case "claude_code":
			client = snapshot
		}
	}
	if custom == nil || custom["configured"] != true || custom["description"] != "Custom configured provider." {
		t.Fatalf("custom alias-named provider snapshot=%+v", custom)
	}
	if client == nil || client["configured"] != false || client["status"] != "client_setup" {
		t.Fatalf("Claude Code registry snapshot=%+v", client)
	}
}

func TestProviderProbeReturnsExplicitSafeDetails(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	config.Update(func(s *config.Settings) {
		s.APIKey = "admin-secret"
		s.AllowUnauthenticatedAPI = false
		s.Providers = map[string]*config.ProviderConfig{"echo": {Type: "echo"}}
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)
	server := httptest.NewServer(NewServer())
	defer server.Close()

	for _, operation := range []string{"test", "repair", "refresh"} {
		status, payload := jsonRequest(t, server.URL+"/admin/api/providers/echo/"+operation, http.MethodPost, "admin-secret", map[string]any{})
		if status != http.StatusOK || payload["success"] != true || payload["status"] != "passed" {
			t.Fatalf("%s probe status=%d payload=%+v", operation, status, payload)
		}
		if detail, _ := payload["details"].(string); detail == "" {
			t.Fatalf("%s probe did not return details: %+v", operation, payload)
		}
	}
}

func TestProviderUpsertInvalidatesCatalogAndLifecycleEvidence(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"fixture-model"}]}`))
	}))
	defer upstream.Close()
	config.Update(func(s *config.Settings) {
		s.APIKey = "admin-secret"
		s.Providers = map[string]*config.ProviderConfig{
			"fixture": {
				Type: "openai_compatible", BaseURL: upstream.URL, APIKey: "fixture",
			},
		}
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)
	if models := providers.RefreshCatalog("fixture"); len(models) != 1 {
		t.Fatalf("catalog models=%+v", models)
	}
	generation, err := iam.ProviderCheckGeneration("fixture", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := iam.RecordProviderCheck(iam.ProviderCheck{
		ProviderID: "fixture", Operation: iam.CheckVerify,
		Generation: generation, Success: true, Detail: "Provider check passed.",
	}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(NewServer())
	defer server.Close()
	status, result := jsonRequest(
		t, server.URL+"/admin/api/providers", http.MethodPost, "admin-secret",
		map[string]any{
			"id": "fixture", "type": "openai_compatible",
			"base_url": upstream.URL,
		},
	)
	if status != http.StatusOK || result["ok"] != true {
		t.Fatalf("upsert status=%d result=%+v", status, result)
	}
	if models, _ := providers.CatalogCached("fixture"); len(models) != 0 {
		t.Fatalf("upsert retained catalog: %+v", models)
	}
	checks, err := iam.LastProviderChecks("")
	if err != nil {
		t.Fatal(err)
	}
	if len(checks["fixture"]) != 0 {
		t.Fatalf("upsert retained checks: %+v", checks)
	}
	current, err := iam.ProviderCheckGeneration("fixture", "")
	if err != nil || current <= generation {
		t.Fatalf("generation current=%d previous=%d err=%v", current, generation, err)
	}
}

func TestPortalProviderStatusUsesOnlyItsPrincipalCatalog(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	settings := config.Defaults()
	settings.Providers = map[string]*config.ProviderConfig{
		"echo": {Type: "echo"},
	}
	config.Update(func(current *config.Settings) { *current = *settings })
	t.Cleanup(func() {
		defaults := config.Defaults()
		config.Update(func(current *config.Settings) { *current = *defaults })
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)
	first, err := iam.CreatePrincipal("human", "authentik:first-catalog", "", "First")
	if err != nil {
		t.Fatal(err)
	}
	second, err := iam.CreatePrincipal("human", "authentik:second-catalog", "", "Second")
	if err != nil {
		t.Fatal(err)
	}
	firstPrincipal := &config.Principal{PrincipalID: first.ID, PrincipalKind: first.Kind}
	if models := providers.RefreshCatalogForPrincipal("echo", firstPrincipal); len(models) == 0 {
		t.Fatal("first principal catalog did not refresh")
	}
	firstRows, err := providerStatusSnapshots(settings, nil, nil, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	secondRows, err := providerStatusSnapshots(settings, nil, nil, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	modelCount := func(rows []map[string]any) int {
		for _, row := range rows {
			if row["id"] == "echo" {
				return row["model_count"].(int)
			}
		}
		return -1
	}
	if got := modelCount(firstRows); got == 0 {
		t.Fatalf("first principal model count=%d", got)
	}
	if got := modelCount(secondRows); got != 0 {
		t.Fatalf("second principal saw another catalog: %d", got)
	}
}

// Portal mode renders one human's view, and the not-discoverable verdict must
// be settled inside that scope. checks and cached models are already narrowed
// to the selected principal, but the final comparison reached for the
// operator-wide CatalogSnapshot, which reports the freshest catalog across ALL
// scopes — so another principal's newer catalog cleared this user's
// not_discoverable state while this user's own credential still could not list
// a thing.
func TestPortalNotDiscoverableSurvivesAnotherPrincipalsNewerCatalog(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	settings := config.Defaults()
	settings.Providers = map[string]*config.ProviderConfig{
		"echo": {Type: "echo"},
	}
	config.Update(func(current *config.Settings) { *current = *settings })
	t.Cleanup(func() {
		defaults := config.Defaults()
		config.Update(func(current *config.Settings) { *current = *defaults })
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)

	viewer, err := iam.CreatePrincipal("human", "fixture:portal-viewer", "", "Viewer")
	if err != nil {
		t.Fatal(err)
	}
	other, err := iam.CreatePrincipal("human", "fixture:other-principal", "", "Other")
	if err != nil {
		t.Fatal(err)
	}
	// The viewer's own catalog-facing check found the catalog unlistable.
	generation, err := iam.ProviderCheckGeneration("echo", viewer.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := iam.RecordProviderCheck(iam.ProviderCheck{
		ProviderID: "echo", Operation: iam.CheckCatalogSync, ScopeKey: viewer.ID,
		Generation: generation, Success: false,
		Detail:    providerCheckDetail(false, "catalog_not_discoverable"),
		CheckedAt: time.Now().Add(-10 * time.Minute).Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	// Another principal then listed models. That catalog is newer than the
	// viewer's failure and belongs to somebody else's credential.
	otherPrincipal := &config.Principal{PrincipalID: other.ID, PrincipalKind: other.Kind}
	if models := providers.RefreshCatalogForPrincipal("echo", otherPrincipal); len(models) == 0 {
		t.Fatal("other principal catalog did not refresh")
	}

	rows, err := providerStatusSnapshots(settings, nil, nil, viewer.ID)
	if err != nil {
		t.Fatal(err)
	}
	var row map[string]any
	for _, candidate := range rows {
		if candidate["id"] == "echo" {
			row = candidate
			break
		}
	}
	if row == nil {
		t.Fatalf("echo row missing from %+v", rows)
	}
	if got := stringOf(row["catalog_state"]); got != "not_discoverable" {
		t.Fatalf("portal catalog_state=%q, want \"not_discoverable\" — another principal's catalog cleared it", got)
	}
	instances, ok := row["instances"].([]map[string]any)
	if !ok || len(instances) != 1 {
		t.Fatalf("instances=%#v, want exactly one", row["instances"])
	}
	if got := stringOf(instances[0]["catalog_state"]); got != "not_discoverable" {
		t.Fatalf("portal instance catalog_state=%q, want \"not_discoverable\"", got)
	}
}
