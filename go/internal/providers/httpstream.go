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

// httpStreamIter returns complete SSE data payloads and surfaces parser or
// mid-stream transport errors via Err.
type httpStreamIter struct {
	resp   *http.Response
	reader *sseRecordReader
	err    error
	prefix string // error message prefix for this provider
}

func newHTTPStreamIter(resp *http.Response, prefix string) *httpStreamIter {
	return &httpStreamIter{resp: resp, reader: newSSERecordReader(resp.Body), prefix: prefix}
}

func (it *httpStreamIter) Next() (string, bool) {
	payload, ok := it.reader.Next()
	if !ok {
		it.finish(it.reader.Err())
	}
	return payload, ok
}

func (it *httpStreamIter) finish(err error) {
	if _, ok := err.(*StreamRecordTooLargeError); ok {
		it.err = err
	} else if err != nil && err != io.EOF {
		it.err = &InvocationError{Msg: it.prefix + ": streaming transport error: " + err.Error()}
	}
}

func (it *httpStreamIter) Err() error { return it.err }
func (it *httpStreamIter) Close() error {
	if it.resp != nil {
		return it.resp.Body.Close()
	}
	return nil
}

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
