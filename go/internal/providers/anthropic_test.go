package providers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"llmgw/internal/config"

	anthropicauth "github.com/xibodev/llm-provider-auth/anthropic"
	core "github.com/xibodev/llmgw-core"
)

func TestAnthropicDeclaresOnlyMessagesWireNative(t *testing.T) {
	provider := AnthropicNativeProvider{}
	if !PreservesWireNativeSurface(provider, "claude-fixture", core.ModelSurfaceMessages) {
		t.Fatal("Anthropic Messages was not declared wire-native")
	}
	if PreservesWireNativeSurface(provider, "claude-fixture", core.ModelSurfaceChatCompletions) {
		t.Fatal("Anthropic Chat translation was declared wire-native")
	}
}

func TestAnthropicCredentialHeadersAcrossRequestSurfaces(t *testing.T) {
	setupToken := anthropicauth.SetupTokenPrefix + strings.Repeat("a", 80)
	for _, tc := range []struct {
		name, credential, wantHeader, wantValue string
		wantBeta                                bool
	}{
		{name: "api key", credential: "fixture-key", wantHeader: "x-api-key", wantValue: "fixture-key"},
		{name: "setup token", credential: setupToken, wantHeader: "Authorization", wantValue: "Bearer " + setupToken, wantBeta: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source, err := anthropicauth.NewHeaderSource(tc.credential)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if got := r.Header.Get(tc.wantHeader); got != tc.wantValue {
					t.Fatalf("%s=%q", tc.wantHeader, got)
				}
				if got := strings.Contains(strings.Join(r.Header.Values("anthropic-beta"), ","), anthropicauth.OAuthBeta); got != tc.wantBeta {
					t.Fatalf("oauth beta=%v", got)
				}
				switch r.URL.Path {
				case "/v1/models":
					_, _ = w.Write([]byte(`{"data":[]}`))
				case "/v1/messages/count_tokens":
					_, _ = w.Write([]byte(`{"input_tokens":1}`))
				default:
					var request map[string]any
					_ = json.NewDecoder(r.Body).Decode(&request)
					if request["stream"] == true {
						_, _ = fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"content\":[]}}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n")
					} else {
						_, _ = w.Write([]byte(`{"content":[]}`))
					}
				}
			}))
			defer server.Close()
			provider := AnthropicNativeProvider{BaseURL: server.URL, Auth: source}
			if _, err := provider.Complete("model", []Message{{"role": "user", "content": "hi"}}, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := provider.CompleteAnthropicMessages("model", map[string]any{"messages": []any{}}); err != nil {
				t.Fatal(err)
			}
			if _, err := provider.CountAnthropicTokens("model", map[string]any{"messages": []any{}}, "", nil); err != nil {
				t.Fatal(err)
			}
			if _, _, err := provider.ListModelsWithError(); err != nil {
				t.Fatal(err)
			}
			if calls != 4 {
				t.Fatalf("calls=%d", calls)
			}
		})
	}
}

func TestAnthropicSetupTokenCompleteAccumulatesNativeStream(t *testing.T) {
	token := anthropicauth.SetupTokenPrefix + strings.Repeat("a", 80)
	source, err := anthropicauth.NewHeaderSource(token)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request["stream"] != true {
			t.Fatalf("stream=%v", request["stream"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"model\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":2}}}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"message_stop\"}\n\n")
	}))
	defer server.Close()
	provider := AnthropicNativeProvider{BaseURL: server.URL, Auth: source}
	response, err := provider.Complete("model", []Message{{"role": "user", "content": "hi"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	message := response["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "hello" {
		t.Fatalf("response=%v", response)
	}
}

func TestAnthropicSetupTokenRejectsIncompleteOrMalformedStream(t *testing.T) {
	token := anthropicauth.SetupTokenPrefix + strings.Repeat("a", 80)
	source, err := anthropicauth.NewHeaderSource(token)
	if err != nil {
		t.Fatal(err)
	}
	for _, response := range []string{
		"data: {\"type\":\"message_start\",\"message\":{\"content\":[]}}\n\n",
		"data: not-json\n\n",
		"data: {\"type\":\"message_stop\"}\n\ndata: {\"type\":\"message_start\",\"message\":{\"content\":[]}}\n\n",
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, response)
		}))
		provider := AnthropicNativeProvider{BaseURL: server.URL, Auth: source}
		_, err := provider.Complete("model", []Message{{"role": "user", "content": "hi"}}, nil)
		server.Close()
		if err == nil {
			t.Fatalf("response %q was accepted", response)
		}
	}
}

func TestAnthropicNativePayloadPreservesThinkingAndOutputConfig(t *testing.T) {
	p := AnthropicNativeProvider{}
	payload, err := p.payload(
		"claude-opus-4.8",
		[]Message{{"role": "user", "content": "hi"}},
		false,
		Kwargs{
			"thinking":      map[string]any{"type": "adaptive"},
			"output_config": map[string]any{"effort": "xhigh"},
			"metadata":      map[string]any{"user_id": "fixture"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	thinking, ok := payload["thinking"].(map[string]any)
	if !ok || thinking["type"] != "adaptive" {
		t.Fatalf("thinking = %#v, want adaptive map", payload["thinking"])
	}
	outputConfig, ok := payload["output_config"].(map[string]any)
	if !ok || outputConfig["effort"] != "xhigh" {
		t.Fatalf("output_config = %#v, want xhigh map", payload["output_config"])
	}
	metadata, ok := payload["metadata"].(map[string]any)
	if !ok || metadata["user_id"] != "fixture" {
		t.Fatalf("metadata = %#v, want fixture user", payload["metadata"])
	}
}

func TestAnthropicNativePayloadTranslationLossPolicy(t *testing.T) {
	p := AnthropicNativeProvider{}
	payload, err := p.payload("model", []Message{{"role": "developer", "content": "instruction"}, {"role": "user", "content": "hi"}}, false, nil)
	if err != nil {
		t.Fatalf("advisory developer conversion rejected: %v", err)
	}
	if payload["system"] != "instruction" {
		t.Fatalf("system=%#v", payload["system"])
	}

	_, err = p.payload("model", []Message{{"role": "unknown", "content": "hi"}}, false, nil)
	if err == nil || !strings.Contains(err.Error(), "messages.0.role") {
		t.Fatalf("material role conversion error=%v", err)
	}
}

func TestAnthropicStreamNormalAndOversizedRecords(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response string
		wantText string
		wantErr  bool
	}{
		{name: "normal", response: "event: message_start\ndata: {\"type\":\"message_start\"}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n", wantText: "hello"},
		{name: "oversized", response: "data: " + strings.Repeat("x", maxStreamRecordWireSize) + "\n\n", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, tc.response)
			}))
			defer server.Close()

			iter, err := (AnthropicNativeProvider{BaseURL: server.URL, Timeout: 2}).Stream("model", []Message{{"role": "user", "content": "hi"}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			var chunks strings.Builder
			for chunk, ok := iter.Next(); ok; chunk, ok = iter.Next() {
				chunks.WriteString(chunk)
			}
			if tc.wantText != "" && !strings.Contains(chunks.String(), tc.wantText) {
				t.Fatalf("chunks = %s", chunks.String())
			}
			var sizeErr *StreamRecordTooLargeError
			if errors.As(iter.Err(), &sizeErr) != tc.wantErr {
				t.Fatalf("error = %#v, want oversized %v", iter.Err(), tc.wantErr)
			}
		})
	}
}

func TestAnthropicListModelsDeclaresMessagesSurface(t *testing.T) {
	discoveredAt := time.Date(2026, time.September, 21, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-test","display_name":"Claude Test"}]}`))
	}))
	defer server.Close()
	models := (AnthropicNativeProvider{BaseURL: server.URL, Now: func() time.Time { return discoveredAt }}).ListModels()
	if len(models) != 1 || len(models[0].SupportedSurfaces) != 1 || models[0].SupportedSurfaces[0] != "/v1/messages" {
		t.Fatalf("models=%+v", models)
	}
	capabilities := models[0].TypedCapabilities
	if capabilities == nil || capabilities.Operations.Chat != core.SupportSupported ||
		capabilities.Surfaces.Messages != core.SupportSupported ||
		capabilities.Surfaces.ChatCompletions != core.SupportUnsupported ||
		capabilities.Surfaces.Responses != core.SupportUnsupported ||
		capabilities.Streaming != core.SupportSupported ||
		capabilities.Provenance.Source != core.ModelCapabilitySourceRegistryStatic ||
		capabilities.Provenance.Confidence != core.ModelCapabilityConfidenceHigh ||
		capabilities.Freshness.DiscoveredAt == nil || !capabilities.Freshness.DiscoveredAt.Equal(discoveredAt) ||
		capabilities.Freshness.ExpiresAt == nil || !capabilities.Freshness.ExpiresAt.Equal(discoveredAt.Add(anthropicCatalogTTL)) {
		t.Fatalf("capability evidence=%+v", capabilities)
	}
}

func TestAnthropicNativeMessagesPreservesOpaquePayloadAndResponse(t *testing.T) {
	const response = `{"id":"msg_native","model":"upstream-model","stop_sequence":"END","content":[{"type":"thinking","thinking":"kept","signature":"sig"},{"type":"unknown","value":1}],"usage":{"input_tokens":9007199254740993,"cache_read_input_tokens":1}}`
	var request map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(response))
	}))
	defer server.Close()
	payload := map[string]any{
		"model": "picker-alias", "stream": true, "system": []any{map[string]any{"type": "text", "text": "client"}},
		"messages": []any{map[string]any{"role": "user", "content": "hi"}}, "thinking": map[string]any{"type": "enabled"},
		"_llmgw_preamble": "policy",
	}
	got, err := (AnthropicNativeProvider{BaseURL: server.URL}).CompleteAnthropicMessages("resolved-model", payload)
	if err != nil {
		t.Fatal(err)
	}
	if request["model"] != "resolved-model" || request["stream"] != false || request["thinking"] == nil {
		t.Fatalf("request=%#v", request)
	}
	system := request["system"].([]any)
	if system[0].(map[string]any)["text"] != "policy" || system[1].(map[string]any)["text"] != "client" {
		t.Fatalf("system=%#v", system)
	}
	actual, _ := json.Marshal(got)
	if got["usage"].(map[string]any)["input_tokens"] != json.Number("9007199254740993") || !strings.Contains(string(actual), "9007199254740993") || !strings.Contains(string(actual), `"type":"unknown"`) {
		t.Fatalf("semantic response=%s", actual)
	}
}

func TestAnthropicNativeMessagesRejectsStructurallyInvalidSuccessPayloads(t *testing.T) {
	for _, body := range []string{"null", `{}`} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			defer server.Close()
			_, err := (AnthropicNativeProvider{BaseURL: server.URL}).CompleteAnthropicMessages("model", map[string]any{"messages": []any{}})
			if err == nil || !InvocationCircuitFailure(err) || InvocationRetryable(err) {
				t.Fatalf("error=%v circuit=%v retry=%v", err, InvocationCircuitFailure(err), InvocationRetryable(err))
			}
		})
	}
}

type messagesTestProvider struct {
	calls int
	err   error
}

type tokenCounterTestProvider struct {
	calls int
	err   error
}

func (p *tokenCounterTestProvider) Complete(string, []Message, Kwargs) (map[string]any, error) {
	return nil, nil
}
func (p *tokenCounterTestProvider) Stream(string, []Message, Kwargs) (StreamIter, error) {
	return nil, nil
}
func (p *tokenCounterTestProvider) ListModels() []ModelInfo { return nil }
func (p *tokenCounterTestProvider) IsStub() bool            { return false }
func (p *tokenCounterTestProvider) CountAnthropicTokens(string, map[string]any, string, []string) (json.Number, error) {
	p.calls++
	if p.err != nil {
		return "", p.err
	}
	return json.Number("1"), nil
}

func (p *messagesTestProvider) Complete(string, []Message, Kwargs) (map[string]any, error) {
	return nil, nil
}
func (p *messagesTestProvider) Stream(string, []Message, Kwargs) (StreamIter, error) { return nil, nil }
func (p *messagesTestProvider) ListModels() []ModelInfo                              { return nil }
func (p *messagesTestProvider) IsStub() bool                                         { return false }
func (p *messagesTestProvider) CompleteAnthropicMessages(string, map[string]any) (map[string]any, error) {
	p.calls++
	if p.err != nil {
		return nil, p.err
	}
	return map[string]any{"id": "ok"}, nil
}

func TestResilientProviderAnthropicMessagesRetryEligibility(t *testing.T) {
	for _, status := range []int{0, 408, 429, 500, 502, 503, 504, 400, 401, 403, 404} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			err := invocationStatus("failure", status)
			if status == 0 {
				err = retryableInvocation("transport failure")
			}
			inner := &messagesTestProvider{err: err}
			wrapped := &ResilientProvider{inner: inner, name: "messages-" + strconv.Itoa(status), policy: config.ProviderPolicy{RetryMaxAttempts: 2}}
			if !SupportsAnthropicMessages(wrapped) {
				t.Fatal("wrapped native capability hidden")
			}
			_, _ = wrapped.CompleteAnthropicMessages("model", map[string]any{})
			want := 1
			if status == 0 || status == 408 || status == 429 || status == 500 || status == 502 || status == 503 || status == 504 {
				want = 2
			}
			if inner.calls != want {
				t.Fatalf("calls=%d want=%d", inner.calls, want)
			}
		})
	}
	inner := &messagesTestProvider{err: &ConfigError{Msg: "compatibility"}}
	wrapped := &ResilientProvider{inner: inner, name: "messages-config", policy: config.ProviderPolicy{RetryMaxAttempts: 2}}
	_, _ = wrapped.CompleteAnthropicMessages("model", map[string]any{})
	if inner.calls != 1 {
		t.Fatalf("compatibility calls=%d", inner.calls)
	}
}

func TestResilientProviderAnthropicMessagesDefinitiveFailureBreaksCircuitStreak(t *testing.T) {
	name := t.Name()
	ResetCircuit(name)
	t.Cleanup(func() { ResetCircuit(name) })
	inner := &messagesTestProvider{err: invocationStatus("temporary", http.StatusServiceUnavailable)}
	wrapped := &ResilientProvider{inner: inner, name: name, policy: config.ProviderPolicy{
		RetryMaxAttempts: 1, CircuitFailureThreshold: 2, CircuitCooldownSeconds: 60,
	}}
	_, _ = wrapped.CompleteAnthropicMessages("model", map[string]any{})
	inner.err = invocationStatus("bad request", http.StatusBadRequest)
	_, _ = wrapped.CompleteAnthropicMessages("model", map[string]any{})
	inner.err = invocationStatus("temporary", http.StatusServiceUnavailable)
	_, _ = wrapped.CompleteAnthropicMessages("model", map[string]any{})
	inner.err = nil
	_, err := wrapped.CompleteAnthropicMessages("model", map[string]any{})
	if err != nil || inner.calls != 4 {
		t.Fatalf("error=%v calls=%d want=4", err, inner.calls)
	}
}

func TestResilientProviderAnthropicTokenCountDefinitiveFailureBreaksCircuitStreak(t *testing.T) {
	name := t.Name()
	ResetCircuit(name)
	t.Cleanup(func() { ResetCircuit(name) })
	inner := &tokenCounterTestProvider{err: invocationStatus("temporary", http.StatusServiceUnavailable)}
	wrapped := &ResilientProvider{inner: inner, name: name, policy: config.ProviderPolicy{
		RetryMaxAttempts: 1, CircuitFailureThreshold: 2, CircuitCooldownSeconds: 60,
	}}
	_, _ = wrapped.CountAnthropicTokens("model", map[string]any{}, "", nil)
	inner.err = invocationStatus("bad request", http.StatusBadRequest)
	_, _ = wrapped.CountAnthropicTokens("model", map[string]any{}, "", nil)
	inner.err = invocationStatus("temporary", http.StatusServiceUnavailable)
	_, _ = wrapped.CountAnthropicTokens("model", map[string]any{}, "", nil)
	inner.err = nil
	_, err := wrapped.CountAnthropicTokens("model", map[string]any{}, "", nil)
	if err != nil || inner.calls != 4 {
		t.Fatalf("error=%v calls=%d want=4", err, inner.calls)
	}
}
