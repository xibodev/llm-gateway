package providers

import (
	"errors"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
)

type retryResponsesProvider struct {
	completeCalls int
	streamCalls   int
}

func (provider *retryResponsesProvider) IsStub() bool { return false }
func (provider *retryResponsesProvider) ListModels() []ModelInfo {
	return []ModelInfo{}
}
func (provider *retryResponsesProvider) Complete(
	string, []Message, Kwargs,
) (map[string]any, error) {
	return nil, errors.New("unused")
}
func (provider *retryResponsesProvider) Stream(
	string, []Message, Kwargs,
) (StreamIter, error) {
	return nil, errors.New("unused")
}
func (provider *retryResponsesProvider) CompleteResponses(
	string, map[string]any,
) (map[string]any, *iam.ProviderAccountObservation, error) {
	provider.completeCalls++
	return nil, nil, invocation("temporary failure")
}
func (provider *retryResponsesProvider) StreamResponses(
	string, map[string]any,
) (StreamIter, *iam.ProviderAccountObservation, error) {
	provider.streamCalls++
	return nil, nil, invocation("temporary failure")
}

func TestV043StatefulResponsesDisableRetries(t *testing.T) {
	for name, payload := range map[string]map[string]any{
		"previous response": {
			"input": "hello", "previous_response_id": "resp_private",
		},
		"conversation": {
			"input": "hello", "conversation": "conv_private",
		},
		"vector store": {
			"input": "hello",
			"tools": []any{map[string]any{
				"type":             "file_search",
				"vector_store_ids": []any{"vs_private"},
			}},
		},
		"stored response": {
			"input": "hello", "store": true,
		},
		"hosted tool": {
			"input": "hello",
			"tools": []any{map[string]any{
				"type": "mcp", "server_label": "write-server",
			}},
		},
		"mcp approval": {
			"input": []any{map[string]any{
				"type": "mcp_approval_response", "approve": true,
			}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			inner := &retryResponsesProvider{}
			provider := &ResilientProvider{
				inner: inner,
				policy: config.ProviderPolicy{
					RetryMaxAttempts: 3,
				},
			}
			_, _, _ = provider.CompleteResponses("model", payload)
			_, _, _ = provider.StreamResponses("model", payload)
			if inner.completeCalls != 1 || inner.streamCalls != 1 {
				t.Fatalf(
					"calls complete=%d stream=%d",
					inner.completeCalls, inner.streamCalls,
				)
			}
		})
	}
}
