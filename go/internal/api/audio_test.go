package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"

	core "github.com/xibodev/llmgw-core"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type failingReadCloser struct{ closed bool }

func (*failingReadCloser) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
func (r *failingReadCloser) Close() error {
	r.closed = true
	return nil
}

func TestResolveAudioTargetSanitizesAmbiguousCategoryErrors(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	oldProviders, oldEndpoints := config.Get().Providers, config.Get().Endpoints
	t.Cleanup(func() {
		config.Update(func(s *config.Settings) {
			s.Providers, s.Endpoints = oldProviders, oldEndpoints
		})
	})
	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{
			"one": {Type: "echo"},
			"two": {Type: "echo"},
		}
		s.Endpoints = map[string]*config.EndpointConfig{
			"smart": {
				Failover: []config.EndpointMember{{Provider: "one", Model: "echo-default"}},
			},
			"SMART": {
				Failover: []config.EndpointMember{{Provider: "two", Model: "echo-default"}},
			},
		}
	})

	_, _, status, message := resolveAudioTarget(nil, "SmArT", core.ModelOperationAudioIn)
	if status != 500 || message != "Gateway is not configured for the requested model." {
		t.Fatalf("ambiguous route status=%d message=%q", status, message)
	}
}

func TestResolveAudioTargetSkipsIneligibleRouteMembers(t *testing.T) {
	oldProviders, oldEndpoints := config.Get().Providers, config.Get().Endpoints
	t.Cleanup(func() {
		config.Update(func(settings *config.Settings) {
			settings.Providers, settings.Endpoints = oldProviders, oldEndpoints
		})
		providers.ResetProviders()
	})
	config.Update(func(settings *config.Settings) {
		settings.Providers = map[string]*config.ProviderConfig{
			"text":  {Type: "echo"},
			"audio": {Type: "openai_compatible", BaseURL: "https://audio.example.test/v1", APIKey: "none"},
		}
		settings.Endpoints = map[string]*config.EndpointConfig{
			"speech": {Failover: []config.EndpointMember{{Provider: "text", Model: "text"}, {Provider: "audio", Model: "voice"}}},
			"stt":    {Failover: []config.EndpointMember{{Provider: "text", Model: "text"}, {Provider: "audio", Model: "whisper"}}},
			"embed":  {Failover: []config.EndpointMember{{Provider: "text", Model: "text"}, {Provider: "audio", Model: "embedding"}}},
		}
	})
	providers.ResetProviders()
	for _, test := range []struct {
		model     string
		operation core.ModelOperation
		wantModel string
	}{
		{model: "speech", operation: core.ModelOperationAudioOut, wantModel: "voice"},
		{model: "stt", operation: core.ModelOperationAudioIn, wantModel: "whisper"},
		{model: "embed", operation: core.ModelOperationEmbeddings, wantModel: "embedding"},
	} {
		providerID, model, status, message := resolveAudioTarget(nil, test.model, test.operation)
		if status != 0 || providerID != "audio" || model != test.wantModel {
			t.Fatalf("%s target=%s/%s status=%d message=%q", test.model, providerID, model, status, message)
		}
	}
}

func configureProxyProvider(t *testing.T, upstreamURL string) {
	oldProviders := config.Get().Providers
	oldUnauthenticated := config.Get().AllowUnauthenticatedAPI
	t.Cleanup(func() {
		config.Update(func(s *config.Settings) {
			s.Providers = oldProviders
			s.AllowUnauthenticatedAPI = oldUnauthenticated
		})
	})
	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{
			"proxy": {Type: "openai_compatible", BaseURL: upstreamURL + "/v1"},
		}
		s.AllowUnauthenticatedAPI = true
	})
}

func assertSafeProxyError(t *testing.T, rec *httptest.ResponseRecorder, status int, secrets ...string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status=%d, want %d; body=%q", rec.Code, status, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type=%q, want application/json", ct)
	}
	var envelope map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil || envelope["error"] == nil {
		t.Fatalf("response is not the standard JSON error envelope: %q (%v)", rec.Body.String(), err)
	}
	for _, secret := range secrets {
		if strings.Contains(rec.Body.String(), secret) {
			t.Fatalf("response leaked %q: %q", secret, rec.Body.String())
		}
	}
}

func TestAudioProxyResponses(t *testing.T) {
	const email = "proxy-owner@example.test"
	token := "sk-" + strings.Repeat("a", 24)
	audio := []byte{0x49, 0x44, 0x33, 0x00, 0xff, 0x7f}

	tests := []struct {
		name        string
		path        string
		status      int
		contentType string
		body        []byte
		location    string
	}{
		{name: "transcription error", path: "/v1/audio/transcriptions", status: 401, contentType: "application/json", body: []byte(`{"error":{"message":"token ` + token + ` belongs to ` + email + `"}}`)},
		{name: "speech error", path: "/v1/audio/speech", status: 429, contentType: "text/plain", body: []byte("Authorization: Bearer " + token + " for " + email)},
		{name: "empty error", path: "/v1/audio/transcriptions", status: 400, contentType: "text/plain"},
		{name: "malformed error", path: "/v1/audio/speech", status: 500, contentType: "application/json", body: []byte(`{"error":`)},
		{name: "redirect", path: "/v1/audio/transcriptions", status: 302, contentType: "text/html", body: []byte("redirect secret " + token), location: "/credential-bearing-target"},
		{name: "transcription success", path: "/v1/audio/transcriptions", status: 200, contentType: "audio/x-test", body: audio},
		{name: "speech success", path: "/v1/audio/speech", status: 200, contentType: "audio/x-test", body: audio},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			resetState(t)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", testCase.contentType)
				if testCase.location != "" {
					w.Header().Set("Location", testCase.location)
				}
				w.WriteHeader(testCase.status)
				_, _ = w.Write(testCase.body)
			}))
			defer upstream.Close()
			configureProxyProvider(t, upstream.URL)

			var requestBody bytes.Buffer
			var req *http.Request
			if strings.Contains(testCase.path, "transcriptions") {
				writer := multipart.NewWriter(&requestBody)
				file, _ := writer.CreateFormFile("file", "sample.wav")
				_, _ = file.Write([]byte("audio input"))
				_ = writer.WriteField("model", "proxy/audio-model")
				_ = writer.Close()
				req = httptest.NewRequest("POST", testCase.path, &requestBody)
				req.Header.Set("Content-Type", writer.FormDataContentType())
			} else {
				req = httptest.NewRequest("POST", testCase.path, strings.NewReader(
					`{"model":"proxy/audio-model","input":"hello","voice":"default"}`))
			}
			rec := httptest.NewRecorder()
			NewServer(Runtime{}).ServeHTTP(rec, req)

			if testCase.status >= 300 {
				assertSafeProxyError(t, rec, testCase.status, token, email)
				if rec.Header().Get("Location") != "" {
					t.Fatalf("redirect Location was copied: %q", rec.Header().Get("Location"))
				}
			} else {
				if rec.Code != testCase.status || !bytes.Equal(rec.Body.Bytes(), testCase.body) {
					t.Fatalf("success changed: status=%d body=%v", rec.Code, rec.Body.Bytes())
				}
				if ct := rec.Header().Get("Content-Type"); ct != testCase.contentType {
					t.Fatalf("Content-Type=%q, want %q", ct, testCase.contentType)
				}
			}

			db, err := iam.DB()
			if err != nil {
				t.Fatal(err)
			}
			var count, status int
			var errorCode string
			if err := db.QueryRow(`SELECT COUNT(*), status_code, COALESCE(error_code, '') FROM usage_events`).Scan(&count, &status, &errorCode); err != nil {
				t.Fatal(err)
			}
			if count != 1 || status != testCase.status || errorCode != audioErrorCode(testCase.status) {
				t.Fatalf("usage=(count=%d status=%d code=%q)", count, status, errorCode)
			}
		})
	}
}

func TestAudioBodyReadFailureRecordsEffectiveFailureAndCloses(t *testing.T) {
	resetState(t)
	configureProxyProvider(t, "http://proxy.test")
	body := &failingReadCloser{}
	oldClient := audioClient
	audioClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body}, nil
	})}
	t.Cleanup(func() { audioClient = oldClient })

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/audio/speech", strings.NewReader(
		`{"model":"proxy/audio-model","input":"hello","voice":"default"}`))
	NewServer(Runtime{}).ServeHTTP(rec, req)
	assertSafeProxyError(t, rec, http.StatusBadGateway)
	if !body.closed {
		t.Fatal("upstream response body was not closed")
	}

	db, err := iam.DB()
	if err != nil {
		t.Fatal(err)
	}
	var count, status int
	var errorCode string
	if err := db.QueryRow(`SELECT COUNT(*), status_code, error_code FROM usage_events`).Scan(&count, &status, &errorCode); err != nil {
		t.Fatal(err)
	}
	if count != 1 || status != 502 || errorCode != "upstream_body_read" {
		t.Fatalf("usage=(count=%d status=%d code=%q)", count, status, errorCode)
	}
}

func configureCoreAudioProvider(t *testing.T, providerID, providerType, upstreamURL string) {
	t.Helper()
	config.Update(func(settings *config.Settings) {
		settings.Providers = map[string]*config.ProviderConfig{
			providerID: {
				Type: providerType, BaseURL: upstreamURL + "/v1", APIKey: "fixture-audio-key",
			},
		}
		settings.Endpoints = map[string]*config.EndpointConfig{}
		settings.AllowUnauthenticatedAPI = true
	})
	providers.ResetProviders()
}

func TestElevenLabsTranscriptionUsesCoreAudioSurface(t *testing.T) {
	resetState(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/speech-to-text" || r.Header.Get("xi-api-key") != "fixture-audio-key" {
			t.Fatalf("request path=%q key=%q", r.URL.Path, r.Header.Get("xi-api-key"))
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		if got := r.FormValue("model_id"); got != "scribe_v2" {
			t.Fatalf("model_id=%q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"fixture transcript","words":[]}`))
	}))
	defer upstream.Close()
	configureCoreAudioProvider(t, "eleven", "elevenlabs", upstream.URL)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	file, _ := writer.CreateFormFile("file", "sample.wav")
	_, _ = file.Write([]byte("fixture wav"))
	_ = writer.WriteField("model", "eleven/scribe_v2")
	_ = writer.Close()
	request := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response := httptest.NewRecorder()
	NewServer(Runtime{}).ServeHTTP(response, request)

	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status=%d content-type=%q body=%s", response.Code, response.Header().Get("Content-Type"), response.Body.String())
	}
	if got := transcriptionText(response.Body.Bytes()); got != "fixture transcript" {
		t.Fatalf("transcript=%q", got)
	}
}

func TestMiMoSpeechUsesCoreAudioSurface(t *testing.T) {
	resetState(t)
	wantAudio := []byte("fixture wav audio")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Api-Key") != "fixture-audio-key" {
			t.Fatalf("request path=%q key=%q", r.URL.Path, r.Header.Get("Api-Key"))
		}
		if authorization := r.Header.Get("Authorization"); authorization != "" {
			t.Fatalf("unexpected Authorization header %q", authorization)
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload["model"] != "mimo-v2.5-tts" {
			t.Fatalf("model=%v", payload["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{
				"audio": map[string]any{"data": base64.StdEncoding.EncodeToString(wantAudio), "format": "wav"},
			}}},
		})
	}))
	defer upstream.Close()
	configureCoreAudioProvider(t, "mimo", "mimo", upstream.URL)

	request := httptest.NewRequest(http.MethodPost, "/v1/audio/speech", strings.NewReader(
		`{"model":"mimo/mimo-v2.5-tts","input":"hello","voice":"mimo_default","response_format":"wav"}`,
	))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	NewServer(Runtime{}).ServeHTTP(response, request)

	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "audio/wav" ||
		!bytes.Equal(response.Body.Bytes(), wantAudio) {
		t.Fatalf("status=%d content-type=%q body=%q", response.Code, response.Header().Get("Content-Type"), response.Body.Bytes())
	}
}

func TestUpstreamErrorStatusPreservesTypedRedirect(t *testing.T) {
	err := providers.HTTPInvocationError("proxy", http.StatusTemporaryRedirect, []byte("redirect"))
	if status := upstreamErrorStatus(err); status != http.StatusTemporaryRedirect {
		t.Fatalf("status=%d, want %d", status, http.StatusTemporaryRedirect)
	}
}
