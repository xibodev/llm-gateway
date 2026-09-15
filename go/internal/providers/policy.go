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
		return invocationStatus(r.name+": circuit breaker open for another "+
			formatSeconds(remaining)+"s", 503)
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

func waitForRetry(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
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
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, observation, ctxErr
		}
		if err == nil {
			r.recordSuccess()
			return result, observation, nil
		}
		if !IsInvocation(err) {
			return nil, observation, err
		}
		lastErr = err
		if !InvocationRetryable(err) {
			if InvocationCircuitFailure(err) {
				r.recordFailure()
			} else {
				r.recordSuccess()
			}
			return nil, observation, err
		}
		if attempt < attempts {
			if waitErr := waitForRetry(ctx, time.Duration(r.nextBackoff(attempt)*float64(time.Second))); waitErr != nil {
				return nil, observation, waitErr
			}
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
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, observation, ctxErr
		}
		if err == nil {
			r.recordSuccess()
			return result, observation, nil
		}
		if errors.Is(err, ErrResponsesUnsupported) || !IsInvocation(err) {
			return nil, observation, err
		}
		lastErr = err
		if !InvocationRetryable(err) {
			if InvocationCircuitFailure(err) {
				r.recordFailure()
			} else {
				r.recordSuccess()
			}
			return nil, observation, err
		}
		if attempt < attempts {
			if waitErr := waitForRetry(ctx, time.Duration(r.nextBackoff(attempt)*float64(time.Second))); waitErr != nil {
				return nil, observation, waitErr
			}
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
	return r.StreamResponsesContext(context.Background(), model, payload)
}

func (r *ResilientProvider) StreamResponsesContext(
	ctx context.Context, model string, payload map[string]any,
) (StreamIter, *iam.ProviderAccountObservation, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
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
		stream, observation, err := StreamResponsesContext(ctx, r.inner, model, payload)
		lastObservation = observation
		if ctxErr := ctx.Err(); ctxErr != nil {
			if stream != nil {
				_ = stream.Close()
			}
			return nil, observation, ctxErr
		}
		if err == nil {
			r.recordSuccess()
			return stream, observation, nil
		}
		if errors.Is(err, ErrResponsesUnsupported) || !IsInvocation(err) {
			return nil, observation, err
		}
		lastErr = err
		if !InvocationRetryable(err) {
			if InvocationCircuitFailure(err) {
				r.recordFailure()
			} else {
				r.recordSuccess()
			}
			return nil, observation, err
		}
		if attempt < attempts {
			if waitErr := waitForRetry(ctx, time.Duration(r.nextBackoff(attempt)*float64(time.Second))); waitErr != nil {
				return nil, observation, waitErr
			}
			continue
		}
		r.recordFailure()
		return nil, observation, err
	}
	return nil, lastObservation, lastErr
}

func (r *ResilientProvider) Stream(model string, messages []Message, kw Kwargs) (StreamIter, error) {
	return r.StreamContext(context.Background(), model, messages, kw)
}

func (r *ResilientProvider) StreamContext(ctx context.Context, model string, messages []Message, kw Kwargs) (StreamIter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := r.checkCircuit(); err != nil {
		return nil, err
	}
	attempts := max1(r.policy.RetryMaxAttempts)
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		it, err := StreamProviderContext(ctx, r.inner, model, messages, kw)
		if ctxErr := ctx.Err(); ctxErr != nil {
			if it != nil {
				_ = it.Close()
			}
			return nil, ctxErr
		}
		if err == nil {
			r.recordSuccess()
			return it, nil
		}
		if !IsInvocation(err) {
			return nil, err
		}
		lastErr = err
		if !InvocationRetryable(err) {
			if InvocationCircuitFailure(err) {
				r.recordFailure()
			} else {
				r.recordSuccess()
			}
			return nil, err
		}
		if attempt < attempts {
			if waitErr := waitForRetry(ctx, time.Duration(r.nextBackoff(attempt)*float64(time.Second))); waitErr != nil {
				return nil, waitErr
			}
			continue
		}
		r.recordFailure()
		return nil, err
	}
	return nil, lastErr
}

func (r *ResilientProvider) DefaultVoice() string {
	if synthesizer, ok := AsSpeechSynthesizer(r.inner); ok {
		return synthesizer.DefaultVoice()
	}
	return ""
}

func (r *ResilientProvider) Synthesize(voice, text, rate string) ([]byte, string, error) {
	return r.SynthesizeContext(context.Background(), voice, text, rate)
}

func (r *ResilientProvider) SynthesizeContext(ctx context.Context, voice, text, rate string) ([]byte, string, error) {
	synthesizer, ok := AsSpeechSynthesizer(r.inner)
	if !ok {
		return nil, "", &ConfigError{Msg: "provider does not support speech synthesis"}
	}
	if err := r.checkCircuit(); err != nil {
		return nil, "", err
	}
	for attempt, attempts := 1, max1(r.policy.RetryMaxAttempts); ; attempt++ {
		var audio []byte
		var format string
		var err error
		if contextual, ok := synthesizer.(ContextSpeechSynthesizer); ok {
			audio, format, err = contextual.SynthesizeContext(ctx, voice, text, rate)
		} else {
			audio, format, err = synthesizer.Synthesize(voice, text, rate)
		}
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		if err == nil {
			r.recordSuccess()
			return audio, format, nil
		}
		if !r.retryNativeInvocation(err, attempt, attempts) {
			return nil, "", err
		}
		if waitErr := waitForRetry(ctx, time.Duration(r.nextBackoff(attempt)*float64(time.Second))); waitErr != nil {
			return nil, "", waitErr
		}
	}
}

func (r *ResilientProvider) GenerateImages(model, prompt string, count int) ([]GeneratedImage, map[string]any, error) {
	return r.GenerateImagesContext(context.Background(), model, prompt, count)
}

func (r *ResilientProvider) GenerateImagesContext(ctx context.Context, model, prompt string, count int) ([]GeneratedImage, map[string]any, error) {
	generator, ok := AsImageGenerator(r.inner)
	if !ok {
		return nil, nil, &ConfigError{Msg: "provider does not support image generation"}
	}
	if err := r.checkCircuit(); err != nil {
		return nil, nil, err
	}
	var images []GeneratedImage
	var usage map[string]any
	var err error
	if contextual, ok := generator.(ContextImageGenerator); ok {
		images, usage, err = contextual.GenerateImagesContext(ctx, model, prompt, count)
	} else {
		images, usage, err = generator.GenerateImages(model, prompt, count)
	}
	if err == nil {
		r.recordSuccess()
	} else {
		r.recordInvocationOutcome(err)
	}
	return images, usage, err
}

func (r *ResilientProvider) StartVideo(model, prompt string, parameters map[string]any) (VideoJob, error) {
	return r.StartVideoContext(context.Background(), model, prompt, parameters)
}

func (r *ResilientProvider) StartVideoContext(ctx context.Context, model, prompt string, parameters map[string]any) (VideoJob, error) {
	generator, ok := AsVideoGenerator(r.inner)
	if !ok {
		return VideoJob{}, &ConfigError{Msg: "provider does not support video generation"}
	}
	if err := r.checkCircuit(); err != nil {
		return VideoJob{}, err
	}
	var job VideoJob
	var err error
	if contextual, ok := generator.(ContextVideoGenerator); ok {
		job, err = contextual.StartVideoContext(ctx, model, prompt, parameters)
	} else {
		job, err = generator.StartVideo(model, prompt, parameters)
	}
	if ctx.Err() != nil {
		return VideoJob{}, ctx.Err()
	}
	if err == nil {
		r.recordSuccess()
	} else {
		r.recordInvocationOutcome(err)
	}
	return job, err
}

func (r *ResilientProvider) PollVideo(operation string) (VideoJob, error) {
	return r.PollVideoContext(context.Background(), operation)
}

func (r *ResilientProvider) PollVideoContext(ctx context.Context, operation string) (VideoJob, error) {
	generator, ok := AsVideoGenerator(r.inner)
	if !ok {
		return VideoJob{}, &ConfigError{Msg: "provider does not support video generation"}
	}
	if err := r.checkCircuit(); err != nil {
		return VideoJob{}, err
	}
	for attempt, attempts := 1, max1(r.policy.RetryMaxAttempts); ; attempt++ {
		var job VideoJob
		var err error
		if contextual, ok := generator.(ContextVideoGenerator); ok {
			job, err = contextual.PollVideoContext(ctx, operation)
		} else {
			job, err = generator.PollVideo(operation)
		}
		if ctx.Err() != nil {
			return VideoJob{}, ctx.Err()
		}
		if err == nil {
			r.recordSuccess()
			return job, nil
		}
		if !r.retryNativeInvocation(err, attempt, attempts) {
			return VideoJob{}, err
		}
		if waitErr := waitForRetry(ctx, time.Duration(r.nextBackoff(attempt)*float64(time.Second))); waitErr != nil {
			return VideoJob{}, waitErr
		}
	}
}

func (r *ResilientProvider) retryNativeInvocation(err error, attempt, attempts int) bool {
	if !InvocationRetryable(err) || attempt >= attempts {
		r.recordInvocationOutcome(err)
		return false
	}
	return true
}

func (r *ResilientProvider) recordInvocationOutcome(err error) {
	if InvocationCircuitFailure(err) {
		r.recordFailure()
	} else if IsInvocation(err) {
		r.recordSuccess()
	}
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
