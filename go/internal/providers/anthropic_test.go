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

	"llmgw/internal/config"
)

func TestAnthropicNativePayloadPreservesThinkingAndOutputConfig(t *testing.T) {
	p := AnthropicNativeProvider{}
	payload := p.payload(
		"claude-opus-4.8",
		[]Message{{"role": "user", "content": "hi"}},
		false,
		Kwargs{
			"thinking":      map[string]any{"type": "adaptive"},
			"output_config": map[string]any{"effort": "xhigh"},
			"metadata":      map[string]any{"user_id": "fixture"},
		},
	)

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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-test","display_name":"Claude Test"}]}`))
	}))
	defer server.Close()
	models := (AnthropicNativeProvider{BaseURL: server.URL}).ListModels()
	if len(models) != 1 || len(models[0].SupportedSurfaces) != 1 || models[0].SupportedSurfaces[0] != "/v1/messages" {
		t.Fatalf("models=%+v", models)
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

type messagesTestProvider struct {
	calls int
	err   error
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
			inner := &messagesTestProvider{err: invocationStatus("failure", status)}
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
