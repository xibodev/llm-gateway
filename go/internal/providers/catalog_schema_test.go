package providers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"llmgw/internal/config"
)

func resetCatalogForTest(t *testing.T) {
	t.Helper()
	reset := func() {
		catMu.Lock()
		catData = nil
		catGeneration = nil
		catLoaded = false
		catMu.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

// A catalog.json written before the supported_endpoints -> supported_surfaces
// rename unmarshals into a nil SupportedSurfaces: the struct tag moved, and
// encoding/json has no idea the two keys are the same field. Those rows are
// also the ones that may still hold the deleted hand-curated Vertex list. Both
// hazards are the same shape — a persisted row whose meaning changed under it —
// so the schema stamp discards them rather than serving them.
func TestPreRenameCatalogEntriesAreDiscarded(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LLMGW_STATE_DIR", dir)
	resetCatalogForTest(t)

	legacy := `{
  "vertex_ai": {
    "models": [
      {"id": "gemini-2.5-pro", "supported_endpoints": ["/v1/chat/completions", "/v1/messages"]},
      {"id": "gemini-1.0-pro", "supported_endpoints": ["/v1/chat/completions"]}
    ],
    "refreshed_at": "` + time.Now().UTC().Format(time.RFC3339) + `"
  }
}`
	if err := os.WriteFile(filepath.Join(dir, "catalog.json"), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	// Guard the premise: the same bytes read into the current struct do lose
	// the surface list. If this ever stops being true the discard is no longer
	// the only thing standing between an upgrade and a mis-routed model.
	var raw map[string]catalogEntry
	if err := json.Unmarshal([]byte(legacy), &raw); err != nil {
		t.Fatal(err)
	}
	if got := raw["vertex_ai"].Models[0].SupportedSurfaces; got != nil {
		t.Fatalf("pre-rename surfaces unexpectedly survived unmarshal: %v", got)
	}

	models, refreshed := CatalogCached("vertex_ai")
	if len(models) != 0 {
		t.Fatalf("unstamped catalog entry was served: %+v", models)
	}
	if !refreshed.IsZero() {
		t.Fatalf("unstamped catalog entry kept its refresh time: %v", refreshed)
	}
	// A zero refresh time is what makes the next read treat the catalog as
	// missing and re-discover it, and what lets the provider status endpoint
	// report "not discoverable" instead of "9 models, stale".
}

// The drop must be limited to entries this build did not write; a normal
// restart must not throw away every provider's catalog.
func TestCurrentSchemaCatalogEntriesSurviveAReload(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LLMGW_STATE_DIR", dir)
	resetCatalogForTest(t)

	owner := &config.Principal{PrincipalID: "prn_owner", PrincipalKind: "human"}
	storeEntry(catalogCacheKey("copilot", owner), []ModelInfo{
		{ID: "gpt-5.5", SupportedSurfaces: []string{"/responses"}},
	})

	catMu.Lock()
	catData = nil
	catGeneration = nil
	catLoaded = false
	catMu.Unlock()

	models, refreshed := CatalogCachedForPrincipal("copilot", owner)
	if len(models) != 1 {
		t.Fatalf("reload dropped a current-schema entry: %+v", models)
	}
	if len(models[0].SupportedSurfaces) != 1 || models[0].SupportedSurfaces[0] != "/responses" {
		t.Fatalf("surfaces did not round-trip: %+v", models[0])
	}
	if refreshed.IsZero() {
		t.Fatal("reload lost the refresh timestamp")
	}
}
