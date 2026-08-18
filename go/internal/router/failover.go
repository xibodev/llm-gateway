// Package router resolves a requested model to a failover chain and executes it,
// recording usage (savings ledger) and throttle/fallback events (telemetry).
package router

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"llmgw/internal/config"
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

func (e *ModelNotFoundError) Error() string {
	return fmt.Sprintf("Model %q is not a category and not a 'provider/model' id. "+
		"Pick one from GET /v1/models: a category name, or '<provider>/<model>'.", e.Requested)
}

// AllTargetsFailed means every failover target for a category failed. Status
// carries the last upstream HTTP status (0 if none) so the API layer can pass
// the real status through instead of masking it.
type AllTargetsFailed struct {
	Msg    string
	Status int
}

func (e *AllTargetsFailed) Error() string { return e.Msg }

// AmbiguousCategoryError reports an invalid configuration containing category
// names that differ only by case.
type AmbiguousCategoryError struct {
	Requested string
	Matches   []string
}

func (e *AmbiguousCategoryError) Error() string {
	return fmt.Sprintf(
		"Category %q is ambiguous; matching configured categories: %s.",
		e.Requested,
		strings.Join(e.Matches, ", "),
	)
}

func findCategory(name string) (string, *config.CategoryConfig, error) {
	s := config.Get()
	if cat, ok := s.Categories[name]; ok {
		return name, cat, nil
	}
	matches := make([]string, 0, 1)
	for cname := range s.Categories {
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
	return matches[0], s.Categories[matches[0]], nil
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
	if t, ok := resolveNativeAlias(name, principal); ok {
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
// that would otherwise 404. Providers are scanned in sorted order so the winner
// is deterministic when the same bare name exists under several providers.
func resolveNativeAlias(
	requested string, principal *config.Principal,
) (Target, bool) {
	base := requested
	if i := strings.IndexByte(base, '['); i >= 0 { // drop a "[1m]"-style variant tag
		base = base[:i]
	}
	base = strings.TrimSpace(base)
	if base == "" {
		return Target{}, false
	}
	// Direct native-name match first, so a real Claude model (claude-opus-4.8)
	// resolves to itself before the discovery-prefix fallback can strip it.
	if t, ok := matchByNativeKey(nativeKey(base), principal); ok {
		return t, true
	}
	// Discovery-alias fallback: Claude Code's model discovery only accepts ids
	// beginning with "claude"/"anthropic", so a non-Claude model surfaced in the
	// picker carries that prefix (e.g. "claude-gemini-3.1-pro-preview"). Strip it
	// and retry so the real model resolves.
	low := strings.ToLower(base)
	for _, pfx := range []string{"claude-", "anthropic-"} {
		if strings.HasPrefix(low, pfx) {
			if t, ok := matchByNativeKey(nativeKey(base[len(pfx):]), principal); ok {
				return t, true
			}
		}
	}
	return Target{}, false
}

// matchByNativeKey finds a catalog model whose canonical key equals key,
// scanning providers in sorted order for a deterministic winner.
func matchByNativeKey(
	key string, principal *config.Principal,
) (Target, bool) {
	if key == "" {
		return Target{}, false
	}
	pids := make([]string, 0, len(config.Get().Providers))
	for pid := range config.Get().Providers {
		pids = append(pids, pid)
	}
	sort.Strings(pids)
	for _, pid := range pids {
		for _, m := range providers.CatalogModelsForPrincipal(pid, principal) {
			if nativeKey(m.ID) == key {
				return Target{Provider: pid, Model: m.ID}, true
			}
		}
	}
	return Target{}, false
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
	if len(s) > 200 {
		return s[:200]
	}
	return s
}
