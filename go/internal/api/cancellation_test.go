package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"llmgw/internal/codexauth"
	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
	"llmgw/internal/router"
)

var _ http.Flusher = (*streamTestWriter)(nil)

func setupCancellationTest(t *testing.T, upstreamURL string) *config.Principal {
	t.Helper()
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	router.ResetSavingsState()
	router.ResetTelemetryState()
	config.Update(func(settings *config.Settings) {
		settings.AllowUnauthenticatedAPI = true
		settings.APIKey = ""
		settings.APIKeys = nil
		settings.Providers = map[string]*config.ProviderConfig{
			"first":  {Type: "openai_compatible", RegistryID: "openai", BaseURL: upstreamURL, APIKey: "fixture"},
			"second": {Type: "openai_compatible", RegistryID: "openai", BaseURL: upstreamURL + "/second", APIKey: "fixture"},
		}
		settings.Endpoints = map[string]*config.EndpointConfig{
			"cancel-route": {Failover: []config.EndpointMember{
				{Provider: "first", Model: "model"},
				{Provider: "second", Model: "model"},
			}},
		}
		settings.Policies.Defaults = config.ProviderPolicy{
			RetryMaxAttempts: 3, RetryInitialBackoffSeconds: 0.01,
			RetryMaxBackoffSeconds: 0.02, RetryBackoffMultiplier: 2,
		}
		settings.Policies.Overrides = map[string]config.ProviderPolicy{}
	})
	providers.ForgetCatalog("first")
	providers.ForgetCatalog("second")
	providers.ResetProviders()
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		providers.ResetProviders()
		iam.ResetForTests()
		router.ResetSavingsState()
		router.ResetTelemetryState()
	})
	return &config.Principal{Project: "local", Key: "local"}
}

type blockingStreamIter struct {
	chunks []string
	index  int
	closed chan struct{}
	once   sync.Once
}

func (iterator *blockingStreamIter) Next() (string, bool) {
	if iterator.index < len(iterator.chunks) {
		chunk := iterator.chunks[iterator.index]
		iterator.index++
		return chunk, true
	}
	<-iterator.closed
	return "", false
}

func (*blockingStreamIter) Err() error { return nil }
func (iterator *blockingStreamIter) Close() error {
	iterator.once.Do(func() { close(iterator.closed) })
	return nil
}

type streamTestWriter struct {
	header      http.Header
	body        bytes.Buffer
	status      int
	flushes     int
	cancel      context.CancelFunc
	cancelAfter int
	writeErrAt  int
	flushErrAt  int
}

func (writer *streamTestWriter) Header() http.Header {
	if writer.header == nil {
		writer.header = make(http.Header)
	}
	return writer.header
}

func (writer *streamTestWriter) WriteHeader(status int) { writer.status = status }
func (writer *streamTestWriter) Write(payload []byte) (int, error) {
	if writer.writeErrAt > 0 && writer.flushes+1 >= writer.writeErrAt {
		return 0, errors.New("fixture write failed")
	}
	return writer.body.Write(payload)
}
func (writer *streamTestWriter) FlushError() error {
	writer.flushes++
	if writer.cancelAfter > 0 && writer.flushes == writer.cancelAfter {
		writer.cancel()
	}
	if writer.flushErrAt > 0 && writer.flushes == writer.flushErrAt {
		return errors.New("fixture flush failed")
	}
	return nil
}
func (writer *streamTestWriter) Flush() { _ = writer.FlushError() }

func usageOutcome(t *testing.T) (count, status, credits int, code string) {
	t.Helper()
	db, err := iam.DB()
	if err != nil {
		t.Fatal(err)
	}
	var statusValue, creditsValue sql.NullInt64
	var codeValue sql.NullString
	if err := db.QueryRow(`SELECT COUNT(*), MAX(status_code), MAX(credits_milli), MAX(error_code) FROM usage_events`).Scan(&count, &statusValue, &creditsValue, &codeValue); err != nil {
		t.Fatal(err)
	}
	return count, int(statusValue.Int64), int(creditsValue.Int64), codeValue.String
}

func assertCancelledUsage(t *testing.T) {
	t.Helper()
	count, status, credits, code := usageOutcome(t)
	if count != 1 || status != 499 || code != "client_cancelled" || credits != 0 {
		t.Fatalf("usage count=%d status=%d code=%q credits=%d", count, status, code, credits)
	}
}

func TestCodingStreamsCancelAfterFirstEvent(t *testing.T) {
	tests := []struct {
		name, path, body, terminal string
	}{
		{"Chat stream", "/v1/chat/completions", `{"model":"cancel-route","stream":true,"messages":[{"role":"user","content":"hi"}]}`, "[DONE]"},
		{"native Responses stream", "/v1/responses", `{"model":"cancel-route","stream":true,"input":"hi"}`, "response.completed"},
		{"adapted Messages stream", "/v1/messages", `{"model":"cancel-route","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, "message_stop"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			var firstCalls, secondCalls atomic.Int32
			upstreamDone := make(chan struct{}, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if strings.HasPrefix(request.URL.Path, "/second/") {
					secondCalls.Add(1)
				} else {
					firstCalls.Add(1)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if request.URL.Path == "/responses" {
					_, _ = io.WriteString(w, `data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_1","object":"response","status":"in_progress","model":"model","output":[]}}`+"\n\n")
				} else {
					_, _ = io.WriteString(w, `data: {"id":"chat_1","object":"chat.completion.chunk","model":"model","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}`+"\n\n")
				}
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
				<-request.Context().Done()
				upstreamDone <- struct{}{}
			}))
			defer upstream.Close()
			setupCancellationTest(t, upstream.URL)
			ctx, cancel := context.WithCancel(context.Background())
			writer := &streamTestWriter{cancel: cancel, cancelAfter: 1}
			request := httptest.NewRequest(http.MethodPost, testCase.path, strings.NewReader(testCase.body)).WithContext(ctx)
			request.Header.Set("Content-Type", "application/json")
			done := make(chan struct{})
			go func() {
				NewServer().ServeHTTP(writer, request)
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("handler did not return promptly")
			}
			select {
			case <-upstreamDone:
			case <-time.After(5 * time.Second):
				t.Fatal("upstream context did not close")
			}
			if firstCalls.Load() != 1 || secondCalls.Load() != 0 || strings.Contains(writer.body.String(), testCase.terminal) {
				t.Fatalf("calls=%d/%d body=%q", firstCalls.Load(), secondCalls.Load(), writer.body.String())
			}
			assertCancelledUsage(t)
		})
	}
}

func TestAdaptedCodingStreamsCancelWithoutFailover(t *testing.T) {
	tests := []struct {
		name, path, body, upstreamPath, surface, partial string
		terminals                                        []string
	}{
		{
			name: "Chat to Responses", path: "/v1/chat/completions",
			body:         `{"model":"cancel-route","stream":true,"messages":[{"role":"user","content":"hi"}],"force_api_support":true}`,
			upstreamPath: "/responses", surface: "/responses",
			partial:   `data: {"type":"response.output_text.delta","sequence_number":1,"response_id":"resp_1","item_id":"msg_1","output_index":0,"content_index":0,"delta":"partial"}` + "\n\n",
			terminals: []string{"[DONE]"},
		},
		{
			name: "Responses to Chat", path: "/v1/responses",
			body:         `{"model":"cancel-route","stream":true,"input":"hi"}`,
			upstreamPath: "/chat/completions", surface: "/chat/completions",
			partial:   `data: {"id":"chat_1","object":"chat.completion.chunk","model":"model","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}` + "\n\n",
			terminals: []string{"response.completed", "response.incomplete"},
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			var firstCalls, secondCalls atomic.Int32
			started := make(chan string, 1)
			upstreamDone := make(chan struct{}, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/models" {
					_, _ = io.WriteString(w, `{"data":[{"id":"model","supported_endpoints":["`+testCase.surface+`"]}]}`)
					return
				}
				if strings.HasPrefix(request.URL.Path, "/second/") {
					secondCalls.Add(1)
				} else {
					firstCalls.Add(1)
				}
				if request.URL.Path != testCase.upstreamPath {
					t.Errorf("upstream path=%q, want %q", request.URL.Path, testCase.upstreamPath)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, testCase.partial)
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
				started <- request.URL.Path
				<-request.Context().Done()
				upstreamDone <- struct{}{}
			}))
			defer upstream.Close()
			setupCancellationTest(t, upstream.URL)
			if testCase.name == "Responses to Chat" {
				config.Update(func(settings *config.Settings) {
					settings.Providers["first"].RegistryID = ""
					settings.Providers["second"].RegistryID = ""
				})
				providers.ResetProviders()
			}
			models := providers.RefreshCatalog("first")
			if len(models) != 1 || len(models[0].SupportedSurfaces) != 1 || models[0].SupportedSurfaces[0] != testCase.surface {
				t.Fatalf("catalog=%+v", models)
			}
			ctx, cancel := context.WithCancel(context.Background())
			writer := &streamTestWriter{}
			request := httptest.NewRequest(http.MethodPost, testCase.path, strings.NewReader(testCase.body)).WithContext(ctx)
			request.Header.Set("Content-Type", "application/json")
			done := make(chan struct{})
			go func() {
				NewServer().ServeHTTP(writer, request)
				close(done)
			}()
			select {
			case path := <-started:
				if path != testCase.upstreamPath {
					t.Fatalf("upstream path=%q, want %q", path, testCase.upstreamPath)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("upstream request did not start")
			}
			cancel()
			select {
			case <-upstreamDone:
			case <-time.After(5 * time.Second):
				t.Fatal("upstream context did not close")
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("handler did not return promptly")
			}
			if firstCalls.Load() != 1 || secondCalls.Load() != 0 {
				t.Fatalf("calls=%d/%d", firstCalls.Load(), secondCalls.Load())
			}
			for _, terminal := range testCase.terminals {
				if strings.Contains(writer.body.String(), terminal) {
					t.Fatalf("terminal %q written in %q", terminal, writer.body.String())
				}
			}
			assertCancelledUsage(t)
		})
	}
}

func TestCodingStreamWriteAndFlushFailuresCloseUpstream(t *testing.T) {
	tests := []struct {
		name, path, body, terminal string
		flushErr                   bool
	}{
		{"Chat write", "/v1/chat/completions", `{"model":"cancel-route","stream":true,"messages":[{"role":"user","content":"hi"}]}`, "[DONE]", false},
		{"Chat flush", "/v1/chat/completions", `{"model":"cancel-route","stream":true,"messages":[{"role":"user","content":"hi"}]}`, "[DONE]", true},
		{"Responses write", "/v1/responses", `{"model":"cancel-route","stream":true,"input":"hi"}`, "response.completed", false},
		{"Responses flush", "/v1/responses", `{"model":"cancel-route","stream":true,"input":"hi"}`, "response.completed", true},
		{"Messages write", "/v1/messages", `{"model":"cancel-route","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, "message_stop", false},
		{"Messages flush", "/v1/messages", `{"model":"cancel-route","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, "message_stop", true},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			upstreamDone := make(chan struct{}, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if request.URL.Path == "/responses" {
					_, _ = io.WriteString(w, `data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_1","object":"response","status":"in_progress","model":"model","output":[]}}`+"\n\n")
				} else {
					_, _ = io.WriteString(w, `data: {"id":"chat_1","object":"chat.completion.chunk","model":"model","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}`+"\n\n")
				}
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
				<-request.Context().Done()
				upstreamDone <- struct{}{}
			}))
			defer upstream.Close()
			setupCancellationTest(t, upstream.URL)
			writer := &streamTestWriter{}
			if testCase.flushErr {
				writer.flushErrAt = 1
			} else {
				writer.writeErrAt = 1
			}
			request := httptest.NewRequest(http.MethodPost, testCase.path, strings.NewReader(testCase.body))
			request.Header.Set("Content-Type", "application/json")
			done := make(chan struct{})
			go func() {
				NewServer().ServeHTTP(writer, request)
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("handler did not return promptly")
			}
			select {
			case <-upstreamDone:
			case <-time.After(5 * time.Second):
				t.Fatal("upstream stream was not closed")
			}
			if strings.Contains(writer.body.String(), testCase.terminal) {
				t.Fatalf("terminal written: %q", writer.body.String())
			}
			assertCancelledUsage(t)
		})
	}
}

func TestTerminalWriteWinsOverLaterContextClose(t *testing.T) {
	principal := setupCancellationTest(t, "http://127.0.0.1:1")
	ctx, cancel := context.WithCancel(context.Background())
	writer := &streamTestWriter{cancel: cancel, cancelAfter: 1}
	iterator := &blockingStreamIter{chunks: []string{`{"type":"response.completed","response":{"id":"resp_1","status":"completed","model":"model","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`}, closed: make(chan struct{})}
	if err := streamNativeResponseEventsContext(writer, ctx, iterator, map[string]any{"input": "hi"}, "cancel-route", &router.Target{Provider: "first", Model: "model"}, principal, time.Now()); err != nil {
		t.Fatal(err)
	}
	_ = iterator.Close()
	count, status, credits, code := usageOutcome(t)
	if count != 1 || status != 200 || credits != 1000 || code != "" || !strings.Contains(writer.body.String(), "response.completed") {
		t.Fatalf("usage count=%d status=%d code=%q credits=%d body=%q", count, status, code, credits, writer.body.String())
	}
}

func TestCodexInFlightStreamCancellation(t *testing.T) {
	var calls atomic.Int32
	started := make(chan struct {
		path string
		body string
	}, 1)
	cancelled := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(request.Body)
		started <- struct {
			path string
			body string
		}{request.URL.Path, string(body)}
		<-request.Context().Done()
		cancelled <- struct{}{}
	}))
	defer upstream.Close()
	oldResponsesBaseURL := codexauth.ResponsesBaseURL
	codexauth.ResponsesBaseURL = upstream.URL + "/backend-api/codex"
	t.Cleanup(func() { codexauth.ResponsesBaseURL = oldResponsesBaseURL })

	setupCancellationTest(t, upstream.URL)
	human, err := iam.CreatePrincipal("human", "fixture:cancel-codex", "", "Codex fixture")
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	config.Update(func(settings *config.Settings) {
		settings.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(key)
		settings.OpenAICodexClientID = "fixture-client"
		settings.Providers = map[string]*config.ProviderConfig{
			"codex": {Type: "openai_compatible", RegistryID: "openai_codex"},
		}
	})
	if _, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
		PrincipalID: human.ID, ProviderID: "codex", Kind: "openai_codex_oauth",
		AccessToken: "fixture-access", RefreshToken: "fixture-refresh",
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	providers.ResetProviders()
	principal := &config.Principal{
		PrincipalID: human.ID, PrincipalKind: "human",
	}
	ctx, cancel := context.WithCancel(context.Background())
	type result struct {
		execution *router.ResponsesExecutionStream
		served    *router.Target
		err       error
	}
	resultCh := make(chan result, 1)
	go func() {
		execution, served, err := router.ExecuteResponsesStreamContext(
			ctx, []router.Target{{Provider: "codex", Model: "model"}},
			map[string]any{"input": "hi", "stream": true}, "codex/model", principal,
		)
		resultCh <- result{execution, served, err}
	}()
	select {
	case request := <-started:
		if request.path != "/backend-api/codex/responses" || !strings.Contains(request.body, `"stream":true`) {
			t.Fatalf("Codex request path=%q body=%q", request.path, request.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Codex request did not start")
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("Codex upstream context was not cancelled")
	}
	select {
	case result := <-resultCh:
		if !errors.Is(result.err, context.Canceled) || result.execution != nil || result.served != nil {
			t.Fatalf("execution=%v served=%v err=%v", result.execution, result.served, result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Codex stream call did not return")
	}
	if calls.Load() != 1 {
		t.Fatalf("Codex upstream calls=%d, want 1", calls.Load())
	}
}

func TestCapturingWriterForwardsFlushError(t *testing.T) {
	underlying := &streamTestWriter{flushErrAt: 1}
	wrapped := &capturingWriter{ResponseWriter: underlying}
	if err := writeAndFlush(wrapped, []byte("event")); err == nil {
		t.Fatal("wrapped flush error was lost")
	}
}
