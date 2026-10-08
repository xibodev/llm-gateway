package providers

import (
	"context"
	"errors"
	"fmt"

	core "github.com/xibodev/llmgw-core"
)

// extensionFailure returns the gateway error for what the core Runtime
// returned for an operation of instance, which the companion daemon serves.
// The resilience wrapper and the router act only on the gateway's
// InvocationError, so without it they would neither repeat nor count such a
// failure, and an endpoint would stop at it instead of trying its next
// member, as it does for the same failure of any other provider.
//
// llmgw-core's extension client classifies a failed answer by the daemon's
// status, which is the upstream's when the daemon passes one on. The error
// keeps that status and the daemon's Retry-After, which core reads only from
// a 429. A daemon that did not answer is a transport failure, which may be
// repeated and which another member may get past, and an answer the client
// could not use counts against the circuit without a repeat. A configuration
// error, the gateway's or core's for the daemon's address, stays one, and so
// does a credential the store could not load, as on every other facade.
func extensionFailure(ctx context.Context, instance string, err error) error {
	var configErr *ConfigError
	var surface *core.SurfaceError
	var failure *core.ProviderError
	switch {
	case errors.As(err, &configErr):
		return configErr
	case ctx.Err() != nil && isContextError(err):
		// The caller left, so nothing is repeated or failed over.
		return ctx.Err()
	case errors.As(err, &surface):
		// The daemon does not serve the surface for this provider, so the
		// request never reached it, and another member may serve it.
		return &InvocationError{Msg: instance + ": " + surface.Error(), FailoverEligible: true}
	case !errors.As(err, &failure):
		if isContextError(err) {
			return err
		}
		return invocation(instance + ": the companion daemon request failed")
	}
	classification := failure.Classification
	if status := classification.StatusCode; status != 0 {
		retryAfter := retryAfterSeconds(classification.RetryAfter)
		var answer *core.ProviderOperationError
		if errors.As(failure, &answer) && answer.Failure.RetryAfter != "" {
			retryAfter = answer.Failure.RetryAfter
		}
		return refusedInvocation(failure.Message, status, retryAfter, err)
	}
	switch failure.Class {
	case core.ProviderErrorTransport:
		return &InvocationError{
			Msg: failure.Message, Retryable: classification.Retryable,
			FailoverEligible: classification.FailoverEligible, CircuitFailure: classification.CircuitFailure,
		}
	case core.ProviderErrorUpstream:
		return circuitFailureInvocation(failure.Message)
	case core.ProviderErrorAuth:
		if failure.Cause != nil {
			return &ConfigError{Msg: fmt.Sprintf("provider '%s': load credential: %v", instance, failure.Cause)}
		}
	}
	return &ConfigError{Msg: failure.Message}
}
