package providers

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"llmgw/internal/config"

	core "github.com/xibodev/llmgw-core"
	coreproviders "github.com/xibodev/llmgw-core/providers"
)

// ollamaDaemon serves handler as an Ollama daemon until the test ends.
func ollamaDaemon(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

// ollamaFixture returns the facade of an Ollama instance at base, which the
// core Runtime of a Runtime installed for the test serves.
func ollamaFixture(t *testing.T, base string) *ollamaProvider {
	t.Helper()
	runtime := InstallForTests(t)
	timeout := 2.0
	cfg := &config.ProviderConfig{Type: "ollama", BaseURL: base, Timeout: &timeout}
	// Add the instance rather than replace the providers, so a test that also
	// builds another fixture keeps that one's settings.
	config.Update(func(s *config.Settings) {
		if s.Providers == nil {
			s.Providers = map[string]*config.ProviderConfig{}
		}
		s.Providers["ollama-fixture"] = cfg
	})
	t.Cleanup(func() { config.Update(func(s *config.Settings) { delete(s.Providers, "ollama-fixture") }) })
	provider, err := runtime.instantiate("ollama-fixture", cfg, gatewayCaller())
	if err != nil {
		t.Fatal(err)
	}
	ollama, ok := provider.(*ollamaProvider)
	if !ok {
		t.Fatalf("provider=%T, want the Ollama facade", provider)
	}
	return ollama
}

// ollamaCatalogFixture returns an Ollama facade at base whose catalog a test
// reads. It changes no settings, so nothing serves its Chat.
func ollamaCatalogFixture(t *testing.T, base string, timeout float64) Provider {
	t.Helper()
	provider, err := Current().newOllamaProvider("catalog-read", gatewayCaller(), base, timeout)
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

// Chat goes through the core Runtime, and the daemon receives the /api/chat
// body the transport sent, byte for byte: a developer message as a system
// one, an empty one dropped, content that is not a string printed, and
// temperature, top_p, tools and the output limit as options, the limit of a
// Responses request included. The answer reads as the transport read it.
func TestOllamaChatGoesThroughTheCoreRuntime(t *testing.T) {
	var bodies []string
	daemon := ollamaDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(raw))
		if r.Method != http.MethodPost || r.URL.Path != "/api/chat" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("request %s %s %q", r.Method, r.URL.Path, r.Header.Get("Content-Type"))
		}
		_, _ = fmt.Fprint(w, `{"message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"add","arguments":{"a":2}}}]},"prompt_eval_count":7,"eval_count":3}`)
	})
	tools := []any{map[string]any{"type": "function", "function": map[string]any{"name": "add"}}}
	response, err := ollamaFixture(t, daemon.URL).Complete("fixture-model", []Message{
		{"role": "developer", "content": "policy"},
		{"role": "user", "content": " "},
		{"content": []any{map[string]any{"type": "text", "text": "hi"}}},
		{"role": "tool", "content": "5", "tool_call_id": "call_0", "name": "add"},
	}, Kwargs{
		"temperature": json.Number("0.2"), "top_p": 0.9, "tools": tools, "stop": []any{"END"},
		"_max_output_tokens": json.Number("64"), "_affinity_key": "fixture-affinity",
	})
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"messages":[{"content":"policy","role":"system"},{"content":"[map[text:hi type:text]]","role":"user"},` +
		`{"content":"5","name":"add","role":"tool","tool_call_id":"call_0"}],"model":"fixture-model",` +
		`"options":{"num_predict":64,"temperature":0.2,"top_p":0.9},"stream":false,"tools":[{"function":{"name":"add"},"type":"function"}]}`
	if len(bodies) != 1 || bodies[0] != want {
		t.Fatalf("bodies=%q\nwant %s", bodies, want)
	}
	encoded, _ := json.Marshal(response)
	const completion = `{"choices":[{"finish_reason":"tool_calls","index":0,"message":{"content":"","role":"assistant",` +
		`"tool_calls":[{"function":{"arguments":"{\"a\":2}","name":"add"},"id":"call_0","index":0,"type":"function"}]}}],` +
		`"id":"chatcmpl-ollama","model":"fixture-model","object":"chat.completion","usage":{"completion_tokens":3,"prompt_tokens":7,"total_tokens":10}}`
	if string(encoded) != completion {
		t.Fatalf("completion=%s\nwant %s", encoded, completion)
	}
}

// drainOllamaStream reads a stream to its end.
func drainOllamaStream(t *testing.T, stream StreamIter, err error) ([]string, error) {
	t.Helper()
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

func TestOllamaStreamNormalAndOversizedRecords(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response string
		wantText string
		wantErr  bool
	}{
		{name: "normal", response: `{"message":{"content":"hello"},"done":false}` + "\n" + `{"done":true}` + "\n", wantText: "hello"},
		{name: "oversized", response: strings.Repeat("x", maxStreamRecordWireSize+1) + "\n", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			daemon := ollamaDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprint(w, tc.response)
			})
			stream, err := ollamaFixture(t, daemon.URL).Stream("model", []Message{{"role": "user", "content": "hi"}}, nil)
			chunks, err := drainOllamaStream(t, stream, err)
			if tc.wantText != "" && !strings.Contains(strings.Join(chunks, ""), tc.wantText) {
				t.Fatalf("chunks = %s", chunks)
			}
			var sizeErr *StreamRecordTooLargeError
			if errors.As(err, &sizeErr) != tc.wantErr {
				t.Fatalf("error = %#v, want oversized %v", err, tc.wantErr)
			}
			if sizeErr != nil && (sizeErr.Format != "NDJSON" || strings.Contains(sizeErr.Error(), strings.Repeat("x", 32))) {
				t.Fatalf("unsafe error = %#v", sizeErr)
			}
		})
	}
}

// A stream relays the chunks the transport's stream returned, byte for
// byte. After tool calls it still finishes with stop: the API layer's chat
// stream is what rewrites that finish to tool_calls.
func TestOllamaStreamFinishesWithStopAfterToolCalls(t *testing.T) {
	daemon := ollamaDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil || body["stream"] != true {
			t.Errorf("stream body=%v", body)
		}
		_, _ = fmt.Fprint(w, `{"message":{"tool_calls":[{"function":{"name":"add","arguments":{"a":2}}}]},"done":false}`+"\n"+`{"done":true}`+"\n")
	})
	stream, err := ollamaFixture(t, daemon.URL).Stream("fixture-model", []Message{{"role": "user", "content": "add"}}, nil)
	chunks, err := drainOllamaStream(t, stream, err)
	want := []string{
		`{"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"{\"a\":2}","name":"add"},"id":"call_0","index":0,"type":"function"}]},` +
			`"finish_reason":null,"index":0}],"id":"chatcmpl-ollama","model":"fixture-model","object":"chat.completion.chunk"}`,
		`{"choices":[{"delta":{},"finish_reason":"stop","index":0}],"id":"chatcmpl-ollama","model":"fixture-model","object":"chat.completion.chunk"}`,
	}
	if err != nil || strings.Join(chunks, "\n") != strings.Join(want, "\n") {
		t.Fatalf("chunks=%q err=%v\nwant %q", chunks, err, want)
	}
}

// The daemon's refusals keep the status, the transport's message and the
// dispositions the router and the resilience wrapper decided on before; the
// transport read no Retry-After, so none is kept. An answer that cannot be
// used counts against the circuit, and a daemon that cannot be reached may
// be repeated, with a message that does not name its address.
func TestOllamaFailuresKeepTheirClassification(t *testing.T) {
	var status int
	var answer string
	daemon := ollamaDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, answer)
	})
	provider := ollamaFixture(t, daemon.URL)
	messages := []Message{{"role": "user", "content": "hi"}}
	calls := map[string]func() error{
		"chat":   func() error { _, err := provider.Complete("fixture-model", messages, nil); return err },
		"stream": func() error { _, err := provider.Stream("fixture-model", messages, nil); return err },
	}
	answer = `{"error":"model 'fixture-model' not found"}`
	for _, fixture := range []struct {
		status                     int
		retryable, failover, trips bool
	}{
		{http.StatusNotFound, false, false, false},
		{http.StatusUnauthorized, false, false, false},
		{http.StatusTooManyRequests, true, true, true},
		{http.StatusServiceUnavailable, true, true, true},
	} {
		status = fixture.status
		want := fmt.Sprintf("ollama: request failed (%d): model 'fixture-model' not found", fixture.status)
		for name, call := range calls {
			err := call()
			if UpstreamStatus(err) != fixture.status || err.Error() != want || InvocationRetryAfter(err) != "" ||
				InvocationRetryable(err) != fixture.retryable || InvocationFailoverEligible(err) != fixture.failover ||
				InvocationCircuitFailure(err) != fixture.trips {
				t.Fatalf("%s %d: err=%v retry-after=%q", name, fixture.status, err, InvocationRetryAfter(err))
			}
		}
	}

	status = http.StatusOK
	for _, unusable := range []string{`not json`, `{"message":{"content":""}}`, `{"done":true}`} {
		answer = unusable
		if err := calls["chat"](); !IsInvocation(err) || UpstreamStatus(err) != 0 || InvocationRetryable(err) || !InvocationCircuitFailure(err) {
			t.Fatalf("answer %s: err=%v", unusable, err)
		}
	}

	daemon.Close()
	for name, call := range calls {
		err := call()
		if !IsInvocation(err) || UpstreamStatus(err) != 0 || !InvocationRetryable(err) || !InvocationFailoverEligible(err) ||
			strings.Contains(err.Error(), strings.TrimPrefix(daemon.URL, "http://")) {
			t.Fatalf("%s unreachable: err=%v", name, err)
		}
	}
}

func TestOllamaNativeRootDiagnostics(t *testing.T) {
	for _, scenario := range []struct {
		base, wantIssue string
	}{
		{base: "http://127.0.0.1:11434"},
		{base: "http://host.docker.internal:11434"},
		{base: "http://127.0.0.1:11434/v1", wantIssue: "not the OpenAI-compatible /v1 URL"},
		{base: "http://127.0.0.1:11434/api", wantIssue: "with no path"},
		{base: "127.0.0.1:11434", wantIssue: "must be an http(s)"},
	} {
		t.Run(scenario.base, func(t *testing.T) {
			issue := coreproviders.OllamaBaseURLIssue(scenario.base)
			if scenario.wantIssue == "" && issue != "" || scenario.wantIssue != "" && !strings.Contains(issue, scenario.wantIssue) {
				t.Fatalf("issue=%q", issue)
			}
			// A base with an issue builds no facade, and the error names
			// the issue, never the base.
			_, err := Current().newOllamaProvider("ollama-fixture", gatewayCaller(), scenario.base, 2)
			if scenario.wantIssue == "" && err != nil || scenario.wantIssue != "" && (!IsConfig(err) || !strings.Contains(err.Error(), issue)) {
				t.Fatalf("facade err=%v", err)
			}
		})
	}
	if _, err := Current().newOllamaProvider("ollama-fixture", gatewayCaller(), "://fixture-secret", 2); !IsConfig(err) || strings.Contains(err.Error(), "fixture-secret") {
		t.Fatalf("unparsable base: err=%v", err)
	}
	if guidance := ollamaProcessBoundaryGuidance("http://localhost:11434"); !strings.Contains(guidance, "container") || !strings.Contains(guidance, "host.docker.internal") {
		t.Fatalf("loopback guidance=%q", guidance)
	}
	if guidance := ollamaProcessBoundaryGuidance("http://ollama:11434"); guidance != "" {
		t.Fatalf("non-loopback guidance=%q", guidance)
	}
}

func TestOllamaConfigurationIssueRejectsOpenAICompatiblePath(t *testing.T) {
	oldProviders := config.Get().Providers
	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{
			"ollama-fixture": {Type: "ollama", BaseURL: "http://127.0.0.1:11434/v1"},
		}
	})
	t.Cleanup(func() { config.Update(func(s *config.Settings) { s.Providers = oldProviders }) })
	if issue := ProviderConfigurationIssue("ollama-fixture"); !strings.Contains(issue, "remove /v1") {
		t.Fatalf("configuration issue=%q", issue)
	}
}

// Ollama is keyless: no caller resolves a credential for an instance, so the
// core Runtime sends every request without one, and Ollama's store, which
// the facade names on each operation, holds nothing.
func TestOllamaInstancesResolveNoCredential(t *testing.T) {
	daemon := ollamaDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("a keyless request carried Authorization")
		}
		_, _ = fmt.Fprint(w, `{"message":{"content":"ok"}}`)
	})
	provider := ollamaFixture(t, daemon.URL)
	if _, err := provider.Complete("fixture-model", []Message{{"role": "user", "content": "hi"}}, nil); err != nil {
		t.Fatal(err)
	}
	ctx := withCoreOperation(t.Context(), ollamaCoreType, gatewayCaller())
	store := provider.runtime.verticals[ollamaCoreType].credentials
	if key, err := store.Resolve(ctx, gatewayCaller(), "ollama-fixture"); !errors.Is(err, core.ErrNoCredential) {
		t.Fatalf("resolved key=%q err=%v", key, err)
	}
	if _, err := (coreCredentials{runtime: provider.runtime}).Load(ctx, "ollama-fixture"); err == nil {
		t.Fatal("Ollama's store loaded a credential")
	}
}
