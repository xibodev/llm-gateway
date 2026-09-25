package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"llmgw/internal/buildinfo"
	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
	"llmgw/internal/router"

	anthropicauth "github.com/xibodev/llm-provider-auth/anthropic"
	gcpauth "github.com/xibodev/llm-provider-auth/gcp"
)

func persist() {
	_ = config.Save()
	providers.ResetProviders()
}

var endpointMutationMu sync.Mutex

func adminAuthed(w http.ResponseWriter, r *http.Request) bool {
	return requireAdmin(w, r)
}

func decodeBody(r *http.Request, v any) bool {
	return json.NewDecoder(r.Body).Decode(v) == nil
}

// maxCredentialBodyBytes bounds the entire request body decodeBodyOrForm will
// read, JSON or multipart alike. A service-account key is a few KB of JSON;
// this leaves comfortable headroom for it plus multipart framing overhead
// without leaving the body effectively unbounded. The bound is enforced by
// wrapping r.Body in http.MaxBytesReader before any parsing touches it --
// ParseMultipartForm's own maxMemory argument only caps what stays resident
// in memory, it is not a request-size limit: a file part larger than it
// spills to a disk-backed temp file with no upper bound, so passing a small
// maxMemory alone does nothing to stop a multi-gigabyte upload.
const maxCredentialBodyBytes = 256 << 10 // 256 KiB

// errBodyTooLarge marks a decodeBodyOrForm failure caused by exceeding
// maxCredentialBodyBytes, so the caller can answer 413 instead of a generic 400.
var errBodyTooLarge = errors.New("request body too large")

// errInvalidBody marks any other decodeBodyOrForm parse failure.
var errInvalidBody = errors.New("invalid body")

// decodeBodyOrForm accepts the same payload as JSON or as multipart/form-data.
// A file-shaped credential (a service-account key is a multi-line PEM-bearing
// JSON document) survives a file part intact, whereas embedding it in a JSON
// string requires the caller to escape it correctly — which shell pipelines
// routinely get wrong, destroying the key before the request is even built.
func decodeBodyOrForm(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxCredentialBodyBytes)
	// RFC 9110 makes the media type case-insensitive, and real clients send
	// "Multipart/Form-Data". Matching the raw header sent those into the JSON
	// branch, which then failed with a generic 400 on a perfectly legal request.
	contentType := strings.ToLower(r.Header.Get("Content-Type"))
	if !strings.HasPrefix(contentType, "multipart/form-data") {
		decoder := json.NewDecoder(r.Body)
		if err := decoder.Decode(v); err != nil {
			return classifyDecodeError(err)
		}
		// Decode stops at the end of the first complete JSON value and never
		// touches the rest of the body, so MaxBytesReader alone does not bound
		// the JSON path: a small valid object followed by megabytes of
		// whitespace decoded successfully and the limit was never crossed.
		// Reading on forces it to be. The read also rejects a second value --
		// one request carries one payload, and silently ignoring whatever
		// follows is how a padded or smuggled body gets through.
		var trailing json.RawMessage
		if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
			if err == nil {
				return errInvalidBody
			}
			return classifyDecodeError(err)
		}
		return nil
	}
	if err := r.ParseMultipartForm(maxCredentialBodyBytes); err != nil {
		return classifyDecodeError(err)
	}
	// Read straight from the parsed form's own maps rather than through
	// r.FormValue/r.FormFile: those also consult r.Form, which net/http
	// populates with the URL query string ahead of the multipart values, so
	// FormValue silently returns a query parameter instead of the submitted
	// form field when both are present. Reading MultipartForm directly means
	// only what the caller actually put in the body is ever used.
	fields := map[string]string{}
	for name, values := range r.MultipartForm.Value {
		if len(values) > 0 {
			fields[name] = values[0]
		}
	}
	// A field may arrive as either a value part or a file part; the file part
	// wins, since that is the form a caller reaches for when the value is a file.
	for name, headers := range r.MultipartForm.File {
		if len(headers) == 0 {
			continue
		}
		file, err := headers[0].Open()
		if err != nil {
			return errInvalidBody
		}
		content, err := io.ReadAll(file)
		file.Close()
		if err != nil {
			return classifyDecodeError(err)
		}
		fields[name] = string(content)
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return errInvalidBody
	}
	if err := json.Unmarshal(encoded, v); err != nil {
		return errInvalidBody
	}
	return nil
}

// classifyDecodeError distinguishes a size-limit failure from any other parse
// failure, so decodeBodyOrForm's caller can answer 413 rather than a generic
// 400 when the body was rejected only for being too large.
func classifyDecodeError(err error) error {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return errBodyTooLarge
	}
	return errInvalidBody
}

// writeDecodeError answers a decodeBodyOrForm failure with the status code it
// implies: 413 when the body exceeded the size limit, 400 otherwise.
func writeDecodeError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	if errors.Is(err, errBodyTooLarge) {
		status = http.StatusRequestEntityTooLarge
	}
	writeError(w, status, err.Error())
}

// flexBool decodes a JSON boolean, matching the JSON wire contract exactly, or
// (as a fallback) the string "true"/"false" that decodeBodyOrForm produces for
// a multipart checkbox field — multipart has no boolean type, so the field
// arrives as text and would otherwise fail json.Unmarshal and reject the whole
// request. A JSON caller sending a literal true/false never reaches the
// fallback, so the wire contract for existing clients is unchanged.
type flexBool bool

func (b *flexBool) UnmarshalJSON(data []byte) error {
	var v bool
	if err := json.Unmarshal(data, &v); err == nil {
		*b = flexBool(v)
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	parsed, err := strconv.ParseBool(s)
	if err != nil {
		return err
	}
	*b = flexBool(parsed)
	return nil
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
			"registry_id":         pc.RegistryID,
			"api_key_set":         secrets[pid] != "" || pc.APIKey != "" || systemConnection,
			"force_api_support":   pc.ForceApiSupport,
			"project":             pc.Project,
			"location":            pc.Location,
			"vertex_request_type": pc.VertexRequestType,
			"default_voice":       pc.DefaultVoice,
			"disabled":            pc.Disabled,
			"models":              len(models),
		}
		if !refreshed.IsZero() {
			entry["catalog_refreshed"] = refreshed.UTC().Format(time.RFC3339)
		}
		provList = append(provList, entry)
	}
	cats := map[string]any{}
	for name, cat := range s.Endpoints {
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
			"allowed_routes":    k.Policy.AllowedRoutes,
			"routes_only":       k.Policy.RoutesOnly,
			"admin_managed":     k.Policy.AdminManaged,
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
			"revealable":            k.Revealable,
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

	build := buildinfo.Current()
	writeJSON(w, 200, map[string]any{
		"version":       build.Version,
		"commit":        build.Commit,
		"build_time":    build.BuildTime,
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
		"categories":                   cats, // Deprecated compatibility alias; use "endpoints".
		"endpoints":                    cats,
		"policies": map[string]any{
			"defaults":  s.Policies.Defaults,
			"overrides": s.Policies.ConfiguredOverrides(),
		},
		"savings": router.Totals(false),
		"copilot": map[string]any{"enabled": providers.CopilotEnabled(), "auth": providers.CopilotAuth().AuthStatus()},
	})
}

type providerBody struct {
	ID                  string   `json:"id"`
	RegistryID          string   `json:"registry_id"`
	Type                string   `json:"type"`
	BaseURL             string   `json:"base_url"`
	APIKey              string   `json:"api_key"`
	CredentialKind      string   `json:"credential_kind"`
	Region              string   `json:"region"`
	Project             string   `json:"project"`
	Location            string   `json:"location"`
	VertexRequestType   *string  `json:"vertex_request_type"`
	PublicOAuthClientID string   `json:"public_oauth_client_id"`
	ClearPublicOAuthID  *bool    `json:"clear_public_oauth_client_id"`
	ForceApiSupport     flexBool `json:"force_api_support"`
}

// POST /admin/api/providers
func handleUpsertProvider(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	endpointMutationMu.Lock()
	defer endpointMutationMu.Unlock()
	var body providerBody
	if err := decodeBodyOrForm(w, r, &body); err != nil {
		writeDecodeError(w, err)
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
	if body.VertexRequestType != nil {
		value := strings.ToLower(strings.TrimSpace(*body.VertexRequestType))
		if body.Type != "vertex_ai" && value != "" {
			writeError(w, 400, "vertex_request_type is only valid for vertex_ai providers")
			return
		}
		if value != "" && value != "default" && value != "paygo" && value != "dedicated" {
			writeError(w, 400, "vertex_request_type must be default, paygo, or dedicated")
			return
		}
		body.VertexRequestType = &value
	}
	if err := providerEndpointCollision(pid); err != nil {
		writeError(w, http.StatusConflict, err.Error())
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
	if body.ClearPublicOAuthID != nil && *body.ClearPublicOAuthID {
		previous := config.Get().Providers[pid]
		if previous != nil && strings.TrimSpace(previous.PublicOAuthClientID) != "" {
			bound, err := providerOAuthClientIDInUse(pid, previous.PublicOAuthClientID)
			if err != nil {
				writeError(w, 500, "Credential store unavailable.")
				return
			}
			if bound {
				writeError(w, http.StatusConflict, "Revoke or reauthorize active OAuth connections before clearing the public OAuth client ID.")
				return
			}
		}
	}
	credentialKind := strings.ToLower(strings.TrimSpace(body.CredentialKind))
	if credentialKind == "" {
		credentialKind = "api_key"
	}
	if raw := strings.TrimSpace(body.APIKey); raw != "" {
		// A service account key is a JSON document, not an opaque secret. The
		// dialog offers one credential field, so detect the document here:
		// stored as an API key it would be sent as x-goog-api-key and rejected
		// by Google, reporting a bad key when the real fault is the wrong kind.
		setupTokenLooking := strings.HasPrefix(raw, "sk-ant-oat")
		if setupTokenLooking && !strings.EqualFold(body.Type, "anthropic") {
			writeError(w, 400, "an Anthropic setup token is only usable by an anthropic provider")
			return
		}
		if strings.EqualFold(body.Type, "anthropic") && setupTokenLooking {
			detectedKind, err := anthropicauth.Kind(body.APIKey)
			if err != nil {
				writeError(w, 400, err.Error())
				return
			}
			credentialKind = string(detectedKind)
		} else if credentialKind == string(anthropicauth.CredentialSetupToken) {
			if !strings.EqualFold(body.Type, "anthropic") {
				writeError(w, 400, "an Anthropic setup token is only usable by an anthropic provider")
				return
			}
			if err := anthropicauth.ValidateSetupToken(body.APIKey); err != nil {
				writeError(w, 400, err.Error())
				return
			}
		} else if credentialKind != "api_key" && credentialKind != gcpauth.CredentialKind {
			writeError(w, 400, "unsupported provider credential kind")
			return
		}
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
		stored, err := iam.PutSystemProviderConnection(
			pid, credentialKind, raw,
		)
		if err != nil {
			writeError(w, 500, "store encrypted system connection: "+err.Error())
			return
		}
		if credentialKind == string(anthropicauth.CredentialSetupToken) && !stored {
			writeError(w, 400, "LLMGW_CREDENTIAL_ENCRYPTION_KEY is required to store an Anthropic setup token")
			return
		}
		if credentialKind == string(anthropicauth.CredentialSetupToken) {
			config.DeleteSecret(pid)
		}
	}
	config.Update(func(s *config.Settings) {
		next := &config.ProviderConfig{
			Type: body.Type, RegistryID: registryID, BaseURL: emptyNil(body.BaseURL),
			Region: emptyNil(body.Region), Project: emptyNil(body.Project),
			Location: emptyNil(body.Location), PublicOAuthClientID: strings.TrimSpace(body.PublicOAuthClientID),
			ForceApiSupport: bool(body.ForceApiSupport),
		}
		if previous := s.Providers[pid]; previous != nil {
			next.Timeout = previous.Timeout
			next.DefaultVoice = previous.DefaultVoice
			next.Disabled = previous.Disabled
			if next.PublicOAuthClientID == "" && (body.ClearPublicOAuthID == nil || !*body.ClearPublicOAuthID) {
				next.PublicOAuthClientID = previous.PublicOAuthClientID
			}
			if body.VertexRequestType == nil {
				next.VertexRequestType = previous.VertexRequestType
			}
		}
		if body.VertexRequestType != nil {
			next.VertexRequestType = *body.VertexRequestType
		}
		s.Providers[pid] = next
	})
	if strings.TrimSpace(body.APIKey) != "" && credentialKind != string(anthropicauth.CredentialSetupToken) {
		config.SaveSecret(pid, strings.TrimSpace(body.APIKey))
	}
	providers.ForgetProvider(pid)
	providers.ForgetCatalog(pid)
	_ = iam.InvalidateProviderChecks(pid)
	persist()
	writeJSON(w, 200, map[string]any{"ok": true, "id": pid})
}

func providerOAuthClientIDInUse(providerID, clientID string) (bool, error) {
	legacyBound, err := iam.ActiveLegacyOAuthProviderCredentialExists(providerID)
	if err != nil || legacyBound {
		return legacyBound, err
	}
	connections, err := iam.ListProviderConnections("", providerID)
	if err != nil {
		return false, err
	}
	for _, connection := range connections {
		if connection.Status != "active" || !strings.Contains(strings.ToLower(connection.Kind), "oauth") {
			continue
		}
		envelope, _, ok, err := iam.OAuthProviderConnectionSecret(connection.PrincipalID, providerID, connection.Name)
		if err != nil {
			return false, err
		}
		storedClientID := strings.TrimSpace(envelope.OAuthClientID)
		profile := strings.TrimSpace(envelope.OAuthProfile)
		if ok && (storedClientID == strings.TrimSpace(clientID) ||
			(storedClientID == "" && (profile == "" || profile == "public_pkce"))) {
			return true, nil
		}
	}
	return false, nil
}

// DELETE /admin/api/providers/{id}
func handleDeleteProvider(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	anonymousProviderMutationMu.Lock()
	defer anonymousProviderMutationMu.Unlock()
	endpointMutationMu.Lock()
	defer endpointMutationMu.Unlock()
	pid := r.PathValue("id")
	connections, err := iam.ListProviderConnections("", pid)
	if err != nil {
		writeError(w, 500, "Credential store unavailable.")
		return
	}
	for _, connection := range connections {
		if connection.Status == "active" && connection.PrincipalKind != "system" {
			writeError(w, http.StatusConflict, "provider still has active connections; revoke them before deleting the provider")
			return
		}
	}
	for endpointName, endpoint := range config.Get().Endpoints {
		if endpoint == nil {
			continue
		}
		for _, member := range endpoint.Failover {
			if member.Provider == pid {
				writeError(w, http.StatusConflict, fmt.Sprintf("provider is still referenced by endpoint %q", endpointName))
				return
			}
		}
	}
	wasAutomationManaged, err := iam.AnonymousProviderManaged(pid)
	if err != nil {
		writeError(w, 500, "read provider automation ownership: "+err.Error())
		return
	}
	if err := iam.ClearAnonymousProviderManaged(pid); err != nil {
		writeError(w, 500, "clear provider automation ownership: "+err.Error())
		return
	}
	restoreConfig, err := config.UpdateAndSave(func(s *config.Settings) error {
		delete(s.Providers, pid)
		return nil
	})
	if err != nil {
		if wasAutomationManaged {
			if markerErr := iam.MarkAnonymousProviderManaged(pid); markerErr != nil {
				writeError(w, 500, "Provider configuration could not be persisted and automation ownership could not be restored.")
				return
			}
		}
		writeError(w, 500, "Provider configuration could not be persisted.")
		return
	}
	if err := iam.RevokeSystemProviderConnection(pid); err != nil {
		restoreErr := restoreConfig()
		markerErr := error(nil)
		if wasAutomationManaged {
			markerErr = iam.MarkAnonymousProviderManaged(pid)
		}
		if restoreErr != nil || markerErr != nil {
			writeError(w, 500, "Provider credential revocation failed and provider configuration could not be fully restored.")
			return
		}
		writeError(w, 500, "revoke system connection: "+err.Error())
		return
	}
	config.DeleteSecret(pid)
	providers.ForgetProvider(pid)
	providers.ForgetCatalog(pid)
	_ = iam.DeleteProviderChecks(pid)
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
	result := runProviderVerify(pid, model, principal)
	auditResult := "failure"
	if success, _ := result["success"].(bool); success {
		auditResult = "success"
	}
	auditAdminResult(r, "provider.verify", "provider", pid, auditResult, map[string]any{"model": model})
	writeJSON(w, 200, result)
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
	result := providers.ReadCachedCatalogForPrincipal(pid, callerOf(principal))
	rows := result.Models
	ids := []string{}
	for _, row := range rows {
		if row.ID != "" {
			ids = append(ids, row.ID)
		}
	}
	writeJSON(w, 200, map[string]any{
		"models": ids, "count": len(rows), "catalog": result.Diagnostics,
		"readiness": catalogReadiness(pid, principal, result),
	})
}

// catalogRowsWithLegacySurfaces renders catalog rows for the wire while keeping
// the compatibility alias GET /v1/models honours (see setSurfaces).
// Marshalling providers.ModelInfo directly emits only "supported_surfaces",
// which would silently change this route's shape for anything still reading the
// pre-rename key while the documented compatibility policy emits both.
//
// Rows round-trip through the struct's own JSON tags rather than being rebuilt
// field by field, so the two shapes cannot drift when ModelInfo gains a field.
func catalogRowsWithLegacySurfaces(rows []providers.ModelInfo) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		entry := map[string]any{}
		raw, err := json.Marshal(row)
		if err != nil {
			continue
		}
		if err := json.Unmarshal(raw, &entry); err != nil {
			continue
		}
		if len(row.SupportedSurfaces) > 0 {
			entry["supported_endpoints"] = row.SupportedSurfaces // Deprecated: use supported_surfaces.
		}
		out = append(out, entry)
	}
	return out
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
	result := providers.ReadCachedCatalogForPrincipal(pid, callerOf(principal))
	rows := catalogRowsWithLegacySurfaces(result.Models)
	automationManaged, automationErr := providers.AutomationManagedAnonymousProvider(pid)
	if automationErr != nil {
		writeError(w, 500, "Model publication evidence is unavailable.")
		return
	}
	if automationManaged {
		evidence, evidenceErr := iam.ProviderModelEvidenceFor(pid, "")
		if evidenceErr != nil {
			writeError(w, 500, "Model publication evidence is unavailable.")
			return
		}
		for _, row := range rows {
			model := strings.TrimSpace(fmt.Sprint(row["id"]))
			current, found := evidence[model]
			state := "unverified"
			if found {
				state = current.State
			}
			row["publication_state"] = state
			published := state == "verified"
			row["published"] = published
			row["disabled"] = !published
			row["admin_unverified_opt_in"] = false
			if current.ObservedAt > 0 {
				row["observed_at"] = time.Unix(current.ObservedAt, 0).UTC().Format(time.RFC3339)
				row["latency_ms"] = current.LatencyMS
			}
			if current.FailureCode != "" {
				row["failure_code"] = current.FailureCode
			}
		}
	}
	resp := map[string]any{
		"models": rows, "catalog": result.Diagnostics,
		"readiness": catalogReadiness(pid, principal, result),
	}
	if t := result.RefreshedAt; !t.IsZero() {
		resp["refreshed_at"] = t.UTC().Format(time.RFC3339)
	}
	writeJSON(w, 200, resp)
}

var catalogLookupForPrincipal = func(providerID, model string, principal *config.Principal) (providers.ModelInfo, bool) {
	return providers.CatalogLookupForPrincipal(providerID, model, callerOf(principal))
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

type endpointBody struct {
	Name     string           `json:"name"`
	Failover []map[string]any `json:"failover"`
}

func storeEndpoint(name string, members []config.EndpointMember) (func() error, error) {
	return config.UpdateAndSave(func(s *config.Settings) error {
		for existingName := range s.Endpoints {
			if existingName != name && strings.EqualFold(existingName, name) {
				return fmt.Errorf("endpoint name collides with existing endpoint %q; names are case-insensitive", existingName)
			}
		}
		s.Endpoints[name] = &config.EndpointConfig{Failover: members}
		return nil
	})
}

func endpointNameCollision(name string) error {
	for existingName := range config.Get().Endpoints {
		if existingName != name && strings.EqualFold(existingName, name) {
			return fmt.Errorf("endpoint name collides with existing endpoint %q; names are case-insensitive", existingName)
		}
	}
	return nil
}

func providerEndpointCollision(providerID string) error {
	for endpointName := range config.Get().Endpoints {
		if strings.EqualFold(endpointName, providerID) {
			return fmt.Errorf("provider id collides with existing endpoint %q", endpointName)
		}
	}
	return nil
}

// handleUpsertEndpoint backs both POST /admin/api/endpoints (canonical) and
// the deprecated POST /admin/api/categories (see server.go route
// registration) — the two must never diverge in behaviour.
func handleUpsertEndpoint(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	endpointMutationMu.Lock()
	defer endpointMutationMu.Unlock()
	var body endpointBody
	if !decodeBody(r, &body) {
		writeError(w, 400, "invalid body")
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeError(w, 400, "endpoint name required")
		return
	}
	isProvider := false
	for providerID := range config.Get().Providers {
		if strings.EqualFold(providerID, name) {
			isProvider = true
			break
		}
	}
	if isProvider || strings.Contains(name, "/") {
		writeError(w, 400, "endpoint name must not contain '/' or collide with a provider id")
		return
	}
	if err := endpointNameCollision(name); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	principal, err := selectedCatalogPrincipal(r)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	members := make([]config.EndpointMember, 0, len(body.Failover))
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
		if _, found := catalogLookupForPrincipal(providerID, modelID, principal); !found {
			writeError(w, 400, fmt.Sprintf("route member %d has unknown model %q for provider %q", index+1, modelID, providerID))
			return
		}
		_, published, publicationErr := providers.AnonymousModelPublication(providerID, modelID)
		if publicationErr != nil {
			writeError(w, 500, "Model publication evidence is unavailable.")
			return
		}
		allowUnverified, _ := member["allow_unverified"].(bool)
		if !published {
			evidence, evidenceErr := iam.ProviderModelEvidenceFor(providerID, "")
			if evidenceErr != nil {
				writeError(w, 500, "Model publication evidence is unavailable.")
				return
			}
			current := evidence[modelID]
			if !allowUnverified || current.State != "unverified" {
				writeError(w, 400, fmt.Sprintf("route member %d is %s and disabled for routing", index+1, modelPublicationState(current.State)))
				return
			}
		} else {
			allowUnverified = false
		}
		members = append(members, config.EndpointMember{Provider: providerID, Model: modelID, AllowUnverified: allowUnverified})
	}
	if len(members) == 0 {
		writeError(w, 400, "a route requires at least one provider/model member")
		return
	}
	if _, err := storeEndpoint(name, members); err != nil {
		writeError(w, 500, "Route configuration could not be persisted.")
		return
	}
	providers.ResetProviders()
	writeJSON(w, 200, map[string]any{"ok": true, "name": name, "members": len(members)})
}

func modelPublicationState(state string) string {
	if state == "" {
		return "unverified"
	}
	return state
}

// handleDeleteEndpoint backs both DELETE /admin/api/endpoints/{name}
// (canonical) and the deprecated DELETE /admin/api/categories/{name} (see
// server.go route registration) — the two must never diverge in behaviour.
func handleDeleteEndpoint(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	endpointMutationMu.Lock()
	defer endpointMutationMu.Unlock()
	name := r.PathValue("name")
	if _, exact := config.Get().Endpoints[name]; !exact {
		matches := []string{}
		for existingName := range config.Get().Endpoints {
			if strings.EqualFold(existingName, name) {
				matches = append(matches, existingName)
			}
		}
		if len(matches) > 1 {
			writeError(w, http.StatusConflict, "endpoint name is ambiguous under case-insensitive matching")
			return
		}
		if len(matches) == 1 {
			name = matches[0]
		}
	}
	if _, err := config.UpdateAndSave(func(s *config.Settings) error {
		delete(s.Endpoints, name)
		return nil
	}); err != nil {
		writeError(w, 500, "Endpoint configuration could not be persisted.")
		return
	}
	providers.ResetProviders()
	writeJSON(w, 200, map[string]any{"ok": true})
}

type keyBody struct {
	ProjectID           string   `json:"project_id"`
	PrincipalID         string   `json:"principal_id"`
	Project             string   `json:"project"`
	Name                string   `json:"name"`
	ExpiresAt           int64    `json:"expires_at"`
	AllowedModels       []string `json:"allowed_models"`
	AllowedRoutes       []string `json:"allowed_routes"`
	RoutesOnly          bool     `json:"routes_only"`
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
	policy := keyPolicyFromBody(body)
	policy.AdminManaged = true
	issued, err := iam.IssueKey(iam.KeyCreate{
		ProjectID: project.ID, PrincipalID: principalID, Name: name,
		ExpiresAt: body.ExpiresAt, Policy: policy,
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

func handleRevealKey(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	if !adminAuthed(w, r) {
		return
	}
	keyID := strings.TrimSpace(r.PathValue("id"))
	token, found, err := iam.RevealAPIKey(keyID)
	if !found {
		writeError(w, 404, "unknown key")
		return
	}
	if errors.Is(err, iam.ErrAPIKeyNotRevealable) {
		writeError(w, 409, "This key predates encrypted key recovery and cannot be revealed.")
		return
	}
	if err != nil {
		writeError(w, 500, "Encrypted key store unavailable.")
		return
	}
	auditAdmin(r, "api_key.reveal", "api_key", keyID, nil)
	writeJSON(w, 200, map[string]any{"token": token})
}

type keyUpdateBody struct {
	ID                  string    `json:"id"`
	Token               string    `json:"token"` // legacy compatibility
	Disabled            *bool     `json:"disabled"`
	ExpiresAt           *int64    `json:"expires_at"`
	AllowedModels       *[]string `json:"allowed_models"`
	AllowedRoutes       *[]string `json:"allowed_routes"`
	RoutesOnly          *bool     `json:"routes_only"`
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
	if body.AllowedRoutes != nil {
		policy.AllowedRoutes = *body.AllowedRoutes
		changedPolicy = true
	}
	if body.RoutesOnly != nil {
		policy.RoutesOnly = *body.RoutesOnly
		changedPolicy = true
	}
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
	update := iam.KeyUpdate{Status: status, ExpiresAt: body.ExpiresAt, Admin: true, ExpectedPolicy: &key.Policy}
	if changedPolicy {
		update.Policy = &policy
	}
	if err := iam.UpdateAPIKey(keyID, update); err != nil {
		if errors.Is(err, iam.ErrAPIKeyConflict) {
			writeError(w, 409, err.Error())
			return
		}
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
	status := "revoked"
	if err := iam.UpdateAPIKey(keyID, iam.KeyUpdate{Status: &status, Admin: true}); err != nil {
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
	dc, err := providers.CopilotAuth().StartDeviceFlow()
	if err != nil {
		writeJSON(w, 200, map[string]any{"error": oauthErrorText(err.Error())})
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
	result := providers.CopilotAuth().PollDeviceFlowOnce(body.DeviceCode)
	status, _ := result["status"].(string)
	detail, _ := result["error"].(string)
	writeJSON(w, 200, safeOAuthPollResponse(status, detail))
}

func handleCopilotLogout(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	writeJSON(w, 200, providers.CopilotAuth().ClearCachedCredentials())
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
