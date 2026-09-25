package providers

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"llmgw/internal/config"

	"github.com/xibodev/llmgw-core/execution"
)

// ---- circuit breaker state ---------------------------------------------- //

// circuits keeps every provider's circuit breaker in one llmgw-core
// HealthTracker, keyed by provider name, so the instances of a provider share
// its circuit and the circuit outlives the provider cache. The zero value is
// ready for use.
//
// Each wrapper counts against the policy it was built with, as it did when it
// kept the counters itself, so a circuit follows the wrapper acting on it:
// every tracker call runs under mu with that wrapper's policy in effect, and
// the tracker reads the policy back while the call holds mu.
type circuits struct {
	mu      sync.Mutex
	tracker *execution.HealthTracker
	policy  execution.HealthPolicy
}

// use puts policy in effect and returns the tracker. The caller holds mu.
func (c *circuits) use(policy config.ProviderPolicy) *execution.HealthTracker {
	if c.tracker == nil {
		c.tracker = execution.NewHealthTracker(execution.HealthOptions{
			Policy:  func(string) execution.HealthPolicy { return c.policy },
			Observe: observeCircuit,
		})
	}
	c.policy = circuitPolicy(policy)
	return c.tracker
}

// available reports whether name's circuit admits a request under policy and,
// when it does not, until when.
func (c *circuits) available(name string, policy config.ProviderPolicy) (bool, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.use(policy).Available(name)
}

// record moves name's circuit by the outcome of one operation under policy:
// nil for a success, otherwise the error the operation returned.
func (c *circuits) record(name string, policy config.ProviderPolicy, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.use(policy).Record(name, err)
}

// reset forgets name's circuit, or every circuit when name is empty.
func (c *circuits) reset(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case name == "":
		c.tracker = nil
	case c.tracker != nil:
		c.tracker.Reset(name)
	}
}

// circuitPolicy is the breaker a provider policy configures: its threshold
// and cooldown, with a streak that never expires. Retry-After starts no
// cooldown; it only lengthens the wait before the wrapper tries again.
func circuitPolicy(policy config.ProviderPolicy) execution.HealthPolicy {
	return execution.HealthPolicy{
		FailureThreshold: policy.CircuitFailureThreshold,
		OpenDuration:     time.Duration(policy.CircuitCooldownSeconds * float64(time.Second)),
		IgnoreRetryAfter: true,
	}
}

// observeCircuit reads an operation's outcome as the wrapper always has: a
// circuit failure extends the streak, any other invocation error ends it as a
// definitive rejection does, and an error that never reached the upstream,
// such as a configuration error, says nothing about the provider.
func observeCircuit(err error) execution.Observation {
	switch {
	case err == nil:
		return execution.Observation{Effect: execution.EffectSuccess}
	case InvocationCircuitFailure(err):
		return execution.Observation{Effect: execution.EffectFailure}
	case IsInvocation(err):
		return execution.Observation{Effect: execution.EffectSuccess}
	}
	return execution.Observation{}
}

// ResetCircuit clears breaker state (test helper).
func (rt *Runtime) ResetCircuit(name string) { rt.circuits.reset(name) }

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
	if available, until := Current().circuits.available(r.name, r.policy); !available {
		remaining := time.Until(until).Seconds()
		return invocationStatus(r.name+": circuit breaker open for another "+
			formatSeconds(remaining)+"s", 503)
	}
	return nil
}

// record moves the provider's circuit by the outcome of an operation's last
// try: nil for a success, otherwise the error the operation returns.
func (r *ResilientProvider) record(err error) {
	if r.policy.CircuitEnabled() {
		Current().circuits.record(r.name, r.policy, err)
	}
}

func (r *ResilientProvider) nextBackoff(attempt int) float64 {
	raw := r.policy.RetryInitialBackoffSeconds * math.Pow(r.policy.RetryBackoffMultiplier, float64(attempt-1))
	return math.Min(raw, r.policy.RetryMaxBackoffSeconds)
}

func (r *ResilientProvider) retryDelay(err error, attempt int) time.Duration {
	delay := time.Duration(r.nextBackoff(attempt) * float64(time.Second))
	retryAfter := strings.TrimSpace(InvocationRetryAfter(err))
	if retryAfter == "" {
		return delay
	}
	if seconds, parseErr := strconv.ParseInt(retryAfter, 10, 64); parseErr == nil {
		if serverDelay := time.Duration(seconds) * time.Second; serverDelay > delay {
			return serverDelay
		}
		return delay
	}
	if retryAt, parseErr := http.ParseTime(retryAfter); parseErr == nil {
		if serverDelay := time.Until(retryAt); serverDelay > delay {
			return serverDelay
		}
	}
	return delay
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
) (map[string]any, *CredentialObservation, error) {
	return r.CompleteContextWithObservation(context.Background(), model, messages, kw)
}

func (r *ResilientProvider) CompleteContextWithObservation(
	ctx context.Context, model string, messages []Message, kw Kwargs,
) (map[string]any, *CredentialObservation, error) {
	ctx, err := ensureProviderZenInvocation(ctx, r.inner)
	if err != nil {
		return nil, nil, err
	}
	if err := r.checkCircuit(); err != nil {
		return nil, nil, err
	}
	for attempt, attempts := 1, max1(r.policy.RetryMaxAttempts); ; attempt++ {
		result, observation, err := CompleteProviderContextWithObservation(
			ctx, r.inner, model, messages, kw,
		)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, observation, ctxErr
		}
		if err == nil {
			r.record(nil)
			return result, observation, nil
		}
		if !IsInvocation(err) || !r.retries(err, attempt, attempts) {
			return nil, observation, err
		}
		if waitErr := waitForRetry(ctx, r.retryDelay(err, attempt)); waitErr != nil {
			return nil, observation, waitErr
		}
	}
}

func (r *ResilientProvider) CompleteResponses(
	model string, payload map[string]any,
) (map[string]any, *CredentialObservation, error) {
	return r.CompleteResponsesContext(context.Background(), model, payload)
}

func (r *ResilientProvider) CompleteResponsesContext(
	ctx context.Context, model string, payload map[string]any,
) (map[string]any, *CredentialObservation, error) {
	ctx, err := ensureProviderZenInvocation(ctx, r.inner)
	if err != nil {
		return nil, nil, err
	}
	if err := r.checkCircuit(); err != nil {
		return nil, nil, err
	}
	attempts := max1(r.policy.RetryMaxAttempts)
	if ResponsesPayloadIsStateful(payload) {
		attempts = 1
	}
	for attempt := 1; ; attempt++ {
		result, observation, err := CompleteResponsesContext(ctx, r.inner, model, payload)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, observation, ctxErr
		}
		if err == nil {
			r.record(nil)
			return result, observation, nil
		}
		if errors.Is(err, ErrResponsesUnsupported) || !IsInvocation(err) || !r.retries(err, attempt, attempts) {
			return nil, observation, err
		}
		if waitErr := waitForRetry(ctx, r.retryDelay(err, attempt)); waitErr != nil {
			return nil, observation, waitErr
		}
	}
}

func (r *ResilientProvider) StreamResponses(
	model string, payload map[string]any,
) (StreamIter, *CredentialObservation, error) {
	return r.StreamResponsesContext(context.Background(), model, payload)
}

func (r *ResilientProvider) StreamResponsesContext(
	ctx context.Context, model string, payload map[string]any,
) (StreamIter, *CredentialObservation, error) {
	ctx, err := ensureProviderZenInvocation(ctx, r.inner)
	if err != nil {
		return nil, nil, err
	}
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
	for attempt := 1; ; attempt++ {
		stream, observation, err := StreamResponsesContext(ctx, r.inner, model, payload)
		if ctxErr := ctx.Err(); ctxErr != nil {
			if stream != nil {
				_ = stream.Close()
			}
			return nil, observation, ctxErr
		}
		if err == nil {
			r.record(nil)
			return stream, observation, nil
		}
		if errors.Is(err, ErrResponsesUnsupported) || !IsInvocation(err) || !r.retries(err, attempt, attempts) {
			return nil, observation, err
		}
		if waitErr := waitForRetry(ctx, r.retryDelay(err, attempt)); waitErr != nil {
			return nil, observation, waitErr
		}
	}
}

func (r *ResilientProvider) Stream(model string, messages []Message, kw Kwargs) (StreamIter, error) {
	return r.StreamContext(context.Background(), model, messages, kw)
}

func (r *ResilientProvider) StreamContext(ctx context.Context, model string, messages []Message, kw Kwargs) (StreamIter, error) {
	ctx, err := ensureProviderZenInvocation(ctx, r.inner)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := r.checkCircuit(); err != nil {
		return nil, err
	}
	for attempt, attempts := 1, max1(r.policy.RetryMaxAttempts); ; attempt++ {
		it, err := StreamProviderContext(ctx, r.inner, model, messages, kw)
		if ctxErr := ctx.Err(); ctxErr != nil {
			if it != nil {
				_ = it.Close()
			}
			return nil, ctxErr
		}
		if err == nil {
			r.record(nil)
			return it, nil
		}
		if !IsInvocation(err) || !r.retries(err, attempt, attempts) {
			return nil, err
		}
		if waitErr := waitForRetry(ctx, r.retryDelay(err, attempt)); waitErr != nil {
			return nil, waitErr
		}
	}
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
			r.record(nil)
			return audio, format, nil
		}
		if !r.retries(err, attempt, attempts) {
			return nil, "", err
		}
		if waitErr := waitForRetry(ctx, r.retryDelay(err, attempt)); waitErr != nil {
			return nil, "", waitErr
		}
	}
}

func (r *ResilientProvider) GenerateImages(model, prompt string, count int) ([]GeneratedImage, map[string]any, error) {
	return r.GenerateImagesContext(context.Background(), model, prompt, count)
}

func (r *ResilientProvider) Embed(ctx context.Context, model string, input any) (map[string]any, error) {
	embedder, ok := AsEmbeddingProvider(r.inner)
	if !ok {
		return nil, &ConfigError{Msg: "provider does not support embeddings"}
	}
	if err := r.checkCircuit(); err != nil {
		return nil, err
	}
	result, err := embedder.Embed(ctx, model, input)
	r.record(err)
	return result, err
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
	r.record(err)
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
	r.record(err)
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
			r.record(nil)
			return job, nil
		}
		if !r.retries(err, attempt, attempts) {
			return VideoJob{}, err
		}
		if waitErr := waitForRetry(ctx, r.retryDelay(err, attempt)); waitErr != nil {
			return VideoJob{}, waitErr
		}
	}
}

// retries reports whether a failed try is repeated: a transient failure with
// attempts left. Otherwise the failure is the operation's outcome, and it
// moves the circuit.
func (r *ResilientProvider) retries(err error, attempt, attempts int) bool {
	if !InvocationRetryable(err) || attempt >= attempts {
		r.record(err)
		return false
	}
	return true
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
