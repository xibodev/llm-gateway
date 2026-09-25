package providers

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"llmgw/internal/config"
)

// These tests pin the resilience wrapper's circuit breaker as its callers
// observe it, whatever holds the circuit's state.

// circuitProvider fails every call with err and succeeds while err is nil.
type circuitProvider struct {
	err   error
	calls int
}

func (p *circuitProvider) IsStub() bool            { return false }
func (p *circuitProvider) ListModels() []ModelInfo { return nil }

func (p *circuitProvider) Complete(string, []Message, Kwargs) (map[string]any, error) {
	p.calls++
	if p.err != nil {
		return nil, p.err
	}
	return map[string]any{"choices": []any{}}, nil
}

func (p *circuitProvider) Stream(string, []Message, Kwargs) (StreamIter, error) {
	p.calls++
	if p.err != nil {
		return nil, p.err
	}
	return &sliceIter{}, nil
}

func unavailable() error { return invocationStatus("temporary", http.StatusServiceUnavailable) }

// circuitFixture wraps a failing provider under a circuit the test starts
// and leaves closed. The name is short: diagnostics redact long identifiers.
func circuitFixture(t *testing.T, policy config.ProviderPolicy) (*ResilientProvider, *circuitProvider) {
	t.Helper()
	const name = "circuit-fixture"
	ResetCircuit(name)
	t.Cleanup(func() { ResetCircuit(name) })
	inner := &circuitProvider{err: unavailable()}
	return &ResilientProvider{inner: inner, name: name, policy: policy}, inner
}

func completeThrough(provider *ResilientProvider) error {
	_, err := provider.CompleteContext(context.Background(), "model", nil, nil)
	return err
}

func streamThrough(provider *ResilientProvider) error {
	_, err := provider.StreamContext(context.Background(), "model", nil, nil)
	return err
}

// An open circuit refuses before the retry loop with a retryable 503 that
// names the provider, so nothing is sent and a chain moves past it.
func TestOpenCircuitRefusesBeforeTheRetryLoop(t *testing.T) {
	provider, inner := circuitFixture(t, config.ProviderPolicy{
		RetryMaxAttempts: 3, CircuitFailureThreshold: 1, CircuitCooldownSeconds: 60,
	})
	if err := completeThrough(provider); UpstreamStatus(err) != http.StatusServiceUnavailable || inner.calls != 3 {
		t.Fatalf("err=%v calls=%d, want the upstream 503 after three tries", err, inner.calls)
	}
	for _, call := range []func(*ResilientProvider) error{completeThrough, streamThrough} {
		err := call(provider)
		if err == nil || !strings.HasPrefix(err.Error(), provider.name+": circuit breaker open for another ") ||
			UpstreamStatus(err) != http.StatusServiceUnavailable || !InvocationFailoverEligible(err) {
			t.Fatalf("refusal=%v", err)
		}
	}
	if inner.calls != 3 {
		t.Fatalf("calls=%d, an open circuit sent a request", inner.calls)
	}
}

// Once its cooldown passes, an open circuit admits requests and keeps its
// streak, so one failure reopens it at once; a success closes it.
func TestCircuitIsHalfOpenAfterItsCooldown(t *testing.T) {
	const cooldown = 200 * time.Millisecond
	provider, inner := circuitFixture(t, config.ProviderPolicy{
		RetryMaxAttempts: 1, CircuitFailureThreshold: 2, CircuitCooldownSeconds: cooldown.Seconds(),
	})
	for range 3 {
		_ = completeThrough(provider)
	}
	if inner.calls != 2 {
		t.Fatalf("calls=%d, want two failures to open the circuit", inner.calls)
	}
	time.Sleep(cooldown + 50*time.Millisecond)
	for range 2 {
		_ = completeThrough(provider)
	}
	if inner.calls != 3 {
		t.Fatalf("calls=%d, want one half-open failure to reopen the circuit", inner.calls)
	}
	time.Sleep(cooldown + 50*time.Millisecond)
	inner.err = nil
	if err := completeThrough(provider); err != nil {
		t.Fatalf("half-open success: %v", err)
	}
	inner.err = unavailable()
	for range 2 {
		_ = completeThrough(provider)
	}
	if inner.calls != 6 {
		t.Fatalf("calls=%d, want the success to close the circuit and end its streak", inner.calls)
	}
}

// Every instance of a provider shares its circuit, as the caller-scoped
// instances of one configured provider do.
func TestInstancesOfAProviderShareItsCircuit(t *testing.T) {
	policy := config.ProviderPolicy{RetryMaxAttempts: 1, CircuitFailureThreshold: 1, CircuitCooldownSeconds: 60}
	first, _ := circuitFixture(t, policy)
	other := &circuitProvider{}
	second := &ResilientProvider{inner: other, name: first.name, policy: policy}
	_ = completeThrough(first)
	if err := completeThrough(second); UpstreamStatus(err) != http.StatusServiceUnavailable || other.calls != 0 {
		t.Fatalf("err=%v calls=%d, want the circuit the first instance opened", err, other.calls)
	}
}

// A failure that never reached the upstream leaves the streak as it was.
func TestErrorsThatNeverReachedTheUpstreamLeaveTheStreak(t *testing.T) {
	provider, inner := circuitFixture(t, config.ProviderPolicy{
		RetryMaxAttempts: 1, CircuitFailureThreshold: 2, CircuitCooldownSeconds: 60,
	})
	_ = completeThrough(provider)
	inner.err = &ConfigError{Msg: "fixture misconfigured"}
	_ = completeThrough(provider)
	inner.err = unavailable()
	_ = completeThrough(provider)
	if err := completeThrough(provider); !strings.Contains(err.Error(), "circuit breaker open") || inner.calls != 3 {
		t.Fatalf("err=%v calls=%d, want the configuration error to keep the streak", err, inner.calls)
	}
}
