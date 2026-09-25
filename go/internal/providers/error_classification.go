package providers

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	core "github.com/xibodev/llmgw-core"
)

// llmgw-core reads a failure only through core.ClassifyError, and an error
// class only from a *core.ProviderError, so the gateway's provider errors
// classify themselves and answer errors.As for that type. The invocation
// classification reads the predicates the resilience wrapper and the router
// already act on, so core and the gateway cannot drift apart; the gateway
// itself never consults core. Every one of those predicates needs an
// InvocationError, which is why the other error types are terminal.
//
// A CatalogError does not classify itself. Routing never sees one, and nothing
// retries a catalog failure, fails it over or counts it against a circuit, so
// core's reading of an unclassified error, terminal, is already the gateway's.
// Where the gateway hands a catalog failure to core, it wraps it in
// core.NewProviderOperationError, which classifies it by status.

// ProviderErrorClassification reports what the gateway does with the failure.
// The resilience wrapper repeats the target while InvocationRetryable holds and
// counts InvocationCircuitFailure against the circuit; the router moves to the
// next candidate while InvocationFailoverEligible holds. StatusCode is the
// status the router reports, and RetryAfter the upstream delay the wrapper
// waits out when it exceeds the backoff.
//
// One error reads differently from the wrapper's control flow: the wrapper's
// own circuit-open error is a plain 503, so it classifies as retryable, but the
// wrapper returns it before its retry loop and the router fails it over
// without repeating it. Repeating it would only meet the open circuit again.
func (e *InvocationError) ProviderErrorClassification() core.ProviderErrorClassification {
	if e == nil {
		return core.ProviderErrorClassification{}
	}
	return core.ProviderErrorClassification{
		StatusCode:       e.Status,
		Retryable:        InvocationRetryable(e),
		FailoverEligible: InvocationFailoverEligible(e),
		CircuitFailure:   InvocationCircuitFailure(e),
		RetryAfter:       retryAfterDelay(e.RetryAfter, time.Now()),
	}
}

// errorClass is the class core derives for health: by status, and transport
// for a statusless failure the gateway repeats. A statusless failure counted
// against the circuit but not repeated is a response the gateway could not
// use, such as invalid JSON, which core calls upstream. Any other statusless
// invocation keeps no class: it is the gateway's catch-all for request
// validation, an empty model result, a canceled request and local provider
// state, and no one core class describes them all.
func (e *InvocationError) errorClass() core.ProviderErrorClass {
	if e.Status == 0 && !InvocationRetryable(e) {
		if InvocationCircuitFailure(e) {
			return core.ProviderErrorUpstream
		}
		return ""
	}
	return core.ClassifyProviderFailure(core.ProviderFailure{StatusCode: e.Status, Err: e}).ErrorClass
}

// ProviderErrorClassification is terminal, although core.NewConfigurationError
// permits failover: the router's generic chain (shouldAdvance in
// internal/router) moves on only past a failover-eligible InvocationError, and
// the resilience wrapper returns anything else untouched. A gateway
// ConfigError and a configuration error core reports itself therefore share a
// class but not a disposition.
//
// Core cannot express the two chains that differ, because there the router
// decides by surface rather than by error. ExecuteAnthropicMessagesContext
// moves past every error that is not an InvocationError and every failure to
// build a provider, and ExecuteAnthropicStreamContext moves past a target its
// Anthropic controls check rejects. On those paths the gateway fails over
// where this classification stops.
func (e *ConfigError) ProviderErrorClassification() core.ProviderErrorClassification {
	return core.ProviderErrorClassification{}
}

func (e *ConfigError) errorClass() core.ProviderErrorClass { return core.ProviderErrorConfiguration }

// ProviderErrorClassification is terminal. Routing meets this error when a
// provider completes a request by reading a stream (completeViaStream): the
// resilience wrapper returns it untouched and the generic chain stops on it,
// although the Messages chain moves past it as it does past a ConfigError.
func (e *StreamRecordTooLargeError) ProviderErrorClassification() core.ProviderErrorClassification {
	return core.ProviderErrorClassification{}
}

// errorClass is upstream: the upstream sent a record the gateway will not read.
func (e *StreamRecordTooLargeError) errorClass() core.ProviderErrorClass {
	return core.ProviderErrorUpstream
}

// As lets errors.As read the error as a *core.ProviderError, which is where
// core looks for an error class; see asProviderError.
func (e *InvocationError) As(target any) bool { return e != nil && asProviderError(target, e) }

func (e *ConfigError) As(target any) bool { return e != nil && asProviderError(target, e) }

func (e *StreamRecordTooLargeError) As(target any) bool {
	return e != nil && asProviderError(target, e)
}

type classifiedError interface {
	error
	core.ProviderErrorClassifier
	errorClass() core.ProviderErrorClass
}

// asProviderError fills a **core.ProviderError target with a view of err, and
// declines when err has no class, so a class-less error stays as invisible to
// core as it was. The view carries no message, because core may show a
// ProviderError's message to a client and a gateway message can quote an
// upstream body; the gateway error stays reachable as its cause.
func asProviderError(target any, err classifiedError) bool {
	view, ok := target.(**core.ProviderError)
	if !ok {
		return false
	}
	class := err.errorClass()
	if class == "" {
		return false
	}
	*view = &core.ProviderError{Class: class, Classification: err.ProviderErrorClassification(), Cause: err}
	return true
}

// retryAfterDelay reads a Retry-After value, delta-seconds or an HTTP-date, as
// a delay from now. Anything else means no delay: a malformed value, a
// negative count, a date already past, or a count a Duration cannot hold.
func retryAfterDelay(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds <= 0 || seconds > int64(math.MaxInt64/time.Second) {
			return 0
		}
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil && at.After(now) {
		return at.Sub(now)
	}
	return 0
}
