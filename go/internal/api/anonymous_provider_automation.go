package api

import (
	"context"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	core "github.com/xibodev/llmgw-core"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
)

const (
	anonymousProviderAutomationEnv      = "LLMGW_ANONYMOUS_PROVIDER_AUTOMATION"
	anonymousProviderAutomationInterval = 24 * time.Hour
)

var anonymousAutomationWake = make(chan struct{}, 1)

type anonymousProviderAutomationSetting struct {
	Override                string `json:"override"`
	DeploymentDefault       bool   `json:"deployment_default"`
	DeploymentDefaultSource string `json:"deployment_default_source"`
	EnvironmentValid        bool   `json:"environment_valid"`
	Effective               bool   `json:"effective"`
	EffectiveSource         string `json:"effective_source"`
	ReconcileRequested      bool   `json:"reconcile_requested"`
}

func anonymousProviderAutomationState() (anonymousProviderAutomationSetting, error) {
	state := anonymousProviderAutomationSetting{
		Override: "inherit", DeploymentDefaultSource: "built_in",
		EnvironmentValid: true, EffectiveSource: "built_in",
	}
	if raw, present := os.LookupEnv(anonymousProviderAutomationEnv); present {
		value, err := strconv.ParseBool(strings.TrimSpace(raw))
		if err != nil {
			state.EnvironmentValid = false
		} else {
			state.DeploymentDefault = value
			state.DeploymentDefaultSource = "environment"
			state.EffectiveSource = "environment"
		}
	}
	state.Effective = state.DeploymentDefault && state.EnvironmentValid
	override, set, err := iam.AnonymousProviderAutomationOverride()
	if err != nil {
		return state, err
	}
	if set {
		state.Override = override
		state.Effective = override == "on"
		state.EffectiveSource = "console_override"
	}
	return state, nil
}

func handleGetAnonymousProviderAutomation(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	state, err := anonymousProviderAutomationState()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Provider automation setting is unavailable.")
		return
	}
	writeJSON(w, http.StatusOK, state)
}

func handleSetAnonymousProviderAutomation(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	var body struct {
		Override string `json:"override"`
	}
	if !decodeBody(r, &body) {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	value := strings.ToLower(strings.TrimSpace(body.Override))
	if value != "inherit" && value != "on" && value != "off" {
		writeError(w, http.StatusBadRequest, "override must be one of inherit, on, or off")
		return
	}
	if err := iam.SetAnonymousProviderAutomationOverride(value); err != nil {
		writeError(w, http.StatusInternalServerError, "Provider automation setting could not be saved.")
		return
	}
	state, err := anonymousProviderAutomationState()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Provider automation setting is unavailable.")
		return
	}
	if state.Effective {
		state.ReconcileRequested = true
		requestAnonymousProviderAutomation()
	}
	auditAdmin(r, "provider_automation.update", "setting", "anonymous-providers", map[string]any{
		"override": value, "effective": state.Effective,
	})
	writeJSON(w, http.StatusOK, state)
}

func handleAutoConnectFreeProviders(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	_ = iam.SetAnonymousProviderAutomationOverride("on")

	profiles := providers.AnonymousProviderProfiles()
	results := make([]map[string]any, 0, len(profiles))
	verifiedCount := 0

	orchestrator, err := providers.NewGatewayProviderOrchestrator(profiles)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Provider automation is unavailable.")
		return
	}
	for _, profile := range profiles {
		providerID, status := ensureAnonymousProvider(profile)
		item := map[string]any{
			"provider_id": providerID,
			"registry_id": profile.RegistryID,
			"status":      status,
		}
		if status == "managed" {
			result := connectAnonymousProvider(r.Context(), orchestrator, profile)
			for key, value := range result {
				if key != "status" {
					item[key] = value
				}
			}
			if result["success"] == true {
				item["status"] = "verified"
				verifiedCount++
			} else if result["catalog_evidence"] == string(core.CatalogDiscovered) {
				item["status"] = "connected"
			} else if details, _ := result["details"].(string); details != "" {
				item["catalog_error"] = details
			}
		}
		results = append(results, item)
	}

	auditAdmin(r, "provider_automation.auto_connect", "provider", "free_anonymous", map[string]any{
		"verified": verifiedCount, "total": len(profiles),
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"verified": verifiedCount,
		"total":    len(profiles),
		"results":  results,
	})
}

func requestAnonymousProviderAutomation() {
	select {
	case anonymousAutomationWake <- struct{}{}:
	default:
	}
}

func StartAnonymousProviderAutomation(parent context.Context) func() {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			runAnonymousProviderAutomation(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			case <-anonymousAutomationWake:
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { cancel(); <-done }) }
}

func runAnonymousProviderAutomation(ctx context.Context) []map[string]any {
	return runAnonymousProviderAutomationProfiles(ctx, providers.AnonymousProviderProfiles())
}

func runAnonymousProviderAutomationProfiles(
	ctx context.Context, profiles []providers.AnonymousProviderProfile,
) []map[string]any {
	state, err := anonymousProviderAutomationState()
	if err != nil || !state.Effective || ctx.Err() != nil {
		return nil
	}
	orchestrator, err := providers.NewGatewayProviderOrchestrator(profiles)
	if err != nil {
		return []map[string]any{{"status": "failed", "failure_code": "orchestrator_unavailable"}}
	}
	results := []map[string]any{}
	for _, profile := range profiles {
		if ctx.Err() != nil {
			break
		}
		current, stateErr := anonymousProviderAutomationState()
		if stateErr != nil || !current.Effective {
			break
		}
		providerID, status := ensureAnonymousProvider(profile)
		if status != "managed" {
			results = append(results, map[string]any{"provider_id": providerID, "status": status})
			continue
		}
		claimed, claimErr := iam.ClaimAnonymousProviderCheck(providerID, time.Now(), anonymousProviderAutomationInterval)
		if claimErr != nil || !claimed {
			continue
		}
		if current, exists := config.Provider(providerID); !exists {
			continue
		} else if _, status = classifyAnonymousProvider(profile, current); status != "managed" {
			results = append(results, map[string]any{"provider_id": providerID, "status": status})
			continue
		}
		currentState, stateErr := anonymousProviderAutomationState()
		if stateErr != nil || !currentState.Effective {
			break
		}
		if current, exists := config.Provider(providerID); !exists {
			continue
		} else if _, status = classifyAnonymousProvider(profile, current); status != "managed" {
			results = append(results, map[string]any{"provider_id": providerID, "status": status})
			continue
		}
		results = append(results, connectAnonymousProvider(ctx, orchestrator, profile))
	}
	return results
}

func connectAnonymousProvider(
	ctx context.Context, orchestrator *core.ProviderOrchestrator, profile providers.AnonymousProviderProfile,
) map[string]any {
	connection := core.ProviderConnection{
		ProviderID: profile.ProviderID, Kind: core.ProviderConnectionAnonymous, AuthKind: core.ProviderAuthAnonymous,
	}
	result, connectErr := orchestrator.Connect(ctx, core.ProviderConnectRequest{
		Connection: connection, PublicationPolicy: core.PublishVerifiedTargets,
	})
	item := map[string]any{
		"provider_id": profile.ProviderID, "operation": "verify", "success": false,
		"status": "failed", "authentication_state": authenticationState(result.Health),
		"catalog_evidence": string(result.Catalog.Status), "completion_evidence": "not_probed",
	}
	if connectErr != nil {
		item["failure_code"] = providerHealthFailureCode(result.Health, true)
		item["details"] = connectErr.Error()
		recordOrchestratorChecks(profile.ProviderID, result)
		return item
	}
	if len(result.Probes) == 0 {
		item["failure_code"] = "model_unavailable"
		item["details"] = "No reviewed anonymous model was available for verification."
		recordOrchestratorChecks(profile.ProviderID, result)
		return item
	}
	probe := result.Probes[0]
	item["model"] = probe.Target.Model
	item["completion_evidence"] = string(probe.Status)
	item["latency_ms"] = probe.Latency.Milliseconds()
	if probe.Status == core.CompletionVerified {
		item["success"] = true
		item["status"] = "passed"
		item["authentication_state"] = "accepted"
		item["failure_code"] = ""
	} else {
		item["failure_code"] = providerHealthFailureCode(result.Health, false)
		item["verification_error"] = "Provider inference verification failed."
	}
	item["retryable"] = result.Health.Retryable
	if result.Health.RetryAfter > 0 {
		item["retry_after"] = strconv.FormatInt(int64(result.Health.RetryAfter/time.Second), 10)
	}
	recordOrchestratorChecks(profile.ProviderID, result)
	return item
}

func authenticationState(health core.ProviderHealthEvidence) string {
	if health.ErrorClass == core.ProviderErrorAuth || health.ErrorClass == core.ProviderErrorForbidden {
		return "rejected"
	}
	if health.Status == core.ProviderHealthHealthy || health.Status == core.ProviderHealthDegraded {
		return "accepted"
	}
	return "unknown"
}

func providerHealthFailureCode(health core.ProviderHealthEvidence, catalog bool) string {
	if health.ErrorClass == core.ProviderErrorAuth || health.ErrorClass == core.ProviderErrorForbidden {
		return "authentication_rejected"
	}
	if catalog {
		return "catalog_failed"
	}
	return "verification_failed"
}

func recordOrchestratorChecks(providerID string, result core.ProviderConnectResult) {
	generation, err := iam.ProviderCheckGeneration(providerID, "")
	if err != nil {
		return
	}
	catalogSuccess := result.Catalog.Status == core.CatalogDiscovered
	_ = iam.RecordProviderCheck(iam.ProviderCheck{
		ProviderID: providerID, Operation: iam.CheckCatalogSync, Generation: generation, Success: catalogSuccess,
		Detail: providerCheckDetail(catalogSuccess, map[bool]string{true: "", false: "catalog_failed"}[catalogSuccess]),
	})
	recordAnonymousAutomationCheck(providerID, "catalog", map[string]any{
		"success": catalogSuccess, "failure_code": map[bool]string{true: "", false: "catalog_failed"}[catalogSuccess],
	})
	for _, probe := range result.Probes {
		success := probe.Status == core.CompletionVerified
		_ = iam.RecordProviderCheck(iam.ProviderCheck{
			ProviderID: providerID, Operation: iam.CheckVerify, Generation: generation, Success: success,
			Detail: providerCheckDetail(success, map[bool]string{true: "", false: "verification_failed"}[success]),
			Model:  probe.Target.Model, LatencyMS: probe.Latency.Milliseconds(),
		})
		recordAnonymousAutomationCheck(providerID, "verify", map[string]any{
			"success": success, "model": probe.Target.Model,
			"failure_code": map[bool]string{true: "", false: "verification_failed"}[success],
		})
	}
}

func ensureAnonymousProvider(profile providers.AnonymousProviderProfile) (string, string) {
	if current, exists := config.Provider(profile.ProviderID); exists {
		return classifyAnonymousProvider(profile, current)
	}
	connectionExists, err := iam.ActiveProviderConnectionExists(profile.ProviderID)
	if err != nil {
		return profile.ProviderID, "credential_store_unavailable"
	}
	if connectionExists {
		return profile.ProviderID, "credential_collision"
	}
	added, err := config.AddProviderIfMissing(profile.ProviderID, &config.ProviderConfig{
		Type: profile.RuntimeType, RegistryID: profile.RegistryID, BaseURL: profile.BaseURL,
	})
	if err != nil {
		log.Printf("anonymous provider automation: add %s: %v", profile.ProviderID, err)
		return profile.ProviderID, "save_failed"
	}
	if added {
		providers.ForgetProvider(profile.ProviderID)
		providers.ForgetCatalog(profile.ProviderID)
		_ = iam.InvalidateProviderChecks(profile.ProviderID)
		_ = iam.RecordAudit(iam.AuditEvent{
			Action: "provider_automation.connect", TargetType: "provider", TargetID: profile.ProviderID,
			Result: "success", Detail: map[string]any{"registry_id": profile.RegistryID, "source": "automation"},
		})
	}
	if !added {
		if current, exists := config.Provider(profile.ProviderID); exists {
			return classifyAnonymousProvider(profile, current)
		}
		return profile.ProviderID, "collision"
	}
	return profile.ProviderID, "managed"
}

func classifyAnonymousProvider(profile providers.AnonymousProviderProfile, current *config.ProviderConfig) (string, string) {
	connectionExists, err := iam.ActiveProviderConnectionExists(profile.ProviderID)
	if err != nil {
		return profile.ProviderID, "credential_store_unavailable"
	}
	if providers.EffectiveRegistryID(profile.ProviderID, current.RegistryID, current.Type) == profile.RegistryID &&
		strings.EqualFold(strings.TrimSpace(current.Type), profile.RuntimeType) &&
		strings.TrimRight(current.BaseURL, "/") == strings.TrimRight(profile.BaseURL, "/") &&
		providers.AnonymousAPIKey(config.ResolveProviderAPIKey(profile.ProviderID, current)) && !connectionExists {
		if current.Disabled {
			return profile.ProviderID, "disabled"
		}
		return profile.ProviderID, "managed"
	}
	return profile.ProviderID, "collision"
}

func recordAnonymousAutomationCheck(providerID, operation string, result map[string]any) {
	success, _ := result["success"].(bool)
	detail := map[string]any{"source": "automation", "operation": operation, "success": success}
	if model, _ := result["model"].(string); model != "" {
		detail["model"] = model
	}
	if failure, _ := result["failure_code"].(string); failure != "" {
		detail["failure_code"] = failure
	}
	_ = iam.RecordAudit(iam.AuditEvent{
		Action: "provider_automation.check", TargetType: "provider", TargetID: providerID,
		Result: map[bool]string{true: "success", false: "failure"}[success], Detail: detail,
	})
}

func runProviderVerifyContext(ctx context.Context, providerID, model string, principal *config.Principal) map[string]any {
	return runProviderVerifyWithContext(ctx, providerID, model, principal, true)
}
