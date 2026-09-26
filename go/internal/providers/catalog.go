package providers

import (
	"context"
	"errors"
	"strings"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"

	core "github.com/xibodev/llmgw-core"
	"github.com/xibodev/llmgw-core/catalog"
	coreproviders "github.com/xibodev/llmgw-core/providers"
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

// catalogSchemaVersion is the stamp core's catalog service puts on every
// catalog it stores for the gateway; see catalogEntry.SchemaVersion.
const catalogSchemaVersion = 6

// catalogTTL is how long core's catalog service serves the gateway's
// catalogs before a read discovers them again.
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
	settings := config.Get()
	cfg, ok := settings.Providers[providerID]
	if !ok {
		return ""
	}
	registryID := EffectiveRegistryID(providerID, cfg.RegistryID, cfg.Type)
	entry, _ := RegistryProviderByID(registryID)
	if entry.RequiresBaseURL && strings.TrimSpace(cfg.BaseURL) == "" {
		return entry.Label + " requires a base URL."
	}
	if ollamaInstance(cfg) {
		if issue := coreproviders.OllamaBaseURLIssue(ollamaBase(settings, cfg)); issue != "" {
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
	}
	return ""
}

// CatalogModels returns the cached model list for a provider, refreshing lazily
// when missing or stale. Core's catalog service runs one discovery of a
// catalog at a time, and a failed refresh keeps whatever was cached.
func (rt *Runtime) CatalogModels(providerID string) []ModelInfo {
	return rt.CatalogModelsForPrincipal(providerID, gatewayCaller())
}

func (rt *Runtime) CatalogModelsForPrincipal(
	providerID string, caller core.Caller,
) []ModelInfo {
	return rt.ReadCatalogForPrincipal(providerID, caller).Models
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
// Core's catalog service serves the stored catalog while it is fresh and
// otherwise discovers it on the gateway's path, keeping the stale rows beside
// a discovery that fails.
func (rt *Runtime) ReadCatalogForPrincipal(providerID string, caller core.Caller) CatalogReadResult {
	result := catalogRead(providerID, caller)
	if result.Err == nil {
		request, _ := rt.catalogRequest(providerID, caller)
		read := rt.catalogService.Read(context.Background(), request)
		// The read's record is the one stored once the discovery is over, so
		// rows and timestamp are one snapshot even when another refresh or an
		// invalidation superseded the discovery.
		result.Models, result.RefreshedAt = gatewayRows(read.Record.Evidence.Models), read.Record.Evidence.ObservedAt
		result.Diagnostics.FromCache, result.Err = read.Diagnostics.FromCache, catalogServiceError(read.Err)
	}
	result.Diagnostics.Status = "synced"
	if len(result.Models) == 0 {
		result.Diagnostics.Status = "empty"
	}
	rt.diagnoseCatalog(providerID, &result)
	return result
}

// ReadCachedCatalogForPrincipal returns the caller-scoped snapshot without
// contacting the provider. Catalog synchronization is an explicit lifecycle
// operation; read endpoints must remain safe when an upstream is slow or down.
func (rt *Runtime) ReadCachedCatalogForPrincipal(providerID string, caller core.Caller) CatalogReadResult {
	result := catalogRead(providerID, caller)
	result.Diagnostics.FromCache = true
	if result.Err == nil {
		evidence := rt.catalogService.Cached(context.Background(), catalogKey(providerID, caller)).Record.Evidence
		result.Models, result.RefreshedAt = gatewayRows(evidence.Models), evidence.ObservedAt
	}
	result.Diagnostics.Status = "synced"
	if result.RefreshedAt.IsZero() {
		result.Diagnostics.Status = "not_synced"
	} else if len(result.Models) == 0 {
		result.Diagnostics.Status = "empty"
	}
	rt.diagnoseCatalog(providerID, &result)
	return result
}

// catalogRead begins a read of the catalog of providerID for caller: the
// scope it is cached under, and why caller may not read it, if it may not.
func catalogRead(providerID string, caller core.Caller) CatalogReadResult {
	result := CatalogReadResult{Diagnostics: CatalogDiagnostics{SourceScope: "gateway", OwnerScope: "gateway"}}
	principalID, kind := callerPrincipal(caller)
	if principalID != "" {
		result.Diagnostics.SourceScope = "principal"
		result.Diagnostics.OwnerScope = "human_owner"
		if kind == "service" && caller.ProjectID != "" {
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
	return result
}

// diagnoseCatalog completes the diagnostics of result: whether its rows
// outlived the instance's TTL, and why the read failed.
func (rt *Runtime) diagnoseCatalog(providerID string, result *CatalogReadResult) {
	result.Diagnostics.Stale = !result.RefreshedAt.IsZero() && time.Since(result.RefreshedAt) > rt.catalogService.TTL(providerID)
	if result.Err != nil {
		result.Diagnostics.Status = "error"
		result.Diagnostics.FailureCode, result.Diagnostics.Detail, result.Diagnostics.UpstreamStatus = CatalogFailure(result.Err)
	}
}

// catalogServiceError is the gateway's error for what a read or a refresh
// of core's catalog service returned: a discovery an invalidation fenced out
// reports that the provider's state changed.
func catalogServiceError(err error) error {
	if errors.Is(err, catalog.ErrStateChanged) {
		return catalogError("catalog_state_changed", "Provider configuration changed during catalog refresh.", 0)
	}
	return err
}

// catalogCacheKey is the catalog.json key of the catalog of providerID for
// caller: the provider's own for an automation-managed anonymous provider,
// whose one catalog every caller shares, and otherwise the caller's scope
// (see providerCacheKey), which core's catalog key carries as its
// CredentialKey.
func catalogCacheKey(providerID string, caller core.Caller) string {
	if managed, err := AutomationManagedAnonymousProvider(providerID); err == nil && managed {
		return providerID
	}
	return providerCacheKey(providerID, caller)
}

// RefreshCatalog forces a re-fetch for one provider and returns the new list.
func (rt *Runtime) RefreshCatalog(providerID string) []ModelInfo {
	return rt.RefreshCatalogForPrincipal(providerID, gatewayCaller())
}

func (rt *Runtime) RefreshCatalogForPrincipal(providerID string, caller core.Caller) []ModelInfo {
	models, _, _ := rt.RefreshCatalogForPrincipalWithError(providerID, caller)
	return models
}

// RefreshCatalogForPrincipalWithError forces a refresh and preserves a safe,
// structured failure for lifecycle diagnostics. Legitimate empty catalogs are
// stored as successful refreshes and returned without an error.
func (rt *Runtime) RefreshCatalogForPrincipalWithError(
	providerID string, caller core.Caller,
) ([]ModelInfo, *CredentialObservation, error) {
	if issue := ProviderConfigurationIssue(providerID); issue != "" {
		return nil, nil, catalogError(
			"catalog_configuration_incomplete", issue, 0,
		)
	}
	if CatalogRequiresPrincipal(providerID) && strings.TrimSpace(callerPrincipalID(caller)) == "" {
		return nil, nil, catalogError(
			"catalog_principal_required",
			"An active human principal is required for this private provider catalog.",
			0,
		)
	}
	request, discovery := rt.catalogRequest(providerID, caller)
	if _, err := rt.catalogService.Refresh(context.Background(), request); err != nil {
		return nil, discovery.observation, catalogServiceError(err)
	}
	return discovery.models, discovery.observation, nil
}

// catalogKey is the core key of the catalog of providerID for caller.
func catalogKey(providerID string, caller core.Caller) core.CatalogKey {
	return catalogKeyOf(catalogCacheKey(providerID, caller))
}

// catalogRequest asks core's catalog service for the catalog of providerID
// for caller, discovered on the gateway's path. The discovery reports what
// it listed and with which credential.
func (rt *Runtime) catalogRequest(providerID string, caller core.Caller) (catalog.Request, *catalogDiscovery) {
	discovery := &catalogDiscovery{runtime: rt, providerID: providerID, caller: caller}
	return catalog.Request{
		Key: catalogKey(providerID, caller), Discover: discovery.discover, Rebase: discovery.rebase,
	}, discovery
}

// catalogDiscovery is one discovery of a caller's catalog: the rows it
// listed, and the connection of a human caller before and after listing.
type catalogDiscovery struct {
	runtime              *Runtime
	providerID           string
	caller               core.Caller
	models               []ModelInfo
	initial, observation *CredentialObservation
}

// discover lists the catalog through the caller's provider. A legitimate
// empty catalog is stored like any other.
func (d *catalogDiscovery) discover(context.Context) ([]core.ModelInfo, error) {
	if d.caller.Kind == core.CallerHuman {
		if observed, found, err := iam.ActiveProviderAccountObservation(
			callerPrincipalID(d.caller), d.providerID,
		); err == nil && found {
			d.initial = credentialObservation(&observed)
		}
	}
	models, observation, err := d.runtime.ListProviderModelsForPrincipalWithError(d.providerID, d.caller)
	d.observation = observation
	if err != nil {
		return nil, err
	}
	if models == nil {
		models = []ModelInfo{}
	}
	d.models = models
	return coreRows(models), nil
}

// rebase lets the discovery store although exactly one hard invalidation
// fenced it, when the listing's own credential refresh caused it: the
// listing used the connection the caller had before, at a newer revision,
// and that is still the caller's active connection.
func (d *catalogDiscovery) rebase() bool {
	if d.initial == nil || d.observation == nil || d.initial.ConnectionID != d.observation.ConnectionID ||
		d.observation.CredentialRevision <= d.initial.CredentialRevision {
		return false
	}
	current, found, err := iam.ActiveProviderAccountObservation(callerPrincipalID(d.caller), d.providerID)
	return err == nil && found && *credentialObservation(&current) == *d.observation
}

// CatalogLookup returns a single model's info from the (lazily-refreshed) catalog.
func (rt *Runtime) CatalogLookup(providerID, model string) (ModelInfo, bool) {
	return rt.CatalogLookupForPrincipal(providerID, model, gatewayCaller())
}

func (rt *Runtime) CatalogLookupForPrincipal(
	providerID, model string, caller core.Caller,
) (ModelInfo, bool) {
	if catalogRead(providerID, caller).Err != nil {
		return ModelInfo{}, false
	}
	request, _ := rt.catalogRequest(providerID, caller)
	// A failed discovery still serves the stale row, as the catalog's
	// models did.
	row, found, _ := rt.catalogService.Lookup(context.Background(), request, model)
	if !found {
		return ModelInfo{}, false
	}
	return gatewayModelInfo(row), true
}

// CatalogCachedLookupForPrincipal returns only already-known capability data and
// never performs provider discovery. Dispatch uses it to avoid adding a catalog
// network call to the request path.
func (rt *Runtime) CatalogCachedLookupForPrincipal(providerID, model string, caller core.Caller) (ModelInfo, bool) {
	key, cached := cachedCatalogKey(providerID, caller)
	if !cached {
		return ModelInfo{}, false
	}
	row, found := rt.catalogService.CachedLookup(context.Background(), key, model)
	if !found {
		return ModelInfo{}, false
	}
	return gatewayModelInfo(row), true
}

// CatalogRefreshedAt reports when a provider's catalog was last refreshed (zero
// time if never).
func (rt *Runtime) CatalogRefreshedAt(providerID string) time.Time {
	return rt.CatalogRefreshedAtForPrincipal(providerID, gatewayCaller())
}

func (rt *Runtime) CatalogRefreshedAtForPrincipal(providerID string, caller core.Caller) time.Time {
	return rt.cachedCatalog(providerID, caller).Evidence.ObservedAt
}

// CatalogCached returns the currently-cached models + refresh time WITHOUT
// triggering a refresh — for fast status reads (e.g. the admin state endpoint).
func (rt *Runtime) CatalogCached(providerID string) ([]ModelInfo, time.Time) {
	return rt.CatalogCachedForPrincipal(providerID, gatewayCaller())
}

func (rt *Runtime) CatalogCachedForPrincipal(providerID string, caller core.Caller) ([]ModelInfo, time.Time) {
	evidence := rt.cachedCatalog(providerID, caller).Evidence
	return gatewayRows(evidence.Models), evidence.ObservedAt
}

// cachedCatalog is the stored catalog of providerID for caller, fresh or
// not, which core's catalog service serves without discovering it.
func (rt *Runtime) cachedCatalog(providerID string, caller core.Caller) core.CatalogRecord {
	key, cached := cachedCatalogKey(providerID, caller)
	if !cached {
		return core.CatalogRecord{}
	}
	return rt.catalogService.Cached(context.Background(), key).Record
}

// cachedCatalogKey is the key of the stored catalog of providerID for
// caller. A provider whose setup is incomplete, or a private catalog read
// without a principal, has none.
func cachedCatalogKey(providerID string, caller core.Caller) (core.CatalogKey, bool) {
	if ProviderConfigurationIssue(providerID) != "" {
		return core.CatalogKey{}, false
	}
	if CatalogRequiresPrincipal(providerID) && strings.TrimSpace(callerPrincipalID(caller)) == "" {
		return core.CatalogKey{}, false
	}
	return catalogKey(providerID, caller), true
}

// ForgetCatalog drops a provider's cached catalog (used when it's deleted).
func (rt *Runtime) ForgetCatalog(providerID string) {
	rt.forgetCatalogs(providerID, catalog.Hard, func(string) bool { return true })
}

func (rt *Runtime) ForgetCatalogForPrincipal(providerID, principalID string) {
	if providerID == "" || principalID == "" {
		return
	}
	rt.forgetCatalogs(providerID, catalog.Hard, principalScopes(principalID))
}

// forgetCatalogAfterProviderPersistence invalidates rows produced before a
// provider-owned token refresh or metadata write without turning the successful
// in-flight operation that performed it into stale work. External credential
// replacement and revocation continue to use ForgetCatalogForPrincipal, whose
// hard invalidation fences that operation instead.
func (rt *Runtime) forgetCatalogAfterProviderPersistence(providerID, principalID string) {
	if providerID == "" || principalID == "" {
		return
	}
	rt.forgetCatalogs(providerID, catalog.Soft, principalScopes(principalID))
}

// ForgetInheritedCatalogs removes only project-scoped service catalogs. Human
// BYOC catalogs use provider@principal keys and must survive unrelated service
// credential imports and bindings.
func (rt *Runtime) ForgetInheritedCatalogs(providerID string) {
	rt.forgetCatalogs(providerID, catalog.Hard, func(scope string) bool { return strings.Contains(scope, "#") })
}

// ForgetCatalogForProject removes inherited service catalogs for one project.
func (rt *Runtime) ForgetCatalogForProject(providerID, projectID string) {
	if providerID == "" || projectID == "" {
		return
	}
	rt.forgetCatalogs(providerID, catalog.Hard, func(scope string) bool {
		return scope != "" && strings.HasSuffix(scope, "#"+projectID)
	})
}

// principalScopes matches the caller scopes of principalID: its own, and
// those of its projects.
func principalScopes(principalID string) func(scope string) bool {
	return func(scope string) bool { return scope == principalID || strings.HasPrefix(scope, principalID+"#") }
}

// forgetCatalogs invalidates each catalog of providerID whose caller scope
// matches, stored or still being discovered, and writes catalog.json once.
func (rt *Runtime) forgetCatalogs(providerID string, mode catalog.Invalidation, matches func(scope string) bool) {
	rt.catalogs.batch(func() {
		for _, key := range rt.catalogs.keys(providerID) {
			if matches(key.CredentialKey) {
				_ = rt.catalogService.Invalidate(context.Background(), key, mode)
			}
		}
	})
}

// CatalogSnapshot reports the freshest catalog known for a provider across
// every principal scope. Catalogs are cached per caller so one principal's
// private credential never yields another's model list, but an operator-wide
// view (the provider hub, the setup guide) must still be able to say whether a
// provider has a synced catalog at all. It is a display signal only: request
// paths keep using the caller-scoped catalog.
func (rt *Runtime) CatalogSnapshot(providerID string) ([]ModelInfo, time.Time) {
	if ProviderConfigurationIssue(providerID) != "" {
		return nil, time.Time{}
	}
	var best core.CatalogRecord
	for _, key := range rt.catalogs.keys(providerID) {
		if record := rt.catalogService.Cached(context.Background(), key).Record; record.Evidence.ObservedAt.After(best.Evidence.ObservedAt) {
			best = record
		}
	}
	return gatewayRows(best.Evidence.Models), best.Evidence.ObservedAt
}
