package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"

	core "github.com/xibodev/llmgw-core"
)

const azureFixtureInstance = "azure-fixture"

// azureFixture configures cfg as an Azure OpenAI instance until the test ends
// and returns the facade the provider factory builds for it, served by a
// Runtime installed for the test. Without credential encryption the instance
// resolves only its configured key.
func azureFixture(t *testing.T, cfg *config.ProviderConfig) AzureOpenAIProvider {
	t.Helper()
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	runtime := InstallForTests(t)
	oldKey, oldProviders := config.Get().CredentialEncryptionKey, config.Get().Providers
	config.Update(func(s *config.Settings) {
		s.CredentialEncryptionKey = ""
		s.Providers = map[string]*config.ProviderConfig{azureFixtureInstance: cfg}
	})
	t.Cleanup(func() {
		config.Update(func(s *config.Settings) { s.CredentialEncryptionKey, s.Providers = oldKey, oldProviders })
	})
	return azureFacade(t, runtime, gatewayCaller())
}

// azureFacade is the facade the provider factory builds for caller.
func azureFacade(t *testing.T, runtime *Runtime, caller core.Caller) AzureOpenAIProvider {
	t.Helper()
	provider, err := runtime.instantiate(config.Get(), azureFixtureInstance, config.Get().Providers[azureFixtureInstance], caller)
	if err != nil {
		t.Fatal(err)
	}
	facade, ok := provider.(AzureOpenAIProvider)
	if !ok {
		t.Fatalf("provider=%T, want the Azure OpenAI facade", provider)
	}
	return facade
}

// azureRecorder records each request's path, api-key and body, and answers
// a Chat completion, streamed when the request streams.
type azureRecorder struct {
	mu    sync.Mutex
	calls []string
	body  []byte
}

func (a *azureRecorder) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		var request map[string]any
		_ = json.Unmarshal(body, &request)
		a.mu.Lock()
		a.calls = append(a.calls, strings.TrimSpace(r.URL.Path+" "+r.Header.Get("api-key")+" "+r.Header.Get("Authorization")))
		a.body = body
		a.mu.Unlock()
		if request["stream"] == true {
			_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
			return
		}
		_, _ = fmt.Fprint(w, `{"id":"chatcmpl-1","model":"gpt-5.6-sol-2026-07-09","choices":[{"message":{"content":"ok"}}]}`)
	}
}

func (a *azureRecorder) last() (string, []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.calls) == 0 {
		return "", nil
	}
	return a.calls[len(a.calls)-1], a.body
}

func (a *azureRecorder) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.calls)
}

// Core's AzureOpenAI sends the body the transport sent: the payload
// buildOpenAIPayload built from withOpenAIOutputLimit's options, byte for
// byte, fields it did not forward dropped.
func TestAzureChatBodyIsTheTransportsPayload(t *testing.T) {
	upstream := &azureRecorder{}
	server := httptest.NewServer(upstream.handler(t))
	defer server.Close()
	provider := azureFixture(t, &config.ProviderConfig{Type: "azure_openai", BaseURL: server.URL, APIKey: "fixture-key"})
	messages := []Message{{"role": "user", "content": "<b>fish & chips</b> \u2028 é"}, {"role": "assistant", "content": nil}}
	for name, kw := range map[string]Kwargs{
		"none": nil,
		"forwarded": {
			"temperature": 0.2, "top_p": json.Number("0.9"), "max_tokens": json.Number("64"), "stop": []any{"END"},
			"tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup"}}}, "tool_choice": "auto",
			"reasoning_effort": "high", "stream_options": map[string]any{"include_usage": true}, "metadata": map[string]any{"user": "fixture"},
			"parallel_tool_calls": true, "thinking": map[string]any{"type": "enabled"},
		},
		"dropped":        {"logprobs": true, "n": json.Number("2"), "temperature": nil, "_affinity_key": "fixture", "_force_api_support": false},
		"output limit":   {"_max_output_tokens": json.Number("128")},
		"explicit limit": {"_max_output_tokens": json.Number("128"), "max_tokens": json.Number("64")},
		"null limit":     {"_max_output_tokens": json.Number("128"), "max_completion_tokens": nil},
	} {
		for _, stream := range []bool{false, true} {
			if stream {
				iter, err := provider.Stream("gpt-5.6-sol", messages, kw)
				if err != nil {
					t.Fatalf("%s stream: %v", name, err)
				}
				_ = iter.Close()
			} else if _, err := provider.Complete("gpt-5.6-sol", messages, kw); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			want, _ := json.Marshal(buildOpenAIPayload("gpt-5.6-sol", messages, stream, withOpenAIOutputLimit(kw)))
			call, body := upstream.last()
			if call != "/openai/v1/chat/completions fixture-key" || string(body) != string(want) {
				t.Fatalf("%s stream=%v: call=%q\nbody=%s\nwant=%s", name, stream, call, body, want)
			}
		}
	}
}

// Each request resolves its key through Azure's store in the provider
// factory's order: the caller's connection, the system connection, then the
// configured key. Without a key nothing is sent, where the transport sent an
// empty api-key.
func TestAzureRequestsResolveTheFactoryPrecedence(t *testing.T) {
	setupCodexProviderTest(t)
	runtime := InstallForTests(t)
	upstream := &azureRecorder{}
	server := httptest.NewServer(upstream.handler(t))
	defer server.Close()
	cfg := &config.ProviderConfig{Type: "azure_openai", BaseURL: server.URL}
	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{azureFixtureInstance: cfg}
	})
	human, err := iam.CreatePrincipal("human", "authentik:azure-owner", "", "Azure Owner")
	if err != nil {
		t.Fatal(err)
	}
	owner := core.Caller{ID: human.ID, Kind: core.CallerHuman}
	hi := []Message{{"role": "user", "content": "hi"}}
	send := func(provider AzureOpenAIProvider, want string) {
		t.Helper()
		_, err := provider.Complete("gpt-fixture", hi, nil)
		if call, _ := upstream.last(); err != nil || call != "/openai/v1/chat/completions "+want {
			t.Fatalf("call=%q err=%v, want key %q", call, err, want)
		}
	}
	expect := func(caller core.Caller, want string) AzureOpenAIProvider {
		t.Helper()
		provider := azureFacade(t, runtime, caller)
		send(provider, want)
		return provider
	}

	if _, err := azureFacade(t, runtime, owner).Complete("gpt-fixture", hi, nil); !IsConfig(err) || upstream.count() != 0 {
		t.Fatalf("a request without a key: err=%v requests=%d", err, upstream.count())
	}
	config.Update(func(s *config.Settings) { s.Providers[azureFixtureInstance].APIKey = "configured-key" })
	expect(owner, "configured-key")
	if _, err := iam.PutSystemProviderConnection(azureFixtureInstance, "api_key", "system-key"); err != nil {
		t.Fatal(err)
	}
	expect(owner, "system-key")
	connection, err := iam.PutProviderConnection(iam.ProviderConnectionCreate{
		PrincipalID: human.ID, ProviderID: azureFixtureInstance, Kind: "api_key", Secret: "personal-key", MakeDefault: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	personal := expect(owner, "personal-key")
	if _, observation, _ := personal.CompleteWithObservation("gpt-fixture", hi, nil); observation == nil || observation.ConnectionID != connection.ID {
		t.Fatalf("observation=%+v, want the factory's personal connection", observation)
	}
	expect(gatewayCaller(), "system-key")
	if err := iam.RevokeProviderConnection(human.ID, connection.ID); err != nil {
		t.Fatal(err)
	}
	send(personal, "system-key")

	oauth, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
		PrincipalID: human.ID, ProviderID: azureFixtureInstance, Kind: "openai_codex_oauth", MakeDefault: true,
		AccessToken: "oauth-access", ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.instantiate(config.Get(), azureFixtureInstance, cfg, owner); !IsConfig(err) {
		t.Fatalf("an OAuth connection built an Azure facade: err=%v", err)
	}
	if _, err := runtime.verticals[azureCoreType].credentials.Load(context.Background(), oauth.ID); !IsConfig(err) {
		t.Fatalf("Azure's store loaded an OAuth connection: err=%v", err)
	}
}

// Core relays Azure's records byte for byte; the facade hands the API layer
// their data as the transport's stream did, and ends as it ended.
func TestAzureStreamRelaysDataEvents(t *testing.T) {
	const first = `{"choices":[{"index":0,"delta":{"content":"a"}}]}`
	const second = `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`
	var response string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, response)
		if strings.HasSuffix(response, "break") {
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		}
	}))
	defer server.Close()
	provider := azureFixture(t, &config.ProviderConfig{Type: "azure_openai", BaseURL: server.URL, APIKey: "fixture-key"})
	drain := func() ([]string, error) {
		t.Helper()
		stream, err := provider.Stream("gpt-fixture", []Message{{"role": "user", "content": "hi"}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer stream.Close()
		var chunks []string
		for chunk, ok := stream.Next(); ok; chunk, ok = stream.Next() {
			chunks = append(chunks, chunk)
		}
		return chunks, stream.Err()
	}

	response = ": keepalive\n\ndata: " + first + "\n\nevent: message\ndata: " + second + "\r\n\r\ndata: [DONE]\n\n"
	if chunks, err := drain(); err != nil || strings.Join(chunks, "|") != first+"|"+second {
		t.Fatalf("chunks=%q err=%v", chunks, err)
	}
	response = "data: " + strings.Repeat("x", maxStreamRecordWireSize) + "\n\n"
	var sizeErr *StreamRecordTooLargeError
	if _, err := drain(); !errors.As(err, &sizeErr) {
		t.Fatalf("oversized record: err=%#v", err)
	}
	response = "data: " + first + "\n\ndata: {\"choices\":[" + "break"
	if chunks, err := drain(); len(chunks) != 1 || !IsInvocation(err) ||
		!strings.HasPrefix(err.Error(), "azure_openai: streaming transport error: ") || InvocationRetryable(err) {
		t.Fatalf("broken stream: chunks=%q err=%v", chunks, err)
	}
}

// Core's failures map back to the errors the transport returned: a refusal
// keeps its status and message but not the Retry-After the transport never
// read, and every other failure keeps the transport's message and
// disposition.
func TestAzureFailuresKeepTheTransportsErrors(t *testing.T) {
	var status int
	var body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, body)
	}))
	defer server.Close()
	provider := azureFixture(t, &config.ProviderConfig{Type: "azure_openai", BaseURL: server.URL, APIKey: "fixture-key"})
	hi := []Message{{"role": "user", "content": "hi"}}
	complete := func() error { _, err := provider.Complete("gpt-fixture", hi, nil); return err }
	stream := func() error { _, err := provider.Stream("gpt-fixture", hi, nil); return err }

	status, body = http.StatusTooManyRequests, `{"error":{"message":"slow down"}}`
	for name, call := range map[string]func() error{"complete": complete, "stream": stream} {
		err := call()
		if err == nil || err.Error() != "azure_openai: upstream returned 429: slow down" || UpstreamStatus(err) != 429 ||
			InvocationRetryAfter(err) != "" || !InvocationRetryable(err) {
			t.Fatalf("%s: err=%v retry-after=%q", name, err, InvocationRetryAfter(err))
		}
	}
	for text, want := range map[string]string{
		`{"id":`:                "azure_openai: invalid JSON in upstream response",
		`{}`:                    "azure_openai: invalid JSON in upstream response",
		`{"choices":[]}`:        "azure_openai: invalid chat response payload",
		`{"choices":["plain"]}`: "azure_openai: invalid chat response payload",
	} {
		status, body = http.StatusOK, text
		if err := complete(); err == nil || err.Error() != want || InvocationRetryable(err) || !InvocationCircuitFailure(err) {
			t.Fatalf("%s: err=%v", text, err)
		}
	}

	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	unanswered := azureFixture(t, &config.ProviderConfig{Type: "azure_openai", BaseURL: closed.URL, APIKey: "fixture-key"})
	for prefix, call := range map[string]func() error{
		"azure_openai: upstream transport error: ":  func() error { _, err := unanswered.Complete("gpt-fixture", hi, nil); return err },
		"azure_openai: streaming transport error: ": func() error { _, err := unanswered.Stream("gpt-fixture", hi, nil); return err },
	} {
		err := call()
		if err == nil || !strings.HasPrefix(err.Error(), prefix+`Post "`+closed.URL+`/openai/v1/chat/completions"`) || !InvocationRetryable(err) {
			t.Fatalf("unanswered: err=%v, want the prefix %q", err, prefix)
		}
	}
}
