// Package router resolves a requested model to a failover chain and executes it,
// recording usage (savings ledger) and throttle/fallback events (telemetry).
package router

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"

	"github.com/xibodev/llm-translate"
	core "github.com/xibodev/llmgw-core"
	"github.com/xibodev/llmgw-core/execution"
)

// Target is one resolved provider/model in a chain.
type Target = core.Target

// Resolution records both the resolved targets and the canonical category name,
// when the request addressed a category rather than a direct model.
type Resolution = core.Resolution

const defaultFallbackTimeout = 2 * time.Minute

type fallbackOptions struct {
	timeout  time.Duration
	affinity string
}

type fallbackOptionsKey struct{}

// WithFallbackOptions attaches request-level routing controls without changing
// provider payloads. A non-positive timeout uses the bounded default.
func WithFallbackOptions(ctx context.Context, timeout time.Duration, affinity string) context.Context {
	return context.WithValue(ctx, fallbackOptionsKey{}, fallbackOptions{timeout: timeout, affinity: strings.TrimSpace(affinity)})
}

func prepareFallback(ctx context.Context, targets []Target, kw providers.Kwargs) (context.Context, context.CancelFunc, []Target) {
	options, _ := ctx.Value(fallbackOptionsKey{}).(fallbackOptions)
	if value, ok := kw["_fallback_timeout_ms"]; ok && options.timeout <= 0 {
		if milliseconds, err := strconv.ParseInt(fmt.Sprint(value), 10, 64); err == nil && milliseconds > 0 {
			options.timeout = time.Duration(milliseconds) * time.Millisecond
		}
	}
	if value, ok := kw["_affinity_key"].(string); ok && options.affinity == "" && strings.TrimSpace(value) != "" {
		options.affinity = strings.TrimSpace(value)
	}
	if options.timeout <= 0 {
		options.timeout = defaultFallbackTimeout
	}
	prepared := append([]Target(nil), targets...)
	if len(prepared) > 1 && options.affinity != "" {
		digest := sha256.Sum256([]byte(options.affinity))
		start := int(binary.BigEndian.Uint64(digest[:8]) % uint64(len(prepared)))
		prepared = append(prepared[start:], prepared[:start]...)
	}
	bounded, cancel := context.WithTimeout(ctx, options.timeout)
	return bounded, cancel, prepared
}

// CompatibilityRequest describes only requirements that can be proven from the
// bounded typed capability contract. Unknown values remain eligible.
type CompatibilityRequest struct {
	Surface   core.ModelSurface
	Tools     bool
	Vision    bool
	Streaming bool
}

// FilterCompatibleTargets removes targets with explicit typed incompatibility.
// It returns a 400-style configuration error when every target is excluded.
func (rt *Runtime) FilterCompatibleTargets(targets []Target, caller core.Caller, request CompatibilityRequest) ([]Target, error) {
	compatible := make([]Target, 0, len(targets))
	for _, target := range targets {
		model, ok := rt.providers().CatalogCachedLookupForPrincipal(target.Provider, target.Model, caller)
		if !ok || model.TypedCapabilities == nil {
			compatible = append(compatible, target)
			continue
		}
		if modelCompatible(model.TypedCapabilities, request) || chatToResponsesCompatible(target, model.TypedCapabilities, request) {
			compatible = append(compatible, target)
		}
	}
	if len(compatible) == 0 {
		return nil, &providers.ConfigError{Msg: "no route member supports the requested operation and capabilities"}
	}
	return compatible, nil
}

func chatToResponsesCompatible(target Target, capabilities *core.ModelCapabilities, request CompatibilityRequest) bool {
	if request.Surface != core.ModelSurfaceChatCompletions || capabilities == nil ||
		capabilities.Surfaces.Responses != core.SupportSupported {
		return false
	}
	providerConfig := config.Get().Providers[target.Provider]
	if !providers.AdaptsChatToNativeResponses(target.Provider, providerConfig) {
		return false
	}
	// The caller asks for Chat, but this provider deliberately adapts that
	// facade to the model's native Responses surface. Native Chat operation
	// support is therefore irrelevant; only the requested semantic features
	// and the Responses transport need to be supported.
	return capabilities.Surfaces.Responses != core.SupportUnsupported &&
		(!request.Tools || capabilities.Tools != core.SupportUnsupported) &&
		(!request.Vision || capabilities.Inputs.Image != core.SupportUnsupported) &&
		(!request.Streaming || capabilities.Streaming != core.SupportUnsupported)
}

func modelCompatible(capabilities *core.ModelCapabilities, request CompatibilityRequest) bool {
	if capabilities == nil {
		return true
	}
	if capabilities.Operations.Chat == core.SupportUnsupported ||
		(request.Tools && capabilities.Tools == core.SupportUnsupported) ||
		(request.Vision && capabilities.Inputs.Image == core.SupportUnsupported) ||
		(request.Streaming && capabilities.Streaming == core.SupportUnsupported) {
		return false
	}
	surface := capabilities.SurfaceCompatibility(request.Surface)
	if surface == core.SupportUnsupported {
		// Responses can use the strict Chat fallback. Chat itself must not
		// claim a known Responses-only target without explicit adaptation.
		if request.Surface == core.ModelSurfaceChatCompletions ||
			capabilities.Surfaces.ChatCompletions == core.SupportUnsupported {
			return false
		}
	}
	return true
}

func shouldAdvance(err error) bool {
	return providers.IsInvocation(err) && providers.InvocationFailoverEligible(err)
}

// walk tries targets in order on llmgw-core's execution.Execute until one
// serves, and returns what it served and the target that served it, nil
// when none did. try runs one target and judges its failure: whether a chain
// moves past a failure is the chain's own predicate, product policy that
// differs by surface, and Execute reads the judgement as the failure's
// classification.
//
// The chains end on the caller's context by their own rules, which try
// applies. A complete chain checks the context before each target, so a
// target that fails as the context ends is reported by its own failure
// unless the chain moves on to another target; a stream chain also checks it
// after each call and stops unrecorded. Execute's own checks would report the
// context's error after any failure, so it walks under a context that never
// ends while try reads the chain's.
func walk[R any](ctx context.Context, targets []Target, try func(Target) (R, error)) (R, *Target) {
	result, err := execution.Execute(context.WithoutCancel(ctx), execution.Executor[Target]{}, targets,
		func(_ context.Context, target Target) (R, error) { return try(target) })
	if err != nil {
		return result.Value, nil
	}
	served := result.Candidate
	return result.Value, &served
}

// judgement is how a chain reads a target's failure. Execute moves past a
// failure whose classification permits failover and stops at any other, so
// a judgement permits failover exactly when the chain moves on. It never
// permits a repeat: the resilience wrapper has already repeated the target
// as far as its policy allows.
type judgement struct {
	err     error
	advance bool
}

func (j *judgement) Error() string { return j.err.Error() }
func (j *judgement) Unwrap() error { return j.err }

func (j *judgement) ProviderErrorClassification() core.ProviderErrorClassification {
	return core.ProviderErrorClassification{FailoverEligible: j.advance}
}

// judge hands err to Execute: the chain moves on when advance holds and
// stops at err otherwise.
func judge(err error, advance bool) error { return &judgement{err: err, advance: advance} }

// stop ends the chain at err.
func stop(err error) error { return judge(err, false) }

func deadlineStatus(err error) int {
	if errors.Is(err, context.DeadlineExceeded) {
		return 504
	}
	return providers.UpstreamStatus(err)
}

// ModelNotFoundError means the requested model is unknown or its route has no
// enabled providers (HTTP 404).
type ModelNotFoundError struct {
	Requested   string
	Unavailable bool
}

// The wording is client-visible prose, written verbatim into a 404 body, and it
// is the only sentence most users ever read about this concept — so it uses the
// product's current vocabulary ("endpoint", matching owned_by: "endpoint" on the
// rows GET /v1/models returns). The internal identifiers around it still say
// "category"; renaming those is a separate, non-user-visible change.
func (e *ModelNotFoundError) Error() string {
	if e.Unavailable {
		return "No enabled provider is available for the requested model or endpoint. Pick an available model from GET /v1/models."
	}
	return fmt.Sprintf("Model %q is not an endpoint and not a 'provider/model' id. "+
		"Pick one from GET /v1/models: an endpoint name, or '<provider>/<model>'.", e.Requested)
}

// AllTargetsFailed means every failover target for a category failed. Status
// carries the last upstream HTTP status (0 if none) so the API layer can pass
// the real status through instead of masking it.
type AllTargetsFailed struct {
	Msg    string
	Status int
}

func (e *AllTargetsFailed) Error() string {
	return providers.SanitizeDiagnosticTextLimit(e.Msg, 2048)
}

// AmbiguousCategoryError reports an invalid configuration containing endpoint
// names that differ only by case.
//
// The type name is an internal identifier and still says "category"; the
// MESSAGE is not internal. It is one of the errors endpoint lookup hands back
// to the API layer, so an operator whose legacy config carries case-colliding
// names reads it — and it must use the same vocabulary as ModelNotFoundError
// and owned_by: "endpoint" rather than the pre-rename word.
type AmbiguousCategoryError struct {
	Requested string
	Matches   []string
}

func (e *AmbiguousCategoryError) Error() string {
	return fmt.Sprintf(
		"Endpoint %q is ambiguous; matching configured endpoints: %s.",
		e.Requested,
		strings.Join(e.Matches, ", "),
	)
}

func findCategory(name string) (string, *config.EndpointConfig, error) {
	s := config.Get()
	if cat, ok := s.Endpoints[name]; ok {
		return name, cat, nil
	}
	matches := make([]string, 0, 1)
	for cname := range s.Endpoints {
		if strings.EqualFold(cname, name) {
			matches = append(matches, cname)
		}
	}
	if len(matches) == 0 {
		return "", nil, nil
	}
	sort.Strings(matches)
	if len(matches) > 1 {
		return "", nil, &AmbiguousCategoryError{Requested: name, Matches: matches}
	}
	return matches[0], s.Endpoints[matches[0]], nil
}

// ResolveTargets maps a requested model to an ordered failover chain.
func (rt *Runtime) ResolveTargets(model string) ([]Target, error) {
	return rt.ResolveTargetsForPrincipal(context.Background(), model, core.Caller{Kind: core.CallerAnonymous})
}

func (rt *Runtime) ResolveTargetsForPrincipal(
	ctx context.Context, model string, caller core.Caller,
) ([]Target, error) {
	resolution, err := rt.ResolveForPrincipal(ctx, model, caller)
	return resolution.Targets, err
}

// ResolveForPrincipal maps a requested model to an ordered failover chain and
// preserves the exact configured category name used by the router.
// Disabled-only routes retain their targets for authorization; callers must
// check availability after policy enforcement and before executing the chain.
// A route outside the key's allowlist, or anything but a route for a
// routes-only key, reads as model-not-found.
func (rt *Runtime) ResolveForPrincipal(
	ctx context.Context, model string, caller core.Caller,
) (Resolution, error) {
	governance := governanceFrom(ctx)
	name := strings.TrimSpace(model)
	if name == "" {
		return Resolution{}, &ModelNotFoundError{Requested: model}
	}
	categoryName, cat, err := findCategory(name)
	if err != nil {
		return Resolution{}, err
	}
	if cat != nil {
		if governance != nil && len(governance.AllowedRoutes) > 0 {
			allowed := false
			for _, r := range governance.AllowedRoutes {
				if strings.EqualFold(r, categoryName) {
					allowed = true
					break
				}
			}
			if !allowed {
				return Resolution{}, &ModelNotFoundError{Requested: name}
			}
		} else if governance != nil && governance.RoutesOnly && len(governance.AllowedRoutes) == 0 {
			return Resolution{}, &ModelNotFoundError{Requested: name}
		}
		if len(cat.Failover) == 0 {
			return Resolution{}, &ModelNotFoundError{Requested: name}
		}
		out := make([]Target, 0, len(cat.Failover))
		for _, m := range cat.Failover {
			if cfg := config.Get().Providers[m.Provider]; cfg != nil && cfg.Disabled {
				continue
			}
			published, err := rt.routeMemberPublished(m, caller)
			if err != nil {
				return Resolution{}, err
			}
			if !published {
				continue
			}
			out = append(out, Target{Provider: m.Provider, Model: m.Model})
		}
		if len(out) == 0 {
			// Preserve identity for policy checks on unavailable routes only.
			// In a mixed route, disabled members must not mask an enabled
			// member's provider-policy or credential denial.
			for _, m := range cat.Failover {
				published, err := rt.routeMemberPublished(m, caller)
				if err != nil {
					return Resolution{}, err
				}
				if !published {
					continue
				}
				out = append(out, Target{Provider: m.Provider, Model: m.Model})
			}
		}
		if len(out) == 0 {
			return Resolution{}, &ModelNotFoundError{Requested: name, Unavailable: true}
		}
		return Resolution{Targets: out, Category: categoryName}, nil
	}
	if governance != nil && governance.RoutesOnly {
		return Resolution{}, &ModelNotFoundError{Requested: name}
	}
	if head, tail, ok := strings.Cut(name, "/"); ok {
		if _, exists := config.Get().Providers[head]; exists && tail != "" {
			if providers.CatalogRequiresPrincipal(head) {
				authorized, err := providers.ProviderCredentialAuthorized(head, caller)
				if err != nil {
					return Resolution{}, err
				}
				if !authorized {
					return Resolution{}, &ModelNotFoundError{Requested: name, Unavailable: true}
				}
				if _, found := rt.providers().CatalogCachedLookupForPrincipal(head, tail, caller); !found {
					return Resolution{}, &ModelNotFoundError{Requested: name, Unavailable: true}
				}
			}
			if _, published, evidenceErr := providers.AnonymousModelPublication(head, tail); evidenceErr != nil || !published {
				return Resolution{}, &ModelNotFoundError{Requested: name, Unavailable: true}
			}
			return Resolution{Targets: []Target{{Provider: head, Model: tail}}}, nil
		}
	}
	if t, ok, err := rt.resolveNativeAlias(ctx, name, caller); err != nil {
		return Resolution{}, err
	} else if ok {
		return Resolution{Targets: []Target{t}}, nil
	}
	return Resolution{}, &ModelNotFoundError{Requested: name}
}

func (rt *Runtime) routeMemberPublished(member config.EndpointMember, caller core.Caller) (bool, error) {
	if providers.CatalogRequiresPrincipal(member.Provider) {
		authorized, err := providers.ProviderCredentialAuthorized(member.Provider, caller)
		if err != nil {
			return false, err
		}
		if !authorized {
			return false, nil
		}
		if _, found := rt.providers().CatalogCachedLookupForPrincipal(member.Provider, member.Model, caller); !found {
			return false, nil
		}
	}
	evidence, published, err := providers.AnonymousModelPublication(member.Provider, member.Model)
	if err != nil {
		return false, err
	}
	return published || (member.AllowUnverified && evidence.State == iam.ModelEvidenceUnverified), nil
}

// nativeKey canonicalises a model name for loose matching: lowercased with
// version separators unified ('.' -> '-'), so the Anthropic-native form Claude
// Code's built-in /model picker sends ("claude-opus-4-8") and the catalog's
// dotted form ("claude-opus-4.8") compare equal.
func nativeKey(s string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(s)), ".", "-")
}

// resolveNativeAlias maps a bare model name that is neither a category nor a
// "provider/model" id onto a catalog entry, so coding CLIs can send provider-
// native names directly. It powers Claude Code's built-in /model picker, whose
// rows send Anthropic-native names with a context tag (e.g. "claude-opus-4-8[1m]")
// that would otherwise 404. Ambiguous canonical names are rejected rather than
// routed to whichever provider happens to sort first.
func (rt *Runtime) resolveNativeAlias(
	ctx context.Context, requested string, caller core.Caller,
) (Target, bool, error) {
	base := requested
	if i := strings.IndexByte(base, '['); i >= 0 { // drop a "[1m]"-style variant tag
		base = base[:i]
	}
	base = strings.TrimSpace(base)
	if base == "" {
		return Target{}, false, nil
	}
	key := nativeKey(base)
	for provider := range config.Get().Providers {
		if nativeKey(provider) == key {
			return Target{}, false, nil
		}
	}
	for endpoint := range config.Get().Endpoints {
		if nativeKey(endpoint) == key {
			return Target{}, false, nil
		}
	}
	candidates, err := rt.NativeAliasCandidates(ctx, caller)
	if err != nil {
		return Target{}, false, err
	}
	if t, found, ambiguous := uniqueNativeCandidate(candidates[key]); ambiguous {
		return Target{}, false, &ModelNotFoundError{Requested: requested}
	} else if found {
		return t, true, nil
	}
	return Target{}, false, nil
}

// NativeAliasCandidates returns policy- and credential-authorized catalog
// targets grouped by the canonical key used for bare-name resolution. A
// governed request also applies its project's policy.
func (rt *Runtime) NativeAliasCandidates(ctx context.Context, caller core.Caller) (map[string][]Target, error) {
	governance := governanceFrom(ctx)
	if governance != nil && governance.RoutesOnly {
		return map[string][]Target{}, nil
	}
	s := config.Get()
	project := iam.ProjectPolicy{}
	if governance != nil && caller.ProjectID != "" {
		var err error
		project, err = iam.GetProjectPolicy(caller.ProjectID)
		if err != nil {
			return nil, err
		}
	}
	pids := make([]string, 0, len(s.Providers))
	for pid := range s.Providers {
		pids = append(pids, pid)
	}
	sort.Strings(pids)
	snapshot := map[string][]providers.ModelInfo{}
	for _, pid := range pids {
		if s.Providers[pid].Disabled {
			continue
		}
		if !aliasProviderAllowed(governance, project, pid) {
			continue
		}
		authorized, err := providers.ProviderCredentialAuthorized(pid, caller)
		if err != nil {
			return nil, err
		}
		if !authorized {
			continue
		}
		models := append([]providers.ModelInfo(nil), rt.providers().CatalogModelsForPrincipal(pid, caller)...)
		sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
		for _, m := range models {
			if _, published, evidenceErr := providers.AnonymousModelPublication(pid, m.ID); evidenceErr != nil || !published {
				continue
			}
			aliases := aliasIDs(m.ID, s.AnthropicDiscoveryAllModels)
			policyCandidates := append([]string{pid + "/" + m.ID, m.ID}, aliases...)
			if governance != nil && (!aliasModelAllowed(governance.AllowedModels, policyCandidates...) ||
				!aliasModelAllowed(project.AllowedModels, policyCandidates...)) {
				continue
			}
			snapshot[pid] = append(snapshot[pid], m)
		}
	}
	return NativeAliasCandidatesFromSnapshot(snapshot, s.AnthropicDiscoveryAllModels), nil
}

// NativeAliasCandidatesFromSnapshot groups an already authorized and
// policy-filtered catalog snapshot without fetching or mutating its rows.
func NativeAliasCandidatesFromSnapshot(
	snapshot map[string][]providers.ModelInfo, allModels bool,
) map[string][]Target {
	out := map[string][]Target{}
	for pid, models := range snapshot {
		for _, model := range models {
			for _, alias := range aliasIDs(model.ID, allModels) {
				key := nativeKey(alias)
				if key != "" {
					out[key] = append(out[key], Target{Provider: pid, Model: model.ID})
				}
			}
		}
	}
	return out
}

func aliasIDs(model string, allModels bool) []string {
	aliases := []string{model}
	low := strings.ToLower(model)
	if allModels && !strings.HasPrefix(low, "claude") && !strings.HasPrefix(low, "anthropic") {
		aliases = append(aliases, "claude-"+model)
	}
	return aliases
}

func uniqueNativeCandidate(candidates []Target) (Target, bool, bool) {
	unique := map[Target]bool{}
	for _, candidate := range candidates {
		unique[candidate] = true
	}
	if len(unique) != 1 {
		return Target{}, false, len(unique) > 1
	}
	for candidate := range unique {
		return candidate, true, false
	}
	return Target{}, false, false
}

func aliasProviderAllowed(governance *Governance, project iam.ProjectPolicy, provider string) bool {
	return governance == nil ||
		(aliasModelAllowed(governance.AllowedProviders, provider) && aliasModelAllowed(project.AllowedProviders, provider))
}

func aliasModelAllowed(allowed []string, candidates ...string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, allowedID := range allowed {
		for _, candidate := range candidates {
			if allowedID == candidate {
				return true
			}
		}
	}
	return false
}

// attempt captures one target's outcome for telemetry.
type attempt struct {
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
	Throttled bool   `json:"throttled,omitempty"`
}

func (rt *Runtime) recordChain(ctx context.Context, requested string, attempts []attempt, served *Target) {
	// Only log interesting chains: a failure occurred, or >1 attempt.
	if len(attempts) == 0 || (len(attempts) == 1 && attempts[0].OK) {
		return
	}
	var sp, sm, project, key string
	if served != nil {
		sp, sm = served.Provider, served.Model
	}
	if governance := governanceFrom(ctx); governance != nil {
		project, key = governance.Project, governance.Key
	}
	rt.recordTelemetryEvent(requested, toEventAttempts(attempts), sp, sm, project, key)
}

// AttemptTrace is a secret-free record of one complete-request routing attempt.
type AttemptTrace struct {
	Provider   string `json:"provider"`
	Model      string `json:"model"`
	Status     string `json:"status"`
	Throttled  bool   `json:"throttled,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}

// ExecuteComplete runs the chain for a non-streaming request. Returns the
// response, the served target, and an error if all targets failed.
func (rt *Runtime) ExecuteComplete(targets []Target, messages []providers.Message, requested string, caller core.Caller, kw providers.Kwargs) (map[string]any, *Target, error) {
	return rt.ExecuteCompleteContext(context.Background(), targets, messages, requested, caller, kw)
}

func (rt *Runtime) ExecuteCompleteContext(ctx context.Context, targets []Target, messages []providers.Message, requested string, caller core.Caller, kw providers.Kwargs) (map[string]any, *Target, error) {
	response, served, _, err := rt.executeCompleteWithTrace(ctx, targets, messages, requested, caller, kw)
	return response, served, err
}

// ExecuteCompleteWithTrace uses the same route/provider execution path as
// ExecuteComplete while returning a safe fallback trace for operator tooling.
func (rt *Runtime) ExecuteCompleteWithTrace(targets []Target, messages []providers.Message, requested string, caller core.Caller, kw providers.Kwargs) (map[string]any, *Target, []AttemptTrace, error) {
	return rt.executeCompleteWithTrace(context.Background(), targets, messages, requested, caller, kw)
}

func (rt *Runtime) ExecuteCompleteWithTraceContext(ctx context.Context, targets []Target, messages []providers.Message, requested string, caller core.Caller, kw providers.Kwargs) (map[string]any, *Target, []AttemptTrace, error) {
	return rt.executeCompleteWithTrace(ctx, targets, messages, requested, caller, kw)
}

// ExecuteResponses preserves the caller's Responses API intent when a target
// supports it natively, falling back through an explicit loss-checked Chat
// adapter only for Chat-only providers.
func (rt *Runtime) ExecuteResponses(
	targets []Target,
	payload map[string]any,
	requested string,
	caller core.Caller,
) (map[string]any, *Target, error) {
	return rt.ExecuteResponsesContext(context.Background(), targets, payload, requested, caller)
}

func (rt *Runtime) ExecuteResponsesContext(
	ctx context.Context,
	targets []Target,
	payload map[string]any,
	requested string,
	caller core.Caller,
) (map[string]any, *Target, error) {
	if providers.ResponsesPayloadIsStateful(payload) && len(targets) > 1 {
		targets = targets[:1]
	}
	ctx, cancel, targets := prepareFallback(ctx, targets, nil)
	defer cancel()
	fallback := translateResponsesFallback(payload)
	var attempts []attempt
	var lastErr error
	lastStatus := 0
	result, served := walk(ctx, targets, func(target Target) (map[string]any, error) {
		if ctx.Err() != nil {
			lastErr = ctx.Err()
			lastStatus = deadlineStatus(lastErr)
			return nil, stop(lastErr)
		}
		provider, err := rt.providers().GetProviderForPrincipal(target.Provider, caller)
		if err != nil {
			attempts = append(attempts, attempt{
				Provider: target.Provider, Model: target.Model,
				Error: truncate(err.Error()), Throttled: providers.IsThrottle(err),
			})
			lastErr = err
			return nil, judge(err, shouldAdvance(err))
		}
		result, _, err := providers.CompleteResponsesContext(ctx, provider, target.Model, payload)
		if errors.Is(err, providers.ErrResponsesUnsupported) {
			if refusal := rt.responsesFallbackRefusal(target, caller, fallback); refusal != nil {
				lastErr, lastStatus = refusal, 400
				attempts = append(attempts, attempt{
					Provider: target.Provider, Model: target.Model,
					Error: truncate(lastErr.Error()),
				})
				return nil, stop(refusal)
			}
			chat, chatErr := providers.CompleteProviderContext(ctx, provider, target.Model, fallback.messages, fallback.kw)
			if chatErr == nil {
				converted := translate.ChatResponseToResponsesWithRequestAndReport(target.Model, chat, payload)
				if lossErr := providers.RejectMaterialLossExceptThoughtSignatures(converted.Report); lossErr != nil {
					chatErr = &providers.ConfigError{Msg: lossErr.Error()}
				} else {
					result = converted.Value
				}
			}
			err = chatErr
		}
		if err != nil {
			attempts = append(attempts, attempt{
				Provider: target.Provider, Model: target.Model,
				Error: truncate(err.Error()), Throttled: providers.IsThrottle(err),
			})
			lastErr = err
			lastStatus = providers.UpstreamStatus(err)
			return nil, judge(err, shouldAdvance(err))
		}
		result["model"] = target.Model
		attempts = append(attempts, attempt{
			Provider: target.Provider, Model: target.Model, OK: true,
		})
		return result, nil
	})
	rt.recordChain(ctx, requested, attempts, served)
	if served != nil {
		return result, served, nil
	}
	if errors.Is(lastErr, context.Canceled) {
		return nil, nil, context.Canceled
	}
	message := "no failover targets"
	if lastErr != nil {
		message = lastErr.Error()
	}
	return nil, nil, &AllTargetsFailed{
		Msg: message, Status: lastStatus,
	}
}

// ExecuteAnthropicMessages preserves native Messages payloads and loss-checks
// the narrower OpenAI Chat adapter used by other configured providers.
func (rt *Runtime) ExecuteAnthropicMessages(
	targets []Target,
	payload map[string]any,
	requested string,
	caller core.Caller,
) (map[string]any, *Target, error) {
	return rt.ExecuteAnthropicMessagesContext(context.Background(), targets, payload, requested, caller)
}

func (rt *Runtime) ExecuteAnthropicMessagesContext(
	ctx context.Context,
	targets []Target,
	payload map[string]any,
	requested string,
	caller core.Caller,
) (map[string]any, *Target, error) {
	conversion := translate.AnthropicRequestToOpenAIWithReport(payload)
	messages, kw := conversion.Value.Messages, conversion.Value.Keywords
	materialErr := conversion.RejectMaterialLoss()
	requiresNative := materialErr != nil
	if requiresNative {
		hasNative := false
		for _, target := range targets {
			provider, err := rt.providers().GetProviderForPrincipal(target.Provider, caller)
			if err == nil && providers.SupportsAnthropicMessages(provider) {
				hasNative = true
				break
			}
		}
		if !hasNative {
			return nil, nil, &AllTargetsFailed{Msg: "Anthropic request requires a native Messages target: " + materialErr.Error(), Status: 400}
		}
	}
	var attempts []attempt
	var lastErr error
	lastStatus := 0
	// This chain moves past every failure but a definitive upstream
	// rejection: a target that cannot be built or cannot take the request
	// through the Chat adapter leaves the request to the next target.
	result, served := walk(ctx, targets, func(target Target) (map[string]any, error) {
		if ctx.Err() != nil {
			lastErr = ctx.Err()
			return nil, stop(lastErr)
		}
		provider, err := rt.providers().GetProviderForPrincipal(target.Provider, caller)
		if err != nil {
			lastErr = err
			attempts = append(attempts, attempt{Provider: target.Provider, Model: target.Model, Error: truncate(err.Error())})
			return nil, judge(err, true)
		}
		var result map[string]any
		if providers.SupportsAnthropicMessages(provider) {
			result, err = providers.CompleteAnthropicMessages(provider, target.Model, payload)
		} else if requiresNative {
			return nil, judge(errChatOnly, true)
		} else if err = rt.anthropicFallbackCompatibility(target, caller, messages, kw); err == nil {
			var chat map[string]any
			chat, err = providers.CompleteProviderContext(ctx, provider, target.Model, messages, kw)
			if err == nil {
				converted := translate.OpenAIResponseToAnthropicWithReport(chat, target.Model)
				if lossErr := providers.RejectMaterialLossExceptThoughtSignatures(converted.Report); lossErr != nil {
					err = &providers.ConfigError{Msg: lossErr.Error()}
				} else {
					result = converted.Value
				}
			}
		}
		if err != nil {
			lastErr = err
			if providers.IsConfig(err) {
				lastStatus = 400
			} else {
				lastStatus = providers.UpstreamStatus(err)
			}
			attempts = append(attempts, attempt{Provider: target.Provider, Model: target.Model, Error: truncate(err.Error()), Throttled: providers.IsThrottle(err)})
			if providers.IsInvocation(err) && !providers.InvocationFailoverEligible(err) {
				lastStatus = providers.UpstreamStatus(err)
				return nil, stop(err)
			}
			return nil, judge(err, true)
		}
		attempts = append(attempts, attempt{Provider: target.Provider, Model: target.Model, OK: true})
		return result, nil
	})
	rt.recordChain(ctx, requested, attempts, served)
	if served != nil {
		return result, served, nil
	}
	message := "no compatible Anthropic Messages target"
	if lastErr != nil {
		message = lastErr.Error()
	}
	return nil, nil, &AllTargetsFailed{Msg: message, Status: lastStatus}
}

// errChatOnly is the failure of a target that a request needing native
// Messages skips, unrecorded: the target has only the Chat adapter, which
// would lose part of the request.
var errChatOnly = errors.New("router: target serves Messages only through the Chat adapter")

func (rt *Runtime) anthropicFallbackCompatibility(target Target, caller core.Caller, messages []map[string]any, kw providers.Kwargs) error {
	if err := rt.anthropicControlsCompatibility(target, caller, kw); err != nil {
		return err
	}
	hasImages := false
	for _, message := range messages {
		parts, _ := message["content"].([]any)
		for _, raw := range parts {
			part, _ := raw.(map[string]any)
			if part["type"] == "image_url" {
				hasImages = true
			}
		}
	}
	if !hasImages {
		return nil
	}
	model, ok := rt.providers().CatalogLookupForPrincipal(target.Provider, target.Model, caller)
	if !ok {
		return &providers.ConfigError{Msg: "Anthropic image adaptation requires verified model vision capability"}
	}
	if !providers.ModelSupportsImageInput(model) {
		return &providers.ConfigError{Msg: "Anthropic image adaptation target is not vision-capable"}
	}
	return nil
}

func (rt *Runtime) anthropicControlsCompatibility(target Target, caller core.Caller, kw providers.Kwargs) error {
	providerConfig := config.Get().Providers[target.Provider]
	if providerConfig == nil {
		return &providers.ConfigError{Msg: "provider is not configured"}
	}
	providerType := strings.ToLower(strings.TrimSpace(providerConfig.Type))
	if providerType == "anthropic" {
		return nil
	}
	if _, present := kw["thinking"]; present {
		if providerType != "github_copilot" {
			return &providers.ConfigError{Msg: "selected provider cannot preserve Anthropic thinking"}
		}
		if providerConfig.ForceApiSupport {
			model, ok := rt.providers().CatalogLookupForPrincipal(target.Provider, target.Model, caller)
			if !ok || translate.PreferredEndpoint(model.SupportedSurfaces) != "chat" {
				return &providers.ConfigError{Msg: "selected provider cannot preserve Anthropic thinking on a Responses-only model"}
			}
		}
	}
	switch providerType {
	case "openai_compatible", "openai", "github_copilot", "bedrock", "litellm", "azure_openai":
		return nil
	}
	if metadata, ok := kw["metadata"].(map[string]any); ok && len(metadata) > 0 {
		return &providers.ConfigError{Msg: "selected provider cannot preserve Anthropic metadata"}
	}
	if kw["reasoning_effort"] != nil {
		return &providers.ConfigError{Msg: "selected provider cannot preserve Anthropic output_config.effort"}
	}
	return nil
}

type ResponsesExecutionStream struct {
	Iter   providers.StreamIter
	Native bool
}

type boundedStream struct {
	providers.StreamIter
	cancel context.CancelFunc
}

func (stream *boundedStream) Next() (string, bool) {
	chunk, ok := stream.StreamIter.Next()
	if !ok {
		stream.cancel()
	}
	return chunk, ok
}

func (stream *boundedStream) Close() error {
	stream.cancel()
	return stream.StreamIter.Close()
}

func (rt *Runtime) ExecuteResponsesStream(
	targets []Target,
	payload map[string]any,
	requested string,
	caller core.Caller,
) (*ResponsesExecutionStream, *Target, error) {
	return rt.ExecuteResponsesStreamContext(context.Background(), targets, payload, requested, caller)
}

func (rt *Runtime) ExecuteResponsesStreamContext(
	ctx context.Context,
	targets []Target,
	payload map[string]any,
	requested string,
	caller core.Caller,
) (*ResponsesExecutionStream, *Target, error) {
	if providers.ResponsesPayloadIsStateful(payload) && len(targets) > 1 {
		targets = targets[:1]
	}
	ctx, cancel, targets := prepareFallback(ctx, targets, nil)
	fallback := translateResponsesFallback(payload)
	var attempts []attempt
	var lastErr error
	lastStatus := 0
	ended := func() error {
		err := ctx.Err()
		if err != nil {
			lastErr, lastStatus = err, deadlineStatus(err)
		}
		return err
	}
	// Like the Chat stream chain, this one commits to the first target whose
	// stream opens; see executeStreamContext.
	opened, served := walk(ctx, targets, func(target Target) (*ResponsesExecutionStream, error) {
		if err := ended(); err != nil {
			return nil, stop(err)
		}
		provider, err := rt.providers().GetProviderForPrincipal(target.Provider, caller)
		if ctxErr := ended(); ctxErr != nil {
			return nil, stop(ctxErr)
		}
		if err != nil {
			attempts = append(attempts, attempt{
				Provider: target.Provider, Model: target.Model,
				Error: truncate(err.Error()), Throttled: providers.IsThrottle(err),
			})
			lastErr = err
			return nil, judge(err, shouldAdvance(err))
		}
		stream, _, err := providers.StreamResponsesContext(ctx, provider, target.Model, payload)
		if ctxErr := ended(); ctxErr != nil {
			if stream != nil {
				_ = stream.Close()
			}
			return nil, stop(ctxErr)
		}
		native := true
		if errors.Is(err, providers.ErrResponsesUnsupported) {
			native = false
			if refusal := rt.responsesFallbackRefusal(target, caller, fallback); refusal != nil {
				lastErr, lastStatus = refusal, 400
				attempts = append(attempts, attempt{
					Provider: target.Provider, Model: target.Model,
					Error: truncate(lastErr.Error()),
				})
				return nil, stop(refusal)
			}
			if ctxErr := ended(); ctxErr != nil {
				return nil, stop(ctxErr)
			}
			stream, err = providers.StreamProviderContext(ctx, provider, target.Model, fallback.messages, fallback.kw)
			if ctxErr := ended(); ctxErr != nil {
				if stream != nil {
					_ = stream.Close()
				}
				return nil, stop(ctxErr)
			}
		}
		if err != nil {
			attempts = append(attempts, attempt{
				Provider: target.Provider, Model: target.Model,
				Error: truncate(err.Error()), Throttled: providers.IsThrottle(err),
			})
			lastErr = err
			lastStatus = providers.UpstreamStatus(err)
			return nil, judge(err, shouldAdvance(err))
		}
		attempts = append(attempts, attempt{
			Provider: target.Provider, Model: target.Model, OK: true,
		})
		return &ResponsesExecutionStream{Iter: stream, Native: native}, nil
	})
	rt.recordChain(ctx, requested, attempts, served)
	if served != nil {
		opened.Iter = &boundedStream{StreamIter: opened.Iter, cancel: cancel}
		return opened, served, nil
	}
	cancel()
	if errors.Is(lastErr, context.Canceled) {
		return nil, nil, context.Canceled
	}
	message := "no failover targets"
	if lastErr != nil {
		message = lastErr.Error()
	}
	return nil, nil, &AllTargetsFailed{
		Msg: message, Status: lastStatus,
	}
}

// responsesFallback is a Responses request translated for the strict Chat
// fallback, which serves it on a target without native Responses.
type responsesFallback struct {
	messages []providers.Message
	kw       providers.Kwargs
	// untranslatable and loss say why the request cannot take the fallback:
	// it does not translate, or its translation loses part of it.
	untranslatable, loss error
}

func translateResponsesFallback(payload map[string]any) responsesFallback {
	conversion, err := translate.ResponsesRequestToChatWithReport(payload)
	return responsesFallback{
		messages: conversion.Value.Messages, kw: conversion.Value.Keywords,
		untranslatable: err, loss: conversion.RejectMaterialLoss(),
	}
}

// responsesFallbackRefusal is why target cannot serve a request through the
// Chat fallback, or nil: a material loss, a request that does not translate,
// or a control the target's provider cannot preserve. A refusal ends the
// chain with a 400.
func (rt *Runtime) responsesFallbackRefusal(target Target, caller core.Caller, fallback responsesFallback) error {
	if fallback.loss != nil {
		return &providers.ConfigError{Msg: fallback.loss.Error()}
	}
	if fallback.untranslatable != nil {
		return &providers.ConfigError{Msg: fallback.untranslatable.Error()}
	}
	return rt.responsesFallbackCompatibility(target, caller, fallback.messages, fallback.kw)
}

func (rt *Runtime) responsesFallbackCompatibility(
	target Target,
	caller core.Caller,
	messages []providers.Message,
	kw providers.Kwargs,
) error {
	providerConfig := config.Get().Providers[target.Provider]
	if providerConfig == nil {
		return &providers.ConfigError{Msg: "provider is not configured"}
	}
	providerType := strings.ToLower(strings.TrimSpace(providerConfig.Type))
	hasImages := false
	for _, message := range messages {
		parts, _ := message["content"].([]any)
		for _, raw := range parts {
			if part, ok := raw.(map[string]any); ok && part["type"] == "image_url" {
				hasImages = true
			}
		}
	}
	if hasImages {
		switch providerType {
		case "openai_compatible", "openai", "github_copilot", "bedrock", "litellm":
			model, ok := rt.providers().CatalogLookupForPrincipal(
				target.Provider, target.Model, caller,
			)
			if !ok {
				return &providers.ConfigError{
					Msg: "image fallback requires verified model capability metadata",
				}
			}
			if !providers.ModelSupportsImageInput(model) {
				return &providers.ConfigError{
					Msg: "selected Chat fallback model is not vision-capable",
				}
			}
		default:
			return &providers.ConfigError{
				Msg: "selected provider cannot preserve Responses image input",
			}
		}
	}
	tools, _ := kw["tools"].([]any)
	if len(tools) > 0 {
		switch providerType {
		case "openai_compatible", "openai", "github_copilot", "bedrock", "litellm",
			"azure_openai", "anthropic", "ollama":
		default:
			return &providers.ConfigError{
				Msg: "selected provider cannot preserve Responses tools",
			}
		}
		if providerType == "anthropic" || providerType == "ollama" {
			if kw["tool_choice"] != nil {
				return &providers.ConfigError{
					Msg: "selected provider cannot preserve Responses tool_choice",
				}
			}
			for _, raw := range tools {
				tool, _ := raw.(map[string]any)
				function, _ := tool["function"].(map[string]any)
				if strict, _ := function["strict"].(bool); strict {
					return &providers.ConfigError{
						Msg: "selected provider cannot preserve strict Responses tools",
					}
				}
			}
		}
	}
	if kw["reasoning_effort"] != nil {
		switch providerType {
		case "openai_compatible", "openai", "github_copilot", "bedrock", "litellm", "azure_openai":
		default:
			return &providers.ConfigError{
				Msg: "selected provider cannot preserve Responses reasoning controls",
			}
		}
	}
	if metadata, ok := kw["metadata"].(map[string]any); ok && len(metadata) > 0 {
		switch providerType {
		case "openai_compatible", "openai", "github_copilot", "bedrock", "litellm", "azure_openai":
		default:
			return &providers.ConfigError{
				Msg: "selected provider cannot preserve Responses metadata",
			}
		}
	}
	if _, present := kw["parallel_tool_calls"]; present {
		switch providerType {
		case "openai_compatible", "openai", "github_copilot", "bedrock", "litellm", "azure_openai":
		default:
			return &providers.ConfigError{Msg: "selected provider cannot preserve Responses parallel_tool_calls"}
		}
	}
	return nil
}

func (rt *Runtime) executeCompleteWithTrace(ctx context.Context, targets []Target, messages []providers.Message, requested string, caller core.Caller, kw providers.Kwargs) (map[string]any, *Target, []AttemptTrace, error) {
	ctx, cancel, targets := prepareFallback(ctx, targets, kw)
	defer cancel()
	var attempts []attempt
	trace := make([]AttemptTrace, 0, len(targets))
	var lastErr error
	failed := func(t Target, attemptStarted time.Time, err error) error {
		throttled := providers.IsThrottle(err)
		attempts = append(attempts, attempt{Provider: t.Provider, Model: t.Model, OK: false, Error: truncate(err.Error()), Throttled: throttled})
		trace = append(trace, AttemptTrace{Provider: t.Provider, Model: t.Model, Status: "failed", Throttled: throttled, DurationMS: time.Since(attemptStarted).Milliseconds()})
		lastErr = err
		return judge(err, shouldAdvance(err))
	}
	result, served := walk(ctx, targets, func(t Target) (map[string]any, error) {
		if ctx.Err() != nil {
			lastErr = ctx.Err()
			return nil, stop(lastErr)
		}
		attemptStarted := time.Now()
		prov, err := rt.providers().GetProviderForPrincipal(t.Provider, caller)
		if err != nil {
			return nil, failed(t, attemptStarted, err)
		}
		result, err := providers.CompleteProviderContext(ctx, prov, t.Model, messages, kw)
		if err != nil {
			return nil, failed(t, attemptStarted, err)
		}
		attempts = append(attempts, attempt{Provider: t.Provider, Model: t.Model, OK: true})
		trace = append(trace, AttemptTrace{Provider: t.Provider, Model: t.Model, Status: "served", DurationMS: time.Since(attemptStarted).Milliseconds()})
		return result, nil
	})
	rt.recordChain(ctx, requested, attempts, served)
	if served != nil {
		return result, served, trace, nil
	}
	if errors.Is(lastErr, context.Canceled) {
		return nil, nil, trace, context.Canceled
	}
	msg := "no failover targets"
	if lastErr != nil {
		msg = lastErr.Error()
	}
	return nil, nil, trace, &AllTargetsFailed{Msg: msg, Status: deadlineStatus(lastErr)}
}

// ExecuteStream runs the chain for a streaming request, failing over pre-first-byte.
func (rt *Runtime) ExecuteStream(targets []Target, messages []providers.Message, requested string, caller core.Caller, kw providers.Kwargs) (providers.StreamIter, *Target, error) {
	return rt.ExecuteStreamContext(context.Background(), targets, messages, requested, caller, kw)
}

func (rt *Runtime) ExecuteStreamContext(ctx context.Context, targets []Target, messages []providers.Message, requested string, caller core.Caller, kw providers.Kwargs) (providers.StreamIter, *Target, error) {
	return rt.executeStreamContext(ctx, targets, messages, requested, caller, kw, nil)
}

func (rt *Runtime) ExecuteAnthropicStreamContext(ctx context.Context, targets []Target, messages []providers.Message, requested string, caller core.Caller, kw providers.Kwargs) (providers.StreamIter, *Target, error) {
	return rt.executeStreamContext(ctx, targets, messages, requested, caller, kw, func(target Target) error {
		return rt.anthropicControlsCompatibility(target, caller, kw)
	})
}

// executeStreamContext commits to the first target whose stream opens. That
// is the failover boundary the gateway documents: a request moves to the next
// target only before the first response byte, and the API layer writes the
// status line and headers as soon as a stream is returned. So the chain walks
// on Execute with a try that opens the stream, not on ExecuteStream, which
// holds frames back until one carries output and would fail over a stream
// that broke after its upstream answered.
func (rt *Runtime) executeStreamContext(ctx context.Context, targets []Target, messages []providers.Message, requested string, caller core.Caller, kw providers.Kwargs, validate func(Target) error) (providers.StreamIter, *Target, error) {
	ctx, cancel, targets := prepareFallback(ctx, targets, kw)
	var attempts []attempt
	var lastErr error
	lastStatus := 0
	ended := func() error {
		err := ctx.Err()
		if err != nil {
			lastErr, lastStatus = err, deadlineStatus(err)
		}
		return err
	}
	failed := func(t Target, err error) error {
		attempts = append(attempts, attempt{Provider: t.Provider, Model: t.Model, OK: false, Error: truncate(err.Error()), Throttled: providers.IsThrottle(err)})
		lastErr = err
		lastStatus = providers.UpstreamStatus(err)
		return judge(err, shouldAdvance(err))
	}
	it, served := walk(ctx, targets, func(t Target) (providers.StreamIter, error) {
		if err := ended(); err != nil {
			return nil, stop(err)
		}
		if validate != nil {
			if err := validate(t); err != nil {
				attempts = append(attempts, attempt{Provider: t.Provider, Model: t.Model, Error: truncate(err.Error())})
				lastErr = err
				lastStatus = 400
				return nil, judge(err, true)
			}
		}
		prov, err := rt.providers().GetProviderForPrincipal(t.Provider, caller)
		if ctxErr := ended(); ctxErr != nil {
			return nil, stop(ctxErr)
		}
		if err != nil {
			return nil, failed(t, err)
		}
		it, err := providers.StreamProviderContext(ctx, prov, t.Model, messages, kw)
		if ctxErr := ended(); ctxErr != nil {
			if it != nil {
				_ = it.Close()
			}
			return nil, stop(ctxErr)
		}
		if err != nil {
			return nil, failed(t, err)
		}
		attempts = append(attempts, attempt{Provider: t.Provider, Model: t.Model, OK: true})
		return it, nil
	})
	rt.recordChain(ctx, requested, attempts, served)
	if served != nil {
		return &boundedStream{StreamIter: it, cancel: cancel}, served, nil
	}
	cancel()
	if errors.Is(lastErr, context.Canceled) {
		return nil, nil, context.Canceled
	}
	msg := "no failover targets"
	if lastErr != nil {
		msg = lastErr.Error()
	}
	return nil, nil, &AllTargetsFailed{Msg: msg, Status: lastStatus}
}

func truncate(s string) string {
	return providers.SanitizeDiagnosticTextLimit(s, 200)
}
