package providers

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const inferenceMaxResponseBytes = 64 << 20

// ---- shared HTTP client ------------------------------------------------- //

func httpClient(timeout float64) *http.Client {
	return &http.Client{Timeout: time.Duration(timeout * float64(time.Second))}
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
