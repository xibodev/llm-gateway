package api

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
)

func TestMaxRequestBodyBytesFollowsTheEnvironment(t *testing.T) {
	for _, test := range []struct {
		value string
		want  int64
	}{
		{"", defaultMaxRequestBodyBytes},
		{"134217728", 128 << 20},
		{" 2097152 ", 2 << 20},
		{"1024", lowestMaxRequestBodyBytes},
		{"0", defaultMaxRequestBodyBytes},
		{"-1", defaultMaxRequestBodyBytes},
		{"64MiB", defaultMaxRequestBodyBytes},
	} {
		t.Setenv("LLMGW_MAX_REQUEST_BODY_BYTES", test.value)
		if got := maxRequestBodyBytes(); got != test.want {
			t.Errorf("LLMGW_MAX_REQUEST_BODY_BYTES=%q gives %d, want %d", test.value, got, test.want)
		}
	}
}

// limitBodiesForTest lowers the request body limit to its floor and opens the
// data plane, so a body just over the limit reaches each handler.
func limitBodiesForTest(t *testing.T) {
	t.Helper()
	resetState(t)
	t.Setenv("LLMGW_MAX_REQUEST_BODY_BYTES", strconv.Itoa(lowestMaxRequestBodyBytes))
	old := *config.Get()
	t.Cleanup(func() { config.Update(func(s *config.Settings) { *s = old }) })
	config.Update(func(s *config.Settings) {
		s.APIKey = "admin-secret"
		s.APIKeys = nil
		s.AllowUnauthenticatedAPI = true
		s.Providers = map[string]*config.ProviderConfig{"echo": {Type: "echo"}}
		s.Endpoints = map[string]*config.EndpointConfig{}
	})
	providers.ResetProviders()
}

func assertBodyTooLarge(t *testing.T, response *httptest.ResponseRecorder, path string, declared bool) {
	t.Helper()
	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if response.Code != http.StatusRequestEntityTooLarge ||
		json.Unmarshal(response.Body.Bytes(), &envelope) != nil ||
		envelope.Error.Code != "413" || envelope.Error.Type != "invalid_request_error" ||
		envelope.Error.Message != requestBodyTooLarge {
		t.Fatalf("%s (declared length %t): status=%d body=%s", path, declared, response.Code, response.Body.String())
	}
}

// oversizedJSON is one valid request object larger than the floor limit.
func oversizedJSON() string {
	return `{"model":"echo/echo-default","input":"` + strings.Repeat("a", lowestMaxRequestBodyBytes) + `"}`
}

// A body over the limit is refused with 413 on every data-plane route: before
// it is read when it declares its length, and where it crosses the limit when
// it does not.
func TestDataPlaneBodyOverTheLimitIsRefused(t *testing.T) {
	limitBodiesForTest(t)
	handler := NewServer(Runtime{})
	body := oversizedJSON()
	for _, path := range []string{
		"/v1/chat/completions", "/v1/responses", "/v1/messages", "/v1/messages/count_tokens",
		"/v1/embeddings", "/v1/audio/speech", "/v1/images/generations", "/v1/videos/generations",
		"/chat/completions",
	} {
		for _, declared := range []bool{true, false} {
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			if !declared {
				request.ContentLength = -1
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			assertBodyTooLarge(t, response, path, declared)
		}
	}
}

// Management routes share the limit, so neither the console nor the
// playground can be made to buffer an unbounded body.
func TestManagementBodyOverTheLimitIsRefused(t *testing.T) {
	limitBodiesForTest(t)
	handler := NewServer(Runtime{})
	body := []byte(oversizedJSON())
	audioType, audioBody := transcriptionUpload(t, bytes.Repeat([]byte{0x7f}, lowestMaxRequestBodyBytes), "model", "echo/whisper")
	for _, test := range []struct {
		path        string
		contentType string
		body        []byte
		declared    bool
	}{
		{"/admin/api/keys", "application/json", body, true},
		{"/admin/api/playground/v1/chat/completions", "application/json", body, true},
		{"/admin/api/playground/v1/chat/completions", "application/json", body, false},
		{"/admin/api/playground/transcription", audioType, audioBody, false},
	} {
		request := httptest.NewRequest(http.MethodPost, test.path, bytes.NewReader(test.body))
		request.Header.Set("Authorization", "Bearer admin-secret")
		request.Header.Set("Content-Type", test.contentType)
		if !test.declared {
			request.ContentLength = -1
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		assertBodyTooLarge(t, response, test.path, test.declared)
	}
}

// transcriptionUpload builds a transcription form: the fields in order, then
// the audio part.
func transcriptionUpload(t *testing.T, audio []byte, fields ...string) (string, []byte) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for i := 0; i+1 < len(fields); i += 2 {
		if err := writer.WriteField(fields[i], fields[i+1]); err != nil {
			t.Fatal(err)
		}
	}
	part, err := writer.CreateFormFile("file", "speech.wav")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(audio); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return writer.FormDataContentType(), body.Bytes()
}

// A multipart upload over the limit is refused the same way, and never
// reaches the provider: the limit, not the multipart parser's in-memory
// threshold, is what bounds it.
func TestTranscriptionOverTheLimitIsRefused(t *testing.T) {
	limitBodiesForTest(t)
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { upstreamCalls.Add(1) }))
	defer upstream.Close()
	configureProxyProvider(t, upstream.URL)
	handler := NewServer(Runtime{})
	contentType, body := transcriptionUpload(t, bytes.Repeat([]byte{0x7f}, lowestMaxRequestBodyBytes), "model", "proxy/whisper")
	for _, declared := range []bool{true, false} {
		request := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", bytes.NewReader(body))
		request.Header.Set("Content-Type", contentType)
		if !declared {
			request.ContentLength = -1
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		assertBodyTooLarge(t, response, "/v1/audio/transcriptions", declared)
	}
	if calls := upstreamCalls.Load(); calls != 0 {
		t.Fatalf("an oversized upload reached the provider %d times", calls)
	}
	// Only the body that reached the handler has a caller to record.
	db, err := iam.DB()
	if err != nil {
		t.Fatal(err)
	}
	var count, status int
	if err := db.QueryRow(`SELECT COUNT(*), MAX(status_code) FROM usage_events`).Scan(&count, &status); err != nil {
		t.Fatal(err)
	}
	if count != 1 || status != http.StatusRequestEntityTooLarge {
		t.Fatalf("usage events=%d status=%d, want one 413", count, status)
	}
}

// The limit refuses only what crosses it: a body just under it is served.
func TestRequestBodyUnderTheLimitIsServed(t *testing.T) {
	limitBodiesForTest(t)
	handler := NewServer(Runtime{})
	content := strings.Repeat("a", lowestMaxRequestBodyBytes-1024)
	body := `{"model":"echo/echo-default","messages":[{"role":"user","content":"` + content + `"}]}`
	for _, declared := range []bool{true, false} {
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		if !declared {
			request.ContentLength = -1
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("declared length %t: status=%d body=%.300s", declared, response.Code, response.Body.String())
		}
	}
}

// Transcription streams the audio into the upstream form instead of parsing
// the whole form first. The provider must still see what it saw before: the
// audio intact, the resolved model, and the first of each forwarded field
// wherever the client placed it.
func TestTranscriptionForwardsTheStreamedUpload(t *testing.T) {
	resetState(t)
	audio := bytes.Repeat([]byte{0x00, 0x7f, 0xff}, 100<<10)
	var gotFields map[string][]string
	var gotAudio []byte
	var gotName string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		gotFields = r.MultipartForm.Value
		if files := r.MultipartForm.File["file"]; len(files) == 1 {
			gotName = files[0].Filename
			file, err := files[0].Open()
			if err == nil {
				gotAudio, _ = io.ReadAll(file)
				_ = file.Close()
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"transcript"}`))
	}))
	defer upstream.Close()
	configureProxyProvider(t, upstream.URL)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("language", "pt")
	_ = writer.WriteField("model", "proxy/whisper")
	part, _ := writer.CreateFormFile("file", "speech.wav")
	_, _ = part.Write(audio)
	second, _ := writer.CreateFormFile("file", "second.wav")
	_, _ = second.Write([]byte("not sent"))
	_ = writer.WriteField("response_format", "json")
	_ = writer.WriteField("language", "en")
	_ = writer.WriteField("unrelated", "not sent")
	_ = writer.Close()
	request := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response := httptest.NewRecorder()
	NewServer(Runtime{}).ServeHTTP(response, request)

	if response.Code != http.StatusOK || response.Body.String() != `{"text":"transcript"}` {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if gotName != "speech.wav" || !bytes.Equal(gotAudio, audio) {
		t.Fatalf("provider received %q with %d bytes, want speech.wav with %d", gotName, len(gotAudio), len(audio))
	}
	want := map[string][]string{"model": {"whisper"}, "language": {"pt"}, "response_format": {"json"}}
	if !reflect.DeepEqual(gotFields, want) {
		t.Fatalf("provider received fields %v, want %v", gotFields, want)
	}
}

// A form that ends before its closing boundary is malformed, not a form
// without audio: the streamed reader must not take a truncated body as the
// end of the form.
func TestTranscriptionRejectsATruncatedForm(t *testing.T) {
	resetState(t)
	configureProxyProvider(t, "http://proxy.test")
	contentType, complete := transcriptionUpload(t, []byte("audio"), "model", "proxy/whisper")
	_, boundary, _ := strings.Cut(contentType, "boundary=")
	for name, body := range map[string][]byte{
		"empty":     nil,
		"truncated": complete[:len(complete)-len("--"+boundary+"--\r\n")],
	} {
		request := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", bytes.NewReader(body))
		request.Header.Set("Content-Type", contentType)
		response := httptest.NewRecorder()
		NewServer(Runtime{}).ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid multipart form") {
			t.Fatalf("%s form: status=%d body=%s", name, response.Code, response.Body.String())
		}
	}
}
