package providers

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"
)

const inferenceMaxResponseBytes = 64 << 20

// ---- shared HTTP client ------------------------------------------------- //

// httpClient bounds a whole request, which suits a catalog or another short
// document; an inference request uses providerClient.
func httpClient(timeout float64) *http.Client {
	return &http.Client{Timeout: time.Duration(timeout * float64(time.Second))}
}

// providerClient returns the client of a provider's inference requests,
// whose timeout, in seconds, bounds waiting rather than answers: a request
// waits that long for its response to begin, and its body that long between
// two reads. An answer that keeps arriving is never cut, however long it
// runs, and one that stalls still ends. A whole-request Client.Timeout would
// also cut a healthy stream that outlasts it. A timeout that is not positive
// waits indefinitely, as a zero Client.Timeout did. A non-streaming body is
// still bounded by the size limit its reader applies.
func providerClient(timeout float64) *http.Client {
	wait := time.Duration(timeout * float64(time.Second))
	if wait <= 0 {
		return &http.Client{}
	}
	transport := &http.Transport{}
	if shared, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = shared.Clone()
	}
	transport.ResponseHeaderTimeout = wait
	return &http.Client{Transport: &idleTimeoutTransport{next: transport, idle: wait}}
}

// idleTimeoutTransport gives every response body it returns an idle timeout;
// see idleBody.
type idleTimeoutTransport struct {
	next http.RoundTripper
	idle time.Duration
}

func (t *idleTimeoutTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.next.RoundTrip(request)
	if err != nil {
		return response, err
	}
	response.Body = newIdleBody(response.Body, t.idle)
	return response, nil
}

// idleTimeoutError ends a response body that sent nothing for the provider's
// timeout.
type idleTimeoutError struct{ idle time.Duration }

func (e *idleTimeoutError) Error() string {
	return "upstream sent nothing for " + e.idle.String()
}

// idleBody is a response body whose reads wait at most idle: a timer closes
// the body under a read that waits longer, which ends the read with an
// idleTimeoutError. Only time spent waiting for the upstream counts, so a
// reader that is slow to ask for more never ends its own stream.
type idleBody struct {
	body  io.ReadCloser
	idle  time.Duration
	timer *time.Timer

	mu      sync.Mutex
	expired bool
	closed  bool
}

func newIdleBody(body io.ReadCloser, idle time.Duration) *idleBody {
	b := &idleBody{body: body, idle: idle}
	b.timer = time.AfterFunc(math.MaxInt64, b.expire)
	b.timer.Stop()
	return b
}

func (b *idleBody) expire() {
	b.mu.Lock()
	b.expired = true
	b.mu.Unlock()
	_ = b.body.Close()
}

func (b *idleBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	if b.expired {
		b.mu.Unlock()
		return 0, &idleTimeoutError{idle: b.idle}
	}
	b.timer.Reset(b.idle)
	b.mu.Unlock()
	n, err := b.body.Read(p)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.timer.Stop() || b.closed {
		return n, err
	}
	// The timer fired during the read: the body is closed, or closing.
	return n, &idleTimeoutError{idle: b.idle}
}

func (b *idleBody) Close() error {
	b.mu.Lock()
	b.closed = true
	b.timer.Stop()
	b.mu.Unlock()
	return b.body.Close()
}

func decodeJSON(r io.Reader) (map[string]any, error) {
	var out map[string]any
	dec := json.NewDecoder(r)
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// readInvocationResponseBody distinguishes a truncated successful response from
// a malformed complete payload. An incomplete error body is discarded so a
// credential fragment cut before its recognizable suffix cannot reach logs.
func readInvocationResponseBody(response *http.Response, prefix string) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(response.Body, inferenceMaxResponseBytes+1))
	if len(raw) > inferenceMaxResponseBytes {
		if response.StatusCode >= http.StatusBadRequest {
			return nil, nil
		}
		return nil, circuitFailureInvocation(prefix + ": response body exceeded the size limit")
	}
	if err != nil && response.StatusCode < http.StatusBadRequest {
		return nil, retryableInvocation(prefix + ": response body transport error: " + err.Error())
	}
	if err != nil {
		return nil, nil
	}
	return raw, nil
}

// extractError pulls a human error message out of a JSON error body, falling
// back to the raw text.
func extractError(body []byte) string {
	var obj map[string]any
	if json.Unmarshal(body, &obj) == nil {
		if err, ok := obj["error"].(map[string]any); ok {
			if msg, ok := err["message"].(string); ok && msg != "" {
				return msg
			}
		}
		if s, ok := obj["error"].(string); ok {
			return s
		}
		return string(body)
	}
	t := strings.TrimSpace(string(body))
	if t == "" {
		return "no body"
	}
	return t
}

// HTTPInvocationError converts a non-success HTTP response into the provider
// error type used by the gateway while preserving its status for the client.
func HTTPInvocationError(prefix string, status int, body []byte) error {
	return invocationStatus(
		SanitizeDiagnosticTextLimit(
			fmt.Sprintf("%s: upstream returned %d: %s", prefix, status, extractError(body)),
			diagnosticErrorLimit,
		),
		status,
	)
}
