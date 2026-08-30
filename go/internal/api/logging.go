package api

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"llmgw/internal/config"
	"llmgw/internal/diagnostics"
)

// Opt-in request metadata logging for debugging what routes a coding CLI uses.
// Enable with LLMGW_LOG_REQUESTS=1; entries are appended to
// <state_dir>/requests.jsonl. Bodies are excluded by default because prompts
// and responses can contain credentials, PII, and proprietary content. An
// operator must separately opt into sanitized body snapshots with
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
	incomplete   bool
	captureBody  bool
}

// capturingReadCloser records at most reqLogBodyCap bytes while allowing the
// handler to consume the original request body in full. Logging must never alter
// request semantics (large context, images and audio routinely exceed 1 MiB).
type capturingReadCloser struct {
	body        bytes.Buffer
	bytesRead   int64
	contentLen  int64
	complete    bool
	captureBody bool
	rc          interface {
		Read([]byte) (int, error)
		Close() error
	}
}

func (c *capturingReadCloser) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	c.bytesRead += int64(n)
	if c.contentLen >= 0 {
		c.complete = c.contentLen < reqLogBodyCap && c.bytesRead == c.contentLen
	} else if err == io.EOF {
		c.complete = c.bytesRead < reqLogBodyCap
	}
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
	if c.wrote {
		return
	}
	c.status = code
	c.wrote = true
	c.ResponseWriter.WriteHeader(code)
}

func (c *capturingWriter) Write(b []byte) (int, error) {
	if !c.wrote {
		c.status = http.StatusOK
		c.wrote = true
	}
	n, err := c.ResponseWriter.Write(b)
	c.bytesWritten += int64(n)
	if n != len(b) || err != nil {
		c.incomplete = true
	}
	if c.captureBody && n > 0 && c.body.Len() < reqLogBodyCap {
		remaining := reqLogBodyCap - c.body.Len()
		if n <= remaining {
			_, _ = c.body.Write(b[:n])
		} else {
			_, _ = c.body.Write(b[:remaining])
		}
	}
	return n, err
}

// Flush preserves SSE streaming through the wrapper.
func (c *capturingWriter) Flush() {
	if !c.wrote {
		c.status = http.StatusOK
		c.wrote = true
		c.ResponseWriter.WriteHeader(http.StatusOK)
	}
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
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
			captured = &capturingReadCloser{rc: r.Body, contentLen: r.ContentLength, captureBody: includeBodies}
			r.Body = captured
		}
		cw := &capturingWriter{
			ResponseWriter: w, status: http.StatusOK, captureBody: includeBodies,
		}
		next.ServeHTTP(cw, r)
		var reqBody []byte
		requestComplete := true
		if captured != nil {
			reqBody = captured.body.Bytes()
			requestComplete = captured.complete
		}
		requestBytes := int64(0)
		if captured != nil {
			requestBytes = captured.bytesRead
		}
		writeRequestLog(
			r, reqBody, requestBytes, requestComplete, cw.status, cw.body.Bytes(),
			cw.bytesWritten, !cw.incomplete, cw.Header().Get("Content-Type"), time.Since(start), includeBodies,
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

func diagnosticBodySnapshot(b []byte, byteCount int64, complete bool, contentType string) any {
	if !complete {
		return bodySnapshotSummary("incomplete_capture", byteCount)
	}
	if byteCount >= reqLogBodyCap || len(b) >= reqLogBodyCap {
		return bodySnapshotSummary("capture_limit_reached", byteCount)
	}

	mediaType, _, _ := mime.ParseMediaType(contentType)
	isJSON := mediaType == "application/json" || strings.HasSuffix(mediaType, "+json")
	trimmed := bytes.TrimSpace(b)
	isJSON = isJSON || len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[')
	var value any
	if err := json.Unmarshal(b, &value); err == nil {
		sanitized := diagnostics.SanitizeStructuredValue(value)
		encoded, err := json.Marshal(sanitized)
		if err != nil || len(encoded) > reqLogBodyCap {
			return bodySnapshotSummary("sanitized_snapshot_too_large", byteCount)
		}
		return sanitized
	} else if isJSON {
		return bodySnapshotSummary("invalid_json", byteCount)
	}

	text := diagnostics.SanitizeText(string(b))
	if len(text) > reqLogBodyCap {
		text = truncateUTF8(text, reqLogBodyCap)
	}
	return text
}

func bodySnapshotSummary(reason string, byteCount int64) map[string]any {
	return map[string]any{
		"snapshot":   "omitted",
		"reason":     reason,
		"byte_count": byteCount,
	}
}

func truncateUTF8(text string, maxBytes int) string {
	if len(text) <= maxBytes {
		return text
	}
	for maxBytes > 0 && !utf8.ValidString(text[:maxBytes]) {
		maxBytes--
	}
	return text[:maxBytes]
}

func writeRequestLog(
	r *http.Request,
	reqBody []byte,
	requestBytes int64,
	requestComplete bool,
	status int,
	respBody []byte,
	responseBytes int64,
	responseComplete bool,
	responseContentType string,
	dur time.Duration,
	includeBodies bool,
) {
	model := ""
	var requestSnapshot any
	// Model-level telemetry already lives in the usage ledger. In metadata-only
	// mode we deliberately avoid buffering/parsing request content just to copy
	// the model id into this debugging log.
	if includeBodies {
		requestSnapshot = diagnosticBodySnapshot(reqBody, requestBytes, requestComplete, r.Header.Get("Content-Type"))
		if m, ok := requestSnapshot.(map[string]any); ok {
			if s, ok := m["model"].(string); ok {
				model = s
			}
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
		entry["request"] = requestSnapshot
		entry["response"] = diagnosticBodySnapshot(respBody, responseBytes, responseComplete, responseContentType)
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
