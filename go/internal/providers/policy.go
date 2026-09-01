package providers

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"
)

// ---- circuit breaker state ---------------------------------------------- //

type circuitState struct {
	mu                  sync.Mutex
	consecutiveFailures int
	openUntil           time.Time
}

var (
	circuitsMu sync.Mutex
	circuits   = map[string]*circuitState{}
)

func getCircuit(name string) *circuitState {
	circuitsMu.Lock()
	defer circuitsMu.Unlock()
	c := circuits[name]
	if c == nil {
		c = &circuitState{}
		circuits[name] = c
	}
	return c
}

// ResetCircuit clears breaker state (test helper).
func ResetCircuit(name string) {
	circuitsMu.Lock()
	defer circuitsMu.Unlock()
	if name == "" {
		circuits = map[string]*circuitState{}
	} else {
		delete(circuits, name)
	}
}

// ResilientProvider wraps a Provider with retry + circuit-breaker behaviour.
type ResilientProvider struct {
	inner  Provider
	name   string
	policy config.ProviderPolicy
}

func (r *ResilientProvider) IsStub() bool { return r.inner.IsStub() }

// Unwrap exposes the decorated provider so capability checks (e.g. speech
// synthesis) can reach the concrete implementation.
func (r *ResilientProvider) Unwrap() Provider        { return r.inner }
func (r *ResilientProvider) ListModels() []ModelInfo { return r.inner.ListModels() }

func (r *ResilientProvider) checkCircuit() error {
	if !r.policy.CircuitEnabled() {
		return nil
	}
	c := getCircuit(r.name)
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.openUntil.IsZero() && time.Now().Before(c.openUntil) {
		remaining := time.Until(c.openUntil).Seconds()
		return invocation(r.name + ": circuit breaker open for another " +
			formatSeconds(remaining) + "s")
	}
	if !c.openUntil.IsZero() && !time.Now().Before(c.openUntil) {
		c.openUntil = time.Time{}
	}
	return nil
}

func (r *ResilientProvider) recordSuccess() {
	if !r.policy.CircuitEnabled() {
		return
	}
	c := getCircuit(r.name)
	c.mu.Lock()
	c.consecutiveFailures = 0
	c.openUntil = time.Time{}
	c.mu.Unlock()
}

func (r *ResilientProvider) recordFailure() {
	if !r.policy.CircuitEnabled() {
		return
	}
	c := getCircuit(r.name)
	c.mu.Lock()
	c.consecutiveFailures++
	if c.consecutiveFailures >= r.policy.CircuitFailureThreshold {
		c.openUntil = time.Now().Add(time.Duration(r.policy.CircuitCooldownSeconds * float64(time.Second)))
	}
	c.mu.Unlock()
}

func (r *ResilientProvider) nextBackoff(attempt int) float64 {
	raw := r.policy.RetryInitialBackoffSeconds * math.Pow(r.policy.RetryBackoffMultiplier, float64(attempt-1))
	return math.Min(raw, r.policy.RetryMaxBackoffSeconds)
}

func (r *ResilientProvider) Complete(model string, messages []Message, kw Kwargs) (map[string]any, error) {
	return r.CompleteContext(context.Background(), model, messages, kw)
}

func (r *ResilientProvider) CompleteContext(ctx context.Context, model string, messages []Message, kw Kwargs) (map[string]any, error) {
	response, _, err := r.CompleteContextWithObservation(ctx, model, messages, kw)
	return response, err
}

func (r *ResilientProvider) CompleteWithObservation(
	model string, messages []Message, kw Kwargs,
) (map[string]any, *iam.ProviderAccountObservation, error) {
	return r.CompleteContextWithObservation(context.Background(), model, messages, kw)
}

func (r *ResilientProvider) CompleteContextWithObservation(
	ctx context.Context, model string, messages []Message, kw Kwargs,
) (map[string]any, *iam.ProviderAccountObservation, error) {
	if err := r.checkCircuit(); err != nil {
		return nil, nil, err
	}
	attempts := max1(r.policy.RetryMaxAttempts)
	var lastErr error
	var lastObservation *iam.ProviderAccountObservation
	for attempt := 1; attempt <= attempts; attempt++ {
		result, observation, err := CompleteProviderContextWithObservation(
			ctx, r.inner, model, messages, kw,
		)
		lastObservation = observation
		if err == nil {
			r.recordSuccess()
			return result, observation, nil
		}
		if !IsInvocation(err) {
			return nil, observation, err
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, observation, err
		}
		if attempt < attempts {
			time.Sleep(time.Duration(r.nextBackoff(attempt) * float64(time.Second)))
			continue
		}
		r.recordFailure()
		return nil, observation, err
	}
	return nil, lastObservation, lastErr
}

func (r *ResilientProvider) CompleteResponses(
	model string, payload map[string]any,
) (map[string]any, *iam.ProviderAccountObservation, error) {
	return r.CompleteResponsesContext(context.Background(), model, payload)
}

func (r *ResilientProvider) CompleteResponsesContext(
	ctx context.Context, model string, payload map[string]any,
) (map[string]any, *iam.ProviderAccountObservation, error) {
	if err := r.checkCircuit(); err != nil {
		return nil, nil, err
	}
	attempts := max1(r.policy.RetryMaxAttempts)
	if ResponsesPayloadIsStateful(payload) {
		attempts = 1
	}
	var lastErr error
	var lastObservation *iam.ProviderAccountObservation
	for attempt := 1; attempt <= attempts; attempt++ {
		result, observation, err := CompleteResponsesContext(ctx, r.inner, model, payload)
		lastObservation = observation
		if err == nil {
			r.recordSuccess()
			return result, observation, nil
		}
		if errors.Is(err, ErrResponsesUnsupported) || !IsInvocation(err) {
			return nil, observation, err
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, observation, err
		}
		if attempt < attempts {
			time.Sleep(time.Duration(r.nextBackoff(attempt) * float64(time.Second)))
			continue
		}
		r.recordFailure()
		return nil, observation, err
	}
	return nil, lastObservation, lastErr
}

func (r *ResilientProvider) StreamResponses(
	model string, payload map[string]any,
) (StreamIter, *iam.ProviderAccountObservation, error) {
	if err := r.checkCircuit(); err != nil {
		return nil, nil, err
	}
	attempts := max1(r.policy.RetryMaxAttempts)
	if ResponsesPayloadIsStateful(payload) {
		attempts = 1
	}
	var lastErr error
	var lastObservation *iam.ProviderAccountObservation
	for attempt := 1; attempt <= attempts; attempt++ {
		stream, observation, err := StreamResponses(r.inner, model, payload)
		lastObservation = observation
		if err == nil {
			r.recordSuccess()
			return stream, observation, nil
		}
		if errors.Is(err, ErrResponsesUnsupported) || !IsInvocation(err) {
			return nil, observation, err
		}
		lastErr = err
		if attempt < attempts {
			time.Sleep(time.Duration(r.nextBackoff(attempt) * float64(time.Second)))
			continue
		}
		r.recordFailure()
		return nil, observation, err
	}
	return nil, lastObservation, lastErr
}

func (r *ResilientProvider) Stream(model string, messages []Message, kw Kwargs) (StreamIter, error) {
	if err := r.checkCircuit(); err != nil {
		return nil, err
	}
	attempts := max1(r.policy.RetryMaxAttempts)
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		it, err := r.inner.Stream(model, messages, kw)
		if err == nil {
			r.recordSuccess()
			return it, nil
		}
		if !IsInvocation(err) {
			return nil, err
		}
		lastErr = err
		if attempt < attempts {
			time.Sleep(time.Duration(r.nextBackoff(attempt) * float64(time.Second)))
			continue
		}
		r.recordFailure()
		return nil, err
	}
	return nil, lastErr
}

func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

func formatSeconds(s float64) string {
	return time.Duration(s * float64(time.Second)).Truncate(100 * time.Millisecond).String()
}
