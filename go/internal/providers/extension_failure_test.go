package providers

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"llmgw/internal/config"

	core "github.com/xibodev/llmgw-core"
)

// extensionProviderFixture configures instance as a provider of
// providerType until the test ends and returns its facade, built by a
// Runtime of its own whose store holds no credential, so each request
// reaches the daemon without one.
func extensionProviderFixture(t *testing.T, providerType, instance string) Provider {
	t.Helper()
	configured := config.Get().Providers
	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{instance: {Type: providerType}}
	})
	t.Cleanup(func() { config.Update(func(s *config.Settings) { s.Providers = configured }) })
	runtime := newRuntime(func(bool) (core.CredentialStore, error) { return core.NewMemoryCredentialStore(), nil })
	return runtime.newExtensionFacade(instance, providerType, core.Caller{})
}

// extensionFacadeFixture is extensionProviderFixture for a provider type
// whose facade is the plain one.
func extensionFacadeFixture(t *testing.T, providerType, instance string) *ExtensionProviderFacade {
	t.Helper()
	return extensionProviderFixture(t, providerType, instance).(*ExtensionProviderFacade)
}

// refuseExtension answers as a daemon that failed an operation with status,
// and with retryAfter unless it is empty.
func refuseExtension(t *testing.T, status int, retryAfter string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		writeExtensionJSON(t, w, status, map[string]any{"error": map[string]any{"message": "fixture refusal", "code": status}})
	}
}

// A failure of a provider the companion daemon serves reaches the resilience
// wrapper and the router as the gateway's InvocationError, which they act
// on: the daemon's status and its Retry-After, whatever the status, a daemon
// that does not answer as a transport failure, and an answer the gateway
// cannot read as a failure of the circuit.
func TestExtensionFailuresAreInvocationErrors(t *testing.T) {
	unreachable := httptest.NewServer(http.NotFoundHandler())
	unreachable.Close()
	notJSON := func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "not json") }
	for name, test := range map[string]struct {
		answer                       http.HandlerFunc
		status                       int
		retryAfter                   string
		retryable, failover, circuit bool
	}{
		"rate limited": {answer: refuseExtension(t, http.StatusTooManyRequests, "7"), status: 429, retryAfter: "7", retryable: true, failover: true, circuit: true},
		"unavailable":  {answer: refuseExtension(t, http.StatusServiceUnavailable, "3"), status: 503, retryAfter: "3", retryable: true, failover: true, circuit: true},
		"bad request":  {answer: refuseExtension(t, http.StatusBadRequest, ""), status: 400},
		"unreachable":  {retryable: true, failover: true, circuit: true},
	} {
		t.Run(name, func(t *testing.T) {
			mux := http.NewServeMux()
			if test.answer != nil {
				mux.HandleFunc("POST /extension/v1/openai_codex/invoke", test.answer)
				mux.HandleFunc("POST /extension/v1/openai_codex/stream", test.answer)
			}
			extensionDaemon(t, mux)
			if test.answer == nil {
				t.Setenv("LLMGW_EXTENSION_URL", unreachable.URL)
			}
			facade := extensionFacadeFixture(t, ExtensionTypeCodex, "codex")
			messages := []Message{{"role": "user", "content": "hi"}}
			_, completeErr := facade.Complete("gpt-test", messages, nil)
			_, streamErr := facade.StreamContext(context.Background(), "gpt-test", messages, nil)
			for operation, err := range map[string]error{"complete": completeErr, "stream": streamErr} {
				if !IsInvocation(err) {
					t.Fatalf("%s error = %T %v, want an InvocationError", operation, err, err)
				}
				if UpstreamStatus(err) != test.status || InvocationRetryAfter(err) != test.retryAfter ||
					InvocationRetryable(err) != test.retryable || InvocationFailoverEligible(err) != test.failover ||
					InvocationCircuitFailure(err) != test.circuit {
					t.Errorf("%s error %q: status %d, Retry-After %q, retryable %t, failover %t, circuit %t",
						operation, err, UpstreamStatus(err), InvocationRetryAfter(err),
						InvocationRetryable(err), InvocationFailoverEligible(err), InvocationCircuitFailure(err))
				}
			}
		})
	}

	t.Run("unreadable answer", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("POST /extension/v1/openai_codex/invoke", notJSON)
		extensionDaemon(t, mux)
		_, err := extensionFacadeFixture(t, ExtensionTypeCodex, "codex").Complete("gpt-test", nil, nil)
		if !IsInvocation(err) || InvocationRetryable(err) || !InvocationFailoverEligible(err) || !InvocationCircuitFailure(err) {
			t.Fatalf("error = %T %v, want a circuit failure that is not repeated", err, err)
		}
	})
}

// A daemon address the gateway cannot use is the gateway's configuration,
// which no retry or other member changes.
func TestExtensionConfigurationErrorsStayConfigErrors(t *testing.T) {
	t.Setenv("LLMGW_EXTENSION_URL", "http://owner:not-a-secret@127.0.0.1:1")
	_, err := extensionFacadeFixture(t, ExtensionTypeCodex, "codex").Complete("gpt-test", nil, nil)
	var configErr *ConfigError
	if !errors.As(err, &configErr) || IsInvocation(err) {
		t.Fatalf("error = %T %v, want a ConfigError", err, err)
	}
	if strings.Contains(err.Error(), "not-a-secret") {
		t.Fatalf("error repeats the address: %q", err)
	}
}

// The resilience wrapper repeats a daemon's 503 and counts the request it
// could not serve against the circuit.
func TestResilienceRepeatsAndCountsCompanionDaemonFailures(t *testing.T) {
	var calls, refusals atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("POST /extension/v1/openai_codex/invoke", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if refusals.Add(-1) >= 0 {
			refuseExtension(t, http.StatusServiceUnavailable, "")(w, r)
			return
		}
		writeExtensionJSON(t, w, http.StatusOK, map[string]any{"choices": []any{}})
	})
	extensionDaemon(t, mux)
	const name = "codex-resilience"
	Current().ResetCircuit(name)
	t.Cleanup(func() { Current().ResetCircuit(name) })
	provider := &ResilientProvider{
		inner: extensionFacadeFixture(t, ExtensionTypeCodex, name), name: name,
		policy: config.ProviderPolicy{RetryMaxAttempts: 2, CircuitFailureThreshold: 1, CircuitCooldownSeconds: 60},
	}
	complete := func() error {
		_, err := provider.CompleteContext(context.Background(), "gpt-test", []Message{{"role": "user", "content": "hi"}}, nil)
		return err
	}

	refusals.Store(1)
	if err := complete(); err != nil || calls.Load() != 2 {
		t.Fatalf("after one 503: err=%v calls=%d, want served on the repeat", err, calls.Load())
	}
	refusals.Store(2)
	if err := complete(); UpstreamStatus(err) != http.StatusServiceUnavailable || calls.Load() != 4 {
		t.Fatalf("after two 503s: err=%v calls=%d, want the 503 after one repeat", err, calls.Load())
	}
	if err := complete(); err == nil || !strings.Contains(err.Error(), "circuit breaker open") || calls.Load() != 4 {
		t.Fatalf("after a counted failure: err=%v calls=%d, want the open circuit", err, calls.Load())
	}
}
