package router

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/providers"
)

// A stream commits to its target once the upstream answers, before any
// content: a request moves to the next target only before the first response
// byte (docs/ROUTING.md, "Failover boundary"). A later failure reaches the
// caller through the stream, and no other target is tried.
func TestStreamCommitsWhenTheUpstreamAnswers(t *testing.T) {
	for name, frames := range map[string]string{
		"headers only":    "",
		"role-only chunk": `data: {"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}` + "\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			setupEcho(t)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, frames)
				w.(http.Flusher).Flush()
				panic(http.ErrAbortHandler)
			}))
			defer upstream.Close()
			config.Update(func(s *config.Settings) {
				s.Providers["answers"] = &config.ProviderConfig{Type: "openai_compatible", BaseURL: upstream.URL}
				s.Policies.Defaults = config.ProviderPolicy{RetryMaxAttempts: 1}
			})
			providers.ResetProviders()
			t.Cleanup(providers.ResetProviders)
			targets := []Target{{Provider: "answers", Model: "model"}, {Provider: "echo", Model: "echo-default"}}
			it, served, err := ExecuteStream(targets, []providers.Message{{"role": "user", "content": "hi"}}, "route", anonymous, nil)
			if err != nil || served == nil || served.Provider != "answers" {
				t.Fatalf("served=%+v err=%v, want the target that answered", served, err)
			}
			defer it.Close()
			for {
				if _, more := it.Next(); !more {
					break
				}
			}
			if it.Err() == nil {
				t.Fatal("the failure after the upstream answered did not reach the caller")
			}
		})
	}
}

// A chain that runs out of targets reports the last target's own failure,
// even when the fallback budget ended during that attempt, which the budget
// never cuts. The budget's 504 is for a chain it stopped before its next
// target.
func TestChainReportsItsLastFailureWhenTheDeadlinePassesDuringIt(t *testing.T) {
	setupEcho(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			return
		case <-time.After(100 * time.Millisecond):
		}
		http.Error(w, `{"error":{"message":"busy"}}`, http.StatusServiceUnavailable)
	}))
	defer upstream.Close()
	config.Update(func(s *config.Settings) {
		s.Providers["stalls"] = &config.ProviderConfig{Type: "openai_compatible", BaseURL: upstream.URL}
		s.Policies.Defaults = config.ProviderPolicy{RetryMaxAttempts: 1}
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)
	ctx := WithFallbackOptions(context.Background(), 20*time.Millisecond, "")
	messages := []providers.Message{{"role": "user", "content": "hi"}}
	stalls := Target{Provider: "stalls", Model: "model"}
	_, _, err := ExecuteCompleteContext(ctx, []Target{stalls}, messages, "route", anonymous, nil)
	var failed *AllTargetsFailed
	if !errors.As(err, &failed) || failed.Status != http.StatusServiceUnavailable {
		t.Fatalf("err=%v, want the target's own 503", err)
	}
	_, _, err = ExecuteCompleteContext(ctx, []Target{stalls, {Provider: "echo", Model: "echo-default"}}, messages, "route", anonymous, nil)
	if !errors.As(err, &failed) || failed.Status != http.StatusGatewayTimeout {
		t.Fatalf("err=%v, want the deadline's 504 before the next target", err)
	}
}

// streamingUpstream serves a Chat stream of chunks frames, one every gap, and
// reports on left when the caller leaves before the end.
func streamingUpstream(t *testing.T, chunks int, gap time.Duration, left chan<- struct{}) string {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		for index := 0; chunks < 0 || index < chunks; index++ {
			if index > 0 {
				select {
				case <-r.Context().Done():
					left <- struct{}{}
					return
				case <-time.After(gap):
				}
			}
			_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"%d\"},\"finish_reason\":null}]}\n\n", index)
			w.(http.Flusher).Flush()
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(upstream.Close)
	return upstream.URL
}

// The budget bounds the wait for a stream to open, not the stream: an open
// stream runs under its request's context however long it lasts, on the Chat
// and the Responses chains alike.
func TestOpenStreamOutlivesTheBudget(t *testing.T) {
	for _, surface := range []string{"chat", "responses"} {
		t.Run(surface, func(t *testing.T) {
			setupEcho(t)
			base := streamingUpstream(t, 5, 150*time.Millisecond, make(chan struct{}, 1))
			config.Update(func(s *config.Settings) {
				s.Providers["streams"] = &config.ProviderConfig{Type: "openai_compatible", RegistryID: "openai", BaseURL: base}
				s.Policies.Defaults = config.ProviderPolicy{RetryMaxAttempts: 1}
			})
			providers.ResetProviders()
			t.Cleanup(providers.ResetProviders)
			budget := 250 * time.Millisecond
			ctx := WithFallbackOptions(context.Background(), budget, "")
			targets := []Target{{Provider: "streams", Model: "model"}}
			started := time.Now()
			var it providers.StreamIter
			var err error
			if surface == "chat" {
				it, _, err = ExecuteStreamContext(ctx, targets, []providers.Message{{"role": "user", "content": "hi"}}, "route", anonymous, nil)
			} else {
				var stream *ResponsesExecutionStream
				stream, _, err = ExecuteResponsesStreamContext(ctx, targets, map[string]any{"input": "hi"}, "route", anonymous)
				if err == nil {
					it = stream.Iter
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			defer it.Close()
			frames := 0
			for _, more := it.Next(); more; _, more = it.Next() {
				frames++
			}
			if it.Err() != nil || frames != 5 {
				t.Fatalf("frames=%d err=%v, want the whole stream", frames, it.Err())
			}
			if elapsed := time.Since(started); elapsed <= budget {
				t.Fatalf("stream took %v, not longer than the %v budget", elapsed, budget)
			}
		})
	}
}

// A stream that has not opened when the budget ends is abandoned, and the
// chain tries no further target: the budget still bounds the wait for an
// answer to begin.
func TestBudgetEndsAStreamThatHasNotOpened(t *testing.T) {
	setupEcho(t)
	left := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// The server notices the caller leave only once it has read the body.
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
			left <- struct{}{}
		case <-time.After(5 * time.Second):
		}
	}))
	defer upstream.Close()
	config.Update(func(s *config.Settings) {
		s.Providers["slow"] = &config.ProviderConfig{Type: "openai_compatible", BaseURL: upstream.URL}
		s.Policies.Defaults = config.ProviderPolicy{RetryMaxAttempts: 1}
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)
	ctx := WithFallbackOptions(context.Background(), 100*time.Millisecond, "")
	targets := []Target{{Provider: "slow", Model: "model"}, {Provider: "echo", Model: "echo-default"}}
	started := time.Now()
	_, served, err := ExecuteStreamContext(ctx, targets, []providers.Message{{"role": "user", "content": "hi"}}, "route", anonymous, nil)
	var failed *AllTargetsFailed
	if served != nil || !errors.As(err, &failed) || failed.Status != http.StatusGatewayTimeout {
		t.Fatalf("served=%+v err=%v, want the deadline's 504", served, err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("the chain waited %v for a stream that never opened", elapsed)
	}
	select {
	case <-left:
	case <-time.After(5 * time.Second):
		t.Fatal("the upstream request was not abandoned")
	}
}

// Detached from the budget, an open stream still ends with its request: a
// client that leaves after the budget ended stops the upstream stream.
func TestOpenStreamEndsWithItsRequest(t *testing.T) {
	setupEcho(t)
	left := make(chan struct{}, 1)
	base := streamingUpstream(t, -1, 20*time.Millisecond, left)
	config.Update(func(s *config.Settings) {
		s.Providers["streams"] = &config.ProviderConfig{Type: "openai_compatible", BaseURL: base}
		s.Policies.Defaults = config.ProviderPolicy{RetryMaxAttempts: 1}
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)
	request, leave := context.WithCancel(context.Background())
	defer leave()
	budget := 100 * time.Millisecond
	ctx := WithFallbackOptions(request, budget, "")
	it, _, err := ExecuteStreamContext(ctx, []Target{{Provider: "streams", Model: "model"}}, []providers.Message{{"role": "user", "content": "hi"}}, "route", anonymous, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer it.Close()
	for until := time.Now().Add(3 * budget); time.Now().Before(until); {
		if _, more := it.Next(); !more {
			t.Fatalf("the stream ended with the budget: %v", it.Err())
		}
	}
	leave()
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		for _, more := it.Next(); more; _, more = it.Next() {
		}
	}()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream outlived its request")
	}
	select {
	case <-left:
	case <-time.After(5 * time.Second):
		t.Fatal("the upstream stream outlived its request")
	}
}

// The budget never cuts a try under way: a non-streaming answer takes as long
// as its target needs to produce it, bounded by the provider's own timeout.
func TestBudgetDoesNotCutANonStreamingAnswer(t *testing.T) {
	setupEcho(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
			return
		case <-time.After(300 * time.Millisecond):
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()
	config.Update(func(s *config.Settings) {
		s.Providers["slow"] = &config.ProviderConfig{Type: "openai_compatible", BaseURL: upstream.URL}
		s.Policies.Defaults = config.ProviderPolicy{RetryMaxAttempts: 1}
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)
	ctx := WithFallbackOptions(context.Background(), 50*time.Millisecond, "")
	response, served, err := ExecuteCompleteContext(ctx, []Target{{Provider: "slow", Model: "model"}}, []providers.Message{{"role": "user", "content": "hi"}}, "route", anonymous, nil)
	if err != nil || served == nil || served.Provider != "slow" || response == nil {
		t.Fatalf("served=%+v err=%v, want the answer the budget outlasted", served, err)
	}
}
