package api

import (
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"llmgw/internal/config"
	"llmgw/internal/copilotauth"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
)

func handleUserMe(w http.ResponseWriter, r *http.Request) {
	principal, ok := requireSSOUser(w, r)
	if !ok {
		return
	}
	memberships, err := iam.ListPrincipalMemberships(principal.ID)
	if err != nil {
		writeError(w, 500, "Identity store unavailable.")
		return
	}
	projects := []iam.Project{}
	for _, membership := range memberships {
		if project, found, err := iam.ProjectByID(membership.ProjectID); err != nil {
			writeError(w, 500, "Identity store unavailable.")
			return
		} else if found {
			projects = append(projects, project)
		}
	}
	keys, err := iam.ListPrincipalAPIKeys(principal.ID)
	if err != nil {
		writeError(w, 500, "Identity store unavailable.")
		return
	}
	credentials, err := iam.ListProviderCredentials(principal.ID)
	if err != nil {
		writeError(w, 500, "Credential store unavailable.")
		return
	}
	connections, err := iam.ListProviderConnections(principal.ID, "")
	if err != nil {
		writeError(w, 500, "Credential store unavailable.")
		return
	}
	providerStatuses, err := providerStatusSnapshots(
		config.Get(), config.LoadSecrets(), connections, principal.ID,
	)
	if err != nil {
		writeError(w, 500, "Provider status is unavailable.")
		return
	}

	connectionProviders := []map[string]any{}
	for providerID, providerConfig := range config.Get().Providers {
		if strings.EqualFold(providerConfig.Type, "github_copilot") {
			continue
		}
		option := map[string]any{
			"id": providerID, "type": providerConfig.Type,
			"registry_id": providerConfig.RegistryID, "label": providerID,
		}
		if entry, ok := providers.RegistryProvider(providerConfig.RegistryID); ok {
			if !contains(entry.AuthMethods, "api_key") {
				continue
			}
			option["label"] = entry.Label
		}
		connectionProviders = append(connectionProviders, option)
	}
	sort.Slice(connectionProviders, func(i, j int) bool {
		return connectionProviders[i]["id"].(string) < connectionProviders[j]["id"].(string)
	})
	writeJSON(w, 200, map[string]any{
		"principal": principal, "memberships": memberships, "projects": projects,
		"keys": keys, "provider_credentials": credentials,
		"provider_connections": connections, "connection_providers": connectionProviders,
		"provider_registry": providers.ProviderRegistry(), "provider_statuses": providerStatuses,
	})
}

func handleUserUsage(w http.ResponseWriter, r *http.Request) {
	principal, ok := requireSSOUser(w, r)
	if !ok {
		return
	}
	filter, err := usageFilterFromRequest(r, principal.ID)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	stats, err := iam.PrincipalUsageStats(filter.From, principal.ID)
	if err != nil {
		writeError(w, 500, "Usage store unavailable.")
		return
	}
	series, err := iam.UsageTimeSeries(filter)
	if err != nil {
		writeError(w, 400, "Usage series is unavailable: "+err.Error())
		return
	}
	stats["series"] = series
	quotaAdvisories, err := providerQuotaAdvisories(principal.ID)
	if err != nil {
		writeError(w, 500, "Provider quota store unavailable.")
		return
	}
	stats["quota_advisories"] = quotaAdvisories
	writeJSON(w, 200, stats)
}

func handleUserCreateKey(w http.ResponseWriter, r *http.Request) {
	principal, ok := requireSSOUser(w, r)
	if !ok {
		return
	}
	var body keyBody
	if !decodeBody(r, &body) {
		writeError(w, 400, "invalid body")
		return
	}
	projectID := strings.TrimSpace(body.ProjectID)
	role, member, err := iam.MembershipRole(projectID, principal.ID)
	if err != nil {
		writeError(w, 500, "Identity store unavailable.")
		return
	}
	if !member || role == "viewer" {
		writeError(w, 403, "Project membership with key-management permission required.")
		return
	}
	issued, err := iam.IssueKey(iam.KeyCreate{
		ProjectID: projectID, PrincipalID: principal.ID, Name: body.Name,
		ExpiresAt: body.ExpiresAt, Policy: keyPolicyFromBody(body),
	})
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	_ = iam.RecordAudit(iam.AuditEvent{
		ActorPrincipalID: principal.ID, Action: "api_key.create",
		TargetType: "api_key", TargetID: issued.ID, Result: "success",
		Detail: map[string]any{"project_id": projectID, "source": "self-service"},
	})
	writeJSON(w, 200, map[string]any{"token": issued.Token, "key": issued.APIKey})
}

func handleUserRevealKey(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	principal, ok := requireSSOUser(w, r)
	if !ok {
		return
	}
	keyID := strings.TrimSpace(r.PathValue("id"))
	key, found, err := iam.APIKeyByID(keyID)
	if err != nil {
		writeError(w, 500, "Identity store unavailable.")
		return
	}
	if !found || key.PrincipalID != principal.ID {
		writeError(w, 404, "unknown key")
		return
	}
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
	_ = iam.RecordAudit(iam.AuditEvent{
		ActorPrincipalID: principal.ID, Action: "api_key.reveal",
		TargetType: "api_key", TargetID: key.ID, Result: "success",
		Detail: map[string]any{"source": "self-service"},
	})
	writeJSON(w, 200, map[string]any{"token": token})
}

func handleUserRevokeKey(w http.ResponseWriter, r *http.Request) {
	principal, ok := requireSSOUser(w, r)
	if !ok {
		return
	}
	key, found, err := iam.APIKeyByID(strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		writeError(w, 500, "Identity store unavailable.")
		return
	}
	if !found || key.PrincipalID != principal.ID {
		writeError(w, 404, "unknown key")
		return
	}
	if err := iam.RevokeAPIKey(key.ID); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	_ = iam.RecordAudit(iam.AuditEvent{
		ActorPrincipalID: principal.ID, Action: "api_key.revoke",
		TargetType: "api_key", TargetID: key.ID, Result: "success",
		Detail: map[string]any{"source": "self-service"},
	})
	writeJSON(w, 200, map[string]any{"ok": true})
}

func handleUserUpdateKey(w http.ResponseWriter, r *http.Request) {
	principal, ok := requireSSOUser(w, r)
	if !ok {
		return
	}
	var body keyUpdateBody
	if !decodeBody(r, &body) {
		writeError(w, 400, "invalid body")
		return
	}
	key, found, err := iam.APIKeyByID(strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		writeError(w, 500, "Identity store unavailable.")
		return
	}
	if !found || key.PrincipalID != principal.ID {
		writeError(w, 404, "unknown key")
		return
	}
	role, member, err := iam.MembershipRole(key.ProjectID, principal.ID)
	if err != nil {
		writeError(w, 500, "Membership store unavailable.")
		return
	}
	if !member || role == "viewer" {
		writeError(w, 403, "Project membership with key-management permission required.")
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
	changed := false
	if body.AllowedModels != nil {
		policy.AllowedModels = *body.AllowedModels
		changed = true
	}
	if body.AllowedProviders != nil {
		policy.AllowedProviders = *body.AllowedProviders
		changed = true
	}
	applyInt := func(value *int, target *int) {
		if value != nil {
			*target = *value
			changed = true
		}
	}
	applyInt64 := func(value *int64, target *int64) {
		if value != nil {
			*target = *value
			changed = true
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
	if changed {
		update.Policy = &policy
	}
	if err := iam.UpdateAPIKey(key.ID, update); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	_ = iam.RecordAudit(iam.AuditEvent{ActorPrincipalID: principal.ID, Action: "api_key.update", TargetType: "api_key", TargetID: key.ID, Result: "success", Detail: map[string]any{"source": "self-service"}})
	writeJSON(w, 200, map[string]any{"ok": true})
}

func handleUserAudit(w http.ResponseWriter, r *http.Request) {
	principal, ok := requireSSOUser(w, r)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	events, err := iam.ListPrincipalAudit(principal.ID, limit)
	if err != nil {
		writeError(w, 500, "Audit store unavailable.")
		return
	}
	writeJSON(w, 200, map[string]any{"events": events})
}

func handleUserCopilotLoginStart(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireSSOUser(w, r); !ok {
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

func handleUserCopilotLoginPoll(w http.ResponseWriter, r *http.Request) {
	principal, ok := requireSSOUser(w, r)
	if !ok {
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
			PrincipalID: principal.ID, ProviderID: "copilot", Kind: "github_oauth",
			Source: iam.ConnectionSourceUser, MakeDefault: true, AccessToken: result.AccessToken,
		}); err != nil {
			writeError(w, 500, oauthErrorText(err.Error()))
			return
		}
		providers.ForgetProviderForPrincipal("copilot", principal.ID)
		providers.ForgetCatalogForPrincipal("copilot", principal.ID)
		_ = iam.RecordAudit(iam.AuditEvent{
			ActorPrincipalID: principal.ID, Action: "oauth_connection.connect",
			TargetType: "principal", TargetID: principal.ID, Result: "success",
			Detail: map[string]any{"provider": "copilot", "source": "self-service"},
		})
	}
	response := safeOAuthPollResponse(result.Status, result.Error)
	writeJSON(w, 200, response)
}

func handleUserCopilotRevoke(w http.ResponseWriter, r *http.Request) {
	principal, ok := requireSSOUser(w, r)
	if !ok {
		return
	}
	if err := iam.RevokeProviderCredential(principal.ID, "copilot"); err != nil {
		writeError(w, 404, oauthErrorText(err.Error()))
		return
	}
	providers.ForgetProviderForPrincipal("copilot", principal.ID)
	providers.ForgetCatalogForPrincipal("copilot", principal.ID)
	_ = iam.RecordAudit(iam.AuditEvent{
		ActorPrincipalID: principal.ID, Action: "oauth_connection.revoke",
		TargetType: "principal", TargetID: principal.ID, Result: "success",
		Detail: map[string]any{"provider": "copilot", "source": "self-service"},
	})
	writeJSON(w, 200, map[string]any{"ok": true})
}

func keyPolicyFromBody(body keyBody) iam.KeyPolicy {
	return iam.KeyPolicy{
		AllowedModels: body.AllowedModels, AllowedProviders: body.AllowedProviders,
		RPM: body.RPM, DailyRequests: body.DailyRequests,
		MonthlyRequests:     body.MonthlyRequests,
		DailyInputTokens:    body.DailyInputTokens,
		DailyOutputTokens:   body.DailyOutputTokens,
		MonthlyTotalTokens:  body.MonthlyTotalTokens,
		DailyCostMicroUSD:   body.DailyCostMicroUSD,
		MonthlyCostMicroUSD: body.MonthlyCostMicroUSD,
		DailyCreditsMilli:   body.DailyCreditsMilli,
		MonthlyCreditsMilli: body.MonthlyCreditsMilli,
	}
}
