package providers

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"

	core "github.com/xibodev/llmgw-core"
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

// durationText reports text that reads as one duration, such as "59.9s".
func durationText(text string) bool {
	_, err := time.ParseDuration(text)
	return err == nil
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
		if remaining := strings.TrimPrefix(err.Error(), provider.name+": circuit breaker open for another "); !durationText(remaining) {
			t.Fatalf("refusal=%v, want the time the circuit stays open as a duration", err)
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

// Instances of a provider share its circuit only within one caller scope, as
// the instances the factory caches for one scope do; another scope's circuit
// stays closed.
func TestInstancesShareTheCircuitOfTheirCallerScope(t *testing.T) {
	policy := config.ProviderPolicy{RetryMaxAttempts: 1, CircuitFailureThreshold: 1, CircuitCooldownSeconds: 60}
	first, _ := circuitFixture(t, policy)
	first.scope = first.name + "@prn_first"
	sameInner, otherInner := &circuitProvider{}, &circuitProvider{}
	same := &ResilientProvider{inner: sameInner, name: first.name, scope: first.scope, policy: policy}
	other := &ResilientProvider{inner: otherInner, name: first.name, scope: first.name + "@prn_other", policy: policy}
	_ = completeThrough(first)
	if err := completeThrough(same); UpstreamStatus(err) != http.StatusServiceUnavailable || sameInner.calls != 0 {
		t.Fatalf("err=%v calls=%d, want the circuit the first instance opened", err, sameInner.calls)
	}
	if err := completeThrough(other); err != nil || otherInner.calls != 1 {
		t.Fatalf("err=%v calls=%d, want another scope's circuit closed", err, otherInner.calls)
	}
}

// Each caller scope the factory builds an instance for keeps its own circuit:
// a principal's rate-limited private key opens only that principal's, and
// neither another principal's answers nor its successes end that streak. The
// gateway's shared key still opens the gateway's circuit, and resetting the
// provider closes every scope's.
func TestEachCallerScopeOfAProviderHasItsOwnCircuit(t *testing.T) {
	var (
		mu       sync.Mutex
		limited  = map[string]bool{"Bearer fixture-key-a": true, "Bearer fixture-key-gateway": true}
		requests = map[string]int{}
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Authorization")
		mu.Lock()
		requests[key]++
		refuse := limited[key]
		mu.Unlock()
		if refuse {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"fixture","object":"chat.completion","model":"fixture-model","choices":[` +
			`{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer upstream.Close()
	installAnonymousFixture(t, map[string]*config.ProviderConfig{
		"scoped": {Type: "openai_compatible", BaseURL: upstream.URL, APIKey: "fixture-key-gateway"},
	})
	config.Update(func(s *config.Settings) {
		s.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
		s.Policies.Overrides = map[string]config.ProviderPolicy{
			"scoped": {RetryMaxAttempts: 1, CircuitFailureThreshold: 2, CircuitCooldownSeconds: 60},
		}
	})
	callers := map[string]core.Caller{"gateway": gatewayCaller()}
	for _, name := range []string{"a", "b"} {
		human, err := iam.CreatePrincipal("human", "fixture:circuit-"+name, "", "Fixture "+name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := iam.PutProviderConnection(iam.ProviderConnectionCreate{
			PrincipalID: human.ID, ProviderID: "scoped", Name: "personal", Kind: "api_key", Secret: "fixture-key-" + name,
		}); err != nil {
			t.Fatal(err)
		}
		callers[name] = core.Caller{ID: human.ID, Kind: core.CallerHuman}
	}
	complete := func(name string) error {
		t.Helper()
		provider, err := GetProviderForPrincipal("scoped", callers[name])
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		_, err = provider.(*ResilientProvider).CompleteContext(
			context.Background(), "fixture-model", []Message{{"role": "user", "content": "hi"}}, nil,
		)
		return err
	}
	sent := func(name string) int {
		mu.Lock()
		defer mu.Unlock()
		return requests["Bearer fixture-key-"+name]
	}
	refused := func(err error) bool { return err != nil && strings.Contains(err.Error(), "circuit breaker open") }

	// a's failures interleave with b's successes, which a shared circuit
	// would count as the end of a's streak.
	for range 2 {
		if err := complete("a"); err == nil || refused(err) {
			t.Fatalf("a: err=%v, want the upstream refusal", err)
		}
		if err := complete("b"); err != nil {
			t.Fatalf("b: %v", err)
		}
	}
	if err := complete("a"); !refused(err) || sent("a") != 2 {
		t.Fatalf("a: err=%v sent=%d, want its own circuit open after two failures", err, sent("a"))
	}
	if err := complete("b"); err != nil || sent("b") != 3 {
		t.Fatalf("b: err=%v sent=%d, want b served past a's open circuit", err, sent("b"))
	}
	for range 2 {
		if err := complete("gateway"); err == nil || refused(err) {
			t.Fatalf("gateway: err=%v, want the upstream refusal", err)
		}
	}
	if err := complete("gateway"); !refused(err) || sent("gateway") != 2 {
		t.Fatalf("gateway: err=%v sent=%d, want the gateway's circuit open", err, sent("gateway"))
	}
	if err := complete("b"); err != nil || sent("b") != 4 {
		t.Fatalf("b: err=%v sent=%d, want b served past the gateway's open circuit", err, sent("b"))
	}

	ResetCircuit("scoped")
	for _, name := range []string{"a", "gateway"} {
		if err := complete(name); refused(err) || sent(name) != 3 {
			t.Fatalf("%s: err=%v sent=%d, want the reset to close every scope's circuit", name, err, sent(name))
		}
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

// Open circuits are listed with the callers each refuses, its failures and
// when it admits requests again. A circuit below its threshold, one a success
// closed and one whose cooldown has passed are not listed, and resetting the
// provider forgets them all.
func TestOpenCircuitsNameTheCallersTheyRefuse(t *testing.T) {
	policy := config.ProviderPolicy{RetryMaxAttempts: 1, CircuitFailureThreshold: 2, CircuitCooldownSeconds: 60}
	gateway, inner := circuitFixture(t, policy)
	name := gateway.name
	scoped := func(scope string) *ResilientProvider {
		return &ResilientProvider{inner: inner, name: name, scope: scope, policy: policy}
	}
	human, service := scoped(name+"@prn_human"), scoped(name+"@prn_service#prj_tools")
	_ = completeThrough(gateway)
	if open := OpenCircuits(name, time.Now()); len(open) != 0 {
		t.Fatalf("a circuit below its threshold is listed: %+v", open)
	}
	_ = completeThrough(gateway)
	for range 2 {
		_ = completeThrough(human)
		_ = completeThrough(service)
	}
	now := time.Now()
	open := OpenCircuits(name, now)
	callers := []string{}
	for _, circuit := range open {
		callers = append(callers, circuit.PrincipalID+"#"+circuit.ProjectID)
		if circuit.Failures != 2 || !circuit.OpenUntil.After(now) || circuit.OpenUntil.After(now.Add(time.Minute)) {
			t.Fatalf("circuit %+v, want two failures and a minute's cooldown", circuit)
		}
	}
	if strings.Join(callers, ",") != "#,prn_human#,prn_service#prj_tools" {
		t.Fatalf("open circuits refuse %v", callers)
	}
	Current().circuits.record(name, human.scope, policy, nil)
	if open := OpenCircuits(name, now); len(open) != 2 || open[1].PrincipalID != "prn_service" {
		t.Fatalf("after a success closed one: %+v", open)
	}
	if open := OpenCircuits(name, now.Add(time.Minute+time.Second)); len(open) != 0 {
		t.Fatalf("circuits past their cooldown are listed: %+v", open)
	}
	ResetCircuit(name)
	circuits := &Current().circuits
	circuits.mu.Lock()
	failing := len(circuits.failing[name])
	circuits.mu.Unlock()
	if open := OpenCircuits(name, now); len(open) != 0 || failing != 0 {
		t.Fatalf("after a reset: %+v, %d failing scopes kept", open, failing)
	}
}
