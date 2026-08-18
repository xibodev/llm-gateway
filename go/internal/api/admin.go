package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/copilotauth"
	"llmgw/internal/gcpauth"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
	"llmgw/internal/router"
)

func persist() {
	_ = config.Save()
	providers.ResetProviders()
}

func copilotEnabled() bool {
	if config.Get().AllowCopilotProxy {
		return true
	}
	v := strings.ToLower(strings.TrimSpace(os.Getenv("LLMGW_EXPERIMENTAL_COPILOT_PROVIDER")))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

func adminAuthed(w http.ResponseWriter, r *http.Request) bool {
	return requireAdmin(w, r)
}

func decodeBody(r *http.Request, v any) bool {
	return json.NewDecoder(r.Body).Decode(v) == nil
}

// GET /admin/api/state
func handleState(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	s := config.Get()
	secrets := config.LoadSecrets()
	connections, err := iam.ListProviderConnections("", "")
	if err != nil {
		writeError(w, 500, "Credential store unavailable.")
		return
	}

	provList := []map[string]any{}
	for pid, pc := range s.Providers {
		models, refreshed := providers.CatalogCached(pid)
		systemConnection, err := iam.SystemProviderConnectionExists(pid)
		if err != nil {
			writeError(w, 500, "Credential store unavailable.")
			return
		}
		entry := map[string]any{
			"id": pid, "type": pc.Type, "base_url": pc.BaseURL, "region": pc.Region,
			"registry_id":       pc.RegistryID,
			"api_key_set":       secrets[pid] != "" || pc.APIKey != "" || systemConnection,
			"force_api_support": pc.ForceApiSupport,
			"project":           pc.Project,
			"location":          pc.Location,
			"default_voice":     pc.DefaultVoice,
			"disabled":          pc.Disabled,
			"models":            len(models),
		}
		if !refreshed.IsZero() {
			entry["catalog_refreshed"] = refreshed.UTC().Format(time.RFC3339)
		}
		provList = append(provList, entry)
	}
	cats := map[string]any{}
	for name, cat := range s.Categories {
		fo := []any{}
		for _, m := range cat.Failover {
			fo = append(fo, map[string]any{"provider": m.Provider, "model": m.Model})
		}
		cats[name] = map[string]any{"failover": fo}
	}
	keys, err := iam.ListAPIKeys("")
	if err != nil {
		writeError(w, 500, "Identity store unavailable.")
		return
	}
	keyRows := make([]map[string]any, 0, len(keys))
	for _, k := range keys {
		keyRows = append(keyRows, map[string]any{
			"id": k.ID, "prefix": k.Prefix, "project_id": k.ProjectID,
			"project": k.Project, "principal_id": k.PrincipalID,
			"principal": k.Principal, "principal_kind": k.Kind,
			"name": k.Name, "status": k.Status, "disabled": k.Status == "disabled", "revoked": k.Status == "revoked",
			"expired": k.Expired, "created": k.CreatedAt, "expires_at": k.ExpiresAt,
			"last_used_at":      k.LastUsedAt,
			"allowed_models":    k.Policy.AllowedModels,
			"allowed_providers": k.Policy.AllowedProviders,
			"rpm":               k.Policy.RPM, "daily_requests": k.Policy.DailyRequests,
			"monthly_requests":      k.Policy.MonthlyRequests,
			"daily_input_tokens":    k.Policy.DailyInputTokens,
			"daily_output_tokens":   k.Policy.DailyOutputTokens,
			"monthly_total_tokens":  k.Policy.MonthlyTotalTokens,
			"daily_cost_microusd":   k.Policy.DailyCostMicroUSD,
			"monthly_cost_microusd": k.Policy.MonthlyCostMicroUSD,
			"daily_credits_milli":   k.Policy.DailyCreditsMilli,
			"monthly_credits_milli": k.Policy.MonthlyCreditsMilli,
		})
	}
	principals, err := iam.ListPrincipals()
	if err != nil {
		writeError(w, 500, "Identity store unavailable.")
		return
	}
	projects, err := iam.ListProjects()
	if err != nil {
		writeError(w, 500, "Identity store unavailable.")
		return
	}
	credentials, err := iam.ListProviderCredentials("")
	if err != nil {
		writeError(w, 500, "Credential store unavailable.")
		return
	}
	credentialBindings, err := iam.ListProviderCredentialBindings("")
	if err != nil {
		writeError(w, 500, "Credential binding store unavailable.")
		return
	}
	memberships, err := iam.ListAllMemberships()
	if err != nil {
		writeError(w, 500, "Identity store unavailable.")
		return
	}

	statusSnapshots, err := providerStatusSnapshots(s, secrets, connections, "")
	if err != nil {
		writeError(w, 500, "Provider status is unavailable.")
		return
	}

	writeJSON(w, 200, map[string]any{
		"version":       config.Version,
		"auth_required": !s.AllowUnauthenticatedAPI,
		"sso": map[string]any{
			"enabled": s.SSOEnabled, "admin_group": s.SSOAdminGroup,
			"auto_provision": s.SSOAutoProvision,
		},
		"provider_types":               providers.ProviderTypes,
		"provider_registry":            providers.ProviderRegistry(),
		"provider_statuses":            statusSnapshots,
		"keys":                         keyRows,
		"principals":                   principals,
		"projects":                     projects,
		"provider_credentials":         credentials,
		"provider_credential_bindings": credentialBindings,
		"provider_connections":         connections,
		"memberships":                  memberships,
		"providers":                    provList,
		"categories":                   cats,
		"policies": map[string]any{
			"defaults":  s.Policies.Defaults,
			"overrides": s.Policies.Overrides,
		},
		"savings": router.Totals(false),
		"copilot": map[string]any{"enabled": copilotEnabled(), "auth": copilotauth.AuthStatus()},
	})
}

type providerBody struct {
	ID              string `json:"id"`
	RegistryID      string `json:"registry_id"`
	Type            string `json:"type"`
	BaseURL         string `json:"base_url"`
	APIKey          string `json:"api_key"`
	Region          string `json:"region"`
	Project         string `json:"project"`
	Location        string `json:"location"`
	ForceApiSupport bool   `json:"force_api_support"`
}

// POST /admin/api/providers
func handleUpsertProvider(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	var body providerBody
	if !decodeBody(r, &body) {
		writeError(w, 400, "invalid body")
		return
	}
	registryID := strings.ToLower(strings.TrimSpace(body.RegistryID))
	var registryEntry providers.RegistryEntry
	if registryID != "" {
		var ok bool
		registryEntry, ok = providers.RegistryProvider(registryID)
		if !ok {
			writeError(w, 400, "unknown provider registry id "+registryID)
			return
		}
		registryID = registryEntry.ID
		if registryEntry.ClientOnly {
			writeError(w, 409, registryEntry.Label+" is a gateway client setup, not an OAuth provider connection")
			return
		}
		if registryEntry.Availability != providers.ProviderAvailable {
			writeError(w, 409, registryEntry.Label+" connection support is not available yet")
			return
		}
		if strings.TrimSpace(body.Type) == "" {
			body.Type = registryEntry.RuntimeType
		} else if !strings.EqualFold(strings.TrimSpace(body.Type), registryEntry.RuntimeType) {
			writeError(w, 400, "provider type does not match registry integration")
			return
		}
		if strings.TrimSpace(body.ID) == "" {
			body.ID = registryEntry.DefaultProviderID
		}
		if strings.TrimSpace(body.BaseURL) == "" {
			body.BaseURL = registryEntry.DefaultBaseURL
		}
		if strings.TrimSpace(body.Region) == "" {
			body.Region = registryEntry.DefaultRegion
		}
		if strings.TrimSpace(body.Location) == "" {
			body.Location = registryEntry.DefaultLocation
		}
		if registryEntry.RequiresBaseURL && strings.TrimSpace(body.BaseURL) == "" {
			writeError(w, 400, "base_url is required for "+registryEntry.Label)
			return
		}
		if registryEntry.ID == "vertex_ai" && strings.TrimSpace(body.Project) == "" {
			writeError(w, 400, "project is required for "+registryEntry.Label)
			return
		}
	}
	pid := strings.TrimSpace(body.ID)
	if pid == "" || strings.Contains(pid, "/") || strings.Contains(pid, " ") {
		writeError(w, 400, "provider id must be non-empty with no spaces or '/'")
		return
	}
	body.Type = strings.ToLower(strings.TrimSpace(body.Type))
	if !contains(providers.ProviderTypes, body.Type) {
		writeError(w, 400, "unknown provider type "+body.Type)
		return
	}
	systemConnection, err := iam.SystemProviderConnectionExists(pid)
	if err != nil {
		writeError(w, 500, "Credential store unavailable.")
		return
	}
	if registryEntry.RequiresAPIKey && strings.TrimSpace(body.APIKey) == "" &&
		config.LoadSecrets()[pid] == "" && !systemConnection {
		writeError(w, 400, "api_key is required for "+registryEntry.Label)
		return
	}
	if raw := strings.TrimSpace(body.APIKey); raw != "" {
		// A service account key is a JSON document, not an opaque secret. The
		// dialog offers one credential field, so detect the document here:
		// stored as an API key it would be sent as x-goog-api-key and rejected
		// by Google, reporting a bad key when the real fault is the wrong kind.
		credentialKind := "api_key"
		if gcpauth.LooksLikeServiceAccount(raw) {
			if !strings.EqualFold(body.Type, "vertex_ai") {
				writeError(w, 400,
					"a Google service account key is only usable by a vertex_ai provider")
				return
			}
			if _, err := gcpauth.Parse([]byte(raw)); err != nil {
				writeError(w, 400, err.Error())
				return
			}
			credentialKind = gcpauth.CredentialKind
		}
		if _, err := iam.PutSystemProviderConnection(
			pid, credentialKind, raw,
		); err != nil {
			writeError(w, 500, "store encrypted system connection: "+err.Error())
			return
		}
	}
	config.Update(func(s *config.Settings) {
		next := &config.ProviderConfig{
			Type: body.Type, RegistryID: registryID, BaseURL: emptyNil(body.BaseURL),
			Region: emptyNil(body.Region), Project: emptyNil(body.Project),
			Location: emptyNil(body.Location), ForceApiSupport: body.ForceApiSupport,
		}
		if previous := s.Providers[pid]; previous != nil {
			next.Timeout = previous.Timeout
			next.DefaultVoice = previous.DefaultVoice
			next.Disabled = previous.Disabled
		}
		s.Providers[pid] = next
	})
	if strings.TrimSpace(body.APIKey) != "" {
		config.SaveSecret(pid, strings.TrimSpace(body.APIKey))
	}
	providers.ForgetProvider(pid)
	providers.ForgetCatalog(pid)
	_ = iam.InvalidateProviderChecks(pid)
	persist()
	writeJSON(w, 200, map[string]any{"ok": true, "id": pid})
}

// DELETE /admin/api/providers/{id}
func handleDeleteProvider(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	pid := r.PathValue("id")
	if err := iam.RevokeSystemProviderConnection(pid); err != nil {
		writeError(w, 500, "revoke system connection: "+err.Error())
		return
	}
	config.Update(func(s *config.Settings) { delete(s.Providers, pid) })
	config.DeleteSecret(pid)
	providers.ForgetProvider(pid)
	providers.ForgetCatalog(pid)
	_ = iam.DeleteProviderChecks(pid)
	persist()
	writeJSON(w, 200, map[string]any{"ok": true})
}

// POST /admin/api/providers/{id}/enabled — toggle a configured provider in or
// out of service without deleting its configuration or credentials.
func handleProviderEnabled(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	pid := r.PathValue("id")
	if _, ok := config.Get().Providers[pid]; !ok {
		writeError(w, 404, "unknown provider")
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if !decodeBody(r, &body) || body.Enabled == nil {
		writeError(w, 400, "body must be {\"enabled\": true|false}")
		return
	}
	enabled := *body.Enabled
	config.Update(func(s *config.Settings) {
		if providerConfig := s.Providers[pid]; providerConfig != nil {
			providerConfig.Disabled = !enabled
		}
	})
	providers.ForgetProvider(pid)
	providers.ForgetCatalog(pid)
	_ = iam.InvalidateProviderChecks(pid)
	persist()
	auditAdmin(r, "provider.enabled", "provider", pid, map[string]any{"enabled": enabled})
	writeJSON(w, 200, map[string]any{"ok": true, "provider_id": pid, "enabled": enabled})
}

// POST /admin/api/providers/{id}/verify — run one real minimal completion.
// This is the only lifecycle operation that proves inference works; the
// test/refresh probes only exercise the catalog API.
func handleVerifyProvider(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	pid := r.PathValue("id")
	if _, ok := config.Get().Providers[pid]; !ok {
		writeError(w, 404, "unknown provider")
		return
	}
	principal, err := catalogPrincipalForProvider(r, pid)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	model := strings.TrimSpace(r.URL.Query().Get("model"))
	auditAdmin(r, "provider.verify", "provider", pid, map[string]any{"model": model})
	writeJSON(w, 200, runProviderVerify(pid, model, principal))
}

func selectedCatalogPrincipal(r *http.Request) (*config.Principal, error) {
	principalID := strings.TrimSpace(r.URL.Query().Get("principal_id"))
	if principalID == "" {
		principalID = getAdminActor(r).PrincipalID
	}
	if principalID == "" {
		return nil, nil
	}
	principal, found, err := iam.PrincipalByID(principalID)
	if err != nil {
		return nil, err
	}
	if !found || principal.Kind != "human" || principal.Status != "active" {
		return nil, fmt.Errorf("principal_id must reference an active human principal")
	}
	selected := &config.Principal{PrincipalID: principal.ID, PrincipalKind: principal.Kind}
	projectID := strings.TrimSpace(r.URL.Query().Get("project_id"))
	if projectID == "" {
		return selected, nil
	}
	project, found, err := iam.ProjectByID(projectID)
	if err != nil {
		return nil, err
	}
	if !found || project.Status != "active" {
		return nil, fmt.Errorf("project_id must reference an active project")
	}
	role, member, err := iam.MembershipRole(project.ID, principal.ID)
	if err != nil {
		return nil, err
	}
	if !member || (role != "owner" && role != "admin") {
		return nil, fmt.Errorf("principal_id must be an owner or admin of project_id")
	}
	selected.ProjectID = project.ID
	selected.Project = project.Slug
	selected.Role = role
	return selected, nil
}

func catalogPrincipalForProvider(r *http.Request, providerID string) (*config.Principal, error) {
	principal, err := selectedCatalogPrincipal(r)
	if err != nil {
		return nil, err
	}
	if principal == nil && providers.CatalogRequiresPrincipal(providerID) {
		return nil, fmt.Errorf("principal_id is required for this private OAuth provider")
	}
	return principal, nil
}

// POST /admin/api/providers/{id}/models â€” model ids from the catalog (cached).
func handleProviderModels(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	pid := r.PathValue("id")
	if _, ok := config.Get().Providers[pid]; !ok {
		writeError(w, 404, "unknown provider")
		return
	}
	principal, err := catalogPrincipalForProvider(r, pid)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	rows := providers.CatalogModelsForPrincipal(pid, principal)
	ids := []string{}
	for _, row := range rows {
		if row.ID != "" {
			ids = append(ids, row.ID)
		}
	}
	writeJSON(w, 200, map[string]any{"models": ids, "count": len(rows)})
}

// GET /admin/api/providers/{id}/catalog â€” full model rows for the model-card page.
func handleProviderCatalog(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	pid := r.PathValue("id")
	if _, ok := config.Get().Providers[pid]; !ok {
		writeError(w, 404, "unknown provider")
		return
	}
	principal, err := catalogPrincipalForProvider(r, pid)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	rows := providers.CatalogModelsForPrincipal(pid, principal)
	if rows == nil {
		rows = []providers.ModelInfo{}
	}
	resp := map[string]any{"models": rows}
	if t := providers.CatalogRefreshedAtForPrincipal(pid, principal); !t.IsZero() {
		resp["refreshed_at"] = t.UTC().Format(time.RFC3339)
	}
	writeJSON(w, 200, resp)
}

// POST /admin/api/providers/{id}/refresh â€” force a catalog refresh with an explicit result.
func handleRefreshProvider(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	pid := r.PathValue("id")
	if _, ok := config.Get().Providers[pid]; !ok {
		writeError(w, 404, "unknown provider")
		return
	}
	principal, err := catalogPrincipalForProvider(r, pid)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, runProviderProbe(pid, "refresh", principal))
}

// POST /admin/api/providers/{id}/test â€” force-refresh the catalog and return a safe, explicit verdict.
func handleTestProvider(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	pid := r.PathValue("id")
	if _, ok := config.Get().Providers[pid]; !ok {
		writeError(w, 404, "unknown provider")
		return
	}
	principal, err := catalogPrincipalForProvider(r, pid)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, runProviderProbe(pid, "test", principal))
}

// POST /admin/api/providers/{id}/repair â€” invalidate local provider and catalog caches before retrying.
func handleRepairProvider(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	pid := r.PathValue("id")
	if _, ok := config.Get().Providers[pid]; !ok {
		writeError(w, 404, "unknown provider")
		return
	}
	principal, err := catalogPrincipalForProvider(r, pid)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, runProviderProbe(pid, "repair", principal))
}

type categoryBody struct {
	Name     string           `json:"name"`
	Failover []map[string]any `json:"failover"`
}

func storeCategory(name string, members []config.CategoryMember) error {
	var collision error
	config.Update(func(s *config.Settings) {
		for existingName := range s.Categories {
			if existingName != name && strings.EqualFold(existingName, name) {
				collision = fmt.Errorf("category name collides with existing category %q; names are case-insensitive", existingName)
				return
			}
		}
		s.Categories[name] = &config.CategoryConfig{Failover: members}
	})
	return collision
}

// POST /admin/api/categories
func handleUpsertCategory(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	var body categoryBody
	if !decodeBody(r, &body) {
		writeError(w, 400, "invalid body")
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeError(w, 400, "category name required")
		return
	}
	if _, isProvider := config.Get().Providers[name]; isProvider || strings.Contains(name, "/") {
		writeError(w, 400, "category name must not contain '/' or collide with a provider id")
		return
	}
	principal, err := selectedCatalogPrincipal(r)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	members := make([]config.CategoryMember, 0, len(body.Failover))
	for index, member := range body.Failover {
		providerRaw, _ := member["provider"].(string)
		modelRaw, _ := member["model"].(string)
		providerID := strings.TrimSpace(providerRaw)
		modelID := strings.TrimSpace(modelRaw)
		if providerID == "" || modelID == "" {
			writeError(w, 400, fmt.Sprintf("route member %d requires provider and model", index+1))
			return
		}
		if _, exists := config.Get().Providers[providerID]; !exists {
			writeError(w, 400, fmt.Sprintf("route member %d has unknown provider %q", index+1, providerID))
			return
		}
		if providers.CatalogRequiresPrincipal(providerID) && principal == nil {
			writeError(w, 400, "principal_id is required to validate this private OAuth provider")
			return
		}
		if _, found := providers.CatalogLookupForPrincipal(providerID, modelID, principal); !found {
			writeError(w, 400, fmt.Sprintf("route member %d has unknown model %q for provider %q", index+1, modelID, providerID))
			return
		}
		members = append(members, config.CategoryMember{Provider: providerID, Model: modelID})
	}
	if len(members) == 0 {
		writeError(w, 400, "a route requires at least one provider/model member")
		return
	}
	if err := storeCategory(name, members); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	persist()
	writeJSON(w, 200, map[string]any{"ok": true, "name": name, "members": len(members)})
}

// DELETE /admin/api/categories/{name}
func handleDeleteCategory(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	name := r.PathValue("name")
	config.Update(func(s *config.Settings) { delete(s.Categories, name) })
	persist()
	writeJSON(w, 200, map[string]any{"ok": true})
}

type keyBody struct {
	ProjectID           string   `json:"project_id"`
	PrincipalID         string   `json:"principal_id"`
	Project             string   `json:"project"`
	Name                string   `json:"name"`
	ExpiresAt           int64    `json:"expires_at"`
	AllowedModels       []string `json:"allowed_models"`
	AllowedProviders    []string `json:"allowed_providers"`
	RPM                 int      `json:"rpm"`
	DailyRequests       int      `json:"daily_requests"`
	MonthlyRequests     int      `json:"monthly_requests"`
	DailyInputTokens    int64    `json:"daily_input_tokens"`
	DailyOutputTokens   int64    `json:"daily_output_tokens"`
	MonthlyTotalTokens  int64    `json:"monthly_total_tokens"`
	DailyCostMicroUSD   int64    `json:"daily_cost_microusd"`
	MonthlyCostMicroUSD int64    `json:"monthly_cost_microusd"`
	DailyCreditsMilli   int64    `json:"daily_credits_milli"`
	MonthlyCreditsMilli int64    `json:"monthly_credits_milli"`
}

// POST /admin/api/keys
func handleCreateKey(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	var body keyBody
	if !decodeBody(r, &body) {
		writeError(w, 400, "invalid body")
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = "key"
	}
	projectID := strings.TrimSpace(body.ProjectID)
	var project iam.Project
	var err error
	if projectID != "" {
		var ok bool
		project, ok, err = iam.ProjectByID(projectID)
		if err != nil {
			writeError(w, 500, "Identity store unavailable.")
			return
		}
		if !ok {
			writeError(w, 404, "unknown project")
			return
		}
	} else {
		slug := strings.TrimSpace(body.Project)
		if slug == "" {
			slug = "default"
		}
		project, err = iam.EnsureProject(slug, slug)
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
	}
	principalID := strings.TrimSpace(body.PrincipalID)
	if principalID == "" {
		subject := "service:" + project.Slug + ":" + strings.ToLower(strings.ReplaceAll(name, " ", "-"))
		principal, err := iam.EnsurePrincipalBySubject("service", subject, "", name)
		if err != nil {
			writeError(w, 500, "Could not create service principal.")
			return
		}
		principalID = principal.ID
		if err := iam.SetMembership(project.ID, principalID, "member"); err != nil {
			writeError(w, 500, "Could not assign project membership.")
			return
		}
	}
	issued, err := iam.IssueKey(iam.KeyCreate{
		ProjectID: project.ID, PrincipalID: principalID, Name: name,
		ExpiresAt: body.ExpiresAt, Policy: keyPolicyFromBody(body),
	})
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	auditAdmin(r, "api_key.create", "api_key", issued.ID, map[string]any{
		"project_id": project.ID, "principal_id": principalID, "name": name,
	})
	writeJSON(w, 200, map[string]any{
		"ok": true, "project": project.Slug, "name": name,
		"token": issued.Token, "key": issued.APIKey,
	})
}

type keyUpdateBody struct {
	ID                  string    `json:"id"`
	Token               string    `json:"token"` // legacy compatibility
	Disabled            *bool     `json:"disabled"`
	ExpiresAt           *int64    `json:"expires_at"`
	AllowedModels       *[]string `json:"allowed_models"`
	AllowedProviders    *[]string `json:"allowed_providers"`
	RPM                 *int      `json:"rpm"`
	DailyRequests       *int      `json:"daily_requests"`
	MonthlyRequests     *int      `json:"monthly_requests"`
	DailyInputTokens    *int64    `json:"daily_input_tokens"`
	DailyOutputTokens   *int64    `json:"daily_output_tokens"`
	MonthlyTotalTokens  *int64    `json:"monthly_total_tokens"`
	DailyCostMicroUSD   *int64    `json:"daily_cost_microusd"`
	MonthlyCostMicroUSD *int64    `json:"monthly_cost_microusd"`
	DailyCreditsMilli   *int64    `json:"daily_credits_milli"`
	MonthlyCreditsMilli *int64    `json:"monthly_credits_milli"`
}

// POST /admin/api/keys/update â€” edit governance on an existing key (partial).
func handleUpdateKey(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	var body keyUpdateBody
	if !decodeBody(r, &body) {
		writeError(w, 400, "invalid body")
		return
	}
	keyID, ok := resolveKeyID(body.ID, body.Token)
	if !ok {
		writeError(w, 404, "unknown key")
		return
	}
	key, ok, err := iam.APIKeyByID(keyID)
	if err != nil {
		writeError(w, 500, "Identity store unavailable.")
		return
	}
	if !ok {
		writeError(w, 404, "unknown key")
		return
	}
	status := (*string)(nil)
	if body.Disabled != nil {
		if key.Status == "revoked" {
			writeError(w, 400, "revoked API keys cannot change status")
			return
		}
		value := "active"
		if *body.Disabled {
			value = "disabled"
		}
		status = &value
	}
	policy := key.Policy
	changedPolicy := false
	if body.AllowedModels != nil {
		policy.AllowedModels = *body.AllowedModels
		changedPolicy = true
	}
	if body.AllowedProviders != nil {
		policy.AllowedProviders = *body.AllowedProviders
		changedPolicy = true
	}
	applyInt := func(src *int, dst *int) {
		if src != nil {
			*dst = *src
			changedPolicy = true
		}
	}
	applyInt64 := func(src *int64, dst *int64) {
		if src != nil {
			*dst = *src
			changedPolicy = true
		}
	}
	applyInt(body.RPM, &policy.RPM)
	applyInt(body.DailyRequests, &policy.DailyRequests)
	applyInt(body.MonthlyRequests, &policy.MonthlyRequests)
	applyInt64(body.DailyInputTokens, &policy.DailyInputTokens)
	applyInt64(body.DailyOutputTokens, &policy.DailyOutputTokens)
	applyInt64(body.MonthlyTotalTokens, &policy.MonthlyTotalTokens)
	applyInt64(body.DailyCostMicroUSD, &policy.DailyCostMicroUSD)
	applyInt64(body.MonthlyCostMicroUSD, &policy.MonthlyCostMicroUSD)
	applyInt64(body.DailyCreditsMilli, &policy.DailyCreditsMilli)
	applyInt64(body.MonthlyCreditsMilli, &policy.MonthlyCreditsMilli)
	update := iam.KeyUpdate{Status: status, ExpiresAt: body.ExpiresAt}
	if changedPolicy {
		update.Policy = &policy
	}
	if err := iam.UpdateAPIKey(keyID, update); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	auditAdmin(r, "api_key.update", "api_key", keyID, nil)
	writeJSON(w, 200, map[string]any{"ok": true})
}

// DELETE /admin/api/keys?id= (token remains accepted for legacy clients).
func handleRevokeKey(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	keyID, ok := resolveKeyID(r.URL.Query().Get("id"), r.URL.Query().Get("token"))
	if !ok {
		writeError(w, 404, "unknown key")
		return
	}
	if err := iam.RevokeAPIKey(keyID); err != nil {
		writeError(w, 404, err.Error())
		return
	}
	auditAdmin(r, "api_key.revoke", "api_key", keyID, nil)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func resolveKeyID(id, token string) (string, bool) {
	if id = strings.TrimSpace(id); id != "" {
		return id, true
	}
	if token = strings.TrimSpace(token); token != "" {
		if p, ok, err := iam.ResolveAPIKey(token); err == nil && ok {
			return p.KeyID, true
		}
	}
	return "", false
}

// POST /admin/api/detect â€” surface locally-discovered providers (does NOT
// hardwire them; the operator adds via POST /admin/api/providers).
func handleDetect(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	candidates := config.DiscoverLocalProviders()
	if candidates == nil {
		candidates = []config.LocalCandidate{}
	}
	writeJSON(w, 200, map[string]any{"candidates": candidates})
}

// GET /admin/api/usage
func handleUsage(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	filter, err := usageFilterFromRequest(r, "")
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	series, err := iam.UsageTimeSeries(filter)
	if err != nil {
		writeError(w, 400, "Usage series is unavailable: "+err.Error())
		return
	}
	controlPlane, err := iam.UsageStats(filter.From)
	if err != nil {
		writeError(w, 500, "Usage store unavailable.")
		return
	}
	quotaAdvisories, err := providerQuotaAdvisories("")
	if err != nil {
		writeError(w, 500, "Provider quota store unavailable.")
		return
	}
	writeJSON(w, 200, map[string]any{
		"totals": router.Totals(false), "by_project": router.ByProject(false),
		"recent": router.RecentUsage(50), "control_plane": controlPlane,
		"series": series, "quota_advisories": quotaAdvisories,
	})
}

// GET /admin/api/telemetry
func handleTelemetry(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	writeJSON(w, 200, map[string]any{"stats": router.TelemetryStats(), "recent": router.RecentTelemetry(50)})
}

// ---- copilot device login ---------------------------------------------- //

func handleCopilotLoginStart(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	dc, err := copilotauth.StartDeviceFlow()
	if err != nil {
		writeJSON(w, 200, map[string]any{"error": truncate200(err.Error())})
		return
	}
	writeJSON(w, 200, map[string]any{
		"device_code": dc.DeviceCode, "user_code": dc.UserCode,
		"verification_uri": dc.VerificationURI, "interval": dc.Interval, "expires_in": dc.ExpiresIn,
	})
}

func handleCopilotLoginPoll(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	var body struct {
		DeviceCode string `json:"device_code"`
	}
	_ = decodeBody(r, &body)
	writeJSON(w, 200, copilotauth.PollDeviceFlowOnce(body.DeviceCode))
}

func handleCopilotLogout(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	writeJSON(w, 200, copilotauth.ClearCachedCredentials())
}

// ---- helpers ------------------------------------------------------------ //

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func emptyNil(s string) string { return strings.TrimSpace(s) }

func truncate200(s string) string {
	if len(s) > 300 {
		return s[:300]
	}
	return s
}
