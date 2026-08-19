package api

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
	"llmgw/internal/router"
)

type configuredProviderSnapshot struct {
	id                 string
	registryID         string
	status             string
	catalogState       string
	catalogRefresh     string
	modelCount         int
	connectionCount    int
	disabled           bool
	lastCheck          *iam.ProviderCheck
	lastVerify         *iam.ProviderCheck
	configurationIssue string
}

// providerStatus computes the honest status ladder for one configured
// provider. Each rung states only what has actually been established:
//
//	needs_credentials -> configured -> catalog_synced -> verified
//
// with check_failed cutting in whenever the most recent recorded check
// failed. "configured" deliberately replaces the old "ready": a stored
// credential proves nothing about the upstream until a check has run.
func providerStatus(credentialPresent bool, modelCount int, checks []iam.ProviderCheck) (string, *iam.ProviderCheck, *iam.ProviderCheck) {
	var lastCheck *iam.ProviderCheck
	var lastVerify *iam.ProviderCheck
	for index := range checks {
		check := &checks[index]
		if lastCheck == nil || check.CheckedAt >= lastCheck.CheckedAt {
			lastCheck = check
		}
		if check.Operation == iam.CheckVerify && check.Success {
			if lastVerify == nil || check.CheckedAt >= lastVerify.CheckedAt {
				lastVerify = check
			}
		}
	}
	if !credentialPresent {
		return "needs_credentials", lastCheck, lastVerify
	}
	if lastCheck != nil && !lastCheck.Success {
		return "check_failed", lastCheck, lastVerify
	}
	if lastVerify != nil {
		return "verified", lastCheck, lastVerify
	}
	if modelCount > 0 {
		return "catalog_synced", lastCheck, lastVerify
	}
	return "configured", lastCheck, lastVerify
}

// providerStatusSnapshots joins the curated registry with safe runtime facts.
// It intentionally exposes no credential payloads or token-derived identifiers.
func providerStatusSnapshots(
	settings *config.Settings, secrets map[string]string, connections []iam.ProviderConnection,
	checkScope string,
) ([]map[string]any, error) {
	connectionCounts := map[string]int{}
	for _, connection := range connections {
		if connection.PrincipalKind == "human" && connection.Status == "active" {
			connectionCounts[connection.ProviderID]++
		}
	}
	allChecks, err := iam.LastProviderChecks(checkScope)
	if err != nil {
		return nil, err
	}

	configured := make([]configuredProviderSnapshot, 0, len(settings.Providers))
	byRegistry := map[string][]configuredProviderSnapshot{}
	for providerID, providerConfig := range settings.Providers {
		var models []providers.ModelInfo
		var refreshed time.Time
		if checkScope == "" {
			models, refreshed = providers.CatalogSnapshot(providerID)
		} else {
			models, refreshed = providers.CatalogCachedForPrincipal(
				providerID,
				&config.Principal{PrincipalID: checkScope, PrincipalKind: "human"},
			)
		}
		systemConnection, err := iam.SystemProviderConnectionExists(providerID)
		if err != nil {
			return nil, err
		}
		credentialPresent := strings.TrimSpace(secrets[providerID]) != "" ||
			strings.TrimSpace(providerConfig.APIKey) != "" || systemConnection ||
			connectionCounts[providerID] > 0
		// Credential-free runtimes are configured by definition once present:
		// local engines and services whose access token is baked in.
		switch strings.ToLower(strings.TrimSpace(providerConfig.Type)) {
		case "ollama", "echo", "edge_tts":
			credentialPresent = true
		}
		status, lastCheck, lastVerify := providerStatus(credentialPresent, len(models), allChecks[providerID])
		configurationIssue := providers.ProviderConfigurationIssue(providerID)
		if configurationIssue != "" {
			status = "misconfigured"
			models = nil
			refreshed = time.Time{}
		}
		if providerConfig.Disabled {
			status = "disabled"
		}
		catalogState := "unknown"
		refreshedAt := ""
		if !refreshed.IsZero() {
			refreshedAt = refreshed.UTC().Format(time.RFC3339)
			if time.Since(refreshed) > time.Hour {
				catalogState = "stale"
			} else {
				catalogState = "fresh"
			}
		}
		registryID := providers.EffectiveRegistryID(
			providerID, providerConfig.RegistryID, providerConfig.Type,
		)
		snapshot := configuredProviderSnapshot{
			id: providerID, registryID: registryID, status: status, catalogState: catalogState,
			catalogRefresh: refreshedAt, modelCount: len(models), connectionCount: connectionCounts[providerID],
			disabled: providerConfig.Disabled, lastCheck: lastCheck, lastVerify: lastVerify,
			configurationIssue: configurationIssue,
		}
		configured = append(configured, snapshot)
		if registryID != "" {
			byRegistry[registryID] = append(byRegistry[registryID], snapshot)
		}
	}

	snapshots := make([]map[string]any, 0, len(configured)+len(providers.ProviderRegistry()))
	knownRegistry := map[string]bool{}
	for _, entry := range providers.ProviderRegistry() {
		knownRegistry[entry.ID] = true
		matches := byRegistry[entry.ID]
		status := "not_configured"
		if entry.ClientOnly {
			status = "client_setup"
		} else if entry.Availability != providers.ProviderAvailable {
			status = "unavailable"
		} else if len(matches) > 0 {
			// A tile aggregates instances; it is not one. Adopting matches[0]'s
			// status let a healthy first instance mask a broken second, so the
			// tile reports only that it is configured and the console renders
			// the composition from instance_status_counts.
			status = "configured"
		}
		modelCount := 0
		connectionCount := 0
		catalogState := "unknown"
		catalogRefreshed := ""
		ids := make([]string, 0, len(matches))
		disabledIDs := make([]string, 0)
		var lastCheck *iam.ProviderCheck
		var lastVerify *iam.ProviderCheck
		configurationIssues := make([]string, 0)
		instances := make([]map[string]any, 0, len(matches))
		instanceStatusCounts := map[string]int{}
		for _, match := range matches {
			ids = append(ids, match.id)
			if match.disabled {
				disabledIDs = append(disabledIDs, match.id)
			}
			modelCount += match.modelCount
			connectionCount += match.connectionCount
			if match.catalogState == "fresh" || catalogState == "unknown" {
				catalogState = match.catalogState
				catalogRefreshed = match.catalogRefresh
			}
			if match.lastCheck != nil && (lastCheck == nil || match.lastCheck.CheckedAt >= lastCheck.CheckedAt) {
				lastCheck = match.lastCheck
			}
			if match.lastVerify != nil && (lastVerify == nil || match.lastVerify.CheckedAt >= lastVerify.CheckedAt) {
				lastVerify = match.lastVerify
			}
			if match.configurationIssue != "" {
				configurationIssues = append(configurationIssues, match.configurationIssue)
			}
			instanceStatusCounts[match.status]++
			instances = append(instances, providerSnapshotRow(map[string]any{
				"id": match.id, "status": match.status,
				"model_count": match.modelCount, "connection_count": match.connectionCount,
				"catalog_state": match.catalogState, "catalog_refreshed": match.catalogRefresh,
				"disabled": match.disabled, "configuration_issue": match.configurationIssue,
			}, match.lastCheck, match.lastVerify))
		}
		snapshots = append(snapshots, providerSnapshotRow(map[string]any{
			"id": entry.ID, "label": entry.Label, "description": entry.Description,
			"protocol": entry.Protocol, "availability": entry.Availability,
			"auth_methods": entry.AuthMethods, "connection_scope": entry.ConnectionScope,
			"client_only": entry.ClientOnly, "configured": len(matches) > 0,
			"configured_provider_ids": ids, "disabled_provider_ids": disabledIDs, "status": status,
			"connection_count": connectionCount, "model_count": modelCount,
			"catalog_state": catalogState, "catalog_refreshed": catalogRefreshed,
			"configuration_issue": strings.Join(configurationIssues, " "),
			"instances": instances, "instance_status_counts": instanceStatusCounts,
		}, lastCheck, lastVerify))
	}
	for _, snapshot := range configured {
		if knownRegistry[snapshot.registryID] {
			continue
		}
		disabledIDs := []string{}
		if snapshot.disabled {
			disabledIDs = append(disabledIDs, snapshot.id)
		}
		snapshots = append(snapshots, providerSnapshotRow(map[string]any{
			"id": snapshot.id, "label": snapshot.id, "description": "Custom configured provider.",
			"protocol": "gateway", "availability": providers.ProviderAvailable,
			"auth_methods": []string{}, "configured": true, "status": snapshot.status,
			"disabled_provider_ids": disabledIDs,
			"connection_count":      snapshot.connectionCount, "model_count": snapshot.modelCount,
			"catalog_state": snapshot.catalogState, "catalog_refreshed": snapshot.catalogRefresh,
			"configuration_issue": snapshot.configurationIssue,
		}, snapshot.lastCheck, snapshot.lastVerify))
	}
	sort.Slice(snapshots, func(i, j int) bool {
		return snapshots[i]["label"].(string) < snapshots[j]["label"].(string)
	})
	return snapshots, nil
}

func providerSnapshotRow(row map[string]any, lastCheck, lastVerify *iam.ProviderCheck) map[string]any {
	if lastCheck != nil {
		row["last_check_operation"] = lastCheck.Operation
		row["last_check_success"] = lastCheck.Success
		row["last_check_detail"] = lastCheck.Detail
		row["last_checked_at"] = time.Unix(lastCheck.CheckedAt, 0).UTC().Format(time.RFC3339)
	}
	if lastVerify != nil {
		row["last_verified_at"] = time.Unix(lastVerify.CheckedAt, 0).UTC().Format(time.RFC3339)
		row["verified_model"] = lastVerify.Model
	}
	return row
}

// checkOperationFor maps the historical HTTP route names onto the canonical
// stored operations.
func checkOperationFor(operation string) string {
	switch operation {
	case "test":
		return iam.CheckReachability
	case "refresh":
		return iam.CheckCatalogSync
	case "repair":
		return iam.CheckCacheReset
	default:
		return operation
	}
}

// runProviderProbe performs an explicit catalog-based reachability check. It is
// deliberately bounded to existing provider behavior and returns a clear result
// even when an upstream does not expose a usable model catalog. It never runs
// an inference request — that is what the verify operation is for.
func runProviderProbe(providerID, operation string, principal *config.Principal) map[string]any {
	started := time.Now()
	checkScope := providerCheckScope(principal)
	checkGeneration, checkGenerationErr := iam.ProviderCheckGeneration(
		providerID, checkScope,
	)
	if _, configured := config.Get().Providers[providerID]; !configured {
		return map[string]any{
			"provider_id": providerID, "operation": operation, "success": false,
			"status": "failed", "details": "Provider was removed before the check started.",
			"failure_code": "provider_removed", "model_count": 0,
			"sample": []string{}, "checked_at": time.Now().UTC().Format(time.RFC3339),
			"latency_ms": time.Since(started).Milliseconds(),
		}
	}
	initialObservation := activeProviderObservation(principal, providerID)
	if operation == "repair" {
		if principal != nil && principal.PrincipalID != "" {
			providers.ForgetProviderForPrincipal(providerID, principal.PrincipalID)
			providers.ForgetCatalogForPrincipal(providerID, principal.PrincipalID)
		} else {
			providers.ForgetProvider(providerID)
			providers.ForgetCatalog(providerID)
		}
	}
	rows, observation, catalogErr := providers.RefreshCatalogForPrincipalWithError(
		providerID, principal,
	)
	checkGeneration = refreshedProviderCheckGeneration(
		providerID, checkScope, checkGeneration,
		initialObservation, observation,
	)
	sample := make([]string, 0, 8)
	for _, row := range rows {
		if row.ID != "" && len(sample) < 8 {
			sample = append(sample, row.ID)
		}
	}
	success := catalogErr == nil && len(rows) > 0
	details := "Catalog listing succeeded and the provider returned models. This confirms endpoint reachability and credential acceptance for the catalog API only — run a test completion to verify inference."
	if cfg := config.Get().Providers[providerID]; cfg != nil &&
		providers.EffectiveRegistryID(providerID, cfg.RegistryID, cfg.Type) == "vertex_ai" &&
		catalogErr == nil && len(rows) > 0 {
		details = "The configured Vertex project/location unlocked the curated publisher-model catalog. Vertex has no equivalent public catalog endpoint, so this does not verify the credential or model access — run a test completion."
	}
	failureCode := ""
	if catalogErr != nil {
		failureCode, details, _ = providers.CatalogFailure(catalogErr)
	} else if !success {
		details = "No models were returned. Verify the endpoint, supported credential, and upstream catalog access before retrying."
		failureCode = "catalog_empty"
	}
	if observation != nil {
		if catalogErr == nil {
			_, _ = iam.RecordProviderAccountSuccessIfCurrent(*observation, time.Now())
		} else if catalogFailureAffectsAccount(failureCode) {
			_, _ = iam.RecordProviderAccountFailureIfCurrent(
				*observation, failureCode, time.Now(),
			)
		}
	}
	latency := time.Since(started).Milliseconds()
	check := iam.ProviderCheck{
		ProviderID: providerID, Operation: checkOperationFor(operation),
		ScopeKey: checkScope, Generation: checkGeneration, Success: success,
		Detail: providerCheckDetail(success, failureCode), LatencyMS: latency,
	}
	applyProviderCheckObservation(&check, observation)
	if checkGenerationErr == nil {
		_ = iam.RecordProviderCheck(check)
	}
	result := map[string]any{
		"provider_id": providerID, "operation": operation, "success": success,
		"status":  map[bool]string{true: "passed", false: "failed"}[success],
		"details": details, "failure_code": failureCode, "model_count": len(rows),
		"sample": sample, "checked_at": time.Now().UTC().Format(time.RFC3339),
		"latency_ms": latency,
	}
	if refreshed := providers.CatalogRefreshedAtForPrincipal(providerID, principal); !refreshed.IsZero() {
		result["catalog_refreshed"] = refreshed.UTC().Format(time.RFC3339)
	}
	return result
}

func providerCheckScope(principal *config.Principal) string {
	if principal == nil || principal.PrincipalKind != "human" {
		return ""
	}
	return strings.TrimSpace(principal.PrincipalID)
}

func activeProviderObservation(
	principal *config.Principal, providerID string,
) *iam.ProviderAccountObservation {
	if principal == nil || principal.PrincipalKind != "human" {
		return nil
	}
	observation, found, err := iam.ActiveProviderAccountObservation(
		principal.PrincipalID, providerID,
	)
	if err != nil || !found {
		return nil
	}
	return &observation
}

func refreshedProviderCheckGeneration(
	providerID, scopeKey string,
	generation int64,
	before, used *iam.ProviderAccountObservation,
) int64 {
	if before == nil || used == nil ||
		before.ConnectionID != used.ConnectionID ||
		used.CredentialRevision <= before.CredentialRevision {
		return generation
	}
	current, found, err := iam.ActiveProviderAccountObservation(scopeKey, providerID)
	if err != nil || !found || current != *used {
		return generation
	}
	refreshed, err := iam.ProviderCheckGeneration(providerID, scopeKey)
	if err != nil || refreshed != generation+1 {
		return generation
	}
	return refreshed
}

func applyProviderCheckObservation(
	check *iam.ProviderCheck, observation *iam.ProviderAccountObservation,
) {
	if check == nil || observation == nil || observation.CredentialRevision <= 0 {
		return
	}
	check.ConnectionID = observation.ConnectionID
	check.CredentialRevision = observation.CredentialRevision
}

func catalogFailureAffectsAccount(code string) bool {
	switch code {
	case "catalog_authentication_failed", "catalog_refresh_failed",
		"catalog_transport_error", "catalog_http_error",
		"catalog_invalid_json", "catalog_invalid_shape":
		return true
	}
	return false
}

func providerCheckDetail(success bool, failureCode string) string {
	if success {
		return "Provider check passed."
	}
	switch failureCode {
	case "catalog_empty":
		return "Provider catalog returned no models."
	case "model_unavailable":
		return "No model was available for verification."
	case "verification_failed":
		return "Provider inference verification failed."
	case "provider_unavailable":
		return "Provider configuration prevented verification."
	default:
		return "Provider check failed (" + strings.TrimSpace(failureCode) + ")."
	}
}

// runProviderVerify executes a real, minimal inference request against the
// provider — the only operation that proves end-to-end that requests work.
func runProviderVerify(providerID, model string, principal *config.Principal) map[string]any {
	started := time.Now()
	checkScope := providerCheckScope(principal)
	checkGeneration, checkGenerationErr := iam.ProviderCheckGeneration(
		providerID, checkScope,
	)
	if _, configured := config.Get().Providers[providerID]; !configured {
		return map[string]any{
			"provider_id": providerID, "operation": "verify", "success": false,
			"status": "failed", "details": "Provider was removed before verification started.",
			"failure_code": "provider_removed", "model": model,
			"checked_at": time.Now().UTC().Format(time.RFC3339),
			"latency_ms": time.Since(started).Milliseconds(),
		}
	}
	var verificationObservation *iam.ProviderAccountObservation
	verificationObservation = activeProviderObservation(principal, providerID)
	initialVerificationObservation := verificationObservation
	fail := func(detail, failureCode string) map[string]any {
		latency := time.Since(started).Milliseconds()
		if strings.TrimSpace(failureCode) == "" {
			failureCode = "verification_failed"
		}
		check := iam.ProviderCheck{
			ProviderID: providerID, Operation: iam.CheckVerify,
			ScopeKey: checkScope, Generation: checkGeneration, Success: false,
			Detail: providerCheckDetail(false, failureCode),
			Model:  model, LatencyMS: latency,
		}
		applyProviderCheckObservation(&check, verificationObservation)
		if checkGenerationErr == nil {
			_ = iam.RecordProviderCheck(check)
		}
		return map[string]any{
			"provider_id": providerID, "operation": "verify", "success": false,
			"status": "failed", "details": detail, "failure_code": failureCode,
			"model": model, "checked_at": time.Now().UTC().Format(time.RFC3339),
			"latency_ms": latency,
		}
	}

	provider, err := providers.GetProviderForPrincipal(providerID, principal)
	if err != nil {
		return fail(
			fmt.Sprintf("Provider could not be instantiated: %v", err),
			"provider_unavailable",
		)
	}

	model = strings.TrimSpace(model)
	// Speech providers verify by synthesizing audio; their "model" is a voice
	// with a built-in default, so no catalog round-trip is required.
	if synthesizer, ok := providers.AsSpeechSynthesizer(provider); ok {
		voice := model
		if voice == "" {
			voice = synthesizer.DefaultVoice()
		}
		audio, format, err := synthesizer.Synthesize(voice, "ok", "+0%")
		latency := time.Since(started).Milliseconds()
		if err != nil {
			return fail(
				fmt.Sprintf("Test synthesis with voice %q failed: %v", voice, err),
				"verification_failed",
			)
		}
		detail := fmt.Sprintf("Test synthesis with voice %q returned %d bytes of %s in %dms. Speech through this provider is verified.", voice, len(audio), format, latency)
		check := iam.ProviderCheck{
			ProviderID: providerID, Operation: iam.CheckVerify,
			ScopeKey: checkScope, Generation: checkGeneration, Success: true,
			Detail: providerCheckDetail(true, ""), Model: voice, LatencyMS: latency,
		}
		applyProviderCheckObservation(&check, verificationObservation)
		if checkGenerationErr == nil {
			_ = iam.RecordProviderCheck(check)
		}
		return map[string]any{
			"provider_id": providerID, "operation": "verify", "success": true,
			"status": "passed", "details": detail, "model": voice,
			"checked_at": time.Now().UTC().Format(time.RFC3339), "latency_ms": latency,
			"audio_bytes": len(audio), "audio_format": format,
		}
	}

	catalogRefreshed := false
	if model == "" {
		rows, catalogObservation, _ := providers.RefreshCatalogForPrincipalWithError(
			providerID, principal,
		)
		catalogRefreshed = true
		if catalogObservation != nil {
			verificationObservation = catalogObservation
		}
		checkGeneration = refreshedProviderCheckGeneration(
			providerID, checkScope, checkGeneration,
			initialVerificationObservation, verificationObservation,
		)
		if len(rows) == 0 {
			return fail(
				"No model is available to verify. Sync the catalog first, or pass an explicit model.",
				"model_unavailable",
			)
		}
		model = rows[0].ID
	}
	if catalogRefreshed {
		provider, err = providers.GetProviderForPrincipal(providerID, principal)
		if err != nil {
			return fail(
				fmt.Sprintf("Provider could not be re-initialized after catalog refresh: %v", err),
				"provider_unavailable",
			)
		}
	}

	messages := []providers.Message{{"role": "user", "content": "Reply with the single word: ok"}}
	preCompletionObservation := verificationObservation
	verifyKw := providers.Kwargs{"max_tokens": 16}
	if providerConfig := config.Get().Providers[providerID]; providerConfig != nil {
		switch providerConfig.Type {
		case "ai_studio", "vertex_ai":
			verifyKw["max_tokens"] = 512
		}
	}
	response, completionObservation, err := providers.CompleteProviderWithObservation(
		provider, model, messages, verifyKw,
	)
	if completionObservation != nil {
		verificationObservation = completionObservation
		checkGeneration = refreshedProviderCheckGeneration(
			providerID, checkScope, checkGeneration,
			preCompletionObservation, completionObservation,
		)
	}
	latency := time.Since(started).Milliseconds()
	if err != nil {
		return fail(
			fmt.Sprintf("Test completion against %q failed: %v", model, err),
			"verification_failed",
		)
	}
	detail := fmt.Sprintf("Test completion against %q succeeded in %dms. Inference through this provider is verified.", model, latency)
	check := iam.ProviderCheck{
		ProviderID: providerID, Operation: iam.CheckVerify,
		ScopeKey: checkScope, Generation: checkGeneration, Success: true,
		Detail: providerCheckDetail(true, ""), Model: model, LatencyMS: latency,
	}
	applyProviderCheckObservation(&check, verificationObservation)
	if checkGenerationErr == nil {
		_ = iam.RecordProviderCheck(check)
	}
	inputTokens, outputTokens := responseUsage(response)
	usagePrincipal := &config.Principal{}
	if principal != nil {
		usagePrincipal = principal
	}
	router.RecordUsage(router.UsageRecord{
		Endpoint: "provider.verify", RequestedModel: model, RoutedModel: model,
		Provider: providerID, Project: usagePrincipal.Project, Key: usagePrincipal.Key,
		ProjectID: usagePrincipal.ProjectID, PrincipalID: usagePrincipal.PrincipalID,
		KeyID: usagePrincipal.KeyID, InputTokens: inputTokens, OutputTokens: outputTokens,
		LatencyMS: latency, IsStub: provider.IsStub(),
	})
	return map[string]any{
		"provider_id": providerID, "operation": "verify", "success": true,
		"status": "passed", "details": detail, "model": model,
		"checked_at": time.Now().UTC().Format(time.RFC3339), "latency_ms": latency,
		"usage": map[string]any{"input_tokens": inputTokens, "output_tokens": outputTokens},
	}
}
