// Package providers implements the upstream provider stack: a facade per
// provider type, which a factory builds from config and a resilience wrapper
// guards with retry and circuit breaking. The facades serve inference on
// llmgw-core, most through its Runtime and the vertical of their type (see
// coreVerticals), and keep on the gateway's own path what core does not
// serve for them, such as catalogs and proxied endpoints. (echo is an
// internal test stub.)
package providers

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	core "github.com/xibodev/llmgw-core"
)

var (
	ErrResponsesUnsupported           = errors.New("provider model does not support native Responses API")
	ErrAnthropicMessagesUnsupported   = errors.New("provider does not support native Anthropic Messages API")
	ErrAnthropicTokenCountUnsupported = errors.New("provider does not support native Anthropic token counting")
	ErrInvalidAnthropicTokenCount     = errors.New("invalid Anthropic token-count response")
)

// Message and Kwargs mirror the dynamic dicts the Python pipeline passes around.
type Message = map[string]any
type Kwargs = map[string]any

type SpeechSynthesizer interface {
	DefaultVoice() string
	Synthesize(voice, text, speed string) ([]byte, string, error)
}

type ContextSpeechSynthesizer interface {
	SynthesizeContext(ctx context.Context, voice, text, speed string) ([]byte, string, error)
}

// ModelInfo is one row of a provider's catalog.
type ModelInfo struct {
	ID                string                  `json:"id"`
	Vendor            string                  `json:"vendor,omitempty"`
	Label             string                  `json:"label,omitempty"`
	Free              bool                    `json:"free,omitempty"`
	Capabilities      map[string]any          `json:"capabilities,omitempty"`
	TypedCapabilities *core.ModelCapabilities `json:"typed_capabilities,omitempty"`
	// SupportedSurfaces are the HTTP surfaces a model can be called through
	// (e.g. "/v1/chat/completions", "/v1/messages") — distinct from an
	// "endpoint" in the routing-target sense used elsewhere in this codebase.
	// The struct tag is the ON-DISK key of the catalog.json cache, and it
	// changed with the field: a pre-rename file therefore unmarshals into a
	// nil list here. That is handled by catalogEntry.SchemaVersion, which
	// discards unstamped entries instead of serving surface-less rows — the
	// persisted format gets a hard cut-over, unlike the public /v1/models
	// compatibility alias (see api/models.go).
	SupportedSurfaces []string `json:"supported_surfaces,omitempty"`
}

// StreamIter yields cleaned SSE chunk payloads (data: prefix stripped, [DONE]
// and blank lines skipped). Ok=false ends the stream; check Err afterwards.
type StreamIter interface {
	Next() (chunk string, ok bool)
	Err() error
	Close() error
}

// Provider is one upstream. Complete/Stream take OpenAI-shaped messages.
//
// Stream returns an error when the connection fails BEFORE the first byte
// (status >= 400, transport error) so the failover executor can advance to the
// next target; a mid-stream error surfaces via StreamIter.Err.
type Provider interface {
	Complete(model string, messages []Message, kw Kwargs) (map[string]any, error)
	Stream(model string, messages []Message, kw Kwargs) (StreamIter, error)
	ListModels() []ModelInfo
	IsStub() bool
}

// WireNativePreservationProvider explicitly declares that a provider forwards
// one client surface without translating its payload or response protocol.
// Implementing a surface method such as ResponsesProvider is not evidence of
// preservation: adapters may implement those methods by translating requests.
type WireNativePreservationProvider interface {
	PreservesWireNativeSurface(model string, surface core.ModelSurface) bool
}

// PreservesWireNativeSurface checks the concrete provider through decorators.
// It deliberately does not infer preservation from any invocation interface.
func PreservesWireNativeSurface(provider Provider, model string, surface core.ModelSurface) bool {
	for provider != nil {
		if declaration, ok := provider.(WireNativePreservationProvider); ok {
			return declaration.PreservesWireNativeSurface(model, surface)
		}
		unwrapper, ok := provider.(interface{ Unwrap() Provider })
		if !ok {
			return false
		}
		provider = unwrapper.Unwrap()
	}
	return false
}

// ContextProvider is the optional non-streaming context-aware provider surface.
// Provider remains unchanged while implementations migrate incrementally.
type ContextProvider interface {
	CompleteContext(context.Context, string, []Message, Kwargs) (map[string]any, error)
}

type ContextStreamProvider interface {
	StreamContext(context.Context, string, []Message, Kwargs) (StreamIter, error)
}

func CompleteProviderContext(ctx context.Context, provider Provider, model string, messages []Message, kw Kwargs) (map[string]any, error) {
	if contextual, ok := provider.(ContextProvider); ok {
		return contextual.CompleteContext(ctx, model, messages, kw)
	}
	return provider.Complete(model, messages, kw)
}

func StreamProviderContext(ctx context.Context, provider Provider, model string, messages []Message, kw Kwargs) (StreamIter, error) {
	if contextual, ok := provider.(ContextStreamProvider); ok {
		return contextual.StreamContext(ctx, model, messages, kw)
	}
	return provider.Stream(model, messages, kw)
}

type detailedCompleter interface {
	CompleteWithObservation(
		model string,
		messages []Message,
		kw Kwargs,
	) (map[string]any, *CredentialObservation, error)
}

type detailedContextCompleter interface {
	CompleteContextWithObservation(context.Context, string, []Message, Kwargs) (map[string]any, *CredentialObservation, error)
}

func CompleteProviderContextWithObservation(ctx context.Context, provider Provider, model string, messages []Message, kw Kwargs) (map[string]any, *CredentialObservation, error) {
	if detailed, ok := provider.(detailedContextCompleter); ok {
		return detailed.CompleteContextWithObservation(ctx, model, messages, kw)
	}
	if contextual, ok := provider.(ContextProvider); ok {
		response, err := contextual.CompleteContext(ctx, model, messages, kw)
		return response, nil, err
	}
	return CompleteProviderWithObservation(provider, model, messages, kw)
}

func CompleteProviderWithObservation(
	provider Provider,
	model string,
	messages []Message,
	kw Kwargs,
) (map[string]any, *CredentialObservation, error) {
	if detailed, ok := provider.(detailedCompleter); ok {
		return detailed.CompleteWithObservation(model, messages, kw)
	}
	response, err := provider.Complete(model, messages, kw)
	return response, nil, err
}

type ResponsesProvider interface {
	CompleteResponses(
		model string,
		payload map[string]any,
	) (map[string]any, *CredentialObservation, error)
	StreamResponses(
		model string,
		payload map[string]any,
	) (StreamIter, *CredentialObservation, error)
}

type ContextResponsesProvider interface {
	CompleteResponsesContext(context.Context, string, map[string]any) (map[string]any, *CredentialObservation, error)
}

type ContextResponsesStreamProvider interface {
	StreamResponsesContext(context.Context, string, map[string]any) (StreamIter, *CredentialObservation, error)
}

// AnthropicMessagesProvider is an optional non-streaming native Messages
// surface. Provider remains OpenAI-shaped for all existing callers.
type AnthropicMessagesProvider interface {
	CompleteAnthropicMessages(model string, payload map[string]any) (map[string]any, error)
}

func CompleteAnthropicMessages(provider Provider, model string, payload map[string]any) (map[string]any, error) {
	if messages, ok := provider.(AnthropicMessagesProvider); ok {
		return messages.CompleteAnthropicMessages(model, payload)
	}
	return nil, ErrAnthropicMessagesUnsupported
}

func SupportsAnthropicMessages(provider Provider) bool {
	for provider != nil {
		if _, ok := provider.(AnthropicMessagesProvider); ok {
			if unwrapper, wrapped := provider.(interface{ Unwrap() Provider }); wrapped {
				provider = unwrapper.Unwrap()
				continue
			}
			return true
		}
		return false
	}
	return false
}

// InvocationRetryable selects the upstream failures safe to repeat: explicitly
// retryable statusless failures, 408, 429, and transient 500/502/503/504 responses.
func InvocationRetryable(err error) bool {
	var invocationError *InvocationError
	if !asError(err, &invocationError) {
		return false
	}
	if invocationError.Status == 0 {
		return invocationError.Retryable
	}
	switch invocationError.Status {
	case 408, 429, 500, 502, 503, 504:
		return true
	default:
		return false
	}
}

// InvocationFailoverEligible selects provider failures that an ordered endpoint
// may move past. A statusless invocation can identify a broken response or local
// provider state that should not repeat against the same target, but it must not
// block a different target from serving the request.
func InvocationFailoverEligible(err error) bool {
	var invocationError *InvocationError
	if !asError(err, &invocationError) {
		return false
	}
	if invocationError.Status == 401 || invocationError.Status == 403 {
		return false
	}
	if invocationError.FailoverEligible || invocationError.Status == 0 {
		return true
	}
	return InvocationRetryable(invocationError)
}

// InvocationCircuitFailure reports whether an invocation represents upstream
// instability that should advance the provider circuit. Definitive request or
// credential rejections reset the transient streak instead.
func InvocationCircuitFailure(err error) bool {
	var invocationError *InvocationError
	if !asError(err, &invocationError) {
		return false
	}
	return invocationError.CircuitFailure || InvocationRetryable(invocationError)
}

// AnthropicMessagesRetryable is kept as the native Messages retry policy.
func AnthropicMessagesRetryable(err error) bool {
	return InvocationRetryable(err)
}

// AnthropicTokenCounter is an optional native Anthropic count_tokens surface.
type AnthropicTokenCounter interface {
	CountAnthropicTokens(model string, payload map[string]any, version string, beta []string) (json.Number, error)
}

func CountAnthropicTokens(provider Provider, model string, payload map[string]any, version string, beta []string) (json.Number, error) {
	if counter, ok := provider.(AnthropicTokenCounter); ok {
		return counter.CountAnthropicTokens(model, payload, version, beta)
	}
	return "", ErrAnthropicTokenCountUnsupported
}

func SupportsAnthropicTokenCount(provider Provider) bool {
	for provider != nil {
		if _, ok := provider.(AnthropicTokenCounter); ok {
			if unwrapper, wrapped := provider.(interface{ Unwrap() Provider }); wrapped {
				provider = unwrapper.Unwrap()
				continue
			}
			return true
		}
		unwrapper, ok := provider.(interface{ Unwrap() Provider })
		if !ok {
			return false
		}
		provider = unwrapper.Unwrap()
	}
	return false
}

func (r *ResilientProvider) CountAnthropicTokens(model string, payload map[string]any, version string, beta []string) (json.Number, error) {
	if !SupportsAnthropicTokenCount(r.inner) {
		return "", ErrAnthropicTokenCountUnsupported
	}
	if err := r.checkCircuit(); err != nil {
		return "", err
	}
	for attempt, attempts := 1, max1(r.policy.RetryMaxAttempts); ; attempt++ {
		result, err := CountAnthropicTokens(r.inner, model, payload, version, beta)
		if err == nil {
			r.record(nil)
			return result, nil
		}
		if errors.Is(err, ErrInvalidAnthropicTokenCount) {
			if InvocationCircuitFailure(err) {
				r.record(err)
			}
			return "", err
		}
		if !AnthropicMessagesRetryable(err) || attempt >= attempts {
			r.record(err)
			return "", err
		}
		time.Sleep(time.Duration(r.nextBackoff(attempt) * float64(time.Second)))
	}
}

func (r *ResilientProvider) CompleteAnthropicMessages(model string, payload map[string]any) (map[string]any, error) {
	if !SupportsAnthropicMessages(r.inner) {
		return nil, ErrAnthropicMessagesUnsupported
	}
	if err := r.checkCircuit(); err != nil {
		return nil, err
	}
	for attempt, attempts := 1, max1(r.policy.RetryMaxAttempts); ; attempt++ {
		result, err := CompleteAnthropicMessages(r.inner, model, payload)
		if err == nil {
			r.record(nil)
			return result, nil
		}
		if errors.Is(err, ErrAnthropicMessagesUnsupported) || !AnthropicMessagesRetryable(err) || attempt >= attempts {
			r.record(err)
			return nil, err
		}
		time.Sleep(time.Duration(r.nextBackoff(attempt) * float64(time.Second)))
	}
}

func CompleteResponses(
	provider Provider,
	model string,
	payload map[string]any,
) (map[string]any, *CredentialObservation, error) {
	if responses, ok := provider.(ResponsesProvider); ok {
		return responses.CompleteResponses(model, payload)
	}
	return nil, nil, ErrResponsesUnsupported
}

func CompleteResponsesContext(ctx context.Context, provider Provider, model string, payload map[string]any) (map[string]any, *CredentialObservation, error) {
	if responses, ok := provider.(ContextResponsesProvider); ok {
		return responses.CompleteResponsesContext(ctx, model, payload)
	}
	return CompleteResponses(provider, model, payload)
}

func StreamResponses(
	provider Provider,
	model string,
	payload map[string]any,
) (StreamIter, *CredentialObservation, error) {
	if responses, ok := provider.(ResponsesProvider); ok {
		return responses.StreamResponses(model, payload)
	}
	return nil, nil, ErrResponsesUnsupported
}

func StreamResponsesContext(
	ctx context.Context,
	provider Provider,
	model string,
	payload map[string]any,
) (StreamIter, *CredentialObservation, error) {
	if responses, ok := provider.(ContextResponsesStreamProvider); ok {
		return responses.StreamResponsesContext(ctx, model, payload)
	}
	return StreamResponses(provider, model, payload)
}

func ResponsesPayloadIsStateful(payload map[string]any) bool {
	if store, _ := payload["store"].(bool); store {
		return true
	}
	if value := payload["previous_response_id"]; value != nil && value != "" {
		return true
	}
	if payload["conversation"] != nil {
		return true
	}
	if prompt, ok := payload["prompt"].(map[string]any); ok && prompt["id"] != nil {
		return true
	}
	if tools, ok := payload["tools"].([]any); ok {
		for _, raw := range tools {
			tool, _ := raw.(map[string]any)
			toolType, _ := tool["type"].(string)
			if toolType != "" && toolType != "function" {
				return true
			}
			if ids, ok := tool["vector_store_ids"].([]any); ok && len(ids) > 0 {
				return true
			}
			if tool["container"] != nil {
				return true
			}
		}
	}
	return responsesValueIsStateful(payload["input"])
}

func responsesValueIsStateful(value any) bool {
	switch current := value.(type) {
	case []any:
		for _, nested := range current {
			if responsesValueIsStateful(nested) {
				return true
			}
		}
	case map[string]any:
		itemType, _ := current["type"].(string)
		switch itemType {
		case "item_reference", "reasoning", "compaction", "computer_call",
			"computer_call_output", "local_shell_call", "local_shell_call_output",
			"mcp_approval_response", "mcp_call", "mcp_list_tools":
			return true
		}
		if current["encrypted_content"] != nil || current["file_id"] != nil {
			return true
		}
		if current["id"] != nil && itemType != "" && itemType != "message" {
			return true
		}
		for _, nested := range current {
			if responsesValueIsStateful(nested) {
				return true
			}
		}
	}
	return false
}

// ---- error kinds -------------------------------------------------------- //

// CatalogError is a safe, structured model-discovery failure. Detail must never
// contain credentials or unredacted upstream response bodies.
type CatalogError struct {
	Code   string
	Detail string
	Status int
}

func (e *CatalogError) Error() string {
	return SanitizeDiagnosticTextLimit(e.Detail, diagnosticErrorLimit)
}

func catalogError(code, detail string, status int) error {
	return &CatalogError{Code: code, Detail: detail, Status: status}
}

// CatalogFailure returns the safe code, detail, and upstream status carried by
// a catalog error. Unknown errors are reduced to a generic redacted failure.
func CatalogFailure(err error) (code, detail string, status int) {
	var catalogErr *CatalogError
	if asError(err, &catalogErr) {
		return catalogErr.Code, catalogErr.Error(), catalogErr.Status
	}
	if err == nil {
		return "", "", 0
	}
	return "catalog_failed", "Provider catalog failed.", 0
}

type detailedModelLister interface {
	ListModelsWithError() (
		[]ModelInfo,
		*CredentialObservation,
		error,
	)
}

func listModelsWithError(
	provider Provider,
) ([]ModelInfo, *CredentialObservation, error) {
	for provider != nil {
		if lister, ok := provider.(detailedModelLister); ok {
			return lister.ListModelsWithError()
		}
		unwrapper, ok := provider.(interface{ Unwrap() Provider })
		if !ok {
			models := provider.ListModels()
			if models == nil {
				return nil, nil, catalogError(
					"catalog_unavailable",
					"Provider catalog was unavailable.",
					0,
				)
			}
			return models, nil, nil
		}
		provider = unwrapper.Unwrap()
	}
	return nil, nil, catalogError("catalog_unavailable", "Provider catalog is unavailable.", 0)
}

// InvocationError is an upstream call failure. Status and Retryable determine
// same-target retries; routing may still use it to advance an eligible chain.
// Status carries the upstream HTTP status (0 if none) so the gateway can pass
// the real status through instead of masking it as a generic 502.
type InvocationError struct {
	Msg              string
	Status           int
	RetryAfter       string
	Retryable        bool
	FailoverEligible bool
	CircuitFailure   bool
}

func (e *InvocationError) Error() string {
	return SanitizeDiagnosticTextLimit(e.Msg, diagnosticErrorLimit)
}

// ConfigError is a provider configuration problem (surfaced as 500, no failover
// retry semantics beyond the chain).
type ConfigError struct{ Msg string }

func (e *ConfigError) Error() string {
	return SanitizeDiagnosticTextLimit(e.Msg, diagnosticErrorLimit)
}

func invocation(format string) error { return &InvocationError{Msg: format} }

func retryableInvocation(message string) error {
	return &InvocationError{Msg: message, Retryable: true, CircuitFailure: true}
}

func circuitFailureInvocation(message string) error {
	return &InvocationError{Msg: message, CircuitFailure: true}
}

func failoverInvocationStatus(message string, status int) error {
	return &InvocationError{Msg: message, Status: status, FailoverEligible: true}
}

// invocationStatus is invocation() that also records the upstream HTTP status.
func invocationStatus(msg string, status int) error {
	return &InvocationError{Msg: msg, Status: status}
}

func invocationStatusRetryAfter(msg string, status int, retryAfter string) error {
	return &InvocationError{Msg: msg, Status: status, RetryAfter: strings.TrimSpace(retryAfter)}
}

// UpstreamStatus returns the upstream HTTP status carried by err, or 0.
func UpstreamStatus(err error) int {
	var e *InvocationError
	if asError(err, &e) {
		return e.Status
	}
	return core.ClassifyError(err).StatusCode
}

// InvocationRetryAfter returns the upstream Retry-After value, when supplied.
func InvocationRetryAfter(err error) string {
	var e *InvocationError
	if asError(err, &e) {
		return e.RetryAfter
	}
	if delay := core.ClassifyError(err).RetryAfter; delay > 0 {
		return strconv.FormatInt(int64(delay/time.Second), 10)
	}
	return ""
}

// IsInvocation reports whether err is (or wraps) an InvocationError.
func IsInvocation(err error) bool {
	var e *InvocationError
	return asError(err, &e)
}

// IsConfig reports whether err is (or wraps) a ConfigError.
func IsConfig(err error) bool {
	var e *ConfigError
	if asError(err, &e) {
		return true
	}
	var providerErr *core.ProviderError
	return errors.As(err, &providerErr) && providerErr.Class == core.ProviderErrorConfiguration
}

// IsThrottle inspects an error message for throttle/rate-limit signals.
func IsThrottle(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "429") || strings.Contains(s, "throttl") ||
		strings.Contains(s, "rate limit") || strings.Contains(s, "too many")
}
