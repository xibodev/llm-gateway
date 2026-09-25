package providers

import (
	"errors"
	"fmt"
	"io"
	"strings"

	core "github.com/xibodev/llmgw-core"
	coreproviders "github.com/xibodev/llmgw-core/providers"
)

// anthropicTransportError returns, for a failure core's Anthropic reports
// without an upstream status, the error the gateway's transport returned for
// the same failure, or nil for one the transport never met. Core names each
// failure by its message. The transport retried a request that broke off,
// its message followed by the cause, and counted an answer it could not use
// against the circuit without retrying it. It keeps the transport's
// disposition where core's differs: a record over the size limit in a
// streamed completion was a stream error the transport retried.
func anthropicTransportError(message, cause string) error {
	switch message {
	case "the anthropic response could not be read":
		return retryableInvocation("anthropic: response body transport error: " + cause)
	case "the anthropic stream broke off", "the anthropic stream sent a record over the size limit":
		return retryableInvocation("anthropic: streaming response error: " + cause)
	case "the anthropic token count response could not be read":
		return retryableInvocation("anthropic: token count: response body transport error: " + cause)
	case "the anthropic response exceeds the size limit":
		return circuitFailureInvocation("anthropic: response body exceeded the size limit")
	case "the anthropic token count response exceeds the size limit":
		return circuitFailureInvocation("anthropic: token count: response body exceeded the size limit")
	case "the anthropic response is not a JSON object":
		return circuitFailureInvocation("anthropic: invalid JSON in upstream response")
	case "the anthropic response is not a Messages response":
		return circuitFailureInvocation("anthropic: invalid Messages response payload")
	case "the anthropic stream continued after message_stop":
		return circuitFailureInvocation("anthropic: data followed streamed Messages terminal event")
	case "the anthropic stream sent an event that is not JSON":
		return circuitFailureInvocation("anthropic: invalid JSON in streamed Messages response")
	case "the anthropic stream started its message twice":
		return circuitFailureInvocation("anthropic: duplicate streamed Messages start event")
	case "the anthropic stream sent tool input that is not JSON":
		return circuitFailureInvocation("anthropic: invalid streamed tool input")
	case "the anthropic stream reported an error":
		return circuitFailureInvocation("anthropic: streamed Messages response reported an error")
	case "the anthropic stream ended its message incomplete", "the anthropic stream ended before its message was complete":
		return circuitFailureInvocation("anthropic: incomplete streamed Messages response")
	case "the anthropic stream assembled no content list":
		return circuitFailureInvocation("anthropic: invalid streamed Messages response")
	}
	return nil
}

// anthropicFailure returns the gateway error for what the core Runtime
// returned for an Anthropic operation: the error the gateway's transport
// returned for the same failure, so retries, failover, circuits and what a
// client reads stay as they were. transport is the transport's message for a
// request that got no answer, which differed by operation.
//
// A refusal keeps its status and the transport's message, which core renders
// the same way, but not its Retry-After: the transport never read one, and
// the resilience wrapper would wait it out and the API layer pass it on.
func anthropicFailure(err error, transport, instance string) error {
	var configErr *ConfigError
	if errors.As(err, &configErr) {
		return configErr
	}
	var refused *coreproviders.InvocationError
	if errors.As(err, &refused) {
		return invocationStatus(refused.Msg, refused.Status)
	}
	var failure *core.ProviderError
	if !errors.As(err, &failure) {
		return invocation("anthropic: shared transport failed")
	}
	cause := ""
	if failure.Cause != nil {
		cause = failure.Cause.Error()
	}
	// An error event's message ends with the event's type in parentheses.
	message, _, _ := strings.Cut(failure.Message, " (")
	if message == "Anthropic could not be reached" {
		return retryableInvocation(transport + cause)
	}
	if mapped := anthropicTransportError(message, cause); mapped != nil {
		return mapped
	}
	switch failure.Class {
	case core.ProviderErrorAuth:
		if failure.Cause != nil {
			return anthropicCredentialFailure(failure.Cause, instance)
		}
	case core.ProviderErrorTransport, core.ProviderErrorUpstream:
		// A failure the transport never met keeps core's disposition.
		classification := failure.Classification
		return &InvocationError{
			Msg: "anthropic: " + failure.Message, Retryable: classification.Retryable,
			FailoverEligible: classification.FailoverEligible, CircuitFailure: classification.CircuitFailure,
		}
	}
	return &ConfigError{Msg: "anthropic: " + failure.Message}
}

// anthropicCredentialFailure reports a credential Anthropic's store could not
// load, a failure the factory reported when it built the facade. The store's
// own refusal of a connection of another kind is kept as it is.
func anthropicCredentialFailure(err error, instance string) error {
	var configErr *ConfigError
	if errors.As(err, &configErr) {
		return configErr
	}
	return &ConfigError{Msg: fmt.Sprintf("provider '%s': load credential: %v", instance, err)}
}

// anthropicCountFailure is anthropicFailure for a token count. The transport
// said a count was refused with "token count returned", and reported a count
// it could not read as ErrInvalidAnthropicTokenCount, never retried.
func anthropicCountFailure(err error, instance string) error {
	var refused *coreproviders.InvocationError
	if errors.As(err, &refused) {
		message := strings.Replace(refused.Msg, "anthropic token count: upstream returned", "anthropic: token count returned", 1)
		return invocationStatus(message, refused.Status)
	}
	var failure *core.ProviderError
	if errors.As(err, &failure) && failure.Message == "anthropic returned an invalid token count" {
		return fmt.Errorf("%w: %w", ErrInvalidAnthropicTokenCount, circuitFailureInvocation("anthropic: invalid token-count response"))
	}
	return anthropicFailure(err, "anthropic: token-count transport error: ", instance)
}

// anthropicStreamEnd is how a Chat stream ends for what core's stream
// returned, as the transport's stream ended: without an error when Anthropic
// ended the stream, message_stop or not, with the gateway's
// StreamRecordTooLargeError for a record over the size limit, and with the
// reader's own error when the stream broke. A stream core relays reports
// only the missing message_stop as an upstream failure without a cause.
func anthropicStreamEnd(err error) error {
	var failure *core.ProviderError
	switch {
	case errors.Is(err, io.EOF):
		return nil
	case !errors.As(err, &failure):
		return err
	case failure.Class == core.ProviderErrorUpstream && failure.Cause == nil:
		return nil
	case failure.Class == core.ProviderErrorUpstream:
		return &StreamRecordTooLargeError{Format: "SSE", Limit: maxStreamRecordWireSize}
	case failure.Cause != nil:
		return failure.Cause
	}
	return err
}
