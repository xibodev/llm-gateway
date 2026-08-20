package api

import (
	"errors"
	"net/http"
	"strings"

	"llmgw/internal/config"
	"llmgw/internal/gcpauth"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
)

type connectionBody struct {
	ProviderID  string   `json:"provider_id"`
	Name        string   `json:"connection_name"`
	Kind        string   `json:"credential_kind"`
	Secret      string   `json:"secret"`
	MakeDefault flexBool `json:"make_default"`
}

func handleListPrincipalConnections(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	principalID := strings.TrimSpace(r.PathValue("id"))
	if _, ok, err := iam.PrincipalByID(principalID); err != nil {
		writeError(w, 500, "Identity store unavailable.")
		return
	} else if !ok {
		writeError(w, 404, "unknown principal")
		return
	}
	connections, err := iam.ListProviderConnections(
		principalID, strings.TrimSpace(r.URL.Query().Get("provider_id")),
	)
	if err != nil {
		writeError(w, 500, "Credential store unavailable.")
		return
	}
	writeJSON(w, 200, map[string]any{"connections": connections})
}

func handleCreatePrincipalConnection(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	principalID := strings.TrimSpace(r.PathValue("id"))
	var body connectionBody
	if err := decodeBodyOrForm(w, r, &body); err != nil {
		writeDecodeError(w, err)
		return
	}
	connection, err := createPersonalProviderConnection(principalID, body, iam.ConnectionSourceAdmin)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	providers.ForgetProviderForPrincipal(connection.ProviderID, principalID)
	providers.ForgetCatalogForPrincipal(connection.ProviderID, principalID)
	auditAdmin(r, "provider_connection.create", "principal", principalID, map[string]any{
		"provider": connection.ProviderID, "connection_id": connection.ID,
		"connection_name": connection.Name,
	})
	writeJSON(w, http.StatusCreated, map[string]any{"connection": connection})
}

func handleRevokePrincipalConnection(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	principalID := strings.TrimSpace(r.PathValue("id"))
	connectionID := strings.TrimSpace(r.PathValue("connection_id"))
	connections, err := iam.ListProviderConnections(principalID, "")
	if err != nil {
		writeError(w, 500, "Credential store unavailable.")
		return
	}
	var providerID string
	for _, connection := range connections {
		if connection.ID == connectionID {
			providerID = connection.ProviderID
			break
		}
	}
	if providerID == "" {
		writeError(w, 404, "provider connection not found")
		return
	}
	if err := iam.RevokeProviderConnection(principalID, connectionID); err != nil {
		if errors.Is(err, iam.ErrProviderConnectionNotFound) {
			writeError(w, 404, err.Error())
		} else {
			writeError(w, 500, "Credential store unavailable.")
		}
		return
	}
	providers.ForgetProviderForPrincipal(providerID, principalID)
	providers.ForgetCatalogForPrincipal(providerID, principalID)
	auditAdmin(r, "provider_connection.revoke", "principal", principalID, map[string]any{
		"provider": providerID, "connection_id": connectionID,
	})
	writeJSON(w, 200, map[string]any{"ok": true})
}

func handleUserConnections(w http.ResponseWriter, r *http.Request) {
	principal, ok := requireSSOUser(w, r)
	if !ok {
		return
	}
	connections, err := iam.ListProviderConnections(
		principal.ID, strings.TrimSpace(r.URL.Query().Get("provider_id")),
	)
	if err != nil {
		writeError(w, 500, "Credential store unavailable.")
		return
	}
	writeJSON(w, 200, map[string]any{"connections": connections})
}

func handleUserCreateConnection(w http.ResponseWriter, r *http.Request) {
	principal, ok := requireSSOUser(w, r)
	if !ok {
		return
	}
	var body connectionBody
	if err := decodeBodyOrForm(w, r, &body); err != nil {
		writeDecodeError(w, err)
		return
	}
	connection, err := createPersonalProviderConnection(
		principal.ID, body, iam.ConnectionSourceUser,
	)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	providers.ForgetProviderForPrincipal(connection.ProviderID, principal.ID)
	providers.ForgetCatalogForPrincipal(connection.ProviderID, principal.ID)
	_ = iam.RecordAudit(iam.AuditEvent{
		ActorPrincipalID: principal.ID, Action: "provider_connection.create",
		TargetType: "principal", TargetID: principal.ID, Result: "success",
		Detail: map[string]any{
			"provider": connection.ProviderID, "connection_id": connection.ID,
			"connection_name": connection.Name, "source": "self-service",
		},
	})
	writeJSON(w, http.StatusCreated, map[string]any{"connection": connection})
}

func handleUserRevokeConnection(w http.ResponseWriter, r *http.Request) {
	principal, ok := requireSSOUser(w, r)
	if !ok {
		return
	}
	connectionID := strings.TrimSpace(r.PathValue("connection_id"))
	connections, err := iam.ListProviderConnections(principal.ID, "")
	if err != nil {
		writeError(w, 500, "Credential store unavailable.")
		return
	}
	var providerID string
	for _, connection := range connections {
		if connection.ID == connectionID {
			providerID = connection.ProviderID
			break
		}
	}
	if providerID == "" {
		writeError(w, 404, "provider connection not found")
		return
	}
	if err := iam.RevokeProviderConnection(principal.ID, connectionID); err != nil {
		if errors.Is(err, iam.ErrProviderConnectionNotFound) {
			writeError(w, 404, err.Error())
		} else {
			writeError(w, 500, "Credential store unavailable.")
		}
		return
	}
	providers.ForgetProviderForPrincipal(providerID, principal.ID)
	providers.ForgetCatalogForPrincipal(providerID, principal.ID)
	_ = iam.RecordAudit(iam.AuditEvent{
		ActorPrincipalID: principal.ID, Action: "provider_connection.revoke",
		TargetType: "principal", TargetID: principal.ID, Result: "success",
		Detail: map[string]any{
			"provider": providerID, "connection_id": connectionID, "source": "self-service",
		},
	})
	writeJSON(w, 200, map[string]any{"ok": true})
}

// runtimeTypeAcceptsAPIKey answers, for a provider configured WITHOUT a
// registry id (the shape the legacy admin page's addProvider() posts, and the
// shape any hand-written config takes), whether its runtime type can consume an
// API-key connection.
//
// It asks the registry manifest rather than a hand-maintained list, because
// that list has now been the bug twice: vertex_ai was missing until a custom
// Vertex provider was found refusing the credential the identical
// registry-configured provider accepts, and azure_openai arrived after that fix
// and was missing again. A manifest that declares auth_methods: ["api_key"] is
// the same statement the registry-id branch above already trusts, so trusting
// it here closes the class instead of the instance.
//
// The literal cases remain for runtimes the manifest cannot speak for: legacy
// type spellings, and litellm, which has no registry entry of its own.
func runtimeTypeAcceptsAPIKey(runtimeType string) bool {
	normalized := strings.ToLower(strings.TrimSpace(runtimeType))
	switch normalized {
	case "openai_compatible", "openai", "litellm":
		return true
	}
	for _, entry := range providers.ProviderRegistry() {
		if entry.Availability != providers.ProviderAvailable {
			continue
		}
		if providers.RegistryRuntimeMatches(entry, normalized) &&
			contains(entry.AuthMethods, "api_key") {
			return true
		}
	}
	return false
}

func createPersonalProviderConnection(
	principalID string, body connectionBody, source string,
) (iam.ProviderConnection, error) {
	providerID := strings.TrimSpace(body.ProviderID)
	providerConfig, ok := config.Get().Providers[providerID]
	if !ok {
		return iam.ProviderConnection{}, &providers.ConfigError{
			Msg: "unknown configured provider " + providerID,
		}
	}
	kind := strings.ToLower(strings.TrimSpace(body.Kind))
	if kind == "" {
		kind = "api_key"
	}
	if kind == gcpauth.CredentialKind {
		// A service-account key authenticates only the Google Cloud surface.
		if !strings.EqualFold(strings.TrimSpace(providerConfig.Type), "vertex_ai") {
			return iam.ProviderConnection{}, &providers.ConfigError{
				Msg: "a service account key is accepted only by a vertex_ai provider",
			}
		}
		if _, err := gcpauth.Parse([]byte(body.Secret)); err != nil {
			return iam.ProviderConnection{}, &providers.ConfigError{Msg: err.Error()}
		}
		return iam.PutProviderConnection(iam.ProviderConnectionCreate{
			PrincipalID: principalID, ProviderID: providerID, Name: body.Name,
			Kind: kind, Secret: body.Secret, Source: source,
			MakeDefault: bool(body.MakeDefault),
		})
	}
	if kind != "api_key" {
		return iam.ProviderConnection{}, &providers.ConfigError{
			Msg: "generic connection creation accepts an API key or a GCP service account key; use the provider OAuth flow",
		}
	}
	if strings.EqualFold(providerConfig.Type, "github_copilot") {
		return iam.ProviderConnection{}, &providers.ConfigError{
			Msg: "GitHub Copilot connections must use the device authorization flow",
		}
	}
	if providerConfig.RegistryID != "" {
		entry, found := providers.RegistryProvider(providerConfig.RegistryID)
		if !found || entry.Availability != providers.ProviderAvailable ||
			!contains(entry.AuthMethods, "api_key") {
			return iam.ProviderConnection{}, &providers.ConfigError{
				Msg: "this provider integration does not accept API-key connections",
			}
		}
	} else if !runtimeTypeAcceptsAPIKey(providerConfig.Type) {
		return iam.ProviderConnection{}, &providers.ConfigError{
			Msg: "this provider type does not consume an API-key connection",
		}
	}
	return iam.PutProviderConnection(iam.ProviderConnectionCreate{
		PrincipalID: principalID, ProviderID: providerID, Name: body.Name,
		Kind: kind, Secret: body.Secret, Source: source,
		MakeDefault: bool(body.MakeDefault),
	})
}
