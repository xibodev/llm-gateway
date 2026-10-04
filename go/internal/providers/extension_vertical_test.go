package providers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/xibodev/llm-provider-auth/tokenstore"
	core "github.com/xibodev/llmgw-core"
	"github.com/xibodev/llmgw-core/oauthflow"
)

// extensionDaemon serves mux as the extension daemon the environment names,
// which admits only requests that carry the shared secret test-secret.
func extensionDaemon(t *testing.T, mux *http.ServeMux) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-secret" {
			t.Errorf("%s %s: Authorization = %q, want the shared secret", r.Method, r.URL.Path, got)
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	t.Setenv("LLMGW_EXTENSION_URL", server.URL)
	t.Setenv("LLMGW_EXTENSION_SECRET", "test-secret")
}

func writeExtensionJSON(t *testing.T, w http.ResponseWriter, status int, value any) {
	t.Helper()
	w.Header().Set("Content-Type", core.ContentTypeJSON)
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Error(err)
	}
}

func TestExtensionVerticalServesThroughTheDaemon(t *testing.T) {
	const reply = `{"choices":[{"message":{"content":"hello from extension"}}]}`
	mux := http.NewServeMux()
	mux.HandleFunc("POST /extension/v1/test_provider/invoke", func(w http.ResponseWriter, r *http.Request) {
		for header, want := range map[string]string{
			"X-Surface":          string(core.ModelSurfaceChatCompletions),
			"X-Model":            "gpt-test",
			"Content-Type":       core.ContentTypeJSON,
			"X-Credential-Token": "access-1",
		} {
			if got := r.Header.Get(header); got != want {
				t.Errorf("invoke %s = %q, want %q", header, got, want)
			}
		}
		w.Header().Set("Content-Type", core.ContentTypeJSON)
		_, _ = io.WriteString(w, reply)
	})
	mux.HandleFunc("POST /extension/v1/test_provider/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", core.ContentTypeEventStream)
		flusher := w.(http.Flusher)
		for _, record := range []string{"data: chunk-1\n\n", ": keepalive\n\n", "data: chunk-2\n\n"} {
			_, _ = io.WriteString(w, record)
			flusher.Flush()
		}
	})
	mux.HandleFunc("GET /extension/v1/test_provider/models", func(w http.ResponseWriter, r *http.Request) {
		writeExtensionJSON(t, w, http.StatusOK, map[string]any{
			"models": []core.ModelInfo{{ID: "gpt-test", DisplayName: "GPT Test"}},
		})
	})
	mux.HandleFunc("POST /extension/v1/test_provider/refresh", func(w http.ResponseWriter, r *http.Request) {
		var refresh struct {
			Record tokenstore.Record `json:"record"`
		}
		if err := json.NewDecoder(r.Body).Decode(&refresh); err != nil || refresh.Record.RefreshToken != "old-refresh" {
			t.Errorf("refresh request = %v, %v", refresh.Record, err)
		}
		writeExtensionJSON(t, w, http.StatusOK, map[string]any{
			"record": tokenstore.Record{AccessToken: "refreshed-access", RefreshToken: "refreshed-refresh"},
		})
	})
	extensionDaemon(t, mux)

	vertical := (&Runtime{}).extensionCoreVertical("test_provider")
	provider, err := vertical.provider(nil, "instance")
	if err != nil {
		t.Fatal(err)
	}
	wantSurfaces := []core.ModelSurface{
		core.ModelSurfaceChatCompletions, core.ModelSurfaceResponses, core.ModelSurfaceMessages,
		core.ModelSurfaceAudioSpeech, core.ModelSurfaceImages,
	}
	if got := provider.NativeSurfaces("gpt-test"); !slices.Equal(got, wantSurfaces) {
		t.Errorf("native surfaces = %v, want %v", got, wantSurfaces)
	}
	request := core.Request{
		Surface:     core.ModelSurfaceChatCompletions,
		Model:       "gpt-test",
		Body:        []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
		ContentType: core.ContentTypeJSON,
		Credential:  &core.Credential{Token: "access-1"},
	}

	resp, err := provider.Invoke(context.Background(), request)
	if err != nil || string(resp.Body) != reply {
		t.Fatalf("invoke = %s, %v", resp.Body, err)
	}

	stream, err := provider.Stream(context.Background(), request)
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	defer stream.Close()
	var frames []string
	for {
		frame, err := stream.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("stream frame error: %v", err)
		}
		frames = append(frames, string(frame))
	}
	// The keepalive carries no data, so the stream skips it.
	if want := []string{"data: chunk-1\n\n", "data: chunk-2\n\n"}; !slices.Equal(frames, want) {
		t.Errorf("frames = %q, want %q", frames, want)
	}

	models, err := provider.ListModels(context.Background(), nil)
	if err != nil || len(models) != 1 || models[0].ID != "gpt-test" {
		t.Errorf("models = %+v, %v", models, err)
	}

	refresh := vertical.refresh(nil, "instance")
	refreshed, err := refresh(context.Background(), tokenstore.Record{AccessToken: "old-access", RefreshToken: "old-refresh"})
	if err != nil || refreshed.AccessToken != "refreshed-access" {
		t.Errorf("refresh = %v, %v", refreshed, err)
	}

	// A surface off the list fails before it reaches the daemon, whose mux
	// has no route for it.
	_, err = provider.Invoke(context.Background(), core.Request{Surface: core.ModelSurfaceEmbeddings, Model: "gpt-test"})
	var surfaceError *core.SurfaceError
	if !errors.As(err, &surfaceError) {
		t.Errorf("embeddings error = %v, want a surface error", err)
	}
}

func TestExtensionFacadeReachesTheDaemonThroughTheSharedClient(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /extension/v1/edge_tts/models", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Credential-Token"); got != "stored-access" {
			t.Errorf("models credential = %q, want the stored one", got)
		}
		writeExtensionJSON(t, w, http.StatusOK, map[string]any{"models": []core.ModelInfo{{ID: "en-US-Voice"}}})
	})
	mux.HandleFunc("POST /extension/v1/edge_tts/invoke", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Header.Get("X-Surface") != string(core.ModelSurfaceAudioSpeech) || r.Header.Get("X-Model") != "en-US-Voice" || string(body) != "hello" {
			t.Errorf("synthesize request: surface %q, model %q, body %q", r.Header.Get("X-Surface"), r.Header.Get("X-Model"), body)
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = io.WriteString(w, "mp3-bytes")
	})
	extensionDaemon(t, mux)

	store := core.NewMemoryCredentialStore()
	if _, err := store.Save(context.Background(), "tts-key", tokenstore.Record{AccessToken: "stored-access", TokenType: "Bearer"}); err != nil {
		t.Fatal(err)
	}
	store.BindShared("tts", "tts-key")
	rt := newRuntime(func(bool) (core.CredentialStore, error) { return store, nil })
	facade := rt.newExtensionFacade("tts", ExtensionTypeEdgeTTS, core.Caller{}).(*ExtensionProviderFacade)

	models, _, err := facade.ListModelsWithError()
	if err != nil || len(models) != 1 || models[0].ID != "en-US-Voice" {
		t.Fatalf("models = %+v, %v", models, err)
	}
	audio, format, err := facade.Synthesize("en-US-Voice", "hello", "")
	if err != nil || string(audio) != "mp3-bytes" || format != "audio/mpeg" {
		t.Fatalf("synthesize = %q (%s), %v", audio, format, err)
	}
}

// The gateway's own controls, the fields prefixed "_", stay in the gateway,
// while the client's fields and the output limit the daemon reads go with
// the request, streamed or not.
func TestExtensionRequestsCarryNoGatewayControls(t *testing.T) {
	bodies := make(chan map[string]any, 2)
	record := func(r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		bodies <- body
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /extension/v1/openai_codex/invoke", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		writeExtensionJSON(t, w, http.StatusOK, map[string]any{"choices": []any{}})
	})
	mux.HandleFunc("POST /extension/v1/openai_codex/stream", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.Header().Set("Content-Type", core.ContentTypeEventStream)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	extensionDaemon(t, mux)
	facade := extensionFacadeFixture(t, ExtensionTypeCodex, "codex")
	messages := []Message{{"role": "user", "content": "hi"}}
	kw := Kwargs{
		"temperature": 0.5, "_max_output_tokens": 64,
		"_affinity_key": "session", "_fallback_timeout_ms": 1000, "_force_api_support": true,
	}

	if _, err := facade.Complete("gpt-test", messages, kw); err != nil {
		t.Fatal(err)
	}
	stream, err := facade.StreamContext(context.Background(), "gpt-test", messages, kw)
	if err != nil {
		t.Fatal(err)
	}
	_ = stream.Close()
	for _, operation := range []string{"complete", "stream"} {
		body := <-bodies
		if body["temperature"] != 0.5 || body["_max_output_tokens"] != float64(64) {
			t.Errorf("%s body %v lost a field the daemon reads", operation, body)
		}
		for field := range body {
			if strings.HasPrefix(field, "_") && field != "_max_output_tokens" {
				t.Errorf("%s body carries the gateway control %s", operation, field)
			}
		}
	}
}

func TestExtensionClientIsSharedUntilTheEnvironmentChanges(t *testing.T) {
	t.Setenv("LLMGW_EXTENSION_URL", "")
	t.Setenv("LLMGW_EXTENSION_SECRET", "one")
	rt := &Runtime{}
	first, err := rt.extensionClient()
	if err != nil {
		t.Fatal(err)
	}
	// An unset address is the default one, so naming it keeps the client.
	t.Setenv("LLMGW_EXTENSION_URL", defaultExtensionURL)
	if again, err := rt.extensionClient(); err != nil || again != first {
		t.Fatalf("client for the default address = %p (err %v), want the shared %p", again, err, first)
	}

	t.Setenv("LLMGW_EXTENSION_SECRET", "two")
	newSecret, err := rt.extensionClient()
	if err != nil || newSecret == first {
		t.Fatalf("client after a secret change = %p (err %v), want a new one", newSecret, err)
	}
	t.Setenv("LLMGW_EXTENSION_URL", "http://127.0.0.1:18889")
	newURL, err := rt.extensionClient()
	if err != nil || newURL == newSecret {
		t.Fatalf("client after an address change = %p (err %v), want a new one", newURL, err)
	}
	if again, err := rt.extensionClient(); err != nil || again != newURL {
		t.Fatalf("client for an unchanged environment = %p (err %v), want the shared %p", again, err, newURL)
	}

	// An address the client cannot use is a configuration error wherever the
	// client is needed, and its message does not repeat the address.
	t.Setenv("LLMGW_EXTENSION_URL", "http://owner:not-a-secret@127.0.0.1:18889")
	var providerError *core.ProviderError
	_, err = rt.extensionClient()
	if !errors.As(err, &providerError) || providerError.Class != core.ProviderErrorConfiguration {
		t.Fatalf("invalid address error = %v, want a configuration error", err)
	}
	if strings.Contains(err.Error(), "not-a-secret") {
		t.Fatalf("invalid address error repeats the address: %q", err)
	}
	if _, err := rt.extensionCoreVertical(ExtensionTypeCodex).provider(nil, "codex"); !errors.As(err, &providerError) || providerError.Class != core.ProviderErrorConfiguration {
		t.Fatalf("provider for an invalid address: %v, want a configuration error", err)
	}
	if _, err := rt.OAuthDriver(ExtensionTypeCodex, "codex", oauthflow.MethodDevice); !errors.As(err, &providerError) || providerError.Class != core.ProviderErrorConfiguration {
		t.Fatalf("sign-in driver for an invalid address: %v, want a configuration error", err)
	}

	// The failed rebuild leaves the last client in place.
	t.Setenv("LLMGW_EXTENSION_URL", "http://127.0.0.1:18889")
	if again, err := rt.extensionClient(); err != nil || again != newURL {
		t.Fatalf("client after an invalid address = %p (err %v), want the shared %p", again, err, newURL)
	}
}

func TestExtensionDaemonErrorsAreBounded(t *testing.T) {
	reason := strings.Repeat("the upstream refused the request ", 100)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /extension/v1/openai_codex/invoke", func(w http.ResponseWriter, r *http.Request) {
		writeExtensionJSON(t, w, http.StatusTooManyRequests, map[string]any{
			"error": map[string]any{"message": reason, "code": http.StatusTooManyRequests},
		})
	})
	extensionDaemon(t, mux)

	provider, err := (&Runtime{}).extensionCoreVertical(ExtensionTypeCodex).provider(nil, "codex")
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.Invoke(context.Background(), core.Request{
		Surface: core.ModelSurfaceResponses, Model: "gpt-test", Body: []byte(`{}`), ContentType: core.ContentTypeJSON,
	})
	var providerError *core.ProviderError
	if !errors.As(err, &providerError) || core.ClassifyError(err).StatusCode != http.StatusTooManyRequests {
		t.Fatalf("daemon error = %v, want a provider error with the daemon's status", err)
	}
	if message := err.Error(); !strings.Contains(message, "the upstream refused the request") || len(message) > 1024 {
		t.Fatalf("daemon error message is %d bytes: %q", len(message), message)
	}
}

func TestExtensionOAuthDriverSignsInThroughTheDaemon(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /extension/v1/openai_codex/oauth/start", func(w http.ResponseWriter, r *http.Request) {
		var start map[string]any
		if err := json.NewDecoder(r.Body).Decode(&start); err != nil || start["method"] != string(oauthflow.MethodDevice) {
			t.Errorf("start request = %v, %v", start, err)
		}
		writeExtensionJSON(t, w, http.StatusOK, map[string]any{
			"authorization": oauthflow.Authorization{
				UserCode:        "USER-123",
				VerificationURI: "https://example.test/verify",
				Secrets:         oauthflow.Secrets{DeviceCode: "device-1"},
			},
		})
	})
	mux.HandleFunc("POST /extension/v1/openai_codex/oauth/poll", func(w http.ResponseWriter, r *http.Request) {
		var poll struct {
			Flow oauthflow.Flow `json:"flow"`
		}
		if err := json.NewDecoder(r.Body).Decode(&poll); err != nil || poll.Flow.Secrets.DeviceCode != "device-1" {
			t.Errorf("poll request did not carry the device code: %v", err)
		}
		writeExtensionJSON(t, w, http.StatusOK, map[string]any{
			"result": oauthflow.PollResult{Status: oauthflow.PollApproved, Record: tokenstore.Record{AccessToken: "device-access"}},
		})
	})
	mux.HandleFunc("POST /extension/v1/openai_codex/oauth/exchange", func(w http.ResponseWriter, r *http.Request) {
		var exchange struct {
			Code string `json:"code"`
		}
		if err := json.NewDecoder(r.Body).Decode(&exchange); err != nil || exchange.Code != "code-1" {
			t.Errorf("exchange request = %+v, %v", exchange, err)
		}
		writeExtensionJSON(t, w, http.StatusOK, map[string]any{"record": tokenstore.Record{AccessToken: "code-access"}})
	})
	extensionDaemon(t, mux)

	driver, err := (&Runtime{}).OAuthDriver(ExtensionTypeCodex, "codex", oauthflow.MethodDevice)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	auth, err := driver.Start(ctx, oauthflow.StartRequest{Method: oauthflow.MethodDevice})
	if err != nil || auth.UserCode != "USER-123" || auth.VerificationURI != "https://example.test/verify" {
		t.Fatalf("start = %+v, %v", auth, err)
	}
	device, ok := driver.(oauthflow.DeviceDriver)
	if !ok {
		t.Fatal("the extension driver is not a device driver")
	}
	result, err := device.Poll(ctx, oauthflow.Flow{Secrets: auth.Secrets})
	if err != nil || result.Status != oauthflow.PollApproved || result.Record.AccessToken != "device-access" {
		t.Fatalf("poll = %v, %v", result.Status, err)
	}
	code, ok := driver.(oauthflow.CodeDriver)
	if !ok {
		t.Fatal("the extension driver is not a code driver")
	}
	record, err := code.Exchange(ctx, oauthflow.Flow{}, "code-1")
	if err != nil || record.AccessToken != "code-access" {
		t.Fatalf("exchange = %v, %v", record, err)
	}
}
