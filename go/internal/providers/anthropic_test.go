package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"

	anthropicauth "github.com/xibodev/llm-provider-auth/anthropic"
	core "github.com/xibodev/llmgw-core"
)

const anthropicFixtureInstance = "anthropic-fixture"

// anthropicFixture configures cfg as an Anthropic instance until the test
// ends and returns the facade the provider factory builds for it, served by a
// Runtime installed for the test. Without credential encryption the instance
// resolves only its configured key.
func anthropicFixture(t *testing.T, cfg *config.ProviderConfig) AnthropicNativeProvider {
	t.Helper()
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	runtime := InstallForTests(t)
	oldKey, oldProviders := config.Get().CredentialEncryptionKey, config.Get().Providers
	config.Update(func(s *config.Settings) {
		s.CredentialEncryptionKey = ""
		s.Providers = map[string]*config.ProviderConfig{anthropicFixtureInstance: cfg}
	})
	t.Cleanup(func() {
		config.Update(func(s *config.Settings) { s.CredentialEncryptionKey, s.Providers = oldKey, oldProviders })
	})
	return anthropicFacade(t, runtime, gatewayCaller())
}

// anthropicFacade is the facade the provider factory builds for caller.
func anthropicFacade(t *testing.T, runtime *Runtime, caller core.Caller) AnthropicNativeProvider {
	t.Helper()
	provider, err := runtime.instantiate(config.Get(), anthropicFixtureInstance, config.Get().Providers[anthropicFixtureInstance], caller)
	if err != nil {
		t.Fatal(err)
	}
	facade, ok := provider.(AnthropicNativeProvider)
	if !ok {
		t.Fatalf("provider=%T, want the Anthropic facade", provider)
	}
	return facade
}

// anthropicServer serves handler until the test ends and returns its URL.
func anthropicServer(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server.URL
}

func anthropicTestAuth(t *testing.T, credential string) anthropicauth.HeaderSource {
	t.Helper()
	source, err := anthropicauth.NewHeaderSource(credential)
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func anthropicSetupToken() string { return anthropicauth.SetupTokenPrefix + strings.Repeat("a", 80) }

// anthropicStreamFixture is a Messages stream that completes with "hello".
const anthropicStreamFixture = "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"model\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":2}}}\n\n" +
	"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
	"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n" +
	"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
	"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
	"data: {\"type\":\"message_stop\"}\n\n"

func TestAnthropicDeclaresOnlyMessagesWireNative(t *testing.T) {
	provider := AnthropicNativeProvider{}
	if !PreservesWireNativeSurface(provider, "claude-fixture", core.ModelSurfaceMessages) {
		t.Fatal("Anthropic Messages was not declared wire-native")
	}
	if PreservesWireNativeSurface(provider, "claude-fixture", core.ModelSurfaceChatCompletions) {
		t.Fatal("Anthropic Chat translation was declared wire-native")
	}
}

func TestAnthropicCredentialHeadersAcrossRequestSurfaces(t *testing.T) {
	setupToken := anthropicSetupToken()
	for _, tc := range []struct {
		name, credential, wantHeader, wantValue string
		wantBeta                                bool
	}{
		{name: "api key", credential: "fixture-key", wantHeader: "x-api-key", wantValue: "fixture-key"},
		{name: "setup token", credential: setupToken, wantHeader: "Authorization", wantValue: "Bearer " + setupToken, wantBeta: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			base := anthropicServer(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if got := r.Header.Get(tc.wantHeader); got != tc.wantValue {
					t.Errorf("%s %s=%q", r.URL.Path, tc.wantHeader, got)
				}
				if got := strings.Contains(strings.Join(r.Header.Values("anthropic-beta"), ","), anthropicauth.OAuthBeta); got != tc.wantBeta {
					t.Errorf("%s oauth beta=%v", r.URL.Path, got)
				}
				switch r.URL.Path {
				case "/v1/models":
					_, _ = w.Write([]byte(`{"data":[]}`))
				case "/v1/messages/count_tokens":
					_, _ = w.Write([]byte(`{"input_tokens":1}`))
				default:
					var request map[string]any
					_ = json.NewDecoder(r.Body).Decode(&request)
					if request["stream"] == true {
						_, _ = fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"content\":[]}}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n")
					} else {
						_, _ = w.Write([]byte(`{"content":[]}`))
					}
				}
			})
			provider := anthropicFixture(t, &config.ProviderConfig{Type: "anthropic", BaseURL: base, APIKey: tc.credential})
			if _, err := provider.Complete("model", []Message{{"role": "user", "content": "hi"}}, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := provider.CompleteAnthropicMessages("model", map[string]any{"messages": []any{}}); err != nil {
				t.Fatal(err)
			}
			if _, err := provider.CountAnthropicTokens("model", map[string]any{"messages": []any{}}, "", nil); err != nil {
				t.Fatal(err)
			}
			if _, _, err := provider.ListModelsWithError(); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 4 {
				t.Fatalf("calls=%d", calls.Load())
			}
		})
	}
}

// Each request resolves its credential through Anthropic's store in the
// provider factory's order: the caller's connection, the system connection,
// the configured key, and without any the request is sent without one. A
// stored setup token reaches core's Anthropic as a setup token, which it
// sends as the OAuth bearer and assembles its completion from a stream.
func TestAnthropicRequestsResolveTheFactoryPrecedence(t *testing.T) {
	setupCodexProviderTest(t)
	runtime := InstallForTests(t)
	var mu sync.Mutex
	last := ""
	base := anthropicServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		last = strings.TrimSpace(r.Header.Get("x-api-key") + " " + r.Header.Get("Authorization") + " " + strings.Join(r.Header.Values("anthropic-beta"), ","))
		mu.Unlock()
		if zenBody(t, r)["stream"] == true {
			_, _ = fmt.Fprint(w, anthropicStreamFixture)
			return
		}
		_, _ = fmt.Fprint(w, `{"content":[]}`)
	})
	cfg := &config.ProviderConfig{Type: "anthropic", BaseURL: base}
	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{anthropicFixtureInstance: cfg}
	})
	human, err := iam.CreatePrincipal("human", "authentik:anthropic-owner", "", "Anthropic Owner")
	if err != nil {
		t.Fatal(err)
	}
	owner := core.Caller{ID: human.ID, Kind: core.CallerHuman}
	send := func(provider AnthropicNativeProvider, want string) {
		t.Helper()
		_, err := provider.CompleteAnthropicMessages("claude-fixture", map[string]any{"messages": []any{}})
		mu.Lock()
		defer mu.Unlock()
		if err != nil || last != want {
			t.Fatalf("credential=%q err=%v, want %q", last, err, want)
		}
	}
	expect := func(caller core.Caller, want string) AnthropicNativeProvider {
		t.Helper()
		provider := anthropicFacade(t, runtime, caller)
		send(provider, want)
		return provider
	}

	expect(owner, "")
	config.Update(func(s *config.Settings) { s.Providers[anthropicFixtureInstance].APIKey = "configured-key" })
	expect(owner, "configured-key")
	token := anthropicSetupToken()
	if _, err := iam.PutSystemProviderConnection(anthropicFixtureInstance, "setup_token", token); err != nil {
		t.Fatal(err)
	}
	subscriber := "Bearer " + token + " " + anthropicauth.OAuthBeta
	expect(owner, subscriber)
	connection, err := iam.PutProviderConnection(iam.ProviderConnectionCreate{
		PrincipalID: human.ID, ProviderID: anthropicFixtureInstance, Kind: "api_key", Secret: "personal-key", MakeDefault: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	personal := expect(owner, "personal-key")
	expect(gatewayCaller(), subscriber)

	// A request resolves its own credential, so one the connection no longer
	// holds is never sent, whatever facade the cache still holds.
	if err := iam.RevokeProviderConnection(human.ID, connection.ID); err != nil {
		t.Fatal(err)
	}
	send(personal, subscriber)

	// A connection of another kind is refused as the factory refused it.
	oauth, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
		PrincipalID: human.ID, ProviderID: anthropicFixtureInstance, Kind: "openai_codex_oauth", MakeDefault: true,
		AccessToken: "oauth-access", ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.instantiate(config.Get(), anthropicFixtureInstance, cfg, owner); !IsConfig(err) {
		t.Fatalf("an OAuth connection built an Anthropic facade: err=%v", err)
	}
	if _, err := runtime.verticals[anthropicCoreType].credentials.Load(context.Background(), oauth.ID); !IsConfig(err) {
		t.Fatalf("Anthropic's store loaded an OAuth connection: err=%v", err)
	}
	if _, err := personal.CompleteAnthropicMessages("claude-fixture", map[string]any{"messages": []any{}}); !IsConfig(err) {
		t.Fatalf("a request was sent with an OAuth connection: err=%v", err)
	}
}

func TestAnthropicSetupTokenCompleteAccumulatesNativeStream(t *testing.T) {
	base := anthropicServer(t, func(w http.ResponseWriter, r *http.Request) {
		if request := zenBody(t, r); request["stream"] != true {
			t.Errorf("stream=%v", request["stream"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, anthropicStreamFixture)
	})
	provider := anthropicFixture(t, &config.ProviderConfig{Type: "anthropic", BaseURL: base, APIKey: anthropicSetupToken()})
	response, err := provider.Complete("model", []Message{{"role": "user", "content": "hi"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	message := response["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "hello" {
		t.Fatalf("response=%v", response)
	}
}

// A streamed completion the transport could not use keeps the message and
// the disposition it gave it: counted against the circuit, never retried.
func TestAnthropicSetupTokenRejectsIncompleteOrMalformedStream(t *testing.T) {
	const start = "data: {\"type\":\"message_start\",\"message\":{\"content\":[]}}\n\n"
	const stop = "data: {\"type\":\"message_stop\"}\n\n"
	for _, tc := range []struct{ response, want string }{
		{start, "anthropic: incomplete streamed Messages response"},
		{stop + start, "anthropic: incomplete streamed Messages response"},
		{"data: not-json\n\n", "anthropic: invalid JSON in streamed Messages response"},
		{start + stop + start, "anthropic: data followed streamed Messages terminal event"},
		{"data: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\"}}\n\n", "anthropic: streamed Messages response reported an error"},
	} {
		t.Run(tc.response, func(t *testing.T) {
			base := anthropicServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, tc.response)
			})
			provider := anthropicFixture(t, &config.ProviderConfig{Type: "anthropic", BaseURL: base, APIKey: anthropicSetupToken()})
			_, err := provider.Complete("model", []Message{{"role": "user", "content": "hi"}}, nil)
			if err == nil || err.Error() != tc.want || !InvocationCircuitFailure(err) || InvocationRetryable(err) {
				t.Fatalf("response %q: err=%v", tc.response, err)
			}
		})
	}
}

func TestAnthropicNativePayloadPreservesThinkingAndOutputConfig(t *testing.T) {
	p := AnthropicNativeProvider{}
	payload, err := p.payload(
		"claude-opus-4.8",
		[]Message{{"role": "user", "content": "hi"}},
		false,
		Kwargs{
			"thinking":      map[string]any{"type": "adaptive"},
			"output_config": map[string]any{"effort": "xhigh"},
			"metadata":      map[string]any{"user_id": "fixture"},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	thinking, ok := payload["thinking"].(map[string]any)
	if !ok || thinking["type"] != "adaptive" {
		t.Fatalf("thinking = %#v, want adaptive map", payload["thinking"])
	}
	outputConfig, ok := payload["output_config"].(map[string]any)
	if !ok || outputConfig["effort"] != "xhigh" {
		t.Fatalf("output_config = %#v, want xhigh map", payload["output_config"])
	}
	metadata, ok := payload["metadata"].(map[string]any)
	if !ok || metadata["user_id"] != "fixture" {
		t.Fatalf("metadata = %#v, want fixture user", payload["metadata"])
	}
}

func TestAnthropicNativePayloadTranslationLossPolicy(t *testing.T) {
	p := AnthropicNativeProvider{}
	payload, err := p.payload("model", []Message{{"role": "developer", "content": "instruction"}, {"role": "user", "content": "hi"}}, false, nil)
	if err != nil {
		t.Fatalf("advisory developer conversion rejected: %v", err)
	}
	if payload["system"] != "instruction" {
		t.Fatalf("system=%#v", payload["system"])
	}

	_, err = p.payload("model", []Message{{"role": "unknown", "content": "hi"}}, false, nil)
	if err == nil || !strings.Contains(err.Error(), "messages.0.role") {
		t.Fatalf("material role conversion error=%v", err)
	}
}

// The Chat stream ends as the transport's did: without an error when
// Anthropic ends it, message_stop or not, and with the gateway's size error
// for a record over the limit.
func TestAnthropicStreamNormalAndOversizedRecords(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response string
		wantText string
		wantErr  bool
	}{
		{name: "normal", response: "event: message_start\ndata: {\"type\":\"message_start\"}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n", wantText: "hello"},
		{name: "oversized", response: "data: " + strings.Repeat("x", maxStreamRecordWireSize) + "\n\n", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := anthropicServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprint(w, tc.response)
			})
			two := 2.0
			provider := anthropicFixture(t, &config.ProviderConfig{Type: "anthropic", BaseURL: base, Timeout: &two})
			iter, err := provider.Stream("model", []Message{{"role": "user", "content": "hi"}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer iter.Close()
			var chunks strings.Builder
			for chunk, ok := iter.Next(); ok; chunk, ok = iter.Next() {
				chunks.WriteString(chunk)
			}
			if tc.wantText != "" && !strings.Contains(chunks.String(), tc.wantText) {
				t.Fatalf("chunks = %s", chunks.String())
			}
			var sizeErr *StreamRecordTooLargeError
			if errors.As(iter.Err(), &sizeErr) != tc.wantErr || (!tc.wantErr && iter.Err() != nil) {
				t.Fatalf("error = %#v, want oversized %v", iter.Err(), tc.wantErr)
			}
		})
	}
}

// A reader that stops reading and closes the stream releases the goroutine
// that re-encodes it, which the transport's stream never did.
func TestAnthropicStreamCloseReleasesTheReencoder(t *testing.T) {
	var events strings.Builder
	for range 64 {
		events.WriteString("data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"x\"}}\n\n")
	}
	base := anthropicServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, events.String())
	})
	provider := anthropicFixture(t, &config.ProviderConfig{Type: "anthropic", BaseURL: base})
	iter, err := provider.Stream("model", []Message{{"role": "user", "content": "hi"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := iter.Next(); !ok {
		t.Fatal("the stream ended before its first chunk")
	}
	_ = iter.Close()
	drained := make(chan struct{})
	go func() {
		for _, ok := iter.Next(); ok; _, ok = iter.Next() {
		}
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("the re-encoder still runs after Close")
	}
}

func TestAnthropicListModelsDeclaresMessagesSurface(t *testing.T) {
	discoveredAt := time.Date(2026, time.September, 21, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-test","display_name":"Claude Test"}]}`))
	}))
	defer server.Close()
	models := (AnthropicNativeProvider{BaseURL: server.URL, Now: func() time.Time { return discoveredAt }}).ListModels()
	if len(models) != 1 || len(models[0].SupportedSurfaces) != 1 || models[0].SupportedSurfaces[0] != "/v1/messages" {
		t.Fatalf("models=%+v", models)
	}
	capabilities := models[0].TypedCapabilities
	if capabilities == nil || capabilities.Operations.Chat != core.SupportSupported ||
		capabilities.Surfaces.Messages != core.SupportSupported ||
		capabilities.Surfaces.ChatCompletions != core.SupportUnsupported ||
		capabilities.Surfaces.Responses != core.SupportUnsupported ||
		capabilities.Streaming != core.SupportSupported ||
		capabilities.Provenance.Source != core.ModelCapabilitySourceRegistryStatic ||
		capabilities.Provenance.Confidence != core.ModelCapabilityConfidenceHigh ||
		capabilities.Freshness.DiscoveredAt == nil || !capabilities.Freshness.DiscoveredAt.Equal(discoveredAt) ||
		capabilities.Freshness.ExpiresAt == nil || !capabilities.Freshness.ExpiresAt.Equal(discoveredAt.Add(anthropicCatalogTTL)) {
		t.Fatalf("capability evidence=%+v", capabilities)
	}
}

func TestAnthropicNativeMessagesPreservesOpaquePayloadAndResponse(t *testing.T) {
	const response = `{"id":"msg_native","model":"upstream-model","stop_sequence":"END","content":[{"type":"thinking","thinking":"kept","signature":"sig"},{"type":"unknown","value":1}],"usage":{"input_tokens":9007199254740993,"cache_read_input_tokens":1}}`
	var request map[string]any
	base := anthropicServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path=%s", r.URL.Path)
		}
		request = zenBody(t, r)
		_, _ = w.Write([]byte(response))
	})
	payload := map[string]any{
		"model": "picker-alias", "stream": true, "system": []any{map[string]any{"type": "text", "text": "client"}},
		"messages": []any{map[string]any{"role": "user", "content": "hi"}}, "thinking": map[string]any{"type": "enabled"},
		"_llmgw_preamble": "policy",
	}
	got, err := anthropicFixture(t, &config.ProviderConfig{Type: "anthropic", BaseURL: base}).CompleteAnthropicMessages("resolved-model", payload)
	if err != nil {
		t.Fatal(err)
	}
	if request["model"] != "resolved-model" || request["stream"] != false || request["thinking"] == nil || request["_llmgw_preamble"] != nil {
		t.Fatalf("request=%#v", request)
	}
	system := request["system"].([]any)
	if system[0].(map[string]any)["text"] != "policy" || system[1].(map[string]any)["text"] != "client" {
		t.Fatalf("system=%#v", system)
	}
	actual, _ := json.Marshal(got)
	if got["usage"].(map[string]any)["input_tokens"] != json.Number("9007199254740993") || !strings.Contains(string(actual), "9007199254740993") || !strings.Contains(string(actual), `"type":"unknown"`) {
		t.Fatalf("semantic response=%s", actual)
	}
}

func TestAnthropicNativeMessagesRejectsStructurallyInvalidSuccessPayloads(t *testing.T) {
	for _, body := range []string{"null", `{}`} {
		t.Run(body, func(t *testing.T) {
			base := anthropicServer(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(body))
			})
			_, err := anthropicFixture(t, &config.ProviderConfig{Type: "anthropic", BaseURL: base}).CompleteAnthropicMessages("model", map[string]any{"messages": []any{}})
			if err == nil || !InvocationCircuitFailure(err) || InvocationRetryable(err) {
				t.Fatalf("error=%v circuit=%v retry=%v", err, InvocationCircuitFailure(err), InvocationRetryable(err))
			}
		})
	}
}

type messagesTestProvider struct {
	calls int
	err   error
}

type tokenCounterTestProvider struct {
	calls int
	err   error
}

func (p *tokenCounterTestProvider) Complete(string, []Message, Kwargs) (map[string]any, error) {
	return nil, nil
}
func (p *tokenCounterTestProvider) Stream(string, []Message, Kwargs) (StreamIter, error) {
	return nil, nil
}
func (p *tokenCounterTestProvider) ListModels() []ModelInfo { return nil }
func (p *tokenCounterTestProvider) IsStub() bool            { return false }
func (p *tokenCounterTestProvider) CountAnthropicTokens(string, map[string]any, string, []string) (json.Number, error) {
	p.calls++
	if p.err != nil {
		return "", p.err
	}
	return json.Number("1"), nil
}

func (p *messagesTestProvider) Complete(string, []Message, Kwargs) (map[string]any, error) {
	return nil, nil
}
func (p *messagesTestProvider) Stream(string, []Message, Kwargs) (StreamIter, error) { return nil, nil }
func (p *messagesTestProvider) ListModels() []ModelInfo                              { return nil }
func (p *messagesTestProvider) IsStub() bool                                         { return false }
func (p *messagesTestProvider) CompleteAnthropicMessages(string, map[string]any) (map[string]any, error) {
	p.calls++
	if p.err != nil {
		return nil, p.err
	}
	return map[string]any{"id": "ok"}, nil
}

func TestResilientProviderAnthropicMessagesRetryEligibility(t *testing.T) {
	for _, status := range []int{0, 408, 429, 500, 502, 503, 504, 400, 401, 403, 404} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			err := invocationStatus("failure", status)
			if status == 0 {
				err = retryableInvocation("transport failure")
			}
			inner := &messagesTestProvider{err: err}
			wrapped := &ResilientProvider{inner: inner, name: "messages-" + strconv.Itoa(status), policy: config.ProviderPolicy{RetryMaxAttempts: 2}}
			if !SupportsAnthropicMessages(wrapped) {
				t.Fatal("wrapped native capability hidden")
			}
			_, _ = wrapped.CompleteAnthropicMessages("model", map[string]any{})
			want := 1
			if status == 0 || status == 408 || status == 429 || status == 500 || status == 502 || status == 503 || status == 504 {
				want = 2
			}
			if inner.calls != want {
				t.Fatalf("calls=%d want=%d", inner.calls, want)
			}
		})
	}
	inner := &messagesTestProvider{err: &ConfigError{Msg: "compatibility"}}
	wrapped := &ResilientProvider{inner: inner, name: "messages-config", policy: config.ProviderPolicy{RetryMaxAttempts: 2}}
	_, _ = wrapped.CompleteAnthropicMessages("model", map[string]any{})
	if inner.calls != 1 {
		t.Fatalf("compatibility calls=%d", inner.calls)
	}
}

func TestResilientProviderAnthropicMessagesDefinitiveFailureBreaksCircuitStreak(t *testing.T) {
	name := t.Name()
	ResetCircuit(name)
	t.Cleanup(func() { ResetCircuit(name) })
	inner := &messagesTestProvider{err: invocationStatus("temporary", http.StatusServiceUnavailable)}
	wrapped := &ResilientProvider{inner: inner, name: name, policy: config.ProviderPolicy{
		RetryMaxAttempts: 1, CircuitFailureThreshold: 2, CircuitCooldownSeconds: 60,
	}}
	_, _ = wrapped.CompleteAnthropicMessages("model", map[string]any{})
	inner.err = invocationStatus("bad request", http.StatusBadRequest)
	_, _ = wrapped.CompleteAnthropicMessages("model", map[string]any{})
	inner.err = invocationStatus("temporary", http.StatusServiceUnavailable)
	_, _ = wrapped.CompleteAnthropicMessages("model", map[string]any{})
	inner.err = nil
	_, err := wrapped.CompleteAnthropicMessages("model", map[string]any{})
	if err != nil || inner.calls != 4 {
		t.Fatalf("error=%v calls=%d want=4", err, inner.calls)
	}
}

func TestResilientProviderAnthropicTokenCountDefinitiveFailureBreaksCircuitStreak(t *testing.T) {
	name := t.Name()
	ResetCircuit(name)
	t.Cleanup(func() { ResetCircuit(name) })
	inner := &tokenCounterTestProvider{err: invocationStatus("temporary", http.StatusServiceUnavailable)}
	wrapped := &ResilientProvider{inner: inner, name: name, policy: config.ProviderPolicy{
		RetryMaxAttempts: 1, CircuitFailureThreshold: 2, CircuitCooldownSeconds: 60,
	}}
	_, _ = wrapped.CountAnthropicTokens("model", map[string]any{}, "", nil)
	inner.err = invocationStatus("bad request", http.StatusBadRequest)
	_, _ = wrapped.CountAnthropicTokens("model", map[string]any{}, "", nil)
	inner.err = invocationStatus("temporary", http.StatusServiceUnavailable)
	_, _ = wrapped.CountAnthropicTokens("model", map[string]any{}, "", nil)
	inner.err = nil
	_, err := wrapped.CountAnthropicTokens("model", map[string]any{}, "", nil)
	if err != nil || inner.calls != 4 {
		t.Fatalf("error=%v calls=%d want=4", err, inner.calls)
	}
}

func TestAnthropicPayloadHonoursJSONNumberMaxTokens(t *testing.T) {
	// Request handlers decode with UseNumber; the streaming path must still
	// forward the caller's limit instead of the provider default.
	payload, err := AnthropicNativeProvider{}.payload("model", []Message{{"role": "user", "content": "hi"}}, true, Kwargs{"max_tokens": json.Number("64")})
	if err != nil {
		t.Fatal(err)
	}
	if payload["max_tokens"] != 64 {
		t.Fatalf("max_tokens=%v, want 64", payload["max_tokens"])
	}
	for input, want := range map[any]int{json.Number("12"): 12, json.Number("12.9"): 12, json.Number("oops"): 0, float64(3): 3, int64(4): 4, "5": 0} {
		if got := intOf(input); got != want {
			t.Errorf("intOf(%#v)=%d, want %d", input, got, want)
		}
	}
}
