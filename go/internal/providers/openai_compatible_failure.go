package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	core "github.com/xibodev/llmgw-core"
	coreproviders "github.com/xibodev/llmgw-core/providers"
)

// openAIOperation is a kind of operation whose failures the gateway's
// OpenAI transport reported in words of its own; see openAIWords.
type openAIOperation int

const (
	openAIChatCompletion openAIOperation = iota
	openAIChatStream
	// Chat over Responses posts one Responses completion, streamed or not,
	// and its stream replays the answer, which cannot break.
	openAIChatOverResponses
	openAIResponsesCompletion
	openAIResponsesStream
)

// openAIWords is how the transport reported an operation's failures: the
// message of a request that got no answer, the prefix of an answer it could
// not read, the words of a refusal, the messages of answers it could not
// use, and the prefix of its stream's errors.
type openAIWords struct {
	transport, read, refusal, invalidJSON, invalidAnswer, stream string
}

func (o openAIOperation) words() openAIWords {
	switch o {
	case openAIChatCompletion:
		return openAIWords{
			transport: "openai: upstream transport error: ", read: "openai", refusal: "openai: upstream returned",
			invalidJSON: "openai: invalid JSON in upstream response", invalidAnswer: "openai: invalid chat response payload",
		}
	case openAIChatStream:
		return openAIWords{
			transport: "openai: streaming transport error: ", read: "openai", refusal: "openai: upstream returned", stream: "openai",
		}
	case openAIResponsesStream:
		return openAIWords{
			transport: "openai: responses streaming transport error: ", read: "openai: responses",
			refusal: "openai: responses endpoint returned", stream: "responses",
		}
	}
	return openAIWords{
		transport: "openai: responses transport error: ", read: "openai: responses", refusal: "openai: responses endpoint returned",
		invalidJSON: "openai: invalid JSON in responses payload", invalidAnswer: "openai: invalid Responses payload", stream: "openai",
	}
}

// invoke sends request through the core Runtime, naming the facade's type
// and caller for the store and the provider the Runtime calls, and decodes
// the answer, which core checked as the transport checked it.
func (p *openAICompatibleProvider) invoke(ctx context.Context, request core.Request, operation openAIOperation) (map[string]any, error) {
	ctx = withCoreOperation(ctx, p.vertical, p.caller)
	response, err := p.runtime.core.Invoke(ctx, p.caller, p.instance, request)
	if err != nil {
		return nil, p.failure(err, operation.words())
	}
	var result map[string]any
	if err := json.Unmarshal(response.Body, &result); err != nil {
		return nil, circuitFailureInvocation(operation.words().invalidJSON)
	}
	return result, nil
}

func (p *openAICompatibleProvider) stream(ctx context.Context, request core.Request, operation openAIOperation) (StreamIter, error) {
	ctx = withCoreOperation(ctx, p.vertical, p.caller)
	stream, err := p.runtime.core.Stream(ctx, p.caller, p.instance, request)
	if err != nil {
		return nil, p.failure(err, operation.words())
	}
	return &relayedStream{inner: stream, prefix: operation.words().stream}, nil
}

// failure returns the gateway error for what the core Runtime returned for
// an operation: the error the transport returned for the same failure, so
// retries, failover, circuits and what a client reads stay as they were,
// the caller's context ending mid-request included. Core names each failure
// by its message, which names the upstream by the facade's label. The one
// message core cannot give is the soft error's upstream words: core never
// quotes an answer's error.
func (p *openAICompatibleProvider) failure(err error, words openAIWords) error {
	var configErr *ConfigError
	var surface *core.SurfaceError
	var refused *coreproviders.InvocationError
	var failure *core.ProviderError
	switch {
	case errors.As(err, &configErr):
		return configErr
	case errors.As(err, &surface):
		return ErrResponsesUnsupported
	case errors.As(err, &refused) && refused.Status != 0:
		return p.refusal(refused, words)
	case !errors.As(err, &failure):
		if isContextError(err) {
			return err
		}
		return invocation("openai: shared transport failed")
	}
	if mapped := p.transportError(failure, words); mapped != nil {
		return mapped
	}
	switch failure.Class {
	case core.ProviderErrorUnsupported:
		// A request or an answer Chat and Responses cannot both carry,
		// reported as the transport's conversion reported it.
		if failure.Cause != nil {
			return &ConfigError{Msg: failure.Cause.Error()}
		}
	case core.ProviderErrorAuth:
		if failure.Cause != nil {
			return &ConfigError{Msg: fmt.Sprintf("provider '%s': load credential: %v", p.instance, failure.Cause)}
		}
	case core.ProviderErrorTransport, core.ProviderErrorUpstream:
		// A failure the transport never met keeps core's disposition.
		classification := failure.Classification
		return &InvocationError{
			Msg: "openai: " + failure.Message, Retryable: classification.Retryable,
			FailoverEligible: classification.FailoverEligible, CircuitFailure: classification.CircuitFailure,
		}
	}
	return &ConfigError{Msg: "openai: " + failure.Message}
}

// refusal is the transport's error for a status the upstream answered with:
// its words for the operation, the upstream's own as core redacted and
// bounded them, and the Retry-After the transport passed on, in seconds.
func (p *openAICompatibleProvider) refusal(refused *coreproviders.InvocationError, words openAIWords) error {
	upstream := strings.TrimPrefix(refused.Msg, fmt.Sprintf("%s: upstream returned %d: ", p.label, refused.Status))
	retryAfter := ""
	if refused.RetryAfter > 0 {
		retryAfter = strconv.FormatInt(int64(refused.RetryAfter/time.Second), 10)
	}
	message := fmt.Sprintf("%s %d: %s", words.refusal, refused.Status, upstream)
	return invocationStatusRetryAfter(message, refused.Status, retryAfter)
}

// transportError is the transport's error for a failure core reports
// without an upstream status, or nil for one the transport never met. The
// transport repeated a request that broke off, its message followed by the
// cause, and counted an answer it could not use against the circuit
// without repeating it.
func (p *openAICompatibleProvider) transportError(failure *core.ProviderError, words openAIWords) error {
	cause := ""
	if failure.Cause != nil {
		cause = failure.Cause.Error()
	}
	label := p.label
	switch failure.Message {
	case label + " could not be reached":
		return retryableInvocation(words.transport + cause)
	case "the " + label + " response could not be read":
		return retryableInvocation(words.read + ": response body transport error: " + cause)
	case "the " + label + " response exceeds the size limit":
		return circuitFailureInvocation(words.read + ": response body exceeded the size limit")
	case "the " + label + " completion is not JSON", "the " + label + " response is not JSON":
		return circuitFailureInvocation(words.invalidJSON)
	case "the " + label + " completion has no choices", "the " + label + " completion has an invalid choice",
		"the " + label + " response has no output":
		return circuitFailureInvocation(words.invalidAnswer)
	case label + " answered with an error":
		return retryableInvocation("openai: upstream returned a soft error")
	}
	return nil
}
