package providers

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"llmgw/internal/config"
)

// A Chat request adaptation serves over Responses is converted as the
// transport converted it: a material loss refuses it before anything is
// sent, and an advisory one is sent.
func TestChatToResponsesTranslationLossPolicy(t *testing.T) {
	upstream, base := newOpenAIUpstream(t)
	provider := openAICompatibleFixture(t, &config.ProviderConfig{Type: "openai_compatible", BaseURL: base, APIKey: "fixture"})
	storeOpenAICatalog(provider, ModelInfo{ID: "model", SupportedSurfaces: []string{"/responses"}})
	messages := []Message{{"role": "user", "content": "hi"}}

	if _, err := provider.Complete("model", messages, Kwargs{"stop": []any{"END"}, "_force_api_support": true}); !IsConfig(err) || !strings.Contains(err.Error(), "stop") {
		t.Fatalf("material conversion error=%v", err)
	}
	if calls := upstream.take(); len(calls) != 0 {
		t.Fatalf("material conversion dispatched %d request(s)", len(calls))
	}

	if _, err := provider.Complete("model", messages, Kwargs{"max_tokens": 8, "_force_api_support": true}); err != nil {
		t.Fatalf("advisory conversion rejected: %v", err)
	}
	calls := upstream.take()
	var request map[string]any
	if len(calls) != 1 || calls[0].path != "/responses" || json.Unmarshal([]byte(calls[0].body), &request) != nil || request["max_output_tokens"] != float64(8) {
		t.Fatalf("calls=%+v", calls)
	}
}

// Every request that carries images is marked with Copilot's vision header,
// which other upstreams ignore, as the transport marked it.
func TestV043OpenAIRequestsCarryVisionHeaders(t *testing.T) {
	image := []any{map[string]any{"type": "input_image", "image_url": "data:image/png;base64,AA=="}}
	for name, invoke := range map[string]func(*openAICompatibleProvider) error{
		"chat": func(provider *openAICompatibleProvider) error {
			_, err := provider.Complete("vision", []Message{{
				"role": "user", "content": []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AA=="}}},
			}}, Kwargs{})
			return err
		},
		"responses": func(provider *openAICompatibleProvider) error {
			_, _, err := provider.CompleteResponses("vision", map[string]any{"input": []any{map[string]any{"type": "message", "role": "user", "content": image}}})
			return err
		},
		"responses stream": func(provider *openAICompatibleProvider) error {
			stream, _, err := provider.StreamResponses("vision", map[string]any{"input": []any{map[string]any{"type": "message", "role": "user", "content": image}}})
			if err != nil {
				return err
			}
			_, _ = stream.Next()
			return stream.Close()
		},
	} {
		t.Run(name, func(t *testing.T) {
			upstream, base := newOpenAIUpstream(t)
			provider := openAICompatibleFixture(t, &config.ProviderConfig{Type: "openai_compatible", RegistryID: "openai", BaseURL: base, APIKey: "fixture"})
			if err := invoke(provider); err != nil {
				t.Fatal(err)
			}
			if calls := upstream.take(); len(calls) != 1 || calls[0].vision != "true" {
				t.Fatalf("calls=%+v", calls)
			}
		})
	}
}

// An answer that cannot be used counts against the circuit and is not
// repeated, as the transport read it.
func TestV043OpenAIRejectsStructurallyInvalidSuccessPayloads(t *testing.T) {
	for name, testCase := range map[string]struct {
		body      string
		responses bool
	}{
		"chat null":       {body: "null"},
		"chat empty":      {body: `{}`},
		"responses null":  {body: "null", responses: true},
		"responses empty": {body: `{}`, responses: true},
	} {
		t.Run(name, func(t *testing.T) {
			upstream, base := newOpenAIUpstream(t)
			upstream.setAnswer(func(w http.ResponseWriter, _ openAICall) { _, _ = io.WriteString(w, testCase.body) })
			provider := openAICompatibleFixture(t, &config.ProviderConfig{Type: "openai_compatible", RegistryID: "openai", BaseURL: base})
			var err error
			if testCase.responses {
				_, _, err = provider.CompleteResponses("model", map[string]any{"input": "hi"})
			} else {
				_, err = provider.Complete("model", []Message{{"role": "user", "content": "hi"}}, nil)
			}
			if err == nil || !InvocationCircuitFailure(err) || InvocationRetryable(err) {
				t.Fatalf("error=%v circuit=%v retry=%v", err, InvocationCircuitFailure(err), InvocationRetryable(err))
			}
		})
	}
}

// An error beside a success status is repeated, as the transport repeated
// it. Its message no longer quotes the upstream's words: core never quotes
// an answer's error.
func TestOpenAIRejectsHTTP200SoftError(t *testing.T) {
	upstream, base := newOpenAIUpstream(t)
	upstream.setAnswer(func(w http.ResponseWriter, _ openAICall) {
		_, _ = io.WriteString(w, `{"error":{"message":"service temporarily overloaded"}}`)
	})
	provider := openAICompatibleFixture(t, &config.ProviderConfig{Type: "openai_compatible", BaseURL: base})
	_, err := provider.Complete("model", []Message{{"role": "user", "content": "hi"}}, nil)
	if err == nil || !InvocationRetryable(err) || err.Error() != "openai: upstream returned a soft error" {
		t.Fatalf("error=%v retryable=%v", err, InvocationRetryable(err))
	}
}

// A rejected key is final: nothing refreshes an API key, so the request is
// sent once. Copilot's rejected session is replaced once; see
// TestCopilotFailuresKeepTheirClassification.
func TestV043OpenAIRejectionIsDefinitive(t *testing.T) {
	for name, invoke := range map[string]func(*openAICompatibleProvider) error{
		"chat": func(provider *openAICompatibleProvider) error {
			_, err := provider.Complete("model", []Message{{"role": "user", "content": "hi"}}, nil)
			return err
		},
		"responses": func(provider *openAICompatibleProvider) error {
			_, _, err := provider.CompleteResponses("model", map[string]any{"input": "hi"})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			upstream, base := newOpenAIUpstream(t)
			upstream.setAnswer(func(w http.ResponseWriter, _ openAICall) {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error":{"message":"rejected"}}`)
			})
			err := invoke(openAICompatibleFixture(t, &config.ProviderConfig{Type: "openai_compatible", RegistryID: "openai", BaseURL: base, APIKey: "fixture"}))
			if err == nil || InvocationRetryable(err) || InvocationFailoverEligible(err) || UpstreamStatus(err) != http.StatusUnauthorized {
				t.Fatalf("error=%v retry=%v failover=%v status=%d", err, InvocationRetryable(err), InvocationFailoverEligible(err), UpstreamStatus(err))
			}
			if calls := upstream.take(); len(calls) != 1 {
				t.Fatalf("requests=%d want=1", len(calls))
			}
		})
	}
}

// The openai entry serves Responses for every model, with no catalog row to
// say so.
func TestV043OfficialOpenAIUsesNativeResponsesWithoutCatalogMetadata(t *testing.T) {
	upstream, base := newOpenAIUpstream(t)
	provider := openAICompatibleFixture(t, &config.ProviderConfig{Type: "openai_compatible", RegistryID: "openai", BaseURL: base, APIKey: "fixture"})
	if _, _, err := provider.CompleteResponses("future-model", map[string]any{"input": "hi"}); err != nil {
		t.Fatal(err)
	}
	if calls := upstream.take(); len(calls) != 1 || calls[0].path != "/responses" {
		t.Fatalf("calls=%+v, want the native Responses endpoint", calls)
	}
}

func TestV043ChatPayloadPreservesFallbackControls(t *testing.T) {
	payload := buildOpenAIPayload("model", []Message{{"role": "user", "content": "hi"}}, true, Kwargs{
		"metadata":            map[string]any{"client": "fixture"},
		"parallel_tool_calls": false,
		"reasoning_effort":    "high",
		"thinking":            map[string]any{"type": "disabled"},
	})
	if value, ok := payload["parallel_tool_calls"].(bool); !ok || value {
		t.Fatalf("parallel_tool_calls=%#v", payload["parallel_tool_calls"])
	}
	if payload["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort=%#v", payload["reasoning_effort"])
	}
	thinking, ok := payload["thinking"].(map[string]any)
	if !ok || thinking["type"] != "disabled" {
		t.Fatalf("thinking=%#v", payload["thinking"])
	}
	metadata, ok := payload["metadata"].(map[string]any)
	if !ok || metadata["client"] != "fixture" {
		t.Fatalf("metadata=%#v", payload["metadata"])
	}
}
