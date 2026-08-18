package api

import (
	"bytes"
	"encoding/json"
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

func TestRequestLogExcludesBodiesByDefault(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LLMGW_STATE_DIR", dir)
	request, _ := http.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	requestBody := []byte(`{"model":"test-model","messages":[{"role":"user","content":"private prompt"}]}`)
	responseBody := []byte(`{"choices":[{"message":{"content":"private response"}}]}`)
	writeRequestLog(
		request, requestBody, int64(len(requestBody)), http.StatusOK,
		responseBody, int64(len(responseBody)), time.Millisecond, false,
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

func TestRequestLogBodiesRequireExplicitUnsafeOptIn(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LLMGW_STATE_DIR", dir)
	request, _ := http.NewRequest(http.MethodPost, "/v1/responses", nil)
	requestBody := []byte(`{"model":"test-model","input":"debug body"}`)
	responseBody := []byte(`{"output_text":"debug response"}`)
	writeRequestLog(
		request, requestBody, int64(len(requestBody)), http.StatusOK,
		responseBody, int64(len(responseBody)), time.Millisecond, true,
	)
	raw, err := os.ReadFile(filepath.Join(config.StateDir(), "requests.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "debug body") ||
		!strings.Contains(string(raw), "debug response") {
		t.Fatalf("explicit body log omitted debug bodies: %s", raw)
	}
}
