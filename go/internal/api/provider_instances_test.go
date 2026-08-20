package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
	"llmgw/internal/router"
)

// Two providers of one registry type must surface as two addressable instances,
// and the tile must not adopt either one's status as its own.
func TestStatusPayloadCarriesEachInstance(t *testing.T) {
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
		s.Endpoints = map[string]*config.EndpointConfig{}
		s.Providers = map[string]*config.ProviderConfig{
			"vertex-prod": {Type: "vertex_ai", RegistryID: "vertex_ai", Project: "p-a", Location: "global"},
			"vertex-dev":  {Type: "vertex_ai", RegistryID: "vertex_ai", Project: "p-b", Location: "us-central1"},
		}
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)

	server := httptest.NewServer(NewServer())
	defer server.Close()

	status, state := jsonRequest(t, server.URL+"/admin/api/state", http.MethodGet, "admin-secret", nil)
	if status != http.StatusOK {
		t.Fatalf("state status=%d", status)
	}

	var tile map[string]any
	for _, raw := range state["provider_statuses"].([]any) {
		row := raw.(map[string]any)
		if row["id"] == "vertex_ai" {
			tile = row
			break
		}
	}
	if tile == nil {
		t.Fatal("vertex_ai tile missing from provider_statuses")
	}

	instances, ok := tile["instances"].([]any)
	if !ok {
		t.Fatalf("instances missing or wrong type: %T", tile["instances"])
	}
	if len(instances) != 2 {
		t.Fatalf("instances=%d, want 2", len(instances))
	}

	seen := map[string]bool{}
	for _, raw := range instances {
		inst := raw.(map[string]any)
		id, _ := inst["id"].(string)
		seen[id] = true
		for _, field := range []string{"status", "model_count", "catalog_state", "disabled"} {
			if _, present := inst[field]; !present {
				t.Fatalf("instance %q missing %q", id, field)
			}
		}
	}
	if !seen["vertex-prod"] || !seen["vertex-dev"] {
		t.Fatalf("instance ids=%v, want vertex-prod and vertex-dev", seen)
	}

	// The tile aggregates; it is not itself an instance, so it must not wear an
	// instance's status. A healthy first instance must never mask a broken second.
	if got := tile["status"]; got != "configured" {
		t.Fatalf("tile status=%v, want \"configured\"", got)
	}
	counts, ok := tile["instance_status_counts"].(map[string]any)
	if !ok {
		t.Fatalf("instance_status_counts missing or wrong type: %T", tile["instance_status_counts"])
	}
	total := 0
	for _, v := range counts {
		total += int(v.(float64))
	}
	if total != 2 {
		t.Fatalf("instance_status_counts sums to %d, want 2", total)
	}
}

// The masking bug itself: with one sound instance and one broken instance under
// the same tile, the broken one must remain visible. Before this change the tile
// took matches[0].status, so whichever instance happened to sort first decided
// what the operator saw.
func TestBrokenInstanceIsNotMaskedByASoundOne(t *testing.T) {
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
	// vertex_ai without a project is a configuration error; with one it is not.
	// That gives two instances under one tile with genuinely different statuses.
	config.Update(func(s *config.Settings) {
		s.APIKey = "admin-secret"
		s.AllowUnauthenticatedAPI = false
		s.Endpoints = map[string]*config.EndpointConfig{}
		s.Providers = map[string]*config.ProviderConfig{
			"vertex-sound":  {Type: "vertex_ai", RegistryID: "vertex_ai", Project: "p-a", Location: "global"},
			"vertex-broken": {Type: "vertex_ai", RegistryID: "vertex_ai", Location: "global"},
		}
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)

	server := httptest.NewServer(NewServer())
	defer server.Close()

	_, state := jsonRequest(t, server.URL+"/admin/api/state", http.MethodGet, "admin-secret", nil)

	var tile map[string]any
	for _, raw := range state["provider_statuses"].([]any) {
		row := raw.(map[string]any)
		if row["id"] == "vertex_ai" {
			tile = row
			break
		}
	}
	if tile == nil {
		t.Fatal("vertex_ai tile missing")
	}

	statusByInstance := map[string]string{}
	for _, raw := range tile["instances"].([]any) {
		inst := raw.(map[string]any)
		statusByInstance[stringOf(inst["id"])] = stringOf(inst["status"])
	}
	if len(statusByInstance) != 2 {
		t.Fatalf("instances=%v, want 2", statusByInstance)
	}
	broken := statusByInstance["vertex-broken"]
	sound := statusByInstance["vertex-sound"]
	if broken == "" {
		t.Fatal("vertex-broken carries no status of its own")
	}
	if broken == sound {
		t.Fatalf("both instances report %q; the broken one is indistinguishable", broken)
	}

	// The broken instance must be represented in the composition the tile
	// advertises, whatever order the instances happened to be built in.
	counts := tile["instance_status_counts"].(map[string]any)
	if _, present := counts[broken]; !present {
		t.Fatalf("instance_status_counts=%v omits the broken status %q", counts, broken)
	}

	// And the tile must not have adopted either instance's status as its own.
	if got := stringOf(tile["status"]); got == broken || got == sound {
		t.Fatalf("tile status=%q copies an instance status; want \"configured\"", got)
	}
}

func stringOf(v any) string {
	s, _ := v.(string)
	return s
}

// Two misconfigured instances must keep their problems separate rather than
// being concatenated into one unreadable sentence.
func TestInstanceConfigurationIssuesStaySeparate(t *testing.T) {
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
	// vertex_ai without a project is a configuration error, so both instances
	// report an issue. Each instance's message must be its own single
	// complaint, not the tile-level join of both instances' messages — a
	// regression that handed every instance the tile's joined string would
	// leave the "present" check green, so we also compare instance-to-instance
	// and instance-to-tile.
	config.Update(func(s *config.Settings) {
		s.APIKey = "admin-secret"
		s.AllowUnauthenticatedAPI = false
		s.Endpoints = map[string]*config.EndpointConfig{}
		s.Providers = map[string]*config.ProviderConfig{
			"vertex-a": {Type: "vertex_ai", RegistryID: "vertex_ai", Location: "global"},
			"vertex-b": {Type: "vertex_ai", RegistryID: "vertex_ai", Location: "global"},
		}
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)

	server := httptest.NewServer(NewServer())
	defer server.Close()

	_, state := jsonRequest(t, server.URL+"/admin/api/state", http.MethodGet, "admin-secret", nil)
	for _, raw := range state["provider_statuses"].([]any) {
		row := raw.(map[string]any)
		if row["id"] != "vertex_ai" {
			continue
		}
		tileIssue := stringOf(row["configuration_issue"])
		issueByInstance := map[string]string{}
		for _, instRaw := range row["instances"].([]any) {
			inst := instRaw.(map[string]any)
			issue := stringOf(inst["configuration_issue"])
			if issue == "" {
				t.Fatalf("instance %v missing configuration_issue", inst["id"])
			}
			issueByInstance[stringOf(inst["id"])] = issue
		}
		a, b := issueByInstance["vertex-a"], issueByInstance["vertex-b"]
		if a != b {
			t.Fatalf("vertex-a issue %q != vertex-b issue %q; both are configured identically", a, b)
		}
		// The tile joins both instances' messages with a space, so a correct
		// per-instance value is strictly shorter than the tile's. Equal length
		// (or equal value) means the instance row picked up the joined string.
		if len(a) >= len(tileIssue) {
			t.Fatalf("instance issue %q is not shorter than tile issue %q; instances appear to carry the joined value", a, tileIssue)
		}
		return
	}
	t.Fatal("vertex_ai tile missing")
}

// A configured provider that no curated tile claims is still an instance. It
// used to arrive carrying only `configured: true`, which left the console's
// lifecycle table with nothing to render: Enable/Disable, test completion and
// cache reset exist nowhere else, so a disabled custom provider could never be
// re-enabled from the UI.
func TestCustomProviderRowCarriesItsOwnInstance(t *testing.T) {
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
	// A non-canonical id with no explicit registry_id resolves to no registry
	// entry, so this provider is emitted by the non-registry loop.
	config.Update(func(s *config.Settings) {
		s.APIKey = "admin-secret"
		s.AllowUnauthenticatedAPI = false
		s.Endpoints = map[string]*config.EndpointConfig{}
		s.Providers = map[string]*config.ProviderConfig{
			"my-openai": {Type: "openai", BaseURL: "http://127.0.0.1:1/v1", Disabled: true},
		}
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)

	server := httptest.NewServer(NewServer())
	defer server.Close()

	_, state := jsonRequest(t, server.URL+"/admin/api/state", http.MethodGet, "admin-secret", nil)

	var row map[string]any
	for _, raw := range state["provider_statuses"].([]any) {
		candidate := raw.(map[string]any)
		if candidate["id"] == "my-openai" {
			row = candidate
			break
		}
	}
	if row == nil {
		t.Fatal("my-openai missing from provider_statuses")
	}

	ids, ok := row["configured_provider_ids"].([]any)
	if !ok || len(ids) != 1 || stringOf(ids[0]) != "my-openai" {
		t.Fatalf("configured_provider_ids=%#v, want [my-openai]", row["configured_provider_ids"])
	}

	instances, ok := row["instances"].([]any)
	if !ok || len(instances) != 1 {
		t.Fatalf("instances=%#v, want exactly one", row["instances"])
	}
	instance := instances[0].(map[string]any)
	if stringOf(instance["id"]) != "my-openai" {
		t.Fatalf("instance id=%v, want my-openai", instance["id"])
	}
	// The lifecycle table renders every one of these; a missing key is a blank
	// cell or a dead action, so each is asserted by name.
	for _, field := range []string{
		"status", "model_count", "connection_count",
		"catalog_state", "catalog_refreshed", "disabled", "configuration_issue",
	} {
		if _, present := instance[field]; !present {
			t.Fatalf("instance missing %q: %#v", field, instance)
		}
	}
	if instance["disabled"] != true {
		t.Fatalf("instance disabled=%v, want true", instance["disabled"])
	}
	if stringOf(instance["status"]) != stringOf(row["status"]) {
		t.Fatalf("instance status=%v, row status=%v; a one-instance row must agree with itself", instance["status"], row["status"])
	}

	counts, ok := row["instance_status_counts"].(map[string]any)
	if !ok {
		t.Fatalf("instance_status_counts missing or wrong type: %T", row["instance_status_counts"])
	}
	if len(counts) != 1 || counts[stringOf(row["status"])] != float64(1) {
		t.Fatalf("instance_status_counts=%#v, want {%q: 1}", counts, stringOf(row["status"]))
	}
}

// A configured provider is free to be named after a registry id it does not
// implement. Both rows then arrive under the same id, so the payload has to say
// which of them is the curated tile — otherwise the console keys them together
// and the tile wears the unrelated provider's data.
func TestCustomRowIsDistinguishableFromACollidingTile(t *testing.T) {
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
	// Named "openai" but running the anthropic runtime, so it resolves to no
	// registry entry and is emitted by the non-registry loop under that id.
	config.Update(func(s *config.Settings) {
		s.APIKey = "admin-secret"
		s.AllowUnauthenticatedAPI = false
		s.Endpoints = map[string]*config.EndpointConfig{}
		s.Providers = map[string]*config.ProviderConfig{
			"openai": {Type: "anthropic"},
		}
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)

	server := httptest.NewServer(NewServer())
	defer server.Close()

	_, state := jsonRequest(t, server.URL+"/admin/api/state", http.MethodGet, "admin-secret", nil)

	rows := []map[string]any{}
	for _, raw := range state["provider_statuses"].([]any) {
		row := raw.(map[string]any)
		if row["id"] == "openai" {
			rows = append(rows, row)
		}
	}
	if len(rows) != 2 {
		t.Fatalf("rows with id openai=%d, want 2 (the curated tile and the custom provider)", len(rows))
	}
	tiles, customs := 0, 0
	for _, row := range rows {
		if row["custom"] == true {
			customs++
			if row["configured"] != true {
				t.Fatalf("custom row is not configured: %#v", row)
			}
			continue
		}
		tiles++
		if row["configured"] != false {
			t.Fatalf("curated tile claims the unrelated provider as its own: %#v", row["configured"])
		}
	}
	if tiles != 1 || customs != 1 {
		t.Fatalf("tiles=%d customs=%d, want exactly one of each", tiles, customs)
	}
}

// A successful verify must not clear the not-discoverable state. Verify runs
// one inference request and never probes the catalog, so it establishes
// nothing about whether the catalog can be listed — but it is a later check,
// and reading the state off "the latest check of any operation" let it silently
// relabel a catalog no credential can list as merely unknown.
func TestSuccessfulVerifyDoesNotClearNotDiscoverable(t *testing.T) {
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
		s.Endpoints = map[string]*config.EndpointConfig{}
		s.Providers = map[string]*config.ProviderConfig{
			"vertex-key-only": {Type: "vertex_ai", RegistryID: "vertex_ai", Project: "p-a", Location: "global", APIKey: "test-key"},
		}
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)

	generation, err := iam.ProviderCheckGeneration("vertex-key-only", "")
	if err != nil {
		t.Fatal(err)
	}
	// Explicit timestamps: the verify must be unambiguously the later check,
	// which is exactly the ordering that used to clear the state.
	now := time.Now().Unix()
	if err := iam.RecordProviderCheck(iam.ProviderCheck{
		ProviderID: "vertex-key-only", Operation: iam.CheckCatalogSync,
		Generation: generation, Success: false,
		Detail:    providerCheckDetail(false, "catalog_not_discoverable"),
		CheckedAt: now - 600,
	}); err != nil {
		t.Fatal(err)
	}
	if err := iam.RecordProviderCheck(iam.ProviderCheck{
		ProviderID: "vertex-key-only", Operation: iam.CheckVerify,
		Generation: generation, Success: true,
		Detail: providerCheckDetail(true, ""),
		Model:  "gemini-fixture", CheckedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(NewServer())
	defer server.Close()
	_, state := jsonRequest(t, server.URL+"/admin/api/state", http.MethodGet, "admin-secret", nil)

	var tile map[string]any
	for _, raw := range state["provider_statuses"].([]any) {
		row := raw.(map[string]any)
		if row["id"] == "vertex_ai" {
			tile = row
			break
		}
	}
	if tile == nil {
		t.Fatal("vertex_ai tile missing from provider_statuses")
	}
	instances, ok := tile["instances"].([]any)
	if !ok || len(instances) != 1 {
		t.Fatalf("instances=%#v, want exactly one", tile["instances"])
	}
	instance := instances[0].(map[string]any)
	// The ladder still reports the verify — that part is correct and must stay.
	if got := stringOf(instance["last_check_operation"]); got != iam.CheckVerify {
		t.Fatalf("last_check_operation=%q, want the verify to remain the latest check", got)
	}
	if got := stringOf(instance["catalog_state"]); got != "not_discoverable" {
		t.Fatalf("instance catalog_state=%q, want \"not_discoverable\"", got)
	}
	if got := stringOf(tile["catalog_state"]); got != "not_discoverable" {
		t.Fatalf("tile catalog_state=%q, want \"not_discoverable\"", got)
	}
}

// A provider whose catalog cannot be listed by any credential (Vertex with
// only an API key; Azure when its deployments route fails) must report that
// plainly rather than the same "unknown" state a provider nobody has ever
// synced would also show.
func TestCatalogStateReportsNotDiscoverableFromLastFailedCheck(t *testing.T) {
	// All three catalog-facing operations must qualify. "repair" (CheckCacheReset)
	// runs the identical unconditional catalog fetch runProviderProbe uses for
	// "test"/"refresh" — it only differs by forgetting cached state first — so a
	// failed repair must relabel the catalog exactly like a failed test or sync.
	// A prior version of this guard excluded CheckCacheReset, which meant
	// clicking "Clear cache & retry" on an already-labelled row silently
	// reverted the display back to "0 models · unknown".
	operations := []struct {
		name      string
		operation string
	}{
		{"reachability", iam.CheckReachability},
		{"catalog_sync", iam.CheckCatalogSync},
		{"cache_reset", iam.CheckCacheReset},
	}
	for _, tc := range operations {
		t.Run(tc.name, func(t *testing.T) {
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
				s.Endpoints = map[string]*config.EndpointConfig{}
				s.Providers = map[string]*config.ProviderConfig{
					"vertex-key-only": {Type: "vertex_ai", RegistryID: "vertex_ai", Project: "p-a", Location: "global", APIKey: "test-key"},
				}
			})
			providers.ResetProviders()
			t.Cleanup(providers.ResetProviders)

			// Seed the check history the way runProviderProbe would have left it
			// after a real check failed with providers.CatalogFailure's
			// "catalog_not_discoverable" code, without making any network call.
			generation, err := iam.ProviderCheckGeneration("vertex-key-only", "")
			if err != nil {
				t.Fatal(err)
			}
			if err := iam.RecordProviderCheck(iam.ProviderCheck{
				ProviderID: "vertex-key-only", Operation: tc.operation,
				Generation: generation, Success: false,
				Detail: providerCheckDetail(false, "catalog_not_discoverable"),
			}); err != nil {
				t.Fatal(err)
			}

			server := httptest.NewServer(NewServer())
			defer server.Close()
			_, state := jsonRequest(t, server.URL+"/admin/api/state", http.MethodGet, "admin-secret", nil)

			var tile map[string]any
			for _, raw := range state["provider_statuses"].([]any) {
				row := raw.(map[string]any)
				if row["id"] == "vertex_ai" {
					tile = row
					break
				}
			}
			if tile == nil {
				t.Fatal("vertex_ai tile missing from provider_statuses")
			}
			instances, ok := tile["instances"].([]any)
			if !ok || len(instances) != 1 {
				t.Fatalf("instances=%#v, want exactly one", tile["instances"])
			}
			instance := instances[0].(map[string]any)
			if got := stringOf(instance["catalog_state"]); got != "not_discoverable" {
				t.Fatalf("instance catalog_state=%q, want \"not_discoverable\"", got)
			}
			if got := stringOf(tile["catalog_state"]); got != "not_discoverable" {
				t.Fatalf("tile catalog_state=%q, want \"not_discoverable\"", got)
			}
		})
	}
}

// One principal's failed API-key check must not label the instance
// undiscoverable while another principal's credential is listing models.
//
// In admin mode LastProviderChecks("") returns every principal's checks and
// CatalogSnapshot reports the freshest catalog across scopes, so reading the
// state off the single latest catalog-facing check made a Vertex API-key user's
// failure the verdict for the whole instance — and for the tile — even though a
// service-account credential in another scope had just discovered models.
func TestNotDiscoverableIsNotClaimedWhenAnotherScopeDiscoveredModels(t *testing.T) {
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
		s.Endpoints = map[string]*config.EndpointConfig{}
		s.Providers = map[string]*config.ProviderConfig{
			"vertex-multi-scope": {Type: "vertex_ai", RegistryID: "vertex_ai", Project: "p-a", Location: "global", APIKey: "test-key"},
		}
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)

	keyOnly, err := iam.CreatePrincipal("human", "fixture:key-only", "", "Key Only")
	if err != nil {
		t.Fatal(err)
	}
	serviceAccount, err := iam.CreatePrincipal("human", "fixture:service-account", "", "Service Account")
	if err != nil {
		t.Fatal(err)
	}
	// The service account listed the catalog first; the API-key principal then
	// failed. The failure is the LATEST check overall, which is exactly the
	// ordering that used to decide the state for both scopes at once.
	now := time.Now().Unix()
	record := func(scopeKey string, success bool, code string, checkedAt int64) {
		t.Helper()
		generation, err := iam.ProviderCheckGeneration("vertex-multi-scope", scopeKey)
		if err != nil {
			t.Fatal(err)
		}
		if err := iam.RecordProviderCheck(iam.ProviderCheck{
			ProviderID: "vertex-multi-scope", Operation: iam.CheckCatalogSync,
			ScopeKey: scopeKey, Generation: generation, Success: success,
			Detail: providerCheckDetail(success, code), CheckedAt: checkedAt,
		}); err != nil {
			t.Fatal(err)
		}
	}
	record(serviceAccount.ID, true, "", now-600)
	record(keyOnly.ID, false, "catalog_not_discoverable", now)

	server := httptest.NewServer(NewServer())
	defer server.Close()
	_, state := jsonRequest(t, server.URL+"/admin/api/state", http.MethodGet, "admin-secret", nil)

	var tile map[string]any
	for _, raw := range state["provider_statuses"].([]any) {
		row := raw.(map[string]any)
		if row["id"] == "vertex_ai" {
			tile = row
			break
		}
	}
	if tile == nil {
		t.Fatal("vertex_ai tile missing from provider_statuses")
	}
	instances, ok := tile["instances"].([]any)
	if !ok || len(instances) != 1 {
		t.Fatalf("instances=%#v, want exactly one", tile["instances"])
	}
	instance := instances[0].(map[string]any)
	if got := stringOf(instance["catalog_state"]); got == "not_discoverable" {
		t.Fatal("instance catalog_state=\"not_discoverable\" while another scope discovered models")
	}
	if got := stringOf(tile["catalog_state"]); got == "not_discoverable" {
		t.Fatal("tile catalog_state=\"not_discoverable\" while another scope discovered models")
	}
}

// The scope-aware rule must not soften the single-scope case: when every scope
// that tried found the catalog unlistable, the instance still says so.
func TestNotDiscoverableHoldsWhenEveryScopeFailed(t *testing.T) {
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
		s.Endpoints = map[string]*config.EndpointConfig{}
		s.Providers = map[string]*config.ProviderConfig{
			"vertex-all-scopes-failed": {Type: "vertex_ai", RegistryID: "vertex_ai", Project: "p-a", Location: "global", APIKey: "test-key"},
		}
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)

	first, err := iam.CreatePrincipal("human", "fixture:first-key", "", "First Key")
	if err != nil {
		t.Fatal(err)
	}
	second, err := iam.CreatePrincipal("human", "fixture:second-key", "", "Second Key")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	for index, scopeKey := range []string{first.ID, second.ID} {
		generation, err := iam.ProviderCheckGeneration("vertex-all-scopes-failed", scopeKey)
		if err != nil {
			t.Fatal(err)
		}
		if err := iam.RecordProviderCheck(iam.ProviderCheck{
			ProviderID: "vertex-all-scopes-failed", Operation: iam.CheckCatalogSync,
			ScopeKey: scopeKey, Generation: generation, Success: false,
			Detail:    providerCheckDetail(false, "catalog_not_discoverable"),
			CheckedAt: now - int64(index*60),
		}); err != nil {
			t.Fatal(err)
		}
	}

	server := httptest.NewServer(NewServer())
	defer server.Close()
	_, state := jsonRequest(t, server.URL+"/admin/api/state", http.MethodGet, "admin-secret", nil)

	var tile map[string]any
	for _, raw := range state["provider_statuses"].([]any) {
		row := raw.(map[string]any)
		if row["id"] == "vertex_ai" {
			tile = row
			break
		}
	}
	if tile == nil {
		t.Fatal("vertex_ai tile missing from provider_statuses")
	}
	instance := tile["instances"].([]any)[0].(map[string]any)
	if got := stringOf(instance["catalog_state"]); got != "not_discoverable" {
		t.Fatalf("instance catalog_state=%q, want \"not_discoverable\"", got)
	}
	if got := stringOf(tile["catalog_state"]); got != "not_discoverable" {
		t.Fatalf("tile catalog_state=%q, want \"not_discoverable\"", got)
	}
}
