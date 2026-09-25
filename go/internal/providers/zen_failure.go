package providers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	core "github.com/xibodev/llmgw-core"
	coreproviders "github.com/xibodev/llmgw-core/providers"
)

// failure returns the gateway error for what the core Runtime returned for
// a Zen operation, so the router and the resilience wrapper decide on it as
// they did on the OpenAI-compatible transport's errors: an upstream status
// keeps its status and Retry-After, and retry, failover and circuit follow
// core's reading of what Zen did. Core's messages quote no upstream body, so
// neither do these.
func (p *zenProvider) failure(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	var configErr *ConfigError
	if errors.As(err, &configErr) {
		return configErr
	}
	var surface *core.SurfaceError
	if errors.As(err, &surface) {
		return ErrResponsesUnsupported
	}
	var upstream *coreproviders.InvocationError
	if errors.As(err, &upstream) {
		retryAfter := ""
		if upstream.RetryAfter > 0 {
			retryAfter = strconv.FormatInt(int64(upstream.RetryAfter/time.Second), 10)
		}
		return &InvocationError{
			Msg: "opencode_zen: " + upstream.Error(), Status: upstream.Status, RetryAfter: retryAfter,
			Retryable: upstream.Retryable, FailoverEligible: upstream.FailoverEligible, CircuitFailure: upstream.CircuitFailure,
		}
	}
	var failure *core.ProviderError
	if !errors.As(err, &failure) {
		return invocation("opencode_zen: shared transport failed")
	}
	switch failure.Class {
	case core.ProviderErrorUpstream:
		// Zen answered with what cannot be used.
		classification := failure.Classification
		return &InvocationError{
			Msg: "opencode_zen: " + failure.Message, Retryable: classification.Retryable,
			FailoverEligible: classification.FailoverEligible, CircuitFailure: classification.CircuitFailure,
		}
	case core.ProviderErrorAuth:
		if failure.Cause == nil {
			// Core refuses to send anonymously a model the caller's catalog
			// does not mark free. The transport sent it, and Zen refused it
			// with 401, which core's guidance explains; the status stays.
			return invocationStatus("opencode_zen: "+failure.Message, http.StatusUnauthorized)
		}
		return &ConfigError{Msg: fmt.Sprintf("provider '%s': load credential: %v", p.instance, failure.Cause)}
	case core.ProviderErrorUnsupported:
		// A request or a response Chat and Responses cannot both carry,
		// reported as the transport's conversion reported it.
		if failure.Cause != nil {
			return &ConfigError{Msg: failure.Cause.Error()}
		}
	}
	return &ConfigError{Msg: "opencode_zen: " + failure.Message}
}

// zenCoreStream relays core's Zen stream as the data events the API layer
// reads, as the transport's stream returned them. Core relays Zen's records
// byte for byte, so each is parsed as that stream parsed a record, and one
// of [DONE] or without data is skipped. The API layer re-encodes every event
// it writes, so what a client reads is unchanged.
type zenCoreStream struct {
	provider *zenProvider
	ctx      context.Context
	inner    core.StreamIter
	err      error
}

func (s *zenCoreStream) Next() (string, bool) {
	for {
		frame, err := s.inner.Next()
		if err != nil {
			if !errors.Is(err, io.EOF) && !zenUnterminated(err) {
				s.err = s.provider.failure(s.ctx, err)
			}
			return "", false
		}
		if data, ok := newSSERecordReader(bytes.NewReader(frame)).Next(); ok {
			return data, true
		}
	}
}

func (s *zenCoreStream) Err() error   { return s.err }
func (s *zenCoreStream) Close() error { return s.inner.Close() }

// zenUnterminated reports a Responses stream that ended before its terminal
// event, which is the one upstream failure core reports without a cause once
// a stream is open. The transport's stream simply ended there, and the API
// layer writes the response.failed that says the stream ended without a
// terminal event, so the relay ends without an error too.
func zenUnterminated(err error) bool {
	var failure *core.ProviderError
	return errors.As(err, &failure) && failure.Class == core.ProviderErrorUpstream && failure.Cause == nil
}
