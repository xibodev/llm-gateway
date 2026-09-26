package router

import (
	"context"

	"llmgw/internal/providers"

	core "github.com/xibodev/llmgw-core"
)

// The functions in this file are the forms of Runtime methods that act on the
// installed router Runtime, for callers that do not own one. They shrink as
// those callers take a Runtime explicitly.

func FilterCompatibleTargets(targets []Target, caller core.Caller, request CompatibilityRequest) ([]Target, error) {
	return Current().FilterCompatibleTargets(targets, caller, request)
}

func ResolveTargets(model string) ([]Target, error) { return Current().ResolveTargets(model) }

func ResolveTargetsForPrincipal(ctx context.Context, model string, caller core.Caller) ([]Target, error) {
	return Current().ResolveTargetsForPrincipal(ctx, model, caller)
}

func ResolveForPrincipal(ctx context.Context, model string, caller core.Caller) (Resolution, error) {
	return Current().ResolveForPrincipal(ctx, model, caller)
}

func NativeAliasCandidates(ctx context.Context, caller core.Caller) (map[string][]Target, error) {
	return Current().NativeAliasCandidates(ctx, caller)
}

func ExecuteComplete(targets []Target, messages []providers.Message, requested string, caller core.Caller, kw providers.Kwargs) (map[string]any, *Target, error) {
	return Current().ExecuteComplete(targets, messages, requested, caller, kw)
}

func ExecuteCompleteContext(ctx context.Context, targets []Target, messages []providers.Message, requested string, caller core.Caller, kw providers.Kwargs) (map[string]any, *Target, error) {
	return Current().ExecuteCompleteContext(ctx, targets, messages, requested, caller, kw)
}

func ExecuteCompleteWithTrace(targets []Target, messages []providers.Message, requested string, caller core.Caller, kw providers.Kwargs) (map[string]any, *Target, []AttemptTrace, error) {
	return Current().ExecuteCompleteWithTrace(targets, messages, requested, caller, kw)
}

func ExecuteCompleteWithTraceContext(ctx context.Context, targets []Target, messages []providers.Message, requested string, caller core.Caller, kw providers.Kwargs) (map[string]any, *Target, []AttemptTrace, error) {
	return Current().ExecuteCompleteWithTraceContext(ctx, targets, messages, requested, caller, kw)
}

func ExecuteResponses(targets []Target, payload map[string]any, requested string, caller core.Caller) (map[string]any, *Target, error) {
	return Current().ExecuteResponses(targets, payload, requested, caller)
}

func ExecuteResponsesContext(ctx context.Context, targets []Target, payload map[string]any, requested string, caller core.Caller) (map[string]any, *Target, error) {
	return Current().ExecuteResponsesContext(ctx, targets, payload, requested, caller)
}

func ExecuteAnthropicMessages(targets []Target, payload map[string]any, requested string, caller core.Caller) (map[string]any, *Target, error) {
	return Current().ExecuteAnthropicMessages(targets, payload, requested, caller)
}

func ExecuteAnthropicMessagesContext(ctx context.Context, targets []Target, payload map[string]any, requested string, caller core.Caller) (map[string]any, *Target, error) {
	return Current().ExecuteAnthropicMessagesContext(ctx, targets, payload, requested, caller)
}

func ExecuteResponsesStream(targets []Target, payload map[string]any, requested string, caller core.Caller) (*ResponsesExecutionStream, *Target, error) {
	return Current().ExecuteResponsesStream(targets, payload, requested, caller)
}

func ExecuteResponsesStreamContext(ctx context.Context, targets []Target, payload map[string]any, requested string, caller core.Caller) (*ResponsesExecutionStream, *Target, error) {
	return Current().ExecuteResponsesStreamContext(ctx, targets, payload, requested, caller)
}

func ExecuteStream(targets []Target, messages []providers.Message, requested string, caller core.Caller, kw providers.Kwargs) (providers.StreamIter, *Target, error) {
	return Current().ExecuteStream(targets, messages, requested, caller, kw)
}

func ExecuteStreamContext(ctx context.Context, targets []Target, messages []providers.Message, requested string, caller core.Caller, kw providers.Kwargs) (providers.StreamIter, *Target, error) {
	return Current().ExecuteStreamContext(ctx, targets, messages, requested, caller, kw)
}

func ExecuteAnthropicStreamContext(ctx context.Context, targets []Target, messages []providers.Message, requested string, caller core.Caller, kw providers.Kwargs) (providers.StreamIter, *Target, error) {
	return Current().ExecuteAnthropicStreamContext(ctx, targets, messages, requested, caller, kw)
}

func RecordUsage(r UsageRecord) { Current().RecordUsage(r) }

func Totals(includeStubs bool) map[string]any { return Current().Totals(includeStubs) }

func ByProject(includeStubs bool) []map[string]any { return Current().ByProject(includeStubs) }

func RecentUsage(limit int) []map[string]any { return Current().RecentUsage(limit) }

func PruneSavingsBefore(cutoff int64) (int64, error) { return Current().PruneSavingsBefore(cutoff) }

func ResetSavingsState() { Current().ResetSavingsState() }

func RecentTelemetry(limit int) []map[string]any { return Current().RecentTelemetry(limit) }

func TelemetryStats() map[string]any { return Current().TelemetryStats() }

func PruneTelemetryBefore(cutoff int64) (int64, error) { return Current().PruneTelemetryBefore(cutoff) }

func ResetTelemetryState() { Current().ResetTelemetryState() }
