// Package router resolves a requested model to a failover chain and executes it,
// recording usage (savings ledger) and throttle/fallback events (telemetry).
package router

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
	"llmgw/internal/translate"
)

// Target is one resolved provider/model in a chain.
type Target struct {
	Provider string
	Model    string
}

// Resolution records both the resolved targets and the canonical category name,
// when the request addressed a category rather than a direct model.
type Resolution struct {
	Targets  []Target
	Category string
}

// ModelNotFoundError means the requested model is neither a category nor a
// provider/model id (HTTP 404).
type ModelNotFoundError struct{ Requested string }

// The wording is client-visible prose, written verbatim into a 404 body, and it
// is the only sentence most users ever read about this concept — so it uses the
// product's current vocabulary ("endpoint", matching owned_by: "endpoint" on the
// rows GET /v1/models returns). The internal identifiers around it still say
// "category"; renaming those is a separate, non-user-visible change.
func (e *ModelNotFoundError) Error() string {
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
	return ResolveTargetsForPrincipal(model, nil)
}

func ResolveTargetsForPrincipal(
	model string, principal *config.Principal,
) ([]Target, error) {
	resolution, err := ResolveForPrincipal(model, principal)
	return resolution.Targets, err
}

// ResolveForPrincipal maps a requested model to an ordered failover chain and
// preserves the exact configured category name used by the router.
func ResolveForPrincipal(
	model string, principal *config.Principal,
) (Resolution, error) {
	name := strings.TrimSpace(model)
	if name == "" {
		return Resolution{}, &ModelNotFoundError{Requested: model}
	}
	categoryName, cat, err := findCategory(name)
	if err != nil {
		return Resolution{}, err
	}
	if cat != nil {
		if len(cat.Failover) == 0 {
			return Resolution{}, &ModelNotFoundError{Requested: name}
		}
		out := make([]Target, len(cat.Failover))
		for i, m := range cat.Failover {
			out[i] = Target{Provider: m.Provider, Model: m.Model}
		}
		return Resolution{Targets: out, Category: categoryName}, nil
	}
	if head, tail, ok := strings.Cut(name, "/"); ok {
		if _, exists := config.Get().Providers[head]; exists && tail != "" {
			return Resolution{Targets: []Target{{Provider: head, Model: tail}}}, nil
		}
	}
	if t, ok, err := resolveNativeAlias(name, principal); err != nil {
		return Resolution{}, err
	} else if ok {
		return Resolution{Targets: []Target{t}}, nil
	}
	return Resolution{}, &ModelNotFoundError{Requested: name}
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
	requested string, principal *config.Principal,
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
	candidates, err := NativeAliasCandidates(principal)
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
// targets grouped by the canonical key used for bare-name resolution.
func NativeAliasCandidates(principal *config.Principal) (map[string][]Target, error) {
	s := config.Get()
	project := iam.ProjectPolicy{}
	if principal != nil && principal.ProjectID != "" {
		var err error
		project, err = iam.GetProjectPolicy(principal.ProjectID)
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
		if !aliasProviderAllowed(principal, project, pid) {
			continue
		}
		authorized, err := providers.ProviderCredentialAuthorized(pid, principal)
		if err != nil {
			return nil, err
		}
		if !authorized {
			continue
		}
		models := append([]providers.ModelInfo(nil), providers.CatalogModelsForPrincipal(pid, principal)...)
		sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
		for _, m := range models {
			aliases := aliasIDs(m.ID, s.AnthropicDiscoveryAllModels)
			policyCandidates := append([]string{pid + "/" + m.ID, m.ID}, aliases...)
			if principal != nil && (!aliasModelAllowed(principal.AllowedModels, policyCandidates...) ||
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

func aliasProviderAllowed(principal *config.Principal, project iam.ProjectPolicy, provider string) bool {
	return principal == nil ||
		(aliasModelAllowed(principal.AllowedProviders, provider) && aliasModelAllowed(project.AllowedProviders, provider))
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

func recordChain(requested string, attempts []attempt, served *Target, principal *config.Principal) {
	// Only log interesting chains: a failure occurred, or >1 attempt.
	if len(attempts) == 0 || (len(attempts) == 1 && attempts[0].OK) {
		return
	}
	var sp, sm, project, key string
	if served != nil {
		sp, sm = served.Provider, served.Model
	}
	if principal != nil {
		project, key = principal.Project, principal.Key
	}
	recordTelemetryEvent(requested, toEventAttempts(attempts), sp, sm, project, key)
}

// AttemptTrace is a secret-free record of one complete-request routing attempt.
type AttemptTrace struct {
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	Status    string `json:"status"`
	Throttled bool   `json:"throttled,omitempty"`
}

// ExecuteComplete runs the chain for a non-streaming request. Returns the
// response, the served target, and an error if all targets failed.
func ExecuteComplete(targets []Target, messages []providers.Message, requested string, principal *config.Principal, kw providers.Kwargs) (map[string]any, *Target, error) {
	response, served, _, err := executeCompleteWithTrace(targets, messages, requested, principal, kw)
	return response, served, err
}

// ExecuteCompleteWithTrace uses the same route/provider execution path as
// ExecuteComplete while returning a safe fallback trace for operator tooling.
func ExecuteCompleteWithTrace(targets []Target, messages []providers.Message, requested string, principal *config.Principal, kw providers.Kwargs) (map[string]any, *Target, []AttemptTrace, error) {
	return executeCompleteWithTrace(targets, messages, requested, principal, kw)
}

// ExecuteResponses preserves the caller's Responses API intent when a target
// supports it natively, falling back through an explicit loss-checked Chat
// adapter only for Chat-only providers.
func ExecuteResponses(
	targets []Target,
	payload map[string]any,
	requested string,
	principal *config.Principal,
) (map[string]any, *Target, error) {
	if providers.ResponsesPayloadIsStateful(payload) && len(targets) > 1 {
		targets = targets[:1]
	}
	chatMessages, chatKw, conversionErr := translate.ResponsesRequestToChat(payload)
	var attempts []attempt
	var lastErr error
	lastStatus := 0
	for index := range targets {
		target := targets[index]
		provider, err := providers.GetProviderForPrincipal(target.Provider, principal)
		if err != nil {
			attempts = append(attempts, attempt{
				Provider: target.Provider, Model: target.Model,
				Error: truncate(err.Error()), Throttled: providers.IsThrottle(err),
			})
			lastErr = err
			continue
		}
		result, _, err := providers.CompleteResponses(provider, target.Model, payload)
		if errors.Is(err, providers.ErrResponsesUnsupported) {
			if conversionErr != nil {
				lastErr = &providers.ConfigError{Msg: conversionErr.Error()}
				lastStatus = 400
				attempts = append(attempts, attempt{
					Provider: target.Provider, Model: target.Model,
					Error: truncate(lastErr.Error()),
				})
				continue
			}
			if compatibilityErr := responsesFallbackCompatibility(
				target, principal, chatMessages, chatKw,
			); compatibilityErr != nil {
				lastErr = compatibilityErr
				lastStatus = 400
				attempts = append(attempts, attempt{
					Provider: target.Provider, Model: target.Model,
					Error: truncate(lastErr.Error()),
				})
				continue
			}
			chat, chatErr := provider.Complete(target.Model, chatMessages, chatKw)
			if chatErr == nil {
				result = translate.ChatResponseToResponsesWithRequest(
					target.Model, chat, payload,
				)
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
			continue
		}
		result["model"] = target.Model
		attempts = append(attempts, attempt{
			Provider: target.Provider, Model: target.Model, OK: true,
		})
		served := target
		recordChain(requested, attempts, &served, principal)
		return result, &served, nil
	}
	recordChain(requested, attempts, nil, principal)
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
	principal *config.Principal,
) (map[string]any, *Target, error) {
	messages, kw, incompatible := translate.AnthropicRequestToOpenAI(payload)
	requiresNative := len(incompatible) > 0
	if requiresNative {
		hasNative := false
		for _, target := range targets {
			provider, err := providers.GetProviderForPrincipal(target.Provider, principal)
			if err == nil && providers.SupportsAnthropicMessages(provider) {
				hasNative = true
				break
			}
		}
		if !hasNative {
			return nil, nil, &AllTargetsFailed{Msg: "Anthropic request requires a native Messages target; unsupported fields: " + strings.Join(incompatible, ", "), Status: 400}
		}
	}
	var attempts []attempt
	var lastErr error
	lastStatus := 0
	for _, target := range targets {
		provider, err := providers.GetProviderForPrincipal(target.Provider, principal)
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
		} else if err = anthropicFallbackCompatibility(target, principal, messages); err == nil {
			var chat map[string]any
			chat, err = provider.Complete(target.Model, messages, kw)
			if err == nil {
				result = translate.OpenAIResponseToAnthropic(chat, target.Model)
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
			if providers.IsInvocation(err) && !providers.AnthropicMessagesRetryable(err) {
				recordChain(requested, attempts, nil, principal)
				return nil, nil, &AllTargetsFailed{Msg: err.Error(), Status: providers.UpstreamStatus(err)}
			}
			continue
		}
		attempts = append(attempts, attempt{Provider: target.Provider, Model: target.Model, OK: true})
		served := target
		recordChain(requested, attempts, &served, principal)
		return result, &served, nil
	}
	recordChain(requested, attempts, nil, principal)
	message := "no compatible Anthropic Messages target"
	if lastErr != nil {
		message = lastErr.Error()
	}
	return nil, nil, &AllTargetsFailed{Msg: message, Status: lastStatus}
}

func anthropicFallbackCompatibility(target Target, principal *config.Principal, messages []map[string]any) error {
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
	model, ok := providers.CatalogLookupForPrincipal(target.Provider, target.Model, principal)
	if !ok || model.Capabilities == nil {
		return &providers.ConfigError{Msg: "Anthropic image adaptation requires verified model vision capability"}
	}
	if vision, _ := model.Capabilities["vision"].(bool); !vision {
		return &providers.ConfigError{Msg: "Anthropic image adaptation target is not vision-capable"}
	}
	return nil
}

type ResponsesExecutionStream struct {
	Iter   providers.StreamIter
	Native bool
}

func ExecuteResponsesStream(
	targets []Target,
	payload map[string]any,
	requested string,
	principal *config.Principal,
) (*ResponsesExecutionStream, *Target, error) {
	if providers.ResponsesPayloadIsStateful(payload) && len(targets) > 1 {
		targets = targets[:1]
	}
	chatMessages, chatKw, conversionErr := translate.ResponsesRequestToChat(payload)
	var attempts []attempt
	var lastErr error
	lastStatus := 0
	for index := range targets {
		target := targets[index]
		provider, err := providers.GetProviderForPrincipal(target.Provider, principal)
		if err != nil {
			attempts = append(attempts, attempt{
				Provider: target.Provider, Model: target.Model,
				Error: truncate(err.Error()), Throttled: providers.IsThrottle(err),
			})
			lastErr = err
			continue
		}
		stream, _, err := providers.StreamResponses(provider, target.Model, payload)
		native := true
		if errors.Is(err, providers.ErrResponsesUnsupported) {
			native = false
			if conversionErr != nil {
				lastErr = &providers.ConfigError{Msg: conversionErr.Error()}
				lastStatus = 400
				attempts = append(attempts, attempt{
					Provider: target.Provider, Model: target.Model,
					Error: truncate(lastErr.Error()),
				})
				continue
			}
			if compatibilityErr := responsesFallbackCompatibility(
				target, principal, chatMessages, chatKw,
			); compatibilityErr != nil {
				lastErr = compatibilityErr
				lastStatus = 400
				attempts = append(attempts, attempt{
					Provider: target.Provider, Model: target.Model,
					Error: truncate(lastErr.Error()),
				})
				continue
			}
			stream, err = provider.Stream(target.Model, chatMessages, chatKw)
		}
		if err != nil {
			attempts = append(attempts, attempt{
				Provider: target.Provider, Model: target.Model,
				Error: truncate(err.Error()), Throttled: providers.IsThrottle(err),
			})
			lastErr = err
			lastStatus = providers.UpstreamStatus(err)
			continue
		}
		attempts = append(attempts, attempt{
			Provider: target.Provider, Model: target.Model, OK: true,
		})
		served := target
		recordChain(requested, attempts, &served, principal)
		return &ResponsesExecutionStream{Iter: stream, Native: native}, &served, nil
	}
	recordChain(requested, attempts, nil, principal)
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
	principal *config.Principal,
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
				target.Provider, target.Model, principal,
			)
			if !ok || model.Capabilities == nil {
				return &providers.ConfigError{
					Msg: "image fallback requires verified model capability metadata",
				}
			}
			if vision, _ := model.Capabilities["vision"].(bool); !vision {
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
			"anthropic", "ollama":
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
		case "openai_compatible", "openai", "github_copilot", "bedrock", "litellm":
		default:
			return &providers.ConfigError{
				Msg: "selected provider cannot preserve Responses reasoning controls",
			}
		}
	}
	if metadata, ok := kw["metadata"].(map[string]any); ok && len(metadata) > 0 {
		switch providerType {
		case "openai_compatible", "openai", "github_copilot", "bedrock", "litellm":
		default:
			return &providers.ConfigError{
				Msg: "selected provider cannot preserve Responses metadata",
			}
		}
	}
	return nil
}

func executeCompleteWithTrace(targets []Target, messages []providers.Message, requested string, principal *config.Principal, kw providers.Kwargs) (map[string]any, *Target, []AttemptTrace, error) {
	var attempts []attempt
	trace := make([]AttemptTrace, 0, len(targets))
	var lastErr error
	for i := range targets {
		t := targets[i]
		prov, err := providers.GetProviderForPrincipal(t.Provider, principal)
		if err != nil {
			throttled := providers.IsThrottle(err)
			attempts = append(attempts, attempt{Provider: t.Provider, Model: t.Model, OK: false, Error: truncate(err.Error()), Throttled: throttled})
			trace = append(trace, AttemptTrace{Provider: t.Provider, Model: t.Model, Status: "failed", Throttled: throttled})
			lastErr = err
			continue
		}
		result, err := prov.Complete(t.Model, messages, kw)
		if err != nil {
			throttled := providers.IsThrottle(err)
			attempts = append(attempts, attempt{Provider: t.Provider, Model: t.Model, OK: false, Error: truncate(err.Error()), Throttled: throttled})
			trace = append(trace, AttemptTrace{Provider: t.Provider, Model: t.Model, Status: "failed", Throttled: throttled})
			lastErr = err
			continue
		}
		attempts = append(attempts, attempt{Provider: t.Provider, Model: t.Model, OK: true})
		trace = append(trace, AttemptTrace{Provider: t.Provider, Model: t.Model, Status: "served"})
		served := t
		recordChain(requested, attempts, &served, principal)
		return result, &served, trace, nil
	}
	recordChain(requested, attempts, nil, principal)
	msg := "no failover targets"
	if lastErr != nil {
		msg = lastErr.Error()
	}
	return nil, nil, trace, &AllTargetsFailed{Msg: msg, Status: providers.UpstreamStatus(lastErr)}
}

// ExecuteStream runs the chain for a streaming request, failing over pre-first-byte.
func ExecuteStream(targets []Target, messages []providers.Message, requested string, principal *config.Principal, kw providers.Kwargs) (providers.StreamIter, *Target, error) {
	var attempts []attempt
	var lastErr error
	for i := range targets {
		t := targets[i]
		prov, err := providers.GetProviderForPrincipal(t.Provider, principal)
		if err != nil {
			attempts = append(attempts, attempt{Provider: t.Provider, Model: t.Model, OK: false, Error: truncate(err.Error()), Throttled: providers.IsThrottle(err)})
			lastErr = err
			continue
		}
		it, err := prov.Stream(t.Model, messages, kw)
		if err != nil {
			attempts = append(attempts, attempt{Provider: t.Provider, Model: t.Model, OK: false, Error: truncate(err.Error()), Throttled: providers.IsThrottle(err)})
			lastErr = err
			continue
		}
		attempts = append(attempts, attempt{Provider: t.Provider, Model: t.Model, OK: true})
		served := t
		recordChain(requested, attempts, &served, principal)
		return it, &served, nil
	}
	recordChain(requested, attempts, nil, principal)
	msg := "no failover targets"
	if lastErr != nil {
		msg = lastErr.Error()
	}
	return nil, nil, &AllTargetsFailed{Msg: msg, Status: providers.UpstreamStatus(lastErr)}
}

func truncate(s string) string {
	return providers.SanitizeDiagnosticTextLimit(s, 200)
}
