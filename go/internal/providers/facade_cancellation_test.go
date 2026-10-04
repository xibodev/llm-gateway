package providers

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"llmgw/internal/config"
)

// holdingUpstream answers each request with its status line and the start
// of a stream, reports on started that the request arrived, and then holds
// the request open until the caller leaves, which it reports on left.
func holdingUpstream(t *testing.T, started, left chan<- struct{}, start string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The server notices the caller leave only once it has read the body.
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, start)
		w.(http.Flusher).Flush()
		started <- struct{}{}
		select {
		case <-r.Context().Done():
			left <- struct{}{}
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// cancelDuring runs call under a context it cancels once the upstream has
// the request, and checks that call returns promptly, reporting the
// cancellation unless stream allows it to end quietly, and that the
// upstream request was closed.
func cancelDuring(t *testing.T, started, left <-chan struct{}, stream bool, call func(context.Context) error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- call(ctx) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the upstream never received the request")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) && !(stream && err == nil) {
			t.Fatalf("err=%v, want the caller's cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the call outlived its caller")
	}
	select {
	case <-left:
	case <-time.After(5 * time.Second):
		t.Fatal("the upstream request outlived its caller")
	}
}

// drain reads stream to its end and returns what ended it.
func drain(stream StreamIter, err error) error {
	if err != nil {
		return err
	}
	defer stream.Close()
	for _, more := stream.Next(); more; _, more = stream.Next() {
	}
	return nil
}

// A caller that leaves ends the native Anthropic request it was waiting on,
// whichever surface it called.
func TestAnthropicCallerCancellationEndsTheUpstreamRequest(t *testing.T) {
	messages := []Message{{"role": "user", "content": "hi"}}
	payload := map[string]any{"messages": []any{}}
	for name, call := range map[string]func(context.Context, AnthropicNativeProvider) error{
		"messages": func(ctx context.Context, p AnthropicNativeProvider) error {
			_, err := p.CompleteAnthropicMessagesContext(ctx, "model", payload)
			return err
		},
		"chat": func(ctx context.Context, p AnthropicNativeProvider) error {
			_, err := p.CompleteContext(ctx, "model", messages, nil)
			return err
		},
		"count": func(ctx context.Context, p AnthropicNativeProvider) error {
			_, err := p.CountAnthropicTokensContext(ctx, "model", payload, "", nil)
			return err
		},
		"stream": func(ctx context.Context, p AnthropicNativeProvider) error {
			return drain(p.StreamContext(ctx, "model", messages, nil))
		},
	} {
		t.Run(name, func(t *testing.T) {
			started, left := make(chan struct{}, 1), make(chan struct{}, 1)
			base := holdingUpstream(t, started, left, anthropicFrames(0)[0])
			provider := anthropicFixture(t, &config.ProviderConfig{Type: "anthropic", BaseURL: base, APIKey: "fixture-key"})
			cancelDuring(t, started, left, name == "stream", func(ctx context.Context) error { return call(ctx, provider) })
		})
	}
}

// A caller that leaves ends the Azure OpenAI request it was waiting on.
func TestAzureCallerCancellationEndsTheUpstreamRequest(t *testing.T) {
	messages := []Message{{"role": "user", "content": "hi"}}
	for name, call := range map[string]func(context.Context, AzureOpenAIProvider) error{
		"chat": func(ctx context.Context, p AzureOpenAIProvider) error {
			_, err := p.CompleteContext(ctx, "model", messages, nil)
			return err
		},
		"stream": func(ctx context.Context, p AzureOpenAIProvider) error {
			return drain(p.StreamContext(ctx, "model", messages, nil))
		},
	} {
		t.Run(name, func(t *testing.T) {
			started, left := make(chan struct{}, 1), make(chan struct{}, 1)
			base := holdingUpstream(t, started, left, chatFrames(1)[0])
			provider := azureFixture(t, &config.ProviderConfig{Type: "azure_openai", BaseURL: base, APIKey: "fixture-key"})
			cancelDuring(t, started, left, name == "stream", func(ctx context.Context) error { return call(ctx, provider) })
		})
	}
}

// The resilience wrapper waits for a native Messages or token-count retry on
// the caller's context, so a caller that leaves ends the wait.
func TestResilientAnthropicRetryWaitEndsWithTheCaller(t *testing.T) {
	policy := config.ProviderPolicy{RetryMaxAttempts: 2, RetryInitialBackoffSeconds: 10, RetryBackoffMultiplier: 1, RetryMaxBackoffSeconds: 10}
	for name, call := range map[string]func(context.Context, *ResilientProvider) error{
		"messages": func(ctx context.Context, p *ResilientProvider) error {
			_, err := p.CompleteAnthropicMessagesContext(ctx, "model", map[string]any{})
			return err
		},
		"count": func(ctx context.Context, p *ResilientProvider) error {
			_, err := p.CountAnthropicTokensContext(ctx, "model", map[string]any{}, "", nil)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			failure := invocationStatus("busy", http.StatusServiceUnavailable)
			var inner Provider = &messagesTestProvider{err: failure}
			if name == "count" {
				inner = &tokenCounterTestProvider{err: failure}
			}
			wrapped := &ResilientProvider{inner: inner, name: t.Name(), policy: policy}
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			started := time.Now()
			if err := call(ctx, wrapped); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("err=%v, want the caller's context error", err)
			}
			if elapsed := time.Since(started); elapsed > 3*time.Second {
				t.Fatalf("the retry wait outlived its caller by %v", elapsed)
			}
		})
	}
}
