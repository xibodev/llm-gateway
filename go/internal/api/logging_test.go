package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"llmgw/internal/config"
)

func TestRotateRequestLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "requests.jsonl")
	if err := os.WriteFile(path, []byte("1234567890"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := rotateRequestLog(path, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("active log still exists: %v", err)
	}
	if raw, err := os.ReadFile(path + ".1"); err != nil || string(raw) != "1234567890" {
		t.Fatalf("rotated log=%q err=%v", raw, err)
	}
}

func TestCapturingReadCloserDoesNotTruncateHandlerBody(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), reqLogBodyCap+4096)
	capture := &capturingReadCloser{
		rc: io.NopCloser(bytes.NewReader(payload)), captureBody: true,
	}
	got, err := io.ReadAll(capture)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("handler body length=%d, want %d", len(got), len(payload))
	}
	if capture.body.Len() != reqLogBodyCap {
		t.Fatalf("logged body length=%d, want cap %d", capture.body.Len(), reqLogBodyCap)
	}
	if capture.bytesRead != int64(len(payload)) {
		t.Fatalf("request byte count=%d, want %d", capture.bytesRead, len(payload))
	}
}

func TestCapturingReadCloserSkipsBodyBufferByDefault(t *testing.T) {
	payload := []byte(`{"model":"private","input":"sensitive prompt"}`)
	capture := &capturingReadCloser{rc: io.NopCloser(bytes.NewReader(payload))}
	got, err := io.ReadAll(capture)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("request body changed while counting bytes")
	}
	if capture.body.Len() != 0 {
		t.Fatalf("metadata-only capture retained %d body bytes", capture.body.Len())
	}
	if capture.bytesRead != int64(len(payload)) {
		t.Fatalf("request byte count=%d, want %d", capture.bytesRead, len(payload))
	}
}

func TestCapturingReadCloserUsesExactContentLengthWithoutEOF(t *testing.T) {
	payload := []byte(`{"model":"test-model","input":{"email":"person@example.com"}}`)
	capture := &capturingReadCloser{
		rc:          io.NopCloser(&singleReadBody{body: payload}),
		contentLen:  int64(len(payload)),
		captureBody: true,
	}
	var decoded map[string]any
	if err := json.NewDecoder(capture).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	if !capture.complete || capture.bytesRead != int64(len(payload)) {
		t.Fatalf("complete=%v bytes=%d", capture.complete, capture.bytesRead)
	}
	snapshot, ok := diagnosticBodySnapshot(capture.body.Bytes(), capture.bytesRead, capture.complete, "application/json").(map[string]any)
	if !ok || snapshot["model"] != "test-model" {
		t.Fatalf("snapshot=%#v", snapshot)
	}
	input := snapshot["input"].(map[string]any)
	if input["email"] != "[redacted]" {
		t.Fatalf("input=%#v", input)
	}
}

func TestCapturingReadCloserDoesNotTrustUnsafeContentLength(t *testing.T) {
	tests := []struct {
		name       string
		contentLen int64
		payload    []byte
	}{
		{"unknown", -1, []byte(`{"model":"test"}`)},
		{"mismatch", 8, []byte(`{"model":"test"}`)},
		{"over cap", reqLogBodyCap + 1, bytes.Repeat([]byte("x"), reqLogBodyCap+1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			capture := &capturingReadCloser{
				rc:          io.NopCloser(&singleReadBody{body: test.payload}),
				contentLen:  test.contentLen,
				captureBody: true,
			}
			buffer := make([]byte, len(test.payload))
			if n, err := capture.Read(buffer); err != nil || n != len(test.payload) {
				t.Fatalf("read=%d err=%v", n, err)
			}
			if capture.complete {
				t.Fatal("content length incorrectly established completeness")
			}
		})
	}
}

func TestRequestLogExcludesBodiesByDefault(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LLMGW_STATE_DIR", dir)
	request, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	requestBody := []byte(`{"model":"test-model","messages":[{"role":"user","content":"private prompt"}]}`)
	responseBody := []byte(`{"choices":[{"message":{"content":"private response"}}]}`)
	writeRequestLog(
		request, requestBody, int64(len(requestBody)), true, http.StatusOK,
		responseBody, int64(len(responseBody)), true, "application/json", time.Millisecond, false,
	)
	raw, err := os.ReadFile(filepath.Join(config.StateDir(), "requests.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "private prompt") ||
		strings.Contains(string(raw), "private response") {
		t.Fatalf("metadata-only log persisted body content: %s", raw)
	}
	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(raw), &entry); err != nil {
		t.Fatal(err)
	}
	if entry["model"] != nil || entry["request"] != nil || entry["response"] != nil {
		t.Fatalf("metadata-only entry=%+v", entry)
	}
	if entry["request_bytes"] != float64(len(requestBody)) ||
		entry["response_bytes"] != float64(len(responseBody)) {
		t.Fatalf("body size metadata=%+v", entry)
	}
}

func TestDiagnosticBodySnapshotSanitizesPlainTextAcrossChunks(t *testing.T) {
	secret := "Bearer chunk-spanning-secret"
	capture := &capturingReadCloser{
		rc:          io.NopCloser(&chunkReader{chunks: [][]byte{[]byte("prefix Bearer chunk-"), []byte("spanning-secret person@example.com")}}),
		contentLen:  -1,
		captureBody: true,
	}
	got, err := io.ReadAll(capture)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "prefix "+secret+" person@example.com" {
		t.Fatalf("downstream request=%q", got)
	}
	snapshot := diagnosticBodySnapshot(capture.body.Bytes(), capture.bytesRead, capture.complete, "text/plain")
	text, ok := snapshot.(string)
	if !ok || strings.Contains(text, "chunk-spanning-secret") || strings.Contains(text, "person@example.com") {
		t.Fatalf("plain-text snapshot=%#v", snapshot)
	}
}

func TestDiagnosticBodySnapshotOmitsUnsafePrefixes(t *testing.T) {
	tests := []struct {
		name        string
		body        []byte
		byteCount   int64
		complete    bool
		contentType string
		reason      string
	}{
		{"capture limit", bytes.Repeat([]byte("secret"), reqLogBodyCap/6+1)[:reqLogBodyCap], reqLogBodyCap + 9, true, "text/plain", "capture_limit_reached"},
		{"incomplete JSON", []byte(`{"token":"secret`), 18, false, "application/json", "incomplete_capture"},
		{"invalid JSON", []byte(`{"token":"secret`), 17, true, "application/json", "invalid_json"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := diagnosticBodySnapshot(test.body, test.byteCount, test.complete, test.contentType)
			summary, ok := got.(map[string]any)
			if !ok || summary["reason"] != test.reason || summary["byte_count"] != test.byteCount {
				t.Fatalf("snapshot=%#v", got)
			}
			encoded, err := json.Marshal(got)
			if err != nil || strings.Contains(string(encoded), "secret") {
				t.Fatalf("unsafe or invalid snapshot=%s err=%v", encoded, err)
			}
		})
	}
}

func TestCapturingWriterPreservesResponseAndFlush(t *testing.T) {
	underlying := &flushRecorder{header: make(http.Header)}
	capture := &capturingWriter{ResponseWriter: underlying, captureBody: true}
	capture.WriteHeader(http.StatusCreated)
	capture.WriteHeader(http.StatusBadGateway)
	first := []byte("data: Bearer stream-")
	second := []byte("secret\n\n")
	if _, err := capture.Write(first); err != nil {
		t.Fatal(err)
	}
	capture.Flush()
	if _, err := capture.Write(second); err != nil {
		t.Fatal(err)
	}

	want := append(append([]byte(nil), first...), second...)
	if underlying.status != http.StatusCreated || !bytes.Equal(underlying.body.Bytes(), want) ||
		capture.bytesWritten != int64(len(want)) || underlying.flushes != 1 {
		t.Fatalf("status=%d body=%q bytes=%d flushes=%d", underlying.status, underlying.body.Bytes(), capture.bytesWritten, underlying.flushes)
	}
	snapshot := diagnosticBodySnapshot(capture.body.Bytes(), capture.bytesWritten, true, "text/event-stream")
	if strings.Contains(snapshot.(string), "stream-secret") {
		t.Fatalf("stream snapshot retained secret: %#v", snapshot)
	}
}

func TestCapturingWriterCountsAndCapturesOnlyWrittenBytes(t *testing.T) {
	underlying := &shortResponseWriter{header: make(http.Header), limit: 5}
	capture := &capturingWriter{ResponseWriter: underlying, captureBody: true}
	payload := []byte("written-secret-tail")
	n, err := capture.Write(payload)
	if n != underlying.limit || err != io.ErrShortWrite {
		t.Fatalf("write=%d err=%v", n, err)
	}
	if capture.bytesWritten != int64(n) || capture.body.String() != string(payload[:n]) ||
		underlying.body.String() != string(payload[:n]) || !capture.incomplete {
		t.Fatalf("client=%q capture=%q bytes=%d incomplete=%v", underlying.body.String(), capture.body.String(), capture.bytesWritten, capture.incomplete)
	}
}

func TestCapturingWriterFlushCommitsImplicitOK(t *testing.T) {
	underlying := &flushRecorder{header: make(http.Header)}
	capture := &capturingWriter{ResponseWriter: underlying, captureBody: true}
	capture.Flush()
	capture.WriteHeader(http.StatusCreated)

	if underlying.status != http.StatusOK || capture.status != http.StatusOK || !capture.wrote || underlying.flushes != 1 {
		t.Fatalf("actual=%d logged=%d wrote=%v flushes=%d", underlying.status, capture.status, capture.wrote, underlying.flushes)
	}
}

type chunkReader struct {
	chunks [][]byte
}

type singleReadBody struct {
	body []byte
	read bool
}

func (r *singleReadBody) Read(p []byte) (int, error) {
	if r.read {
		return 0, errors.New("unexpected read after content length")
	}
	r.read = true
	return copy(p, r.body), nil
}

func (r *chunkReader) Read(p []byte) (int, error) {
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.chunks[0])
	r.chunks[0] = r.chunks[0][n:]
	if len(r.chunks[0]) == 0 {
		r.chunks = r.chunks[1:]
	}
	return n, nil
}

type flushRecorder struct {
	header  http.Header
	body    bytes.Buffer
	status  int
	flushes int
}

func (w *flushRecorder) Header() http.Header         { return w.header }
func (w *flushRecorder) WriteHeader(status int)      { w.status = status }
func (w *flushRecorder) Write(p []byte) (int, error) { return w.body.Write(p) }
func (w *flushRecorder) Flush()                      { w.flushes++ }

type shortResponseWriter struct {
	header http.Header
	body   bytes.Buffer
	limit  int
}

func (w *shortResponseWriter) Header() http.Header { return w.header }
func (w *shortResponseWriter) WriteHeader(int)     {}
func (w *shortResponseWriter) Write(p []byte) (int, error) {
	n, _ := w.body.Write(p[:w.limit])
	return n, io.ErrShortWrite
}

func TestRequestLogBodiesAreSanitized(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LLMGW_STATE_DIR", dir)
	request, _ := http.NewRequest(http.MethodPost, "/v1/responses", nil)
	requestBody := []byte(`{"model":"test-model","input":{"email":"person@example.com","authorization":"Bearer request-secret-value"}}`)
	responseBody := []byte(`{"output":[{"text":"email person@example.com token sk-response-secret"}]}`)
	writeRequestLog(
		request, requestBody, int64(len(requestBody)), true, http.StatusOK,
		responseBody, int64(len(responseBody)), true, "application/json", time.Millisecond, true,
	)
	raw, err := os.ReadFile(filepath.Join(config.StateDir(), "requests.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "person@example.com") || strings.Contains(string(raw), "request-secret") ||
		strings.Contains(string(raw), "response-secret") {
		t.Fatalf("body log retained sensitive values: %s", raw)
	}
	if !strings.Contains(string(raw), `"authorization":"[redacted]"`) ||
		!strings.Contains(string(raw), `"email":"[redacted]"`) {
		t.Fatalf("body log did not retain sanitized structure: %s", raw)
	}
}
