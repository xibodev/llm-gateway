package providers

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	copilotauth "github.com/xibodev/llm-provider-auth/copilot"
	core "github.com/xibodev/llmgw-core"
)

// failure returns the gateway error for what the core Runtime returned for a
// Copilot operation, so the router and the resilience wrapper decide on it as
// they did on the OpenAI transport's errors. The store's and the
// conversion's errors are the gateway's own; a Responses endpoint Copilot
// does not serve is the Chat fallback; a session the shared client could not
// obtain keeps the gateway's guidance, which errors.Is finds; and an upstream
// status keeps its status and Retry-After, while retry, failover and circuit
// follow core's reading of what Copilot did. Core's messages quote no
// upstream body, so neither do these.
func (p *copilotProvider) failure(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	var configErr *ConfigError
	var invocationErr *InvocationError
	var surface *core.SurfaceError
	var authErr *copilotauth.AuthError
	switch {
	case errors.As(err, &configErr):
		return configErr
	case errors.As(err, &invocationErr):
		return invocationErr
	case errors.As(err, &surface):
		return ErrResponsesUnsupported
	case errors.As(err, &authErr):
		return copilotInvocationError(authErr)
	}
	var failure *core.ProviderError
	if !errors.As(err, &failure) {
		return invocation("github_copilot: shared transport failed")
	}
	switch {
	case failure.Class == core.ProviderErrorUnsupported && failure.Cause != nil:
		// A request or an answer Chat and Responses cannot both carry,
		// reported as the transport's conversion reported it.
		return &ConfigError{Msg: failure.Cause.Error()}
	case failure.Class == core.ProviderErrorInvalidRequest:
		return &ConfigError{Msg: "github_copilot: " + failure.Message}
	}
	classification := failure.Classification
	retryAfter := ""
	if classification.RetryAfter > 0 {
		retryAfter = strconv.FormatInt(int64(classification.RetryAfter/time.Second), 10)
	}
	status := classification.StatusCode
	return &InvocationError{
		Msg: "github_copilot: " + failure.Message, Status: status, RetryAfter: retryAfter,
		Retryable: classification.Retryable, CircuitFailure: classification.CircuitFailure,
		// The transport marked Copilot rejecting its credential eligible;
		// routing reads 401 and 403 as final all the same.
		FailoverEligible: classification.FailoverEligible ||
			status == http.StatusUnauthorized || status == http.StatusForbidden,
	}
}

// copilotCoreStream relays core's Copilot stream as the data events the API
// layer reads, as the transport's stream returned them. Core relays
// Copilot's records as sent, and a Chat stream it serves over Responses as
// Chat chunks, so each record is parsed as that stream parsed a record, and
// one of [DONE] or without data is skipped. The API layer re-encodes every
// event it writes, so what a client reads is unchanged.
type copilotCoreStream struct {
	provider *copilotProvider
	ctx      context.Context
	inner    core.StreamIter
	err      error
}

func (s *copilotCoreStream) Next() (string, bool) {
	for {
		frame, err := s.inner.Next()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				s.err = s.provider.failure(s.ctx, err)
			}
			return "", false
		}
		if data, ok := newSSERecordReader(bytes.NewReader(frame)).Next(); ok {
			return data, true
		}
	}
}

func (s *copilotCoreStream) Err() error   { return s.err }
func (s *copilotCoreStream) Close() error { return s.inner.Close() }
