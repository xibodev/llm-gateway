package router

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/providers"

	core "github.com/xibodev/llmgw-core"
)

// gatewayDisposition is what the gateway does with a failed attempt, read from
// the predicates it acts on rather than restated: the resilience wrapper
// repeats the target while InvocationRetryable holds, the chain moves to the
// next candidate while shouldAdvance holds, and anything else ends the chain.
func gatewayDisposition(err error) core.Disposition {
	switch {
	case providers.InvocationRetryable(err):
		return core.DispositionRetryable
	case shouldAdvance(err):
		return core.DispositionFailover
	}
	return core.DispositionTerminal
}

func TestCoreClassificationMatchesGatewayRouting(t *testing.T) {
	setupEcho(t)
	// One attempt and no circuit, so the transport fixture is a single
	// unwrapped call that leaves no breaker state behind.
	policy := config.Get().Policies.Defaults
	config.Update(func(s *config.Settings) { s.Policies.Defaults = config.ProviderPolicy{RetryMaxAttempts: 1} })
	providers.ResetProviders()
	t.Cleanup(func() {
		config.Update(func(s *config.Settings) { s.Policies.Defaults = policy })
		providers.ResetProviders()
	})

	bad, buildErr := providers.GetProvider("bad")
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	_, transport := providers.CompleteProviderContext(context.Background(), bad, "x", []providers.Message{{"role": "user", "content": "hi"}}, nil)
	if providers.UpstreamStatus(transport) != 0 || !providers.InvocationRetryable(transport) {
		t.Fatalf("fixture is not a transport failure: %v", transport)
	}
	_, factory := providers.GetProvider("missing")
	cases := map[string]error{
		"canceled":       context.Canceled,
		"deadline":       context.DeadlineExceeded,
		"plain":          errors.New("fixture"),
		"transport":      transport,
		"factory config": factory,
		"router config":  Current().responsesFallbackCompatibility(Target{Provider: "missing", Model: "m"}, anonymous, nil, nil),
		"config":         &providers.ConfigError{Msg: "fixture"},
		"stream record":  &providers.StreamRecordTooLargeError{Format: "SSE", Limit: 4 << 20},
	}
	// Each InvocationError constructor in provider.go sets a subset of these
	// fields: invocation none, retryableInvocation Retryable and CircuitFailure,
	// circuitFailureInvocation CircuitFailure, invocationStatus and
	// invocationStatusRetryAfter a Status, and failoverInvocationStatus a Status
	// and FailoverEligible. Walking every combination covers them all, and the
	// literals other code builds.
	for _, status := range []int{0, 400, 401, 403, 404, 408, 409, 429, 500, 502, 503, 504} {
		for flags := range 8 {
			invocation := &providers.InvocationError{
				Msg: "fixture", Status: status,
				Retryable: flags&1 != 0, FailoverEligible: flags&2 != 0, CircuitFailure: flags&4 != 0,
			}
			cases[fmt.Sprintf("status %d retryable %t failover %t circuit %t", status,
				invocation.Retryable, invocation.FailoverEligible, invocation.CircuitFailure)] = invocation
		}
		if status != 0 {
			cases["HTTPInvocationError "+strconv.Itoa(status)] = providers.HTTPInvocationError("fixture", status, nil)
		}
	}
	for name, err := range cases {
		t.Run(name, func(t *testing.T) {
			if err == nil {
				t.Fatal("fixture produced no error")
			}
			for _, candidate := range []error{err, fmt.Errorf("attempt: %w", err)} {
				got := core.ClassifyError(candidate)
				if want := gatewayDisposition(candidate); got.Disposition() != want {
					t.Errorf("core disposition of %q is %s, the gateway's is %s", candidate, got.Disposition(), want)
				}
				// Core lets a retryable failure fail over as well; that holds
				// because the chain advances past everything the wrapper repeats.
				if advance := shouldAdvance(candidate); got.FailoverEligible != advance {
					t.Errorf("core failover of %q is %t, shouldAdvance is %t", candidate, got.FailoverEligible, advance)
				}
				if circuit := providers.InvocationCircuitFailure(candidate); got.CircuitFailure != circuit {
					t.Errorf("core circuit failure of %q is %t, the wrapper's is %t", candidate, got.CircuitFailure, circuit)
				}
				if status := providers.UpstreamStatus(candidate); got.StatusCode != status {
					t.Errorf("core status of %q is %d, the router reports %d", candidate, got.StatusCode, status)
				}
			}
		})
	}
}

// The generic chain stops on a ConfigError, as its classification says, but
// the native Messages chain moves past every error that is not an
// InvocationError, so there the gateway fails over where core would stop. The
// router decides that by surface, which a per-error classification cannot
// express; this pins the divergence so a change on either side revisits it.
func TestConfigErrorEndsGenericChainButNotMessagesChain(t *testing.T) {
	setupEcho(t)
	config.Update(func(s *config.Settings) {
		s.Providers["misconfigured"] = &config.ProviderConfig{Type: "unknown-fixture"}
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)
	if _, err := providers.GetProvider("misconfigured"); !providers.IsConfig(err) ||
		core.ClassifyError(err).Disposition() != core.DispositionTerminal {
		t.Fatalf("fixture is not a terminal ConfigError: %v", err)
	}
	targets := []Target{{Provider: "misconfigured", Model: "m"}, {Provider: "echo", Model: "echo-default"}}

	messages := []providers.Message{{"role": "user", "content": "hi"}}
	if _, served, err := ExecuteComplete(targets, messages, "route", anonymous, nil); err == nil || served != nil {
		t.Fatalf("the generic chain moved past a ConfigError: served=%+v err=%v", served, err)
	}
	payload := map[string]any{"model": "route", "max_tokens": 16, "messages": []any{
		map[string]any{"role": "user", "content": "hi"},
	}}
	if _, served, err := ExecuteAnthropicMessages(targets, payload, "route", anonymous); err != nil || served == nil || served.Provider != "echo" {
		t.Fatalf("the Messages chain stopped at a ConfigError: served=%+v err=%v", served, err)
	}
}
