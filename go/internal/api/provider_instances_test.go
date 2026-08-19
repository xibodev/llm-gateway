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
		s.Categories = map[string]*config.CategoryConfig{}
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
		s.Categories = map[string]*config.CategoryConfig{}
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
		s.Categories = map[string]*config.CategoryConfig{}
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
