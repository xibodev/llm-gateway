package api

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/diagnostics"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
)

const maxOAuthDiagnosticChars = 300

func oauthErrorText(text string) string {
	return diagnostics.SanitizeTextLimit(text, maxOAuthDiagnosticChars)
}

func safeOAuthPollResponse(status, detail string) map[string]any {
	response := map[string]any{"status": status}
	if detail := oauthErrorText(detail); detail != "" {
		response["error"] = detail
	}
	return response
}

func oauthAdapterFor(providerRef string) (string, string, providers.ProviderAuthAdapter, error) {
	reference := strings.ToLower(strings.TrimSpace(providerRef))
	if reference == "" {
		return "", "", nil, fmt.Errorf("provider_id is required")
	}
	providerID := reference
	runtimeType := ""
	registryID := ""
	if providerConfig, ok := config.Get().Providers[reference]; ok {
		runtimeType = strings.ToLower(strings.TrimSpace(providerConfig.Type))
		registryID = providers.EffectiveRegistryID(
			reference, providerConfig.RegistryID, providerConfig.Type,
		)
	} else {
		registryID = providers.CanonicalRegistryID(reference)
		matches := make([]string, 0, 1)
		matchRegistryIDs := map[string]string{}
		settings := config.Get()
		for id, providerConfig := range settings.Providers {
			configuredRegistryID := providers.EffectiveRegistryID(
				id, providerConfig.RegistryID, providerConfig.Type,
			)
			if configuredRegistryID == registryID {
				matches = append(matches, id)
				matchRegistryIDs[id] = configuredRegistryID
			}
		}
		sort.Strings(matches)
		if len(matches) > 1 {
			return "", "", nil, fmt.Errorf(
				"multiple configured providers use registry %q; select a configured provider id",
				registryID,
			)
		}
		if len(matches) == 1 {
			providerID = matches[0]
			providerConfig := settings.Providers[providerID]
			runtimeType = strings.ToLower(strings.TrimSpace(providerConfig.Type))
			registryID = matchRegistryIDs[providerID]
		}
	}
	if runtimeType == "" && registryID == "github_copilot" {
		providerID, runtimeType, registryID = "copilot", "github_copilot", "github_copilot"
	}
	if runtimeType == "" && registryID == "openai_codex" {
		providerID, runtimeType, registryID = "codex", "openai_codex", "openai_codex"
	}
	if registryID == "" {
		switch runtimeType {
		case "github_copilot":
			registryID = "github_copilot"
		case "openai_codex":
			registryID = "openai_codex"
		}
	}
	entry, ok := providers.RegistryProviderByID(registryID)
	if !ok || entry.AuthAdapter == "" {
		return "", "", nil, fmt.Errorf("provider %q does not expose a supported official OAuth flow", providerRef)
	}
	if runtimeType != "" && !providers.RegistryRuntimeMatches(entry, runtimeType) {
		return "", "", nil, fmt.Errorf(
			"provider %q runtime %q does not match registry integration %q",
			providerID, runtimeType, entry.ID,
		)
	}
	adapter, err := providers.NewProviderAuthAdapter(entry.AuthAdapter, providerID)
	if err != nil {
		return "", "", nil, err
	}
	return providerID, adapter.CredentialKind(), adapter, nil
}

func ensureOAuthProviderConfig(providerID, providerRef string) {
	if _, exists := config.Get().Providers[providerID]; exists {
		return
	}
	reference := providers.CanonicalRegistryID(providerRef)
	if reference == "copilot" {
		reference = "github_copilot"
	}
	if reference == "codex" {
		reference = "openai_codex"
	}
	entry, found := providers.RegistryProvider(reference)
	if !found || entry.ClientOnly {
		return
	}
	config.Update(func(settings *config.Settings) {
		settings.Providers[providerID] = &config.ProviderConfig{
			Type: entry.RuntimeType, RegistryID: entry.ID, BaseURL: entry.DefaultBaseURL, Region: entry.DefaultRegion,
		}
	})
	persist()
}

type oauthFlowState struct {
	PrincipalID  string
	ProviderID   string
	StartedAt    int64
	ExpiresAt    int64
	NextPollAt   int64
	Interval     int
	PrivateState string
	Generation   uint64
}

const maxOAuthFlowsPerPrincipal = 5

var oauthFlows = struct {
	sync.Mutex
	values         map[string]oauthFlowState
	nextGeneration uint64
}{values: map[string]oauthFlowState{}}

var oauthFlowCleanupOnce sync.Once

func oauthFlowKey(principalID, providerID, deviceCode string) string {
	return principalID + "|" + providerID + "|" + deviceCode
}

func storeOAuthFlow(key string, flow oauthFlowState) {
	ensureOAuthFlowCleanup()
	now := time.Now().Unix()
	oauthFlows.Lock()
	pruneOAuthFlowsLocked(now)
	oauthFlows.nextGeneration++
	flow.Generation = oauthFlows.nextGeneration
	for {
		count := 0
		oldestKey := ""
		oldestAt := int64(0)
		oldestGeneration := uint64(0)
		for existingKey, existing := range oauthFlows.values {
			if existing.PrincipalID != flow.PrincipalID {
				continue
			}
			count++
			if oldestKey == "" || existing.StartedAt < oldestAt ||
				(existing.StartedAt == oldestAt && existing.Generation < oldestGeneration) {
				oldestKey = existingKey
				oldestAt = existing.StartedAt
				oldestGeneration = existing.Generation
			}
		}
		if count < maxOAuthFlowsPerPrincipal || oldestKey == "" {
			break
		}
		delete(oauthFlows.values, oldestKey)
	}
	oauthFlows.values[key] = flow
	oauthFlows.Unlock()
}

func ensureOAuthFlowCleanup() {
	oauthFlowCleanupOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(time.Minute)
			defer ticker.Stop()
			for now := range ticker.C {
				oauthFlows.Lock()
				pruneOAuthFlowsLocked(now.Unix())
				oauthFlows.Unlock()
			}
		}()
	})
}

func pruneOAuthFlowsLocked(now int64) {
	for key, flow := range oauthFlows.values {
		if flow.ExpiresAt > 0 && flow.ExpiresAt <= now {
			delete(oauthFlows.values, key)
		}
	}
}

func applyOAuthPollResult(
	key string, observed oauthFlowState, result providers.ProviderAuthPoll, now int64,
) bool {
	oauthFlows.Lock()
	defer oauthFlows.Unlock()
	current, ok := oauthFlows.values[key]
	if !ok || current.Generation != observed.Generation {
		return false
	}
	if current.ExpiresAt > 0 && current.ExpiresAt <= now {
		delete(oauthFlows.values, key)
		return false
	}
	switch result.Status {
	case "pending":
		current.NextPollAt = now + int64(current.Interval)
		oauthFlows.values[key] = current
	case "slow_down":
		current.Interval += 5
		current.NextPollAt = now + int64(current.Interval)
		oauthFlows.values[key] = current
	default:
		delete(oauthFlows.values, key)
	}
	return true
}

func oauthRegistryIDForRef(providerRef string) string {
	reference := strings.ToLower(strings.TrimSpace(providerRef))
	if providerConfig, ok := config.Get().Providers[reference]; ok {
		return providers.EffectiveRegistryID(
			reference, providerConfig.RegistryID, providerConfig.Type,
		)
	}
	return providers.CanonicalRegistryID(reference)
}

func configureOAuthClientID(providerRef, clientID string, allowUpdate bool) error {
	if oauthRegistryIDForRef(providerRef) != "openai_codex" {
		return nil
	}
	clientID = strings.TrimSpace(clientID)
	if clientID != "" {
		if !allowUpdate {
			return fmt.Errorf("Only an administrator can configure the Codex OAuth client ID.")
		}
		previous := config.Get().OpenAICodexClientID
		config.Update(func(settings *config.Settings) {
			settings.OpenAICodexClientID = clientID
		})
		if err := config.Save(); err != nil {
			config.Update(func(settings *config.Settings) {
				settings.OpenAICodexClientID = previous
			})
			return fmt.Errorf("Could not persist the Codex OAuth client ID.")
		}
	}
	if strings.TrimSpace(config.Get().OpenAICodexClientID) == "" {
		return fmt.Errorf("OpenAI Codex client ID is required. Enter it in the official sign-in dialog.")
	}
	return nil
}

func startOAuthFlow(
	principal iam.Principal, providerRef, clientID string, allowClientIDUpdate bool,
) (map[string]any, error) {
	if err := configureOAuthClientID(providerRef, clientID, allowClientIDUpdate); err != nil {
		return nil, err
	}
	providerID, _, adapter, err := oauthAdapterFor(providerRef)
	if err != nil {
		return nil, err
	}
	ensureOAuthProviderConfig(providerID, providerRef)
	deviceAdapter, ok := adapter.(providers.DeviceProviderAuthAdapter)
	if !ok {
		return nil, fmt.Errorf("provider %q does not support device authorization", providerRef)
	}
	start, err := deviceAdapter.StartDevice(context.Background())
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(start.DeviceCode) == "" || strings.TrimSpace(start.UserCode) == "" {
		return nil, fmt.Errorf("official OAuth device flow returned incomplete authorization data")
	}
	now := time.Now().Unix()
	expiresAt := now + int64(start.ExpiresIn)
	storeOAuthFlow(oauthFlowKey(principal.ID, providerID, start.DeviceCode), oauthFlowState{
		PrincipalID: principal.ID, ProviderID: providerID, StartedAt: now, ExpiresAt: expiresAt,
		NextPollAt: now + int64(start.Interval), Interval: start.Interval, PrivateState: start.PrivateState,
	})
	return map[string]any{
		"provider_id": providerID, "flow": "device_code", "device_code": start.DeviceCode,
		"user_code": start.UserCode, "verification_uri": start.VerificationURI,
		"interval": start.Interval, "expires_in": start.ExpiresIn, "expires_at": expiresAt,
	}, nil
}

func pollOAuthFlow(principal iam.Principal, providerRef, deviceCode, name, source string) map[string]any {
	providerID, kind, adapter, err := oauthAdapterFor(providerRef)
	if err != nil {
		return safeOAuthPollResponse("error", err.Error())
	}
	deviceCode = strings.TrimSpace(deviceCode)
	if deviceCode == "" {
		return safeOAuthPollResponse("error", "device_code is required")
	}
	now := time.Now().Unix()
	key := oauthFlowKey(principal.ID, providerID, deviceCode)
	oauthFlows.Lock()
	flow, tracked := oauthFlows.values[key]
	if tracked && flow.ExpiresAt > 0 && now >= flow.ExpiresAt {
		delete(oauthFlows.values, key)
		oauthFlows.Unlock()
		return safeOAuthPollResponse("expired", "Device authorization expired. Start again.")
	}
	if tracked && flow.NextPollAt > now {
		oauthFlows.Unlock()
		return safeOAuthPollResponse("slow_down", "Wait for the provider polling interval before retrying.")
	}
	if !tracked {
		oauthFlows.Unlock()
		return safeOAuthPollResponse("expired", "Device authorization is no longer active. Start again.")
	}
	privateState := flow.PrivateState
	oauthFlows.Unlock()

	deviceAdapter, ok := adapter.(providers.DeviceProviderAuthAdapter)
	if !ok {
		return safeOAuthPollResponse("error", "Provider does not support device authorization.")
	}
	result := providers.SafeProviderAuthPoll(deviceAdapter.PollDevice(context.Background(), deviceCode, privateState))
	if !applyOAuthPollResult(key, flow, result, time.Now().Unix()) {
		return safeOAuthPollResponse("expired", "Device authorization is no longer active. Start again.")
	}

	out := safeOAuthPollResponse(result.Status, result.Error)
	if result.Status != "authorized" {
		return out
	}
	connection, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
		PrincipalID: principal.ID, ProviderID: providerID, Name: name, Kind: kind, Source: source,
		MakeDefault: true, AccessToken: result.AccessToken, RefreshToken: result.RefreshToken,
		IDToken: result.IDToken, TokenType: result.TokenType, ExpiresAt: result.ExpiresAt, AccountID: result.AccountID, AccountLabel: result.AccountLabel, Status: "active",
	})
	if err != nil {
		return safeOAuthPollResponse("error", "Could not store the private OAuth connection.")
	}
	providers.ForgetProviderForPrincipal(providerID, principal.ID)
	providers.ForgetCatalogForPrincipal(providerID, principal.ID)
	out["connection"] = connection
	return out
}

func refreshOAuthFlow(principal iam.Principal, providerRef, name string) map[string]any {
	providerID, _, adapter, err := oauthAdapterFor(providerRef)
	if err != nil {
		return safeOAuthPollResponse("error", err.Error())
	}
	if guardedAdapter, ok := adapter.(providers.GuardedRefreshProviderAuthAdapter); ok {
		envelope, connection, err := guardedAdapter.RefreshConnection(
			context.Background(), principal.ID, providerID, name,
		)
		if err != nil {
			return safeOAuthPollResponse("error", "OAuth refresh failed.")
		}
		providers.ForgetProviderForPrincipal(providerID, principal.ID)
		providers.ForgetCatalogForPrincipal(providerID, principal.ID)
		return map[string]any{
			"status": "refreshed", "expires_at": envelope.ExpiresAt,
			"connection": connection,
		}
	}
	envelope, connection, ok, err := iam.OAuthProviderConnectionSecret(principal.ID, providerID, name)
	if err != nil {
		return safeOAuthPollResponse("error", "Could not load the private OAuth connection.")
	}
	if !ok {
		return safeOAuthPollResponse("missing", "No active OAuth connection exists.")
	}
	refreshAdapter, ok := adapter.(providers.RefreshableProviderAuthAdapter)
	if !ok {
		return safeOAuthPollResponse("unsupported", "Provider does not support OAuth refresh.")
	}
	refreshed, err := refreshAdapter.Refresh(context.Background(), envelope)
	if err != nil {
		return safeOAuthPollResponse("error", "OAuth refresh failed.")
	}
	if refreshed.AccessToken != "" {
		refreshToken := refreshed.RefreshToken
		if refreshToken == "" {
			refreshToken = envelope.RefreshToken
		}
		idToken := refreshed.IDToken
		if idToken == "" {
			idToken = envelope.IDToken
		}
		accountID := refreshed.AccountID
		if accountID == "" {
			accountID = envelope.AccountID
		}
		accountLabel := refreshed.AccountLabel
		if accountLabel == "" {
			accountLabel = envelope.AccountLabel
		}
		tokenType := refreshed.TokenType
		if tokenType == "" {
			tokenType = envelope.TokenType
		}
		expiresAt := refreshed.ExpiresAt
		if expiresAt == 0 {
			expiresAt = envelope.ExpiresAt
		}
		if _, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
			PrincipalID: principal.ID, ProviderID: providerID, Name: connection.Name, Kind: connection.Kind,
			Source: connection.Source, MakeDefault: connection.IsDefault, AccessToken: refreshed.AccessToken,
			RefreshToken: refreshToken, IDToken: idToken, TokenType: tokenType, ExpiresAt: expiresAt,
			AccountID: accountID, AccountLabel: accountLabel, Status: "active",
		}); err != nil {
			return safeOAuthPollResponse("error", "OAuth refresh could not be stored.")
		}
	}
	providers.ForgetProviderForPrincipal(providerID, principal.ID)
	providers.ForgetCatalogForPrincipal(providerID, principal.ID)
	return map[string]any{"status": refreshed.Status, "expires_at": refreshed.ExpiresAt}
}

func revokeOAuthFlow(principal iam.Principal, providerRef, name string) map[string]any {
	providerID, _, adapter, err := oauthAdapterFor(providerRef)
	if err != nil {
		return safeOAuthPollResponse("error", err.Error())
	}
	envelope, connection, ok, err := iam.OAuthProviderConnectionSecret(principal.ID, providerID, name)
	if err != nil || !ok {
		return safeOAuthPollResponse("missing", "No active OAuth connection exists.")
	}
	upstream := "revoked"
	if revokeAdapter, ok := adapter.(providers.RevocableProviderAuthAdapter); ok {
		if err := revokeAdapter.Revoke(context.Background(), envelope); err != nil {
			upstream = "best_effort_failed"
		}
	} else {
		upstream = "not_supported"
	}
	revoked, err := iam.RevokeOAuthProviderConnectionIfCurrent(connection, envelope)
	if err != nil {
		return safeOAuthPollResponse("error", "Local OAuth revocation failed.")
	}
	if !revoked {
		return safeOAuthPollResponse("changed", "OAuth connection changed while revocation was in progress.")
	}
	providers.ForgetProviderForPrincipal(providerID, principal.ID)
	providers.ForgetCatalogForPrincipal(providerID, principal.ID)
	return map[string]any{"status": "revoked", "upstream_revocation": upstream}
}

func oauthPrincipalByID(principalID string) (iam.Principal, error) {
	principal, found, err := iam.PrincipalByID(strings.TrimSpace(principalID))
	if err != nil {
		return iam.Principal{}, err
	}
	if !found {
		return iam.Principal{}, fmt.Errorf("unknown principal")
	}
	if principal.Kind != "human" {
		return iam.Principal{}, fmt.Errorf("OAuth connections require a human principal")
	}
	return principal, nil
}

func oauthPollInput(r *http.Request) (string, string, bool) {
	var body struct {
		DeviceCode     string `json:"device_code"`
		ConnectionName string `json:"connection_name"`
	}
	if !decodeBody(r, &body) || strings.TrimSpace(body.DeviceCode) == "" {
		return "", "", false
	}
	return body.DeviceCode, body.ConnectionName, true
}

func oauthStartClientID(r *http.Request) string {
	var body struct {
		ClientID string `json:"client_id"`
	}
	if !decodeBody(r, &body) {
		return ""
	}
	return strings.TrimSpace(body.ClientID)
}

func handleUserOAuthStart(w http.ResponseWriter, r *http.Request) {
	principal, ok := requireSSOUser(w, r)
	if !ok {
		return
	}
	response, err := startOAuthFlow(
		principal, r.PathValue("provider_id"), "", false,
	)
	if err != nil {
		writeError(w, 400, oauthErrorText(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func handleUserOAuthPoll(w http.ResponseWriter, r *http.Request) {
	principal, ok := requireSSOUser(w, r)
	if !ok {
		return
	}
	deviceCode, name, valid := oauthPollInput(r)
	if !valid {
		writeError(w, 400, "device_code required")
		return
	}
	response := pollOAuthFlow(principal, r.PathValue("provider_id"), deviceCode, name, iam.ConnectionSourceUser)
	if response["status"] == "authorized" {
		_ = iam.RecordAudit(iam.AuditEvent{ActorPrincipalID: principal.ID, Action: "oauth_connection.connect", TargetType: "principal", TargetID: principal.ID, Result: "success", Detail: map[string]any{"provider": r.PathValue("provider_id"), "source": "self-service"}})
	}
	writeJSON(w, http.StatusOK, response)
}

func handleUserOAuthRefresh(w http.ResponseWriter, r *http.Request) {
	principal, ok := requireSSOUser(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, refreshOAuthFlow(principal, r.PathValue("provider_id"), r.URL.Query().Get("connection_name")))
}

func handlePrincipalOAuthStart(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	principal, err := oauthPrincipalByID(r.PathValue("id"))
	if err != nil {
		writeError(w, 400, oauthErrorText(err.Error()))
		return
	}
	response, err := startOAuthFlow(
		principal, r.PathValue("provider_id"), oauthStartClientID(r), true,
	)
	if err != nil {
		writeError(w, 400, oauthErrorText(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func handlePrincipalOAuthPoll(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	principal, err := oauthPrincipalByID(r.PathValue("id"))
	if err != nil {
		writeError(w, 400, oauthErrorText(err.Error()))
		return
	}
	deviceCode, name, valid := oauthPollInput(r)
	if !valid {
		writeError(w, 400, "device_code required")
		return
	}
	response := pollOAuthFlow(principal, r.PathValue("provider_id"), deviceCode, name, iam.ConnectionSourceAdmin)
	if response["status"] == "authorized" {
		auditAdmin(r, "oauth_connection.connect", "principal", principal.ID, map[string]any{"provider": r.PathValue("provider_id")})
	}
	writeJSON(w, http.StatusOK, response)
}

func handleUserOAuthRevoke(w http.ResponseWriter, r *http.Request) {
	principal, ok := requireSSOUser(w, r)
	if !ok {
		return
	}
	response := revokeOAuthFlow(principal, r.PathValue("provider_id"), r.URL.Query().Get("connection_name"))
	if response["status"] == "revoked" {
		_ = iam.RecordAudit(iam.AuditEvent{ActorPrincipalID: principal.ID, Action: "oauth_connection.revoke", TargetType: "principal", TargetID: principal.ID, Result: "success", Detail: map[string]any{"provider": r.PathValue("provider_id"), "source": "self-service"}})
	}
	writeJSON(w, http.StatusOK, response)
}

func handlePrincipalOAuthRefresh(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	principal, err := oauthPrincipalByID(r.PathValue("id"))
	if err != nil {
		writeError(w, 400, oauthErrorText(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, refreshOAuthFlow(principal, r.PathValue("provider_id"), r.URL.Query().Get("connection_name")))
}

func handlePrincipalOAuthRevoke(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	principal, err := oauthPrincipalByID(r.PathValue("id"))
	if err != nil {
		writeError(w, 400, oauthErrorText(err.Error()))
		return
	}
	response := revokeOAuthFlow(principal, r.PathValue("provider_id"), r.URL.Query().Get("connection_name"))
	if response["status"] == "revoked" {
		auditAdmin(r, "oauth_connection.revoke", "principal", principal.ID, map[string]any{"provider": r.PathValue("provider_id")})
	}
	writeJSON(w, http.StatusOK, response)
}
