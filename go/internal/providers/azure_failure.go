package providers

import (
	"errors"
	"fmt"
	"io"

	core "github.com/xibodev/llmgw-core"
	coreproviders "github.com/xibodev/llmgw-core/providers"
)

// azureFailure returns the gateway error for what the core Runtime returned
// for an Azure OpenAI operation: the error the gateway's transport returned
// for the same failure, so retries, failover, circuits and what a client
// reads stay as they were. transport is the transport's message for a
// request that got no answer, which differed between a completion and a
// stream. Core names each failure by its message.
//
// A refusal keeps its status and the transport's message, which core renders
// the same way, but not its Retry-After: the transport never read one, and
// the resilience wrapper would wait it out and the API layer pass it on.
func azureFailure(err error, transport, instance string) error {
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
		return invocation("azure_openai: shared transport failed")
	}
	cause := ""
	if failure.Cause != nil {
		cause = failure.Cause.Error()
	}
	switch failure.Message {
	case "Azure OpenAI could not be reached":
		return retryableInvocation(transport + cause)
	case "the azure_openai response could not be read":
		return retryableInvocation("azure_openai: response body transport error: " + cause)
	case "the azure_openai response exceeds the size limit":
		return circuitFailureInvocation("azure_openai: response body exceeded the size limit")
	case "the azure_openai response is not a JSON object":
		return circuitFailureInvocation("azure_openai: invalid JSON in upstream response")
	case "the azure_openai response has no choices", "the azure_openai response has an invalid choice":
		return circuitFailureInvocation("azure_openai: invalid chat response payload")
	case "Azure OpenAI needs a credential with an API key":
		return &ConfigError{Msg: fmt.Sprintf("provider '%s': azure_openai needs an API key, and none is configured", instance)}
	}
	switch failure.Class {
	case core.ProviderErrorAuth:
		if failure.Cause != nil {
			return &ConfigError{Msg: fmt.Sprintf("provider '%s': load credential: %v", instance, failure.Cause)}
		}
	case core.ProviderErrorTransport, core.ProviderErrorUpstream:
		// A failure the transport never met, such as a redirect off the
		// resource it would have followed, keeps core's disposition.
		classification := failure.Classification
		return &InvocationError{
			Msg: "azure_openai: " + failure.Message, Retryable: classification.Retryable,
			FailoverEligible: classification.FailoverEligible, CircuitFailure: classification.CircuitFailure,
		}
	}
	return &ConfigError{Msg: "azure_openai: " + failure.Message}
}

// azureChatStream relays core's Azure stream as the data events the API layer
// reads, as the transport's stream returned them; see azureStreamEnd for how
// it ends.
type azureChatStream struct {
	inner core.StreamIter
	err   error
}

func (s *azureChatStream) Next() (string, bool) {
	data, err := nextCoreData(s.inner)
	if err != nil {
		s.err = azureStreamEnd(err)
		return "", false
	}
	return data, true
}

func (s *azureChatStream) Err() error   { return s.err }
func (s *azureChatStream) Close() error { return s.inner.Close() }

// azureStreamEnd is how a Chat stream ends for what core's stream returned,
// as the transport's stream ended: without an error at the stream's end, with
// the gateway's StreamRecordTooLargeError for a record over the size limit,
// the one upstream failure a relayed stream reports, and with the
// transport's streaming error when the stream broke.
func azureStreamEnd(err error) error {
	var failure *core.ProviderError
	switch {
	case errors.Is(err, io.EOF):
		return nil
	case !errors.As(err, &failure):
		return err
	case failure.Class == core.ProviderErrorUpstream:
		return &StreamRecordTooLargeError{Format: "SSE", Limit: maxStreamRecordWireSize}
	case failure.Cause != nil:
		return &InvocationError{Msg: "azure_openai: streaming transport error: " + failure.Cause.Error()}
	}
	return err
}
