package api

import (
	"net/http"
	"strings"

	"llmgw/internal/config"
	"llmgw/internal/copilotauth"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
)

type principalBody struct {
	Kind            string `json:"kind"`
	ExternalSubject string `json:"external_subject"`
	Email           string `json:"email"`
	DisplayName     string `json:"display_name"`
}

func handleListPrincipals(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	items, err := iam.ListPrincipals()
	if err != nil {
		writeError(w, 500, "Identity store unavailable.")
		return
	}
	writeJSON(w, 200, map[string]any{"principals": items})
}

func handleCreatePrincipal(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	var body principalBody
	if !decodeBody(r, &body) {
		writeError(w, 400, "invalid body")
		return
	}
	principal, err := iam.CreatePrincipal(
		strings.TrimSpace(body.Kind), strings.TrimSpace(body.ExternalSubject),
		strings.TrimSpace(body.Email), strings.TrimSpace(body.DisplayName),
	)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	auditAdmin(r, "principal.create", "principal", principal.ID, map[string]any{"kind": principal.Kind})
	writeJSON(w, 201, principal)
}

type statusBody struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

func handlePrincipalStatus(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	var body statusBody
	if !decodeBody(r, &body) {
		writeError(w, 400, "invalid body")
		return
	}
	if err := iam.SetPrincipalStatus(strings.TrimSpace(body.ID), strings.TrimSpace(body.Status)); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	auditAdmin(r, "principal.status", "principal", body.ID, map[string]any{"status": body.Status})
	writeJSON(w, 200, map[string]any{"ok": true})
}

type projectBody struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

func handleListProjects(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	items, err := iam.ListProjects()
	if err != nil {
		writeError(w, 500, "Identity store unavailable.")
		return
	}
	writeJSON(w, 200, map[string]any{"projects": items})
}

func handleCreateProject(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	var body projectBody
	if !decodeBody(r, &body) {
		writeError(w, 400, "invalid body")
		return
	}
	project, err := iam.CreateProject(body.Slug, body.Name)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	auditAdmin(r, "project.create", "project", project.ID, map[string]any{"slug": project.Slug})
	writeJSON(w, 201, project)
}

func handleProjectStatus(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	var body statusBody
	if !decodeBody(r, &body) {
		writeError(w, 400, "invalid body")
		return
	}
	if err := iam.SetProjectStatus(strings.TrimSpace(body.ID), strings.TrimSpace(body.Status)); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	auditAdmin(r, "project.status", "project", body.ID, map[string]any{"status": body.Status})
	writeJSON(w, 200, map[string]any{"ok": true})
}

func handleGetProjectPolicy(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	policy, err := iam.GetProjectPolicy(strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		writeError(w, 500, "Project policy store unavailable.")
		return
	}
	writeJSON(w, 200, policy)
}

func handleSetProjectPolicy(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	var policy iam.KeyPolicy
	if !decodeBody(r, &policy) {
		writeError(w, 400, "invalid body")
		return
	}
	result, err := iam.SetProjectPolicy(strings.TrimSpace(r.PathValue("id")), policy)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	auditAdmin(r, "project.policy", "project", result.ProjectID, nil)
	writeJSON(w, 200, result)
}

type credentialBindingBody struct {
	ProviderID    string `json:"provider_id"`
	PrincipalKind string `json:"principal_kind"`
	CredentialID  string `json:"credential_id"`
	Status        string `json:"status"`
}

func handleListProviderCredentialBindings(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	items, err := iam.ListProviderCredentialBindings(strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		writeError(w, 500, "Credential binding store unavailable.")
		return
	}
	writeJSON(w, 200, map[string]any{"bindings": items})
}

func handleSetProviderCredentialBinding(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	var body credentialBindingBody
	if !decodeBody(r, &body) {
		writeError(w, 400, "invalid body")
		return
	}
	binding, err := iam.SetProviderCredentialBinding(
		strings.TrimSpace(r.PathValue("id")), strings.TrimSpace(body.ProviderID),
		strings.TrimSpace(body.PrincipalKind), strings.TrimSpace(body.CredentialID),
	)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	providers.ResetProviders()
	providers.ForgetCatalogForProject(binding.ProviderID, binding.ProjectID)
	auditAdmin(r, "provider_credential_binding.set", "project", binding.ProjectID, map[string]any{
		"provider_id": binding.ProviderID, "principal_kind": binding.PrincipalKind,
		"credential_id": binding.CredentialID,
	})
	writeJSON(w, 200, binding)
}

func handleProviderCredentialBindingStatus(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	var body credentialBindingBody
	if !decodeBody(r, &body) {
		writeError(w, 400, "invalid body")
		return
	}
	projectID := strings.TrimSpace(r.PathValue("id"))
	providerID := strings.TrimSpace(body.ProviderID)
	principalKind := strings.TrimSpace(body.PrincipalKind)
	status := strings.TrimSpace(body.Status)
	if err := iam.SetProviderCredentialBindingStatus(
		projectID, providerID, principalKind, status,
	); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	providers.ResetProviders()
	providers.ForgetCatalogForProject(providerID, projectID)
	auditAdmin(r, "provider_credential_binding.status", "project", projectID, map[string]any{
		"provider_id": providerID, "principal_kind": principalKind, "status": status,
	})
	writeJSON(w, 200, map[string]any{"ok": true})
}

func handleImportSharedProviderCredential(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	providerID := strings.TrimSpace(r.PathValue("id"))
	provider, ok := config.Get().Providers[providerID]
	if !ok {
		writeError(w, 404, "unknown provider")
		return
	}
	if !strings.EqualFold(provider.Type, "github_copilot") {
		writeError(w, 400, "configured shared credential import is supported only for github_copilot")
		return
	}
	var body struct {
		Source string `json:"source"`
	}
	if !decodeBody(r, &body) || strings.TrimSpace(body.Source) != "configured" {
		writeError(w, 400, "source must be 'configured'")
		return
	}
	secret, err := copilotauth.ResolveOAuthToken()
	if err != nil {
		writeError(w, 400, "configured provider credential is unavailable")
		return
	}
	credential, err := iam.PutGatewayProviderCredential(providerID, "github_oauth", secret)
	if err != nil {
		writeError(w, 500, "Could not encrypt provider credential.")
		return
	}
	providers.ResetProviders()
	providers.ForgetInheritedCatalogs(providerID)
	auditAdmin(r, "provider_credential.import", "provider_credential", credential.ID, map[string]any{
		"provider_id": providerID, "source": "configured",
	})
	writeJSON(w, 200, map[string]any{"ok": true, "credential": credential})
}

func handleProviderCredentialStatus(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	var body struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if !decodeBody(r, &body) {
		writeError(w, 400, "invalid body")
		return
	}
	credentialID := strings.TrimSpace(body.ID)
	credential, ok, err := iam.ProviderCredentialByID(credentialID)
	if err != nil {
		writeError(w, 500, "Credential store unavailable.")
		return
	}
	if !ok {
		writeError(w, 404, "provider credential not found")
		return
	}
	owner, ok, err := iam.PrincipalByID(credential.PrincipalID)
	if err != nil {
		writeError(w, 500, "Credential owner store unavailable.")
		return
	}
	if !ok {
		writeError(w, 500, "Provider credential owner is unavailable.")
		return
	}
	if err := iam.SetProviderCredentialStatus(credentialID, strings.TrimSpace(body.Status)); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	providers.ResetProviders()
	if owner.Kind == "system" {
		providers.ForgetInheritedCatalogs(credential.ProviderID)
	} else {
		providers.ForgetCatalogForPrincipal(credential.ProviderID, credential.PrincipalID)
	}
	auditAdmin(r, "provider_credential.status", "provider_credential", body.ID, map[string]any{
		"provider_id": credential.ProviderID, "status": body.Status,
	})
	writeJSON(w, 200, map[string]any{"ok": true})
}

type membershipBody struct {
	ProjectID   string `json:"project_id"`
	PrincipalID string `json:"principal_id"`
	Role        string `json:"role"`
}

func handleListMemberships(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	projectID := strings.TrimSpace(r.URL.Query().Get("project_id"))
	if projectID == "" {
		writeError(w, 400, "project_id required")
		return
	}
	items, err := iam.ListMemberships(projectID)
	if err != nil {
		writeError(w, 500, "Identity store unavailable.")
		return
	}
	writeJSON(w, 200, map[string]any{"memberships": items})
}

func handleSetMembership(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	var body membershipBody
	if !decodeBody(r, &body) {
		writeError(w, 400, "invalid body")
		return
	}
	if err := iam.SetMembership(
		strings.TrimSpace(body.ProjectID), strings.TrimSpace(body.PrincipalID),
		strings.TrimSpace(body.Role),
	); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	auditAdmin(r, "membership.set", "project", body.ProjectID, map[string]any{
		"principal_id": body.PrincipalID, "role": body.Role,
	})
	writeJSON(w, 200, map[string]any{"ok": true})
}

func handleDeleteMembership(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	projectID := strings.TrimSpace(r.URL.Query().Get("project_id"))
	principalID := strings.TrimSpace(r.URL.Query().Get("principal_id"))
	if projectID == "" || principalID == "" {
		writeError(w, 400, "project_id and principal_id required")
		return
	}
	if err := iam.RemoveMembership(projectID, principalID); err != nil {
		writeError(w, 500, "Identity store unavailable.")
		return
	}
	auditAdmin(r, "membership.remove", "project", projectID, map[string]any{"principal_id": principalID})
	writeJSON(w, 200, map[string]any{"ok": true})
}

func handleListPrincipalCredentials(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	items, err := iam.ListProviderCredentials(strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		writeError(w, 500, "Credential store unavailable.")
		return
	}
	writeJSON(w, 200, map[string]any{"credentials": items})
}

func handlePrincipalCopilotLoginStart(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	principal, ok, err := iam.PrincipalByID(strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		writeError(w, 500, "Identity store unavailable.")
		return
	}
	if !ok {
		writeError(w, 404, "unknown principal")
		return
	}
	if principal.Kind != "human" {
		writeError(w, 400, "Copilot BYOC requires a human principal.")
		return
	}
	dc, err := copilotauth.StartDeviceFlow()
	if err != nil {
		writeError(w, 502, oauthErrorText(err.Error()))
		return
	}
	writeJSON(w, 200, map[string]any{
		"device_code": dc.DeviceCode, "user_code": dc.UserCode,
		"verification_uri": dc.VerificationURI, "interval": dc.Interval,
		"expires_in": dc.ExpiresIn,
	})
}

func handlePrincipalCopilotLoginPoll(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	principalID := strings.TrimSpace(r.PathValue("id"))
	principal, ok, err := iam.PrincipalByID(principalID)
	if err != nil {
		writeError(w, 500, "Identity store unavailable.")
		return
	}
	if !ok {
		writeError(w, 404, "unknown principal")
		return
	}
	if principal.Kind != "human" {
		writeError(w, 400, "Copilot BYOC requires a human principal.")
		return
	}
	var body struct {
		DeviceCode string `json:"device_code"`
	}
	if !decodeBody(r, &body) || strings.TrimSpace(body.DeviceCode) == "" {
		writeError(w, 400, "device_code required")
		return
	}
	result := copilotauth.PollDeviceFlowTokenOnce(body.DeviceCode)
	if result.Status == "authorized" {
		if _, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
			PrincipalID: principalID, ProviderID: "copilot", Kind: "github_oauth",
			Source: iam.ConnectionSourceAdmin, MakeDefault: true, AccessToken: result.AccessToken,
		}); err != nil {
			writeError(w, 500, oauthErrorText(err.Error()))
			return
		}
		providers.ForgetProviderForPrincipal("copilot", principalID)
		providers.ForgetCatalogForPrincipal("copilot", principalID)
		auditAdmin(r, "oauth_connection.connect", "principal", principalID, map[string]any{"provider": "copilot"})
	}
	response := safeOAuthPollResponse(result.Status, result.Error)
	writeJSON(w, 200, response)
}

func handlePrincipalCopilotRevoke(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	principalID := strings.TrimSpace(r.PathValue("id"))
	if err := iam.RevokeProviderCredential(principalID, "copilot"); err != nil {
		writeError(w, 404, oauthErrorText(err.Error()))
		return
	}
	providers.ForgetProviderForPrincipal("copilot", principalID)
	providers.ForgetCatalogForPrincipal("copilot", principalID)
	auditAdmin(r, "oauth_connection.revoke", "principal", principalID, map[string]any{"provider": "copilot"})
	writeJSON(w, 200, map[string]any{"ok": true})
}
