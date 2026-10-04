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
