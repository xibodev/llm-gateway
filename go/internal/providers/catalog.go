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

	core "github.com/xibodev/llmgw-core"
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
	//   - v1 anonymous OpenCode Zen rows include paid and deprecated models
	//     because they predate the active zero-cost metadata intersection.
	//   - v2 rows predate persisted typed capability snapshots and discovery
	//     timestamps.
	//   - v3 anonymous OpenCode Zen rows predate shared Zen discovery evidence.
	//   - v4 Vertex rows dropped callable global models whose supportedActions
	//     field was absent or empty.
	//   - v5 Codex rows could omit the provider's invariant Responses surface
	//     when the upstream catalog omitted its legacy supported_endpoints field.
	//
	// Bump this whenever a release changes what a persisted row means; the
	// cost is one forced re-discovery per provider, the alternative is
	// serving data whose meaning has moved underneath it.
	SchemaVersion int         `json:"schema_version,omitempty"`
	Models        []ModelInfo `json:"models"`
	RefreshedAt   time.Time   `json:"refreshed_at"`
}

const catalogSchemaVersion = 6

var (
	catMu         sync.Mutex
	catData       map[string]catalogEntry
	catGeneration map[string]uint64
	// Credential refreshes invalidate previously cached rows, but unlike a
	// revoke, reauthorization, or config edit they do not fence the provider
	// operation that performed the refresh.
	catPersistenceGeneration map[string]uint64
	catLoaded                bool
)

const catalogTTL = time.Hour

// CatalogRequiresPrincipal identifies integrations whose catalogs must be
// resolved through one human-owned connection rather than a global credential.
func CatalogRequiresPrincipal(providerID string) bool {
	cfg, ok := config.Get().Providers[providerID]
	if !ok || cfg == nil {
		return false
	}
	switch EffectiveRegistryID(providerID, cfg.RegistryID, cfg.Type) {
	case "openai_codex", "google_antigravity":
		return true
	default:
		return false
	}
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
	if strings.EqualFold(strings.TrimSpace(cfg.Type), "ollama") {
		base := cfg.BaseURL
		if strings.TrimSpace(base) == "" {
			base = config.Get().OllamaBaseURL
		}
		if issue := ollamaBaseURLIssue(base); issue != "" {
			return issue
		}
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
		if EffectiveCodexClientID() == "" {
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
	catPersistenceGeneration = map[string]uint64{}
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
	refreshedAt := time.Now()
	catData[providerID] = catalogEntry{
		SchemaVersion: catalogSchemaVersion, Models: catalogModelsWithTypedCapabilities(models, refreshedAt), RefreshedAt: refreshedAt,
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

type catalogRevision struct {
	generation            uint64
	persistenceGeneration uint64
}

func catalogRevisionFor(providerID string) catalogRevision {
	catMu.Lock()
	defer catMu.Unlock()
	loadCatalogLocked()
	if _, exists := catGeneration[providerID]; !exists {
		catGeneration[providerID] = 0
	}
	if _, exists := catPersistenceGeneration[providerID]; !exists {
		catPersistenceGeneration[providerID] = 0
	}
	return catalogRevision{
		generation:            catGeneration[providerID],
		persistenceGeneration: catPersistenceGeneration[providerID],
	}
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
	refreshedAt := time.Now()
	catData[providerID] = catalogEntry{
		SchemaVersion: catalogSchemaVersion, Models: catalogModelsWithTypedCapabilities(models, refreshedAt), RefreshedAt: refreshedAt,
	}
	saveCatalogLocked()
	return true
}

func storeEntryIfRevision(providerID string, models []ModelInfo, expected catalogRevision) bool {
	catMu.Lock()
	defer catMu.Unlock()
	loadCatalogLocked()
	if catGeneration[providerID] != expected.generation ||
		catPersistenceGeneration[providerID] < expected.persistenceGeneration {
		return false
	}
	refreshedAt := time.Now()
	catData[providerID] = catalogEntry{
		SchemaVersion: catalogSchemaVersion, Models: catalogModelsWithTypedCapabilities(models, refreshedAt), RefreshedAt: refreshedAt,
	}
	saveCatalogLocked()
	return true
}

func catalogModelsWithTypedCapabilities(models []ModelInfo, discoveredAt time.Time) []ModelInfo {
	out := make([]ModelInfo, len(models))
	copy(out, models)
	for index := range out {
		if out[index].TypedCapabilities != nil {
			continue
		}
		out[index].TypedCapabilities = AdaptModelCapabilities(
			out[index].Capabilities, out[index].SupportedSurfaces, discoveredAt, time.Time{},
		)
	}
	return out
}

// CatalogModels returns the cached model list for a provider, refreshing lazily
// when missing or stale. The upstream fetch runs OUTSIDE the lock; a failed
// refresh keeps whatever was cached.
func CatalogModels(providerID string) []ModelInfo {
	return CatalogModelsForPrincipal(providerID, gatewayCaller())
}

func CatalogModelsForPrincipal(
	providerID string, caller core.Caller,
) []ModelInfo {
	return ReadCatalogForPrincipal(providerID, caller).Models
}

// CatalogReadResult preserves discovery failures without discarding usable rows.
// Models alone remain a best-effort compatibility API, not readiness evidence.
type CatalogReadResult struct {
	Models      []ModelInfo
	RefreshedAt time.Time
	Diagnostics CatalogDiagnostics
	Err         error
}

type CatalogDiagnostics struct {
	Status         string `json:"status"`
	FailureCode    string `json:"failure_code,omitempty"`
	Detail         string `json:"detail,omitempty"`
	UpstreamStatus int    `json:"upstream_status,omitempty"`
	Stale          bool   `json:"stale"`
	FromCache      bool   `json:"from_cache"`
	// SourceScope describes the cache boundary, not the credential's owner.
	SourceScope string `json:"source_scope"`
	// OwnerScope describes whose credential was used without exposing identity.
	OwnerScope string `json:"owner_scope"`
}

// ReadCatalogForPrincipal never borrows another caller's cache or runs inference.
// A successful empty discovery replaces old rows and is cached for the same TTL.
func ReadCatalogForPrincipal(providerID string, caller core.Caller) CatalogReadResult {
	return readCatalogForPrincipal(providerID, caller, RefreshCatalogForPrincipalWithError)
}

// ReadCachedCatalogForPrincipal returns the caller-scoped snapshot without
// contacting the provider. Catalog synchronization is an explicit lifecycle
// operation; read endpoints must remain safe when an upstream is slow or down.
func ReadCachedCatalogForPrincipal(providerID string, caller core.Caller) CatalogReadResult {
	result := CatalogReadResult{Diagnostics: CatalogDiagnostics{SourceScope: "gateway", OwnerScope: "gateway", FromCache: true}}
	principalID := callerPrincipalID(caller)
	if principalID != "" {
		result.Diagnostics.SourceScope = "principal"
		result.Diagnostics.OwnerScope = "human_owner"
		if caller.Kind == core.CallerService && caller.ProjectID != "" {
			result.Diagnostics.SourceScope = "service_project"
			result.Diagnostics.OwnerScope = "service_project"
		}
	}
	if issue := ProviderConfigurationIssue(providerID); issue != "" {
		result.Err = catalogError("catalog_configuration_incomplete", issue, 0)
	} else if CatalogRequiresPrincipal(providerID) && strings.TrimSpace(principalID) == "" {
		result.Err = catalogError("catalog_principal_required", "An active human principal is required for this private provider catalog.", 0)
	} else if authorized, err := ProviderCredentialAuthorized(providerID, caller); err != nil || !authorized {
		result.Err = catalogError("catalog_authentication_failed", "Provider credential is unavailable for catalog access.", 0)
	}
	if result.Err == nil {
		if entry, ok := cachedEntry(catalogCacheKey(providerID, caller)); ok {
			result.Models, result.RefreshedAt = entry.Models, entry.RefreshedAt
		}
	}
	result.Diagnostics.Status = "synced"
	if result.RefreshedAt.IsZero() {
		result.Diagnostics.Status = "not_synced"
	} else if len(result.Models) == 0 {
		result.Diagnostics.Status = "empty"
	}
	result.Diagnostics.Stale = !result.RefreshedAt.IsZero() && time.Since(result.RefreshedAt) > catalogTTL
	if result.Err != nil {
		result.Diagnostics.Status = "error"
		result.Diagnostics.FailureCode, result.Diagnostics.Detail, result.Diagnostics.UpstreamStatus = CatalogFailure(result.Err)
	}
	return result
}

func readCatalogForPrincipal(
	providerID string, caller core.Caller,
	refresh func(string, core.Caller) ([]ModelInfo, *iam.ProviderAccountObservation, error),
) CatalogReadResult {
	result := CatalogReadResult{Diagnostics: CatalogDiagnostics{SourceScope: "gateway", OwnerScope: "gateway"}}
	principalID := callerPrincipalID(caller)
	if principalID != "" {
		result.Diagnostics.SourceScope = "principal"
		result.Diagnostics.OwnerScope = "human_owner"
		if caller.Kind == core.CallerService && caller.ProjectID != "" {
			result.Diagnostics.SourceScope = "service_project"
			result.Diagnostics.OwnerScope = "service_project"
		}
	}
	if issue := ProviderConfigurationIssue(providerID); issue != "" {
		result.Err = catalogError("catalog_configuration_incomplete", issue, 0)
	} else if CatalogRequiresPrincipal(providerID) && strings.TrimSpace(principalID) == "" {
		result.Err = catalogError("catalog_principal_required", "An active human principal is required for this private provider catalog.", 0)
	} else if authorized, err := ProviderCredentialAuthorized(providerID, caller); err != nil || !authorized {
		result.Err = catalogError("catalog_authentication_failed", "Provider credential is unavailable for catalog access.", 0)
	}
	cacheKey := catalogCacheKey(providerID, caller)
	if result.Err == nil {
		e, ok := cachedEntry(cacheKey)
		if ok && time.Since(e.RefreshedAt) <= catalogTTL {
			result.Models, result.RefreshedAt = e.Models, e.RefreshedAt
			result.Diagnostics.FromCache = true
		} else {
			_, _, result.Err = refresh(providerID, caller)
			// Use one authoritative snapshot for both rows and timestamp: another
			// refresh or invalidation may have superseded the discovery result.
			if current, exists := cachedEntry(cacheKey); exists {
				result.Models, result.RefreshedAt = current.Models, current.RefreshedAt
				if result.Err != nil {
					result.Diagnostics.FromCache = true
				}
			} else if result.Err == nil {
				result.Err = catalogError(
					"catalog_state_changed",
					"Provider configuration changed during catalog refresh.",
					0,
				)
			}
		}
	}
	result.Diagnostics.Status = "synced"
	if len(result.Models) == 0 {
		result.Diagnostics.Status = "empty"
	}
	result.Diagnostics.Stale = !result.RefreshedAt.IsZero() && time.Since(result.RefreshedAt) > catalogTTL
	if result.Err != nil {
		result.Diagnostics.Status = "error"
		result.Diagnostics.FailureCode, result.Diagnostics.Detail, result.Diagnostics.UpstreamStatus = CatalogFailure(result.Err)
	}
	return result
}

func catalogCacheKey(providerID string, caller core.Caller) string {
	if managed, err := AutomationManagedAnonymousProvider(providerID); err == nil && managed {
		return providerID
	}
	return providerCacheKey(providerID, caller)
}

// RefreshCatalog forces a re-fetch for one provider and returns the new list.
func RefreshCatalog(providerID string) []ModelInfo {
	return RefreshCatalogForPrincipal(providerID, gatewayCaller())
}

func RefreshCatalogForPrincipal(providerID string, caller core.Caller) []ModelInfo {
	models, _, _ := RefreshCatalogForPrincipalWithError(providerID, caller)
	return models
}

// RefreshCatalogForPrincipalWithError forces a refresh and preserves a safe,
// structured failure for lifecycle diagnostics. Legitimate empty catalogs are
// stored as successful refreshes and returned without an error.
func RefreshCatalogForPrincipalWithError(
	providerID string, caller core.Caller,
) ([]ModelInfo, *iam.ProviderAccountObservation, error) {
	if issue := ProviderConfigurationIssue(providerID); issue != "" {
		return nil, nil, catalogError(
			"catalog_configuration_incomplete", issue, 0,
		)
	}
	principalID := callerPrincipalID(caller)
	if CatalogRequiresPrincipal(providerID) && strings.TrimSpace(principalID) == "" {
		return nil, nil, catalogError(
			"catalog_principal_required",
			"An active human principal is required for this private provider catalog.",
			0,
		)
	}
	cacheKey := catalogCacheKey(providerID, caller)
	revision := catalogRevisionFor(cacheKey)
	var initialObservation *iam.ProviderAccountObservation
	if caller.Kind == core.CallerHuman {
		if observed, found, err := iam.ActiveProviderAccountObservation(
			principalID, providerID,
		); err == nil && found {
			initialObservation = &observed
		}
	}
	models, observation, err := ListProviderModelsForPrincipalWithError(providerID, caller)
	if err != nil {
		return nil, observation, err
	}
	if models == nil {
		models = []ModelInfo{}
	}
	if !storeEntryIfRevision(cacheKey, models, revision) {
		currentRevision := catalogRevisionFor(cacheKey)
		refreshRebased := initialObservation != nil && observation != nil &&
			initialObservation.ConnectionID == observation.ConnectionID &&
			observation.CredentialRevision > initialObservation.CredentialRevision &&
			currentRevision.generation == revision.generation+1
		if refreshRebased {
			current, found, currentErr := iam.ActiveProviderAccountObservation(
				principalID, providerID,
			)
			refreshRebased = currentErr == nil && found && current == *observation &&
				storeEntryIfRevision(cacheKey, models, currentRevision)
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
	return CatalogLookupForPrincipal(providerID, model, gatewayCaller())
}

func CatalogLookupForPrincipal(
	providerID, model string, caller core.Caller,
) (ModelInfo, bool) {
	for _, m := range CatalogModelsForPrincipal(providerID, caller) {
		if m.ID == model {
			return m, true
		}
	}
	return ModelInfo{}, false
}

// CatalogCachedLookupForPrincipal returns only already-known capability data and
// never performs provider discovery. Dispatch uses it to avoid adding a catalog
// network call to the request path.
func CatalogCachedLookupForPrincipal(providerID, model string, caller core.Caller) (ModelInfo, bool) {
	models, _ := CatalogCachedForPrincipal(providerID, caller)
	for _, current := range models {
		if current.ID == model {
			return current, true
		}
	}
	return ModelInfo{}, false
}

// CatalogRefreshedAt reports when a provider's catalog was last refreshed (zero
// time if never).
func CatalogRefreshedAt(providerID string) time.Time {
	return CatalogRefreshedAtForPrincipal(providerID, gatewayCaller())
}

func CatalogRefreshedAtForPrincipal(providerID string, caller core.Caller) time.Time {
	if ProviderConfigurationIssue(providerID) != "" {
		return time.Time{}
	}
	if CatalogRequiresPrincipal(providerID) && strings.TrimSpace(callerPrincipalID(caller)) == "" {
		return time.Time{}
	}
	e, _ := cachedEntry(catalogCacheKey(providerID, caller))
	return e.RefreshedAt
}

// CatalogCached returns the currently-cached models + refresh time WITHOUT
// triggering a refresh — for fast status reads (e.g. the admin state endpoint).
func CatalogCached(providerID string) ([]ModelInfo, time.Time) {
	return CatalogCachedForPrincipal(providerID, gatewayCaller())
}

func CatalogCachedForPrincipal(providerID string, caller core.Caller) ([]ModelInfo, time.Time) {
	if ProviderConfigurationIssue(providerID) != "" {
		return nil, time.Time{}
	}
	if CatalogRequiresPrincipal(providerID) && strings.TrimSpace(callerPrincipalID(caller)) == "" {
		return nil, time.Time{}
	}
	e, _ := cachedEntry(catalogCacheKey(providerID, caller))
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

// forgetCatalogAfterProviderPersistence invalidates rows produced before a
// provider-owned token refresh or metadata write without turning the successful
// in-flight operation that performed it into stale work. External credential
// replacement and revocation continue to use ForgetCatalogForPrincipal and
// advance the hard generation instead.
func forgetCatalogAfterProviderPersistence(providerID, principalID string) {
	if providerID == "" || principalID == "" {
		return
	}
	key := providerID + "@" + principalID
	catMu.Lock()
	defer catMu.Unlock()
	loadCatalogLocked()
	changed := false
	for candidate := range catData {
		if candidate == key || strings.HasPrefix(candidate, key+"#") {
			delete(catData, candidate)
			catPersistenceGeneration[candidate]++
			changed = true
		}
	}
	if _, exists := catPersistenceGeneration[key]; !exists {
		catPersistenceGeneration[key] = 1
	} else if !changed {
		catPersistenceGeneration[key]++
	}
	if changed {
		saveCatalogLocked()
	}
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
