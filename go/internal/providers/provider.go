// Package providers implements the upstream provider stack, organized on two
// axes: a wire-standard TRANSPORT (OpenAIProvider for /chat/completions, the
// native AnthropicProvider, Ollama) and a pluggable AUTH strategy (bearer key,
// Copilot OAuth session). One OpenAI transport therefore backs every
// OpenAI-compatible type — openai_compatible, bedrock, litellm, github_copilot —
// which differ only in auth + base URL. A factory builds them from config and a
// resilience wrapper adds retry + circuit breaking. (echo is an internal test stub.)
package providers

import (
	"errors"
	"strings"

	"llmgw/internal/iam"
)

var ErrResponsesUnsupported = errors.New("provider model does not support native Responses API")

// Message and Kwargs mirror the dynamic dicts the Python pipeline passes around.
type Message = map[string]any
type Kwargs = map[string]any

// ModelInfo is one row of a provider's catalog.
type ModelInfo struct {
	ID           string         `json:"id"`
	Vendor       string         `json:"vendor,omitempty"`
	Label        string         `json:"label,omitempty"`
	Capabilities map[string]any `json:"capabilities,omitempty"`
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

type detailedCompleter interface {
	CompleteWithObservation(
		model string,
		messages []Message,
		kw Kwargs,
	) (map[string]any, *iam.ProviderAccountObservation, error)
}

func CompleteProviderWithObservation(
	provider Provider,
	model string,
	messages []Message,
	kw Kwargs,
) (map[string]any, *iam.ProviderAccountObservation, error) {
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
	) (map[string]any, *iam.ProviderAccountObservation, error)
	StreamResponses(
		model string,
		payload map[string]any,
	) (StreamIter, *iam.ProviderAccountObservation, error)
}

func CompleteResponses(
	provider Provider,
	model string,
	payload map[string]any,
) (map[string]any, *iam.ProviderAccountObservation, error) {
	if responses, ok := provider.(ResponsesProvider); ok {
		return responses.CompleteResponses(model, payload)
	}
	return nil, nil, ErrResponsesUnsupported
}

func StreamResponses(
	provider Provider,
	model string,
	payload map[string]any,
) (StreamIter, *iam.ProviderAccountObservation, error) {
	if responses, ok := provider.(ResponsesProvider); ok {
		return responses.StreamResponses(model, payload)
	}
	return nil, nil, ErrResponsesUnsupported
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
		*iam.ProviderAccountObservation,
		error,
	)
}

func listModelsWithError(
	provider Provider,
) ([]ModelInfo, *iam.ProviderAccountObservation, error) {
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

// InvocationError is an upstream call failure. It triggers retry + failover.
// Status carries the upstream HTTP status (0 if none) so the gateway can pass
// the real status through instead of masking it as a generic 502.
type InvocationError struct {
	Msg    string
	Status int
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

// invocationStatus is invocation() that also records the upstream HTTP status.
func invocationStatus(msg string, status int) error {
	return &InvocationError{Msg: msg, Status: status}
}

// UpstreamStatus returns the upstream HTTP status carried by err, or 0.
func UpstreamStatus(err error) int {
	var e *InvocationError
	if asError(err, &e) {
		return e.Status
	}
	return 0
}

// IsInvocation reports whether err is (or wraps) an InvocationError.
func IsInvocation(err error) bool {
	var e *InvocationError
	return asError(err, &e)
}

// IsConfig reports whether err is (or wraps) a ConfigError.
func IsConfig(err error) bool {
	var e *ConfigError
	return asError(err, &e)
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
