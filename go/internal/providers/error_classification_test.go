package providers

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"llmgw/internal/config"

	core "github.com/xibodev/llmgw-core"
)

// classified is the reading core should take of a gateway error.
type classified struct {
	status      int
	disposition core.Disposition
	circuit     bool
	retryAfter  time.Duration
	class       core.ProviderErrorClass
}

type classificationCase struct {
	err  error
	want classified
}

// assertClassified reads err, bare and wrapped, the way core does:
// ClassifyError for the routing metadata and errors.As for the error class.
func assertClassified(t *testing.T, err error, want classified) {
	t.Helper()
	for _, candidate := range []error{err, fmt.Errorf("attempt: %w", err)} {
		got := core.ClassifyError(candidate)
		if got.StatusCode != want.status || got.Disposition() != want.disposition ||
			got.CircuitFailure != want.circuit || got.RetryAfter != want.retryAfter {
			t.Fatalf("ClassifyError(%q) = %+v (%s), want %+v", candidate, got, got.Disposition(), want)
		}
		var view *core.ProviderError
		found := errors.As(candidate, &view)
		if found != (want.class != "") {
			t.Fatalf("errors.As(%q) found a ProviderError: %t, want class %q", candidate, found, want.class)
		}
		if found && (view.Class != want.class || view.Classification != got || view.Message != "" || !errors.Is(view, err)) {
			t.Fatalf("ProviderError view of %q = %+v, want class %q, no message and the gateway cause", candidate, view, want.class)
		}
	}
}

func TestInvocationErrorClassification(t *testing.T) {
	cases := map[string]classificationCase{
		"invocation": {invocation("fixture"), classified{disposition: core.DispositionFailover}},
		"retryableInvocation": {retryableInvocation("fixture"), classified{
			disposition: core.DispositionRetryable, circuit: true, class: core.ProviderErrorTransport,
		}},
		"circuitFailureInvocation": {circuitFailureInvocation("fixture"), classified{
			disposition: core.DispositionFailover, circuit: true, class: core.ProviderErrorUpstream,
		}},
	}
	for _, status := range []int{400, 401, 403, 404, 408, 409, 429, 500, 502, 503, 504} {
		plain := classified{status: status, disposition: core.DispositionTerminal, class: core.ProviderErrorUpstream}
		switch status {
		case http.StatusUnauthorized:
			plain.class = core.ProviderErrorAuth
		case http.StatusForbidden:
			plain.class = core.ProviderErrorForbidden
		case http.StatusTooManyRequests:
			plain.class = core.ProviderErrorRateLimited
		}
		if status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500 {
			plain.disposition, plain.circuit = core.DispositionRetryable, true
		}
		delayed := plain
		delayed.retryAfter = 7 * time.Second
		// The flag moves a definitive failure on, but never a rejected credential.
		eligible := plain
		if plain.disposition == core.DispositionTerminal && status != http.StatusUnauthorized && status != http.StatusForbidden {
			eligible.disposition = core.DispositionFailover
		}
		name := " " + strconv.Itoa(status)
		cases["invocationStatus"+name] = classificationCase{invocationStatus("fixture", status), plain}
		cases["HTTPInvocationError"+name] = classificationCase{HTTPInvocationError("fixture", status, nil), plain}
		cases["invocationStatusRetryAfter"+name] = classificationCase{invocationStatusRetryAfter("fixture", status, " 7 "), delayed}
		cases["failoverInvocationStatus"+name] = classificationCase{failoverInvocationStatus("fixture", status), eligible}
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) { assertClassified(t, tc.err, tc.want) })
	}
}

func TestNonInvocationErrorsAreTerminal(t *testing.T) {
	for name, tc := range map[string]classificationCase{
		"ConfigError": {&ConfigError{Msg: "provider 'fixture': unknown type 'fixture'"}, classified{
			disposition: core.DispositionTerminal, class: core.ProviderErrorConfiguration,
		}},
		"StreamRecordTooLargeError": {&StreamRecordTooLargeError{Format: "SSE", Limit: maxStreamRecordWireSize}, classified{
			disposition: core.DispositionTerminal, class: core.ProviderErrorUpstream,
		}},
	} {
		t.Run(name, func(t *testing.T) { assertClassified(t, tc.err, tc.want) })
	}
}

func TestNilProviderErrorsHaveNoClassification(t *testing.T) {
	var view *core.ProviderError
	for _, err := range []interface {
		core.ProviderErrorClassifier
		As(any) bool
	}{(*InvocationError)(nil), (*ConfigError)(nil), (*StreamRecordTooLargeError)(nil)} {
		if err.ProviderErrorClassification() != (core.ProviderErrorClassification{}) || err.As(&view) {
			t.Fatalf("a nil %T classified itself", err)
		}
	}
}

func TestRetryAfterDelay(t *testing.T) {
	now := time.Unix(1_900_000_000, 0).UTC()
	for value, want := range map[string]time.Duration{
		"":           0,
		"7":          7 * time.Second,
		" 120 ":      2 * time.Minute,
		"0":          0,
		"-5":         0,
		"1.5":        0,
		"soon":       0,
		"9223372036": 9223372036 * time.Second,
		"9223372037": 0,
		now.Add(90 * time.Second).Format(http.TimeFormat): 90 * time.Second,
		now.Add(time.Hour).Format(time.RFC850):            time.Hour,
		now.Format(http.TimeFormat):                       0,
		now.Add(-time.Minute).Format(http.TimeFormat):     0,
	} {
		if got := retryAfterDelay(value, now); got != want {
			t.Errorf("retryAfterDelay(%q) = %v, want %v", value, got, want)
		}
	}
}

// The wrapper waits the longer of its backoff and the upstream's delay, so the
// delay core reads must be the one the wrapper honours.
func TestRetryAfterIsTheDelayTheWrapperHonours(t *testing.T) {
	provider := &ResilientProvider{policy: config.ProviderPolicy{
		RetryInitialBackoffSeconds: 0.01, RetryBackoffMultiplier: 2, RetryMaxBackoffSeconds: 1,
	}}
	backoff := provider.retryDelay(invocationStatus("fixture", 503), 1)
	for _, value := range []string{"2", " 3 ", "0", "-4", "later"} {
		err := invocationStatusRetryAfter("fixture", 503, value)
		if got, want := provider.retryDelay(err, 1), max(backoff, core.ClassifyError(err).RetryAfter); got != want {
			t.Errorf("Retry-After %q: the wrapper waits %v, core's delay implies %v", value, got, want)
		}
	}
}
