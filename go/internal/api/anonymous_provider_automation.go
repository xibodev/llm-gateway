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
var anonymousProviderMutationMu sync.Mutex

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
	generation, generationErr := iam.ProviderCheckGeneration(profile.ProviderID, "")
	if generationErr != nil {
		return map[string]any{
			"provider_id": profile.ProviderID, "operation": "verify", "success": false,
			"status": "failed", "failure_code": "evidence_unavailable",
			"catalog_evidence": "not_probed", "completion_evidence": "not_probed",
		}
	}
	ctx = providers.WithProviderEvidenceGeneration(ctx, generation)
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
		recordOrchestratorChecks(profile.ProviderID, generation, result)
		return item
	}
	if len(result.Probes) == 0 {
		item["failure_code"] = "model_unavailable"
		item["details"] = "No reviewed anonymous model was available for verification."
		recordOrchestratorChecks(profile.ProviderID, generation, result)
		return item
	}
	item["targets"] = coreTargetsForWire(result.Targets)
	item["published"] = len(result.Targets)
	item["probed"] = len(result.Probes)
	verified := 0
	failed := 0
	for _, probe := range result.Probes {
		if probe.Status == core.CompletionVerified {
			verified++
		} else {
			failed++
		}
	}
	item["verified"] = verified
	item["failed"] = failed
	item["completion_evidence"] = map[bool]string{true: "verified", false: "failed"}[failed == 0]
	if verified > 0 {
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
	recordOrchestratorChecks(profile.ProviderID, generation, result)
	return item
}

func coreTargetsForWire(targets []core.Target) []map[string]string {
	out := make([]map[string]string, 0, len(targets))
	for _, target := range targets {
		out = append(out, map[string]string{"provider": target.Provider, "model": target.Model})
	}
	return out
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

func recordOrchestratorChecks(providerID string, generation int64, result core.ProviderConnectResult) {
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
		failureCode := ""
		if !success {
			failureCode = strings.TrimSpace(probe.FailureCode)
			if failureCode == "" || failureCode == string(core.ProviderErrorNone) {
				failureCode = "verification_failed"
			}
		}
		_ = iam.RecordProviderCheck(iam.ProviderCheck{
			ProviderID: providerID, Operation: iam.CheckVerify, Generation: generation, Success: success,
			Detail: providerCheckDetail(success, failureCode),
			Model:  probe.Target.Model, LatencyMS: probe.Latency.Milliseconds(),
		})
		if probe.Status == core.CompletionVerified || probe.Status == core.CompletionFailed {
			state := "failed"
			if success {
				state = "verified"
			}
			_ = iam.RecordProviderModelEvidence(iam.ProviderModelEvidence{
				ProviderID: providerID, Model: probe.Target.Model,
				Operation: iam.ModelEvidenceCompletion, State: state,
				ObservedAt: probe.ObservedAt.Unix(), LatencyMS: probe.Latency.Milliseconds(),
				FailureCode: failureCode, Generation: generation,
			})
		}
		recordAnonymousAutomationCheck(providerID, "verify", map[string]any{
			"success": success, "model": probe.Target.Model,
			"failure_code": failureCode,
		})
	}
}

func ensureAnonymousProvider(profile providers.AnonymousProviderProfile) (string, string) {
	anonymousProviderMutationMu.Lock()
	defer anonymousProviderMutationMu.Unlock()
	endpointMutationMu.Lock()
	defer endpointMutationMu.Unlock()
	if current, exists := config.Provider(profile.ProviderID); exists {
		return classifyAnonymousProvider(profile, current)
	}
	if providerEndpointCollision(profile.ProviderID) != nil {
		return profile.ProviderID, "collision"
	}
	connectionExists, err := iam.ActiveProviderConnectionExists(profile.ProviderID)
	if err != nil {
		return profile.ProviderID, "credential_store_unavailable"
	}
	if connectionExists {
		return profile.ProviderID, "credential_collision"
	}
	if err := iam.MarkAnonymousProviderManaged(profile.ProviderID); err != nil {
		return profile.ProviderID, "credential_store_unavailable"
	}
	added := false
	_, err = config.UpdateAndSave(func(s *config.Settings) error {
		if s.Providers[profile.ProviderID] != nil {
			return nil
		}
		added = true
		s.Providers[profile.ProviderID] = &config.ProviderConfig{
			Type: profile.RuntimeType, RegistryID: profile.RegistryID, BaseURL: profile.BaseURL,
		}
		return nil
	})
	if err != nil {
		_ = iam.ClearAnonymousProviderManaged(profile.ProviderID)
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
		_ = iam.ClearAnonymousProviderManaged(profile.ProviderID)
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
	managed, err := iam.AnonymousProviderManaged(profile.ProviderID)
	if err != nil {
		return profile.ProviderID, "credential_store_unavailable"
	}
	if !managed {
		return profile.ProviderID, "collision"
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
	// A non-empty completion proves inference availability. Requiring an exact
	// acknowledgement conflates instruction-following quality with reachability.
	return runProviderVerifyWithContext(ctx, providerID, model, principal, false)
}
