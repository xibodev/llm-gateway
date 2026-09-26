package providers

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"llmgw/internal/config"
)

// anthropicCalls are the facade's operations that reach Messages, each with
// a minimal request.
var anthropicCalls = map[string]func(AnthropicNativeProvider) error{
	"messages": func(p AnthropicNativeProvider) error {
		_, err := p.CompleteAnthropicMessages("model", map[string]any{"messages": []any{}})
		return err
	},
	"chat": func(p AnthropicNativeProvider) error {
		_, err := p.Complete("model", []Message{{"role": "user", "content": "hi"}}, nil)
		return err
	},
	"stream": func(p AnthropicNativeProvider) error {
		stream, err := p.Stream("model", []Message{{"role": "user", "content": "hi"}}, nil)
		if err == nil {
			_ = stream.Close()
		}
		return err
	},
}

func anthropicCount(p AnthropicNativeProvider) error {
	_, err := p.CountAnthropicTokens("model", map[string]any{"messages": []any{}}, "", nil)
	return err
}

// Core's failures map back to the errors the gateway's transport returned,
// so the router, the resilience wrapper and a client read them as before: a
// refusal keeps its status and message but not the Retry-After the transport
// never read, and every other failure keeps the transport's message and
// disposition.
func TestAnthropicFailuresKeepTheTransportsErrors(t *testing.T) {
	var status atomic.Int32
	var body atomic.Value
	respond := func(code int, text string) { status.Store(int32(code)); body.Store(text) }
	base := anthropicServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(int(status.Load()))
		_, _ = fmt.Fprint(w, body.Load())
	})
	provider := anthropicFixture(t, &config.ProviderConfig{Type: "anthropic", BaseURL: base, APIKey: "fixture-key"})

	respond(http.StatusTooManyRequests, `{"error":{"message":"slow down"}}`)
	for name, call := range anthropicCalls {
		err := call(provider)
		if err == nil || err.Error() != "anthropic: upstream returned 429: slow down" || UpstreamStatus(err) != 429 ||
			InvocationRetryAfter(err) != "" || !InvocationRetryable(err) || !IsThrottle(err) {
			t.Fatalf("%s: err=%v retry-after=%q", name, err, InvocationRetryAfter(err))
		}
	}
	if err := anthropicCount(provider); err == nil || err.Error() != "anthropic: token count returned 429: slow down" ||
		UpstreamStatus(err) != 429 || InvocationRetryAfter(err) != "" || !InvocationRetryable(err) {
		t.Fatalf("count: err=%v", err)
	}
	respond(http.StatusBadRequest, "")
	for name, call := range anthropicCalls {
		err := call(provider)
		if err == nil || err.Error() != "anthropic: upstream returned 400: no body" || UpstreamStatus(err) != 400 ||
			InvocationRetryable(err) || InvocationFailoverEligible(err) || InvocationCircuitFailure(err) {
			t.Fatalf("%s: err=%v", name, err)
		}
	}

	for text, want := range map[string]string{
		`{"id":`:       "anthropic: invalid JSON in upstream response",
		`{"id":"msg"}`: "anthropic: invalid Messages response payload",
	} {
		respond(http.StatusOK, text)
		for _, name := range []string{"messages", "chat"} {
			err := anthropicCalls[name](provider)
			if err == nil || err.Error() != want || UpstreamStatus(err) != 0 ||
				InvocationRetryable(err) || !InvocationFailoverEligible(err) || !InvocationCircuitFailure(err) {
				t.Fatalf("%s %s: err=%v", name, text, err)
			}
		}
	}
	for _, text := range []string{`{"input_tokens":1.5}`, `{"input_tokens":-1}`, `{}`} {
		respond(http.StatusOK, text)
		if err := anthropicCount(provider); !errors.Is(err, ErrInvalidAnthropicTokenCount) ||
			InvocationRetryable(err) || !InvocationCircuitFailure(err) {
			t.Fatalf("count %s: err=%v", text, err)
		}
	}
}

// A request that gets no answer keeps the transport's message for how it was
// sent, followed by the client's error, and is retried.
func TestAnthropicUnansweredRequestsKeepTheTransportsMessages(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	base := closed.URL
	closed.Close()
	for _, tc := range []struct {
		name, key, prefix string
		call              func(AnthropicNativeProvider) error
	}{
		{"messages", "fixture-key", "anthropic: upstream transport error: ", anthropicCalls["messages"]},
		{"chat", "fixture-key", "anthropic: upstream transport error: ", anthropicCalls["chat"]},
		{"stream", "fixture-key", "anthropic: streaming transport error: ", anthropicCalls["stream"]},
		{"count", "fixture-key", "anthropic: token-count transport error: ", anthropicCount},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := anthropicFixture(t, &config.ProviderConfig{Type: "anthropic", BaseURL: base, APIKey: tc.key})
			err := tc.call(provider)
			if err == nil || !strings.HasPrefix(err.Error(), tc.prefix+`Post "`+base+`/v1/messages`) ||
				UpstreamStatus(err) != 0 || !InvocationRetryable(err) || !InvocationCircuitFailure(err) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}
