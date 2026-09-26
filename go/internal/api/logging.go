package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"llmgw/internal/config"
)

// Opt-in request metadata logging for debugging what routes a coding CLI uses.
// Enable with LLMGW_LOG_REQUESTS=1; entries are appended to
// <state_dir>/requests.jsonl. Bodies are excluded by default because prompts
// and responses can contain credentials, PII, and proprietary content. An
// operator must separately opt into unsafe body capture with
// LLMGW_LOG_REQUEST_BODIES=1.

const reqLogBodyCap = 1 << 20                      // 1 MiB
const defaultRequestLogMaxBytes = int64(100 << 20) // 100 MiB

var (
	logOnce    sync.Once
	logEnabled bool
	logMu      sync.Mutex
)

func requestLoggingEnabled() bool {
	logOnce.Do(func() {
		switch strings.ToLower(strings.TrimSpace(os.Getenv("LLMGW_LOG_REQUESTS"))) {
		case "1", "true", "yes", "on":
			logEnabled = true
		}
	})
	return logEnabled
}

func requestLogPath() string { return filepath.Join(config.StateDir(), "requests.jsonl") }

// capturingWriter tees the response body (up to a cap) while forwarding
// everything to the real client, and remembers the status code.
type capturingWriter struct {
	http.ResponseWriter
	status       int
	body         bytes.Buffer
	bytesWritten int64
	wrote        bool
	captureBody  bool
}

// capturingReadCloser records at most reqLogBodyCap bytes while allowing the
// handler to consume the original request body in full. Logging must never alter
// request semantics (large context, images and audio routinely exceed 1 MiB).
type capturingReadCloser struct {
	body        bytes.Buffer
	bytesRead   int64
	captureBody bool
	rc          interface {
		Read([]byte) (int, error)
		Close() error
	}
}

func (c *capturingReadCloser) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	c.bytesRead += int64(n)
	if c.captureBody && n > 0 && c.body.Len() < reqLogBodyCap {
		keep := reqLogBodyCap - c.body.Len()
		if n < keep {
			keep = n
		}
		_, _ = c.body.Write(p[:keep])
	}
	return n, err
}

func (c *capturingReadCloser) Close() error { return c.rc.Close() }

func (c *capturingWriter) WriteHeader(code int) {
	c.status = code
	c.wrote = true
	c.ResponseWriter.WriteHeader(code)
}

func (c *capturingWriter) Write(b []byte) (int, error) {
	if !c.wrote {
		c.status = http.StatusOK
		c.wrote = true
	}
	c.bytesWritten += int64(len(b))
	if c.captureBody && c.body.Len() < reqLogBodyCap {
		if n := reqLogBodyCap - c.body.Len(); len(b) <= n {
			c.body.Write(b)
		} else {
			c.body.Write(b[:n])
		}
	}
	return c.ResponseWriter.Write(b)
}

// Flush preserves SSE streaming through the wrapper.
func (c *capturingWriter) Flush() {
	_ = c.FlushError()
}

func (c *capturingWriter) FlushError() error {
	return http.NewResponseController(c.ResponseWriter).Flush()
}

// requestLogMiddleware logs POST /v1/* traffic when LLMGW_LOG_REQUESTS is set.
func requestLogMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !requestLoggingEnabled() || r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, "/v1/") {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		includeBodies := requestBodyLoggingEnabled()
		var captured *capturingReadCloser
		if r.Body != nil {
			captured = &capturingReadCloser{rc: r.Body, captureBody: includeBodies}
			r.Body = captured
		}
		cw := &capturingWriter{
			ResponseWriter: w, status: http.StatusOK, captureBody: includeBodies,
		}
		next.ServeHTTP(cw, r)
		var reqBody []byte
		if captured != nil {
			reqBody = captured.body.Bytes()
		}
		requestBytes := int64(0)
		if captured != nil {
			requestBytes = captured.bytesRead
		}
		writeRequestLog(
			r, reqBody, requestBytes, cw.status, cw.body.Bytes(),
			cw.bytesWritten, time.Since(start), includeBodies,
		)
	})
}

func requestBodyLoggingEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LLMGW_LOG_REQUEST_BODIES"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func asJSONOrString(b []byte) any {
	var m map[string]any
	if json.Unmarshal(b, &m) == nil {
		return m
	}
	return string(b)
}

func writeRequestLog(
	r *http.Request,
	reqBody []byte,
	requestBytes int64,
	status int,
	respBody []byte,
	responseBytes int64,
	dur time.Duration,
	includeBodies bool,
) {
	model := ""
	var m map[string]any
	// Model-level telemetry already lives in the usage ledger. In metadata-only
	// mode we deliberately avoid buffering/parsing request content just to copy
	// the model id into this debugging log.
	if includeBodies && json.Unmarshal(reqBody, &m) == nil {
		if s, ok := m["model"].(string); ok {
			model = s
		}
	}
	entry := map[string]any{
		"ts":             time.Now().UTC().Format(time.RFC3339Nano),
		"path":           r.URL.Path,
		"status":         status,
		"dur_ms":         dur.Milliseconds(),
		"adapted":        r.Header.Get("X-LLMGW-Adapted"),
		"request_bytes":  requestBytes,
		"response_bytes": responseBytes,
	}
	if includeBodies {
		entry["model"] = model
		entry["request"] = asJSONOrString(reqBody)
		entry["response"] = asJSONOrString(respBody)
	}
	b, err := json.Marshal(entry)
	if err != nil {
		return
	}
	logMu.Lock()
	defer logMu.Unlock()
	path := requestLogPath()
	if err := rotateRequestLog(path, requestLogMaxBytes()); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

func requestLogMaxBytes() int64 {
	raw := strings.TrimSpace(os.Getenv("LLMGW_LOG_REQUESTS_MAX_BYTES"))
	if raw == "" {
		return defaultRequestLogMaxBytes
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 {
		return defaultRequestLogMaxBytes
	}
	return value
}

func rotateRequestLog(path string, maxBytes int64) error {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil || info.Size() < maxBytes {
		return err
	}
	rotated := path + ".1"
	if err := os.Remove(rotated); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Rename(path, rotated)
}
