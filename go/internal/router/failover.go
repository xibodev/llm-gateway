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
func FilterCompatibleTargets(targets []Target, caller core.Caller, request CompatibilityRequest) ([]Target, error) {
	compatible := make([]Target, 0, len(targets))
	for _, target := range targets {
		model, ok := providers.CatalogCachedLookupForPrincipal(target.Provider, target.Model, caller)
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
func ResolveTargets(model string) ([]Target, error) {
	return ResolveTargetsForPrincipal(context.Background(), model, core.Caller{Kind: core.CallerAnonymous})
}

func ResolveTargetsForPrincipal(
	ctx context.Context, model string, caller core.Caller,
) ([]Target, error) {
	resolution, err := ResolveForPrincipal(ctx, model, caller)
	return resolution.Targets, err
}

// ResolveForPrincipal maps a requested model to an ordered failover chain and
// preserves the exact configured category name used by the router.
// Disabled-only routes retain their targets for authorization; callers must
// check availability after policy enforcement and before executing the chain.
// A route outside the key's allowlist, or anything but a route for a
// routes-only key, reads as model-not-found.
func ResolveForPrincipal(
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
			published, err := routeMemberPublished(m, caller)
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
				published, err := routeMemberPublished(m, caller)
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
				if _, found := providers.CatalogCachedLookupForPrincipal(head, tail, caller); !found {
					return Resolution{}, &ModelNotFoundError{Requested: name, Unavailable: true}
				}
			}
			if _, published, evidenceErr := providers.AnonymousModelPublication(head, tail); evidenceErr != nil || !published {
				return Resolution{}, &ModelNotFoundError{Requested: name, Unavailable: true}
			}
			return Resolution{Targets: []Target{{Provider: head, Model: tail}}}, nil
		}
	}
	if t, ok, err := resolveNativeAlias(ctx, name, caller); err != nil {
		return Resolution{}, err
	} else if ok {
		return Resolution{Targets: []Target{t}}, nil
	}
	return Resolution{}, &ModelNotFoundError{Requested: name}
}

func routeMemberPublished(member config.EndpointMember, caller core.Caller) (bool, error) {
	if providers.CatalogRequiresPrincipal(member.Provider) {
		authorized, err := providers.ProviderCredentialAuthorized(member.Provider, caller)
		if err != nil {
			return false, err
		}
		if !authorized {
			return false, nil
		}
		if _, found := providers.CatalogCachedLookupForPrincipal(member.Provider, member.Model, caller); !found {
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
func resolveNativeAlias(
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
	candidates, err := NativeAliasCandidates(ctx, caller)
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
func NativeAliasCandidates(ctx context.Context, caller core.Caller) (map[string][]Target, error) {
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
		models := append([]providers.ModelInfo(nil), providers.CatalogModelsForPrincipal(pid, caller)...)
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

func recordChain(ctx context.Context, requested string, attempts []attempt, served *Target) {
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
	recordTelemetryEvent(requested, toEventAttempts(attempts), sp, sm, project, key)
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
func ExecuteComplete(targets []Target, messages []providers.Message, requested string, caller core.Caller, kw providers.Kwargs) (map[string]any, *Target, error) {
	return ExecuteCompleteContext(context.Background(), targets, messages, requested, caller, kw)
}

func ExecuteCompleteContext(ctx context.Context, targets []Target, messages []providers.Message, requested string, caller core.Caller, kw providers.Kwargs) (map[string]any, *Target, error) {
	response, served, _, err := executeCompleteWithTrace(ctx, targets, messages, requested, caller, kw)
	return response, served, err
}

// ExecuteCompleteWithTrace uses the same route/provider execution path as
// ExecuteComplete while returning a safe fallback trace for operator tooling.
func ExecuteCompleteWithTrace(targets []Target, messages []providers.Message, requested string, caller core.Caller, kw providers.Kwargs) (map[string]any, *Target, []AttemptTrace, error) {
	return executeCompleteWithTrace(context.Background(), targets, messages, requested, caller, kw)
}

func ExecuteCompleteWithTraceContext(ctx context.Context, targets []Target, messages []providers.Message, requested string, caller core.Caller, kw providers.Kwargs) (map[string]any, *Target, []AttemptTrace, error) {
	return executeCompleteWithTrace(ctx, targets, messages, requested, caller, kw)
}

// ExecuteResponses preserves the caller's Responses API intent when a target
// supports it natively, falling back through an explicit loss-checked Chat
// adapter only for Chat-only providers.
func ExecuteResponses(
	targets []Target,
	payload map[string]any,
	requested string,
	caller core.Caller,
) (map[string]any, *Target, error) {
	return ExecuteResponsesContext(context.Background(), targets, payload, requested, caller)
}

func ExecuteResponsesContext(
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
	conversion, conversionErr := translate.ResponsesRequestToChatWithReport(payload)
	chatMessages, chatKw := conversion.Value.Messages, conversion.Value.Keywords
	materialErr := conversion.RejectMaterialLoss()
	var attempts []attempt
	var lastErr error
	lastStatus := 0
	for index := range targets {
		if ctx.Err() != nil {
			lastErr = ctx.Err()
			lastStatus = deadlineStatus(lastErr)
			break
		}
		target := targets[index]
		provider, err := providers.GetProviderForPrincipal(target.Provider, caller)
		if err != nil {
			attempts = append(attempts, attempt{
				Provider: target.Provider, Model: target.Model,
				Error: truncate(err.Error()), Throttled: providers.IsThrottle(err),
			})
			lastErr = err
			if shouldAdvance(err) {
				continue
			}
			break
		}
		result, _, err := providers.CompleteResponsesContext(ctx, provider, target.Model, payload)
		if errors.Is(err, providers.ErrResponsesUnsupported) {
			if materialErr != nil {
				lastErr = &providers.ConfigError{Msg: materialErr.Error()}
				lastStatus = 400
				attempts = append(attempts, attempt{
					Provider: target.Provider, Model: target.Model,
					Error: truncate(lastErr.Error()),
				})
				break
			}
			if conversionErr != nil {
				lastErr = &providers.ConfigError{Msg: conversionErr.Error()}
				lastStatus = 400
				attempts = append(attempts, attempt{
					Provider: target.Provider, Model: target.Model,
					Error: truncate(lastErr.Error()),
				})
				break
			}
			if compatibilityErr := responsesFallbackCompatibility(
				target, caller, chatMessages, chatKw,
			); compatibilityErr != nil {
				lastErr = compatibilityErr
				lastStatus = 400
				attempts = append(attempts, attempt{
					Provider: target.Provider, Model: target.Model,
					Error: truncate(lastErr.Error()),
				})
				break
			}
			chat, chatErr := providers.CompleteProviderContext(ctx, provider, target.Model, chatMessages, chatKw)
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
			if shouldAdvance(err) {
				continue
			}
			break
		}
		result["model"] = target.Model
		attempts = append(attempts, attempt{
			Provider: target.Provider, Model: target.Model, OK: true,
		})
		served := target
		recordChain(ctx, requested, attempts, &served)
		return result, &served, nil
	}
	recordChain(ctx, requested, attempts, nil)
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
func ExecuteAnthropicMessages(
	targets []Target,
	payload map[string]any,
	requested string,
	caller core.Caller,
) (map[string]any, *Target, error) {
	return ExecuteAnthropicMessagesContext(context.Background(), targets, payload, requested, caller)
}

func ExecuteAnthropicMessagesContext(
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
			provider, err := providers.GetProviderForPrincipal(target.Provider, caller)
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
	for _, target := range targets {
		if ctx.Err() != nil {
			lastErr = ctx.Err()
			break
		}
		provider, err := providers.GetProviderForPrincipal(target.Provider, caller)
		if err != nil {
			lastErr = err
			attempts = append(attempts, attempt{Provider: target.Provider, Model: target.Model, Error: truncate(err.Error())})
			continue
		}
		var result map[string]any
		if providers.SupportsAnthropicMessages(provider) {
			result, err = providers.CompleteAnthropicMessages(provider, target.Model, payload)
		} else if requiresNative {
			continue
		} else if err = anthropicFallbackCompatibility(target, caller, messages, kw); err == nil {
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
				recordChain(ctx, requested, attempts, nil)
				return nil, nil, &AllTargetsFailed{Msg: err.Error(), Status: providers.UpstreamStatus(err)}
			}
			continue
		}
		attempts = append(attempts, attempt{Provider: target.Provider, Model: target.Model, OK: true})
		served := target
		recordChain(ctx, requested, attempts, &served)
		return result, &served, nil
	}
	recordChain(ctx, requested, attempts, nil)
	message := "no compatible Anthropic Messages target"
	if lastErr != nil {
		message = lastErr.Error()
	}
	return nil, nil, &AllTargetsFailed{Msg: message, Status: lastStatus}
}

func anthropicFallbackCompatibility(target Target, caller core.Caller, messages []map[string]any, kw providers.Kwargs) error {
	if err := anthropicControlsCompatibility(target, caller, kw); err != nil {
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
	model, ok := providers.CatalogLookupForPrincipal(target.Provider, target.Model, caller)
	if !ok {
		return &providers.ConfigError{Msg: "Anthropic image adaptation requires verified model vision capability"}
	}
	if !providers.ModelSupportsImageInput(model) {
		return &providers.ConfigError{Msg: "Anthropic image adaptation target is not vision-capable"}
	}
	return nil
}

func anthropicControlsCompatibility(target Target, caller core.Caller, kw providers.Kwargs) error {
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
			model, ok := providers.CatalogLookupForPrincipal(target.Provider, target.Model, caller)
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

func ExecuteResponsesStream(
	targets []Target,
	payload map[string]any,
	requested string,
	caller core.Caller,
) (*ResponsesExecutionStream, *Target, error) {
	return ExecuteResponsesStreamContext(context.Background(), targets, payload, requested, caller)
}

func ExecuteResponsesStreamContext(
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
	conversion, conversionErr := translate.ResponsesRequestToChatWithReport(payload)
	chatMessages, chatKw := conversion.Value.Messages, conversion.Value.Keywords
	materialErr := conversion.RejectMaterialLoss()
	var attempts []attempt
	var lastErr error
	lastStatus := 0
	for index := range targets {
		if err := ctx.Err(); err != nil {
			lastErr = err
			lastStatus = deadlineStatus(err)
			break
		}
		target := targets[index]
		provider, err := providers.GetProviderForPrincipal(target.Provider, caller)
		if ctxErr := ctx.Err(); ctxErr != nil {
			lastErr = ctxErr
			lastStatus = deadlineStatus(ctxErr)
			break
		}
		if err != nil {
			attempts = append(attempts, attempt{
				Provider: target.Provider, Model: target.Model,
				Error: truncate(err.Error()), Throttled: providers.IsThrottle(err),
			})
			lastErr = err
			if shouldAdvance(err) {
				continue
			}
			break
		}
		stream, _, err := providers.StreamResponsesContext(ctx, provider, target.Model, payload)
		if ctxErr := ctx.Err(); ctxErr != nil {
			if stream != nil {
				_ = stream.Close()
			}
			lastErr = ctxErr
			lastStatus = deadlineStatus(ctxErr)
			break
		}
		native := true
		if errors.Is(err, providers.ErrResponsesUnsupported) {
			native = false
			if materialErr != nil {
				lastErr = &providers.ConfigError{Msg: materialErr.Error()}
				lastStatus = 400
				attempts = append(attempts, attempt{
					Provider: target.Provider, Model: target.Model,
					Error: truncate(lastErr.Error()),
				})
				break
			}
			if conversionErr != nil {
				lastErr = &providers.ConfigError{Msg: conversionErr.Error()}
				lastStatus = 400
				attempts = append(attempts, attempt{
					Provider: target.Provider, Model: target.Model,
					Error: truncate(lastErr.Error()),
				})
				break
			}
			if compatibilityErr := responsesFallbackCompatibility(
				target, caller, chatMessages, chatKw,
			); compatibilityErr != nil {
				lastErr = compatibilityErr
				lastStatus = 400
				attempts = append(attempts, attempt{
					Provider: target.Provider, Model: target.Model,
					Error: truncate(lastErr.Error()),
				})
				break
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				lastErr = ctxErr
				lastStatus = deadlineStatus(ctxErr)
				break
			}
			stream, err = providers.StreamProviderContext(ctx, provider, target.Model, chatMessages, chatKw)
			if ctxErr := ctx.Err(); ctxErr != nil {
				if stream != nil {
					_ = stream.Close()
				}
				lastErr = ctxErr
				lastStatus = deadlineStatus(ctxErr)
				break
			}
		}
		if err != nil {
			attempts = append(attempts, attempt{
				Provider: target.Provider, Model: target.Model,
				Error: truncate(err.Error()), Throttled: providers.IsThrottle(err),
			})
			lastErr = err
			lastStatus = providers.UpstreamStatus(err)
			if shouldAdvance(err) {
				continue
			}
			break
		}
		attempts = append(attempts, attempt{
			Provider: target.Provider, Model: target.Model, OK: true,
		})
		served := target
		recordChain(ctx, requested, attempts, &served)
		return &ResponsesExecutionStream{Iter: &boundedStream{StreamIter: stream, cancel: cancel}, Native: native}, &served, nil
	}
	cancel()
	recordChain(ctx, requested, attempts, nil)
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

func responsesFallbackCompatibility(
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
			model, ok := providers.CatalogLookupForPrincipal(
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

func executeCompleteWithTrace(ctx context.Context, targets []Target, messages []providers.Message, requested string, caller core.Caller, kw providers.Kwargs) (map[string]any, *Target, []AttemptTrace, error) {
	ctx, cancel, targets := prepareFallback(ctx, targets, kw)
	defer cancel()
	var attempts []attempt
	trace := make([]AttemptTrace, 0, len(targets))
	var lastErr error
	for i := range targets {
		if ctx.Err() != nil {
			lastErr = ctx.Err()
			break
		}
		t := targets[i]
		attemptStarted := time.Now()
		prov, err := providers.GetProviderForPrincipal(t.Provider, caller)
		if err != nil {
			throttled := providers.IsThrottle(err)
			attempts = append(attempts, attempt{Provider: t.Provider, Model: t.Model, OK: false, Error: truncate(err.Error()), Throttled: throttled})
			trace = append(trace, AttemptTrace{Provider: t.Provider, Model: t.Model, Status: "failed", Throttled: throttled, DurationMS: time.Since(attemptStarted).Milliseconds()})
			lastErr = err
			if shouldAdvance(err) {
				continue
			}
			break
		}
		result, err := providers.CompleteProviderContext(ctx, prov, t.Model, messages, kw)
		if err != nil {
			throttled := providers.IsThrottle(err)
			attempts = append(attempts, attempt{Provider: t.Provider, Model: t.Model, OK: false, Error: truncate(err.Error()), Throttled: throttled})
			trace = append(trace, AttemptTrace{Provider: t.Provider, Model: t.Model, Status: "failed", Throttled: throttled, DurationMS: time.Since(attemptStarted).Milliseconds()})
			lastErr = err
			if shouldAdvance(err) {
				continue
			}
			break
		}
		attempts = append(attempts, attempt{Provider: t.Provider, Model: t.Model, OK: true})
		trace = append(trace, AttemptTrace{Provider: t.Provider, Model: t.Model, Status: "served", DurationMS: time.Since(attemptStarted).Milliseconds()})
		served := t
		recordChain(ctx, requested, attempts, &served)
		return result, &served, trace, nil
	}
	recordChain(ctx, requested, attempts, nil)
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
func ExecuteStream(targets []Target, messages []providers.Message, requested string, caller core.Caller, kw providers.Kwargs) (providers.StreamIter, *Target, error) {
	return ExecuteStreamContext(context.Background(), targets, messages, requested, caller, kw)
}

func ExecuteStreamContext(ctx context.Context, targets []Target, messages []providers.Message, requested string, caller core.Caller, kw providers.Kwargs) (providers.StreamIter, *Target, error) {
	return executeStreamContext(ctx, targets, messages, requested, caller, kw, nil)
}

func ExecuteAnthropicStreamContext(ctx context.Context, targets []Target, messages []providers.Message, requested string, caller core.Caller, kw providers.Kwargs) (providers.StreamIter, *Target, error) {
	return executeStreamContext(ctx, targets, messages, requested, caller, kw, func(target Target) error {
		return anthropicControlsCompatibility(target, caller, kw)
	})
}

func executeStreamContext(ctx context.Context, targets []Target, messages []providers.Message, requested string, caller core.Caller, kw providers.Kwargs, validate func(Target) error) (providers.StreamIter, *Target, error) {
	ctx, cancel, targets := prepareFallback(ctx, targets, kw)
	var attempts []attempt
	var lastErr error
	lastStatus := 0
	for i := range targets {
		if err := ctx.Err(); err != nil {
			lastErr = err
			lastStatus = deadlineStatus(err)
			break
		}
		t := targets[i]
		if validate != nil {
			if err := validate(t); err != nil {
				attempts = append(attempts, attempt{Provider: t.Provider, Model: t.Model, Error: truncate(err.Error())})
				lastErr = err
				lastStatus = 400
				continue
			}
		}
		prov, err := providers.GetProviderForPrincipal(t.Provider, caller)
		if ctxErr := ctx.Err(); ctxErr != nil {
			lastErr = ctxErr
			lastStatus = deadlineStatus(ctxErr)
			break
		}
		if err != nil {
			attempts = append(attempts, attempt{Provider: t.Provider, Model: t.Model, OK: false, Error: truncate(err.Error()), Throttled: providers.IsThrottle(err)})
			lastErr = err
			lastStatus = providers.UpstreamStatus(err)
			if shouldAdvance(err) {
				continue
			}
			break
		}
		it, err := providers.StreamProviderContext(ctx, prov, t.Model, messages, kw)
		if ctxErr := ctx.Err(); ctxErr != nil {
			if it != nil {
				_ = it.Close()
			}
			lastErr = ctxErr
			lastStatus = deadlineStatus(ctxErr)
			break
		}
		if err != nil {
			attempts = append(attempts, attempt{Provider: t.Provider, Model: t.Model, OK: false, Error: truncate(err.Error()), Throttled: providers.IsThrottle(err)})
			lastErr = err
			lastStatus = providers.UpstreamStatus(err)
			if shouldAdvance(err) {
				continue
			}
			break
		}
		attempts = append(attempts, attempt{Provider: t.Provider, Model: t.Model, OK: true})
		served := t
		recordChain(ctx, requested, attempts, &served)
		return &boundedStream{StreamIter: it, cancel: cancel}, &served, nil
	}
	cancel()
	recordChain(ctx, requested, attempts, nil)
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
