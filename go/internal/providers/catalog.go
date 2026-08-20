package providers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"
)

// A persisted, per-provider model catalog. It is the single source of truth for
// /v1/models, the admin model-card page, and API-adaptation routing decisions
// (a model's supported_surfaces). Refreshes lazily when missing or stale
// (>1h) — frequent enough to surface newly released provider models without a
// cron, while avoiding an upstream fetch on every CLI startup — and on an
// explicit provider Test / Refresh.
type catalogEntry struct {
	// SchemaVersion records which release's rules produced Models. Entries
	// stamped with anything else are dropped on load instead of being served
	// or used as a fallback, because a cached row is only as trustworthy as
	// the code that wrote it:
	//
	//   - v0 (unstamped) rows predate the supported_endpoints ->
	//     supported_surfaces field rename, so their surface lists unmarshal
	//     into a nil SupportedSurfaces. Serving them would silently route a
	//     Responses-only model to /chat/completions and drop it from the
	//     /v1/models chat listing.
	//   - v0 Vertex rows may be the deleted hand-curated publisher list,
	//     six of whose nine ids exist in no region a given instance can call.
	//     A key-only Vertex instance can never refresh, so without this drop
	//     it would serve that list forever.
	//
	// Bump this whenever a release changes what a persisted row means; the
	// cost is one forced re-discovery per provider, the alternative is
	// serving data whose meaning has moved underneath it.
	SchemaVersion int         `json:"schema_version,omitempty"`
	Models        []ModelInfo `json:"models"`
	RefreshedAt   time.Time   `json:"refreshed_at"`
}

const catalogSchemaVersion = 1

var (
	catMu         sync.Mutex
	catData       map[string]catalogEntry
	catGeneration map[string]uint64
	catLoaded     bool
)

const catalogTTL = time.Hour

// CatalogRequiresPrincipal identifies integrations whose catalogs must be
// resolved through one human-owned connection rather than a global credential.
func CatalogRequiresPrincipal(providerID string) bool {
	cfg, ok := config.Get().Providers[providerID]
	return ok && EffectiveRegistryID(providerID, cfg.RegistryID, cfg.Type) == "openai_codex"
}

// ProviderConfigurationIssue reports setup that must be completed before a
// provider can expose a runnable catalog. Cached rows never override this gate.
func ProviderConfigurationIssue(providerID string) string {
	cfg, ok := config.Get().Providers[providerID]
	if !ok {
		return ""
	}
	registryID := EffectiveRegistryID(providerID, cfg.RegistryID, cfg.Type)
	entry, _ := RegistryProviderByID(registryID)
	if entry.RequiresBaseURL && strings.TrimSpace(cfg.BaseURL) == "" {
		return entry.Label + " requires a base URL."
	}
	switch registryID {
	case "vertex_ai":
		if strings.TrimSpace(cfg.Project) == "" {
			return "Vertex AI requires a Google Cloud project ID."
		}
	case "azure_openai":
		// Reported here as well as at instantiate so an endpoint that cannot be
		// normalised is visible on the provider row instead of only failing the
		// first request made through it.
		if _, err := azureInferenceBaseURL(cfg.BaseURL); err != nil {
			return "Azure OpenAI base URL is not a resource endpoint: it must be an http(s) URL with no path, or a path ending in /openai/v1."
		}
	case "openai_codex":
		if strings.TrimSpace(config.Get().OpenAICodexClientID) == "" {
			return "OpenAI Codex requires an OAuth client ID."
		}
	}
	return ""
}

func catalogPath() string { return filepath.Join(config.StateDir(), "catalog.json") }

func loadCatalogLocked() {
	if catLoaded {
		return
	}
	catData = map[string]catalogEntry{}
	catGeneration = map[string]uint64{}
	if b, err := os.ReadFile(catalogPath()); err == nil {
		_ = json.Unmarshal(b, &catData)
	}
	// Drop every entry this build cannot vouch for. The file is not rewritten
	// here: a read must not have a write side-effect, and the next successful
	// refresh persists the pruned map anyway. Until then the drop simply
	// repeats on each process start, which costs one map walk.
	for key, entry := range catData {
		if entry.SchemaVersion != catalogSchemaVersion {
			delete(catData, key)
		}
	}
	catLoaded = true
}

func saveCatalogLocked() {
	_ = os.MkdirAll(config.StateDir(), 0o755)
	if b, err := json.MarshalIndent(catData, "", "  "); err == nil {
		_ = os.WriteFile(catalogPath(), b, 0o644)
	}
}

func cachedEntry(providerID string) (catalogEntry, bool) {
	catMu.Lock()
	defer catMu.Unlock()
	loadCatalogLocked()
	e, ok := catData[providerID]
	return e, ok
}

func storeEntry(providerID string, models []ModelInfo) {
	catMu.Lock()
	defer catMu.Unlock()
	loadCatalogLocked()
	catData[providerID] = catalogEntry{
		SchemaVersion: catalogSchemaVersion, Models: models, RefreshedAt: time.Now(),
	}
	saveCatalogLocked()
}

func catalogGenerationFor(providerID string) uint64 {
	catMu.Lock()
	defer catMu.Unlock()
	loadCatalogLocked()
	if _, exists := catGeneration[providerID]; !exists {
		catGeneration[providerID] = 0
	}
	return catGeneration[providerID]
}

func storeEntryIfGeneration(
	providerID string, models []ModelInfo, expected uint64,
) bool {
	catMu.Lock()
	defer catMu.Unlock()
	loadCatalogLocked()
	if catGeneration[providerID] != expected {
		return false
	}
	catData[providerID] = catalogEntry{
		SchemaVersion: catalogSchemaVersion, Models: models, RefreshedAt: time.Now(),
	}
	saveCatalogLocked()
	return true
}

// CatalogModels returns the cached model list for a provider, refreshing lazily
// when missing or stale. The upstream fetch runs OUTSIDE the lock; a failed
// refresh keeps whatever was cached.
func CatalogModels(providerID string) []ModelInfo {
	return CatalogModelsForPrincipal(providerID, nil)
}

func CatalogModelsForPrincipal(
	providerID string, principal *config.Principal,
) []ModelInfo {
	if ProviderConfigurationIssue(providerID) != "" {
		return nil
	}
	authorized, err := ProviderCredentialAuthorized(providerID, principal)
	if err != nil || !authorized {
		return nil
	}
	cacheKey := catalogCacheKey(providerID, principal)
	e, ok := cachedEntry(cacheKey)
	if ok && time.Since(e.RefreshedAt) <= catalogTTL {
		return e.Models
	}
	generation := catalogGenerationFor(cacheKey)
	if models := ListProviderModelsForPrincipal(providerID, principal); len(models) > 0 {
		storeEntryIfGeneration(cacheKey, models, generation)
		return models
	}
	return e.Models
}

func catalogCacheKey(providerID string, principal *config.Principal) string {
	return providerCacheKey(providerID, principal)
}

// RefreshCatalog forces a re-fetch for one provider and returns the new list.
func RefreshCatalog(providerID string) []ModelInfo {
	return RefreshCatalogForPrincipal(providerID, nil)
}

func RefreshCatalogForPrincipal(providerID string, principal *config.Principal) []ModelInfo {
	models, _, _ := RefreshCatalogForPrincipalWithError(providerID, principal)
	return models
}

// RefreshCatalogForPrincipalWithError forces a refresh and preserves a safe,
// structured failure for lifecycle diagnostics. Legitimate empty catalogs are
// stored as successful refreshes and returned without an error.
func RefreshCatalogForPrincipalWithError(
	providerID string, principal *config.Principal,
) ([]ModelInfo, *iam.ProviderAccountObservation, error) {
	if issue := ProviderConfigurationIssue(providerID); issue != "" {
		return nil, nil, catalogError(
			"catalog_configuration_incomplete", issue, 0,
		)
	}
	if CatalogRequiresPrincipal(providerID) && (principal == nil || strings.TrimSpace(principal.PrincipalID) == "") {
		return nil, nil, catalogError(
			"catalog_principal_required",
			"An active human principal is required for this private provider catalog.",
			0,
		)
	}
	cacheKey := catalogCacheKey(providerID, principal)
	generation := catalogGenerationFor(cacheKey)
	var initialObservation *iam.ProviderAccountObservation
	if principal != nil && principal.PrincipalKind == "human" {
		if observed, found, err := iam.ActiveProviderAccountObservation(
			principal.PrincipalID, providerID,
		); err == nil && found {
			initialObservation = &observed
		}
	}
	models, observation, err := ListProviderModelsForPrincipalWithError(providerID, principal)
	if err != nil {
		return nil, observation, err
	}
	if models == nil {
		models = []ModelInfo{}
	}
	if !storeEntryIfGeneration(cacheKey, models, generation) {
		currentGeneration := catalogGenerationFor(cacheKey)
		refreshRebased := initialObservation != nil && observation != nil &&
			initialObservation.ConnectionID == observation.ConnectionID &&
			observation.CredentialRevision > initialObservation.CredentialRevision &&
			currentGeneration == generation+1
		if refreshRebased {
			current, found, currentErr := iam.ActiveProviderAccountObservation(
				principal.PrincipalID, providerID,
			)
			refreshRebased = currentErr == nil && found && current == *observation &&
				storeEntryIfGeneration(cacheKey, models, currentGeneration)
		}
		if !refreshRebased {
			return nil, observation, catalogError(
				"catalog_state_changed",
				"Provider configuration changed during catalog refresh.",
				0,
			)
		}
	}
	return models, observation, nil
}

// CatalogLookup returns a single model's info from the (lazily-refreshed) catalog.
func CatalogLookup(providerID, model string) (ModelInfo, bool) {
	return CatalogLookupForPrincipal(providerID, model, nil)
}

func CatalogLookupForPrincipal(
	providerID, model string, principal *config.Principal,
) (ModelInfo, bool) {
	for _, m := range CatalogModelsForPrincipal(providerID, principal) {
		if m.ID == model {
			return m, true
		}
	}
	return ModelInfo{}, false
}

// CatalogRefreshedAt reports when a provider's catalog was last refreshed (zero
// time if never).
func CatalogRefreshedAt(providerID string) time.Time {
	return CatalogRefreshedAtForPrincipal(providerID, nil)
}

func CatalogRefreshedAtForPrincipal(providerID string, principal *config.Principal) time.Time {
	if ProviderConfigurationIssue(providerID) != "" {
		return time.Time{}
	}
	if CatalogRequiresPrincipal(providerID) && (principal == nil || strings.TrimSpace(principal.PrincipalID) == "") {
		return time.Time{}
	}
	e, _ := cachedEntry(catalogCacheKey(providerID, principal))
	return e.RefreshedAt
}

// CatalogCached returns the currently-cached models + refresh time WITHOUT
// triggering a refresh — for fast status reads (e.g. the admin state endpoint).
func CatalogCached(providerID string) ([]ModelInfo, time.Time) {
	return CatalogCachedForPrincipal(providerID, nil)
}

func CatalogCachedForPrincipal(providerID string, principal *config.Principal) ([]ModelInfo, time.Time) {
	if ProviderConfigurationIssue(providerID) != "" {
		return nil, time.Time{}
	}
	if CatalogRequiresPrincipal(providerID) && (principal == nil || strings.TrimSpace(principal.PrincipalID) == "") {
		return nil, time.Time{}
	}
	e, _ := cachedEntry(catalogCacheKey(providerID, principal))
	return e.Models, e.RefreshedAt
}

// ForgetCatalog drops a provider's cached catalog (used when it's deleted).
func ForgetCatalog(providerID string) {
	forgetCatalogMatching(func(key string) bool {
		return key == providerID || strings.HasPrefix(key, providerID+"@")
	})
}

func ForgetCatalogForPrincipal(providerID, principalID string) {
	if providerID == "" || principalID == "" {
		return
	}
	key := providerID + "@" + principalID
	forgetCatalogMatching(func(candidate string) bool {
		return candidate == key || strings.HasPrefix(candidate, key+"#")
	})
}

// ForgetInheritedCatalogs removes only project-scoped service catalogs. Human
// BYOC catalogs use provider@principal keys and must survive unrelated service
// credential imports and bindings.
func ForgetInheritedCatalogs(providerID string) {
	forgetCatalogMatching(func(key string) bool {
		return strings.HasPrefix(key, providerID+"@") && strings.Contains(key, "#")
	})
}

// ForgetCatalogForProject removes inherited service catalogs for one project.
func ForgetCatalogForProject(providerID, projectID string) {
	if providerID == "" || projectID == "" {
		return
	}
	forgetCatalogMatching(func(key string) bool {
		return strings.HasPrefix(key, providerID+"@") && strings.HasSuffix(key, "#"+projectID)
	})
}

func forgetCatalogMatching(matches func(string) bool) {
	catMu.Lock()
	defer catMu.Unlock()
	loadCatalogLocked()
	changed := false
	invalidated := map[string]bool{}
	for key := range catData {
		if matches(key) {
			delete(catData, key)
			invalidated[key] = true
			changed = true
		}
	}
	for key := range catGeneration {
		if matches(key) {
			invalidated[key] = true
		}
	}
	for key := range invalidated {
		catGeneration[key]++
	}
	if changed {
		saveCatalogLocked()
	}
}

// CatalogSnapshot reports the freshest catalog known for a provider across
// every principal scope. Catalogs are cached per caller so one principal's
// private credential never yields another's model list, but an operator-wide
// view (the provider hub, the setup guide) must still be able to say whether a
// provider has a synced catalog at all. It is a display signal only: request
// paths keep using the caller-scoped catalog.
func CatalogSnapshot(providerID string) ([]ModelInfo, time.Time) {
	if ProviderConfigurationIssue(providerID) != "" {
		return nil, time.Time{}
	}
	catMu.Lock()
	defer catMu.Unlock()
	loadCatalogLocked()
	var best catalogEntry
	for key, entry := range catData {
		if key != providerID && !strings.HasPrefix(key, providerID+"@") {
			continue
		}
		if entry.RefreshedAt.After(best.RefreshedAt) {
			best = entry
		}
	}
	return best.Models, best.RefreshedAt
}
