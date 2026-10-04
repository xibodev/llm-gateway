package providers

import (
	"context"
	"net/http"
	"testing"
	"time"

	"llmgw/internal/config"
)

// A failover chain's budget ends retries with it: the wrapper starts no
// retry it would have to wait past the deadline for, and returns the failure
// at once, so the chain can move on. Before the deadline it retries as its
// policy says.
func TestRetryDeadlineEndsRetries(t *testing.T) {
	policy := config.ProviderPolicy{
		RetryMaxAttempts: 3, RetryInitialBackoffSeconds: 0.001,
		RetryBackoffMultiplier: 1, RetryMaxBackoffSeconds: 0.001,
	}
	for _, test := range []struct {
		name     string
		deadline time.Time
		want     int
	}{
		{"passed", time.Now(), 1},
		{"ahead", time.Now().Add(time.Hour), 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			inner := &retryResponsesProvider{status: http.StatusServiceUnavailable}
			provider := &ResilientProvider{inner: inner, name: t.Name(), policy: policy}
			ctx := WithRetryDeadline(context.Background(), test.deadline)
			_, err := provider.CompleteContext(ctx, "model", nil, nil)
			if UpstreamStatus(err) != http.StatusServiceUnavailable || inner.completeCalls != test.want {
				t.Fatalf("err=%v calls=%d, want the failure after %d calls", err, inner.completeCalls, test.want)
			}
		})
	}
}

// The backoff is drawn with full jitter: uniformly between zero and the
// policy's exponential backoff, capped as before, so callers that failed
// together do not retry together.
func TestRetryBackoffHasFullJitter(t *testing.T) {
	provider := &ResilientProvider{policy: config.ProviderPolicy{
		RetryInitialBackoffSeconds: 1, RetryBackoffMultiplier: 2, RetryMaxBackoffSeconds: 8,
	}}
	for attempt, ceiling := range map[int]time.Duration{1: time.Second, 3: 4 * time.Second, 10: 8 * time.Second} {
		seen := map[time.Duration]bool{}
		for range 200 {
			delay, ok := provider.retryDelay(invocationStatus("busy", http.StatusServiceUnavailable), attempt)
			if !ok || delay < 0 || delay > ceiling {
				t.Fatalf("attempt %d: delay=%v (%v), want a wait within %v", attempt, delay, ok, ceiling)
			}
			seen[delay] = true
		}
		if len(seen) < 2 {
			t.Fatalf("attempt %d: 200 draws all waited the same", attempt)
		}
	}
	if delay, ok := (&ResilientProvider{}).retryDelay(invocationStatus("busy", http.StatusServiceUnavailable), 1); !ok || delay != 0 {
		t.Fatalf("no backoff: delay=%v (%v)", delay, ok)
	}
}

// The wrapper waits out a Retry-After of at most maxRetryAfterWait. A longer
// one gets no retry at all: the failure, Retry-After included, goes back to
// the router at once, which can try another target.
func TestRetryAfterBeyondTheCapIsNotWaitedOut(t *testing.T) {
	for value, want := range map[string]time.Duration{
		"5": 5 * time.Second, "6": 0, "86400": 0,
		time.Now().Add(time.Minute).UTC().Format(http.TimeFormat): 0,
	} {
		delay, ok := waitBeforeRetry(invocationStatusRetryAfter("throttled", http.StatusTooManyRequests, value), 0)
		if ok != (want > 0) || delay != want {
			t.Errorf("Retry-After %q: delay=%v (%v), want %v", value, delay, ok, want)
		}
	}
	inner := &messagesTestProvider{err: invocationStatusRetryAfter("throttled", http.StatusTooManyRequests, "30")}
	wrapped := &ResilientProvider{inner: inner, name: t.Name(), policy: config.ProviderPolicy{RetryMaxAttempts: 3}}
	started := time.Now()
	_, err := wrapped.CompleteAnthropicMessages("model", map[string]any{})
	if inner.calls != 1 || UpstreamStatus(err) != http.StatusTooManyRequests || InvocationRetryAfter(err) != "30" {
		t.Fatalf("calls=%d err=%v retry-after=%q, want the throttle returned at once", inner.calls, err, InvocationRetryAfter(err))
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("the wrapper waited %v on a Retry-After it does not honour", elapsed)
	}
}
