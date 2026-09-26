package api

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	core "github.com/xibodev/llmgw-core"
	"github.com/xibodev/llmgw-core/anonymous"

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

func (s *server) handleAutoConnectFreeProviders(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	_ = iam.SetAnonymousProviderAutomationOverride("on")

	profiles := providers.AnonymousProviderProfiles()
	orchestrator, err := newAnonymousOrchestrator(s.providers(), profiles)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Provider automation is unavailable.")
		return
	}
	results := make([]map[string]any, 0, len(profiles))
	verifiedCount := 0
	for _, result := range orchestrator.ConnectAll(r.Context()) {
		item := anonymousResultItem(result)
		item["registry_id"] = result.RegistryID
		if result.Operation != "" {
			// A checked provider is managed until its check says more.
			item["status"] = anonymous.StatusManaged
			if result.Success {
				item["status"] = "verified"
				verifiedCount++
			} else if result.CatalogEvidence == core.CatalogDiscovered {
				item["status"] = "connected"
			} else if result.Details != "" {
				item["catalog_error"] = result.Details
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

// StartAnonymousProviderAutomation runs the automation at once, then hourly
// and when a setting change requests it, against runtime until the returned
// stop runs.
func StartAnonymousProviderAutomation(parent context.Context, runtime *providers.Runtime) func() {
	orchestrator, err := newAnonymousOrchestrator(runtime, providers.AnonymousProviderProfiles())
	if err != nil {
		// The registry vets its anonymous profiles as it loads, so this does
		// not happen; if it did, no run could connect a provider, as before.
		return func() {}
	}
	return orchestrator.Start(parent, time.Hour, anonymousAutomationWake)
}

// newAnonymousOrchestrator returns llmgw-core's anonymous orchestrator for
// profiles, which runs the automation against runtime: it vets the profiles
// against the effective registry, probes every model a catalog lists, and
// publishes the ones that answer. The gateway supplies the Hooks, and the
// catalog and probe paths through runtime.
func newAnonymousOrchestrator(
	runtime *providers.Runtime, profiles []providers.AnonymousProviderProfile,
) (*anonymous.Orchestrator, error) {
	automation := newAnonymousAutomation(runtime, profiles)
	return anonymous.New(anonymous.Options{
		Profiles: profiles, Registry: providers.EffectiveRegistry(),
		Catalog: automation, Invoker: runtime.AnonymousInvoker(profiles), Hooks: automation,
		CheckInterval: anonymousProviderAutomationInterval,
	})
}

// anonymousAutomation is the gateway's side of the orchestrator. Its Hooks
// are the setting, the enrollment policy, the daily claim and the evidence;
// as its Catalog it prepares a check's model evidence once the catalog is
// read. The orchestrator checks one provider at a time and asks each check's
// generation before its catalog, so Generation keeps it for Discover.
type anonymousAutomation struct {
	runtime  *providers.Runtime
	profiles map[string]providers.AnonymousProviderProfile
	catalog  anonymous.Catalog

	mu          sync.Mutex
	generations map[string]int64
}

func newAnonymousAutomation(
	runtime *providers.Runtime, profiles []providers.AnonymousProviderProfile,
) *anonymousAutomation {
	automation := &anonymousAutomation{
		runtime:     runtime,
		profiles:    make(map[string]providers.AnonymousProviderProfile, len(profiles)),
		catalog:     runtime.AnonymousCatalog(),
		generations: map[string]int64{},
	}
	for _, profile := range profiles {
		automation.profiles[profile.ProviderID] = profile
	}
	return automation
}

// Enabled reports the effective setting. A setting that cannot be read is
// off.
func (a *anonymousAutomation) Enabled(context.Context) bool {
	state, err := anonymousProviderAutomationState()
	return err == nil && state.Effective
}

// Enroll applies the gateway's enrollment policy; see ensureAnonymousProvider.
func (a *anonymousAutomation) Enroll(_ context.Context, profile providers.AnonymousProviderProfile) (string, string) {
	return ensureAnonymousProvider(a.runtime, profile)
}

// Claim claims the provider's daily check and then reads the provider again
// under the claim, declining one that changed since Enroll: the orchestrator
// has no second enrollment check of its own. A declined claim still consumes
// the day, as the gateway's skipped check always did.
func (a *anonymousAutomation) Claim(_ context.Context, providerID string, at time.Time, every time.Duration) (bool, error) {
	claimed, err := iam.ClaimAnonymousProviderCheck(providerID, at, every)
	if err != nil || !claimed {
		return false, err
	}
	current, exists := config.Provider(providerID)
	if !exists {
		return false, nil
	}
	_, status := classifyAnonymousProvider(a.profiles[providerID], current)
	return status == anonymous.StatusManaged, nil
}

// Generation reads the provider's evidence generation, the fence that a later
// invalidation advances while the check is in flight.
func (a *anonymousAutomation) Generation(_ context.Context, providerID string) (int64, error) {
	generation, err := iam.ProviderCheckGeneration(providerID, "")
	if err != nil {
		return 0, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.generations[providerID] = generation
	return generation, nil
}

// Discover reads the catalog and, before any probe, reconciles the provider's
// model evidence with it under the check's generation: a model the catalog
// no longer lists goes stale, and every listed one is unverified until its
// probe is recorded.
func (a *anonymousAutomation) Discover(ctx context.Context, caller core.Caller, providerID string) ([]core.ModelInfo, error) {
	models, err := a.catalog.Discover(ctx, caller, providerID)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	generation, ok := a.generations[providerID]
	a.mu.Unlock()
	if !ok {
		return nil, errors.New("provider evidence generation is required")
	}
	modelIDs := make([]string, 0, len(models))
	for _, model := range models {
		modelIDs = append(modelIDs, model.ID)
	}
	if err := iam.ReconcileProviderModelCatalog(
		providerID, "", iam.ModelEvidenceCompletion, modelIDs, generation,
	); err != nil {
		return nil, fmt.Errorf("prepare model evidence: %w", err)
	}
	return models, nil
}

// Record keeps a check's evidence. It never fails the check: the automation
// has always recorded on a best-effort basis.
func (a *anonymousAutomation) Record(_ context.Context, providerID string, generation int64, result anonymous.Result) error {
	recordOrchestratorChecks(providerID, generation, result.Connect)
	return nil
}

// anonymousResultItem is result as the automation has always reported it: a
// provider it leaves alone by its enrollment status; a check without an
// evidence generation by its failure; a check no probe ran for with why; and
// any other with its probes, published targets and retry advice.
func anonymousResultItem(result anonymous.Result) map[string]any {
	if result.Operation == "" {
		return map[string]any{"provider_id": result.ProviderID, "status": result.Status}
	}
	item := map[string]any{
		"provider_id": result.ProviderID, "operation": result.Operation, "success": result.Success,
		"status": result.Status, "failure_code": result.FailureCode,
		"catalog_evidence":    string(result.CatalogEvidence),
		"completion_evidence": string(result.CompletionEvidence),
	}
	if result.FailureCode == anonymous.FailureEvidenceUnavailable {
		return item
	}
	item["authentication_state"] = result.AuthenticationState
	if result.Probed == 0 {
		item["details"] = result.Details
		return item
	}
	item["targets"] = coreTargetsForWire(result.Targets)
	item["published"], item["probed"] = result.Published, result.Probed
	item["verified"], item["failed"] = result.Verified, result.Failed
	if result.VerificationError != "" {
		item["verification_error"] = result.VerificationError
	}
	item["retryable"] = result.Retryable
	if result.RetryAfter > 0 {
		item["retry_after"] = strconv.FormatInt(int64(result.RetryAfter/time.Second), 10)
	}
	return item
}

func coreTargetsForWire(targets []core.Target) []map[string]string {
	out := make([]map[string]string, 0, len(targets))
	for _, target := range targets {
		out = append(out, map[string]string{"provider": target.Provider, "model": target.Model})
	}
	return out
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
		failureCode := anonymous.ProbeFailureCode(probe)
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

func ensureAnonymousProvider(runtime *providers.Runtime, profile providers.AnonymousProviderProfile) (string, string) {
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
		runtime.ForgetProvider(profile.ProviderID)
		runtime.ForgetCatalog(profile.ProviderID)
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
