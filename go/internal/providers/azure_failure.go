package providers

import (
	"errors"
	"fmt"

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
