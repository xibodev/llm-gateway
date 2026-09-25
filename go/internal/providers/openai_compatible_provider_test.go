package providers

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"

	core "github.com/xibodev/llmgw-core"
)

const openAIFixtureInstance = "openai-fixture"

// openAICompatibleFixture configures cfg as an instance until the test ends
// and returns the facade the provider factory builds for it, served by a
// Runtime installed for the test. Without credential encryption the instance
// resolves only its configured key.
func openAICompatibleFixture(t *testing.T, cfg *config.ProviderConfig) *openAICompatibleProvider {
	t.Helper()
	return openAIFacade(t, openAIFixtureRuntime(t, cfg), gatewayCaller())
}

// openAIFixtureRuntime configures cfg as the one instance until the test
// ends, and returns the Runtime installed for the test.
func openAIFixtureRuntime(t *testing.T, cfg *config.ProviderConfig) *Runtime {
	t.Helper()
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	runtime := InstallForTests(t)
	oldKey, oldProviders := config.Get().CredentialEncryptionKey, config.Get().Providers
	config.Update(func(s *config.Settings) {
		s.CredentialEncryptionKey = ""
		s.Providers = map[string]*config.ProviderConfig{openAIFixtureInstance: cfg}
	})
	t.Cleanup(func() {
		config.Update(func(s *config.Settings) { s.CredentialEncryptionKey, s.Providers = oldKey, oldProviders })
	})
	return runtime
}

// openAIFacade is the facade the provider factory builds for caller.
func openAIFacade(t *testing.T, runtime *Runtime, caller core.Caller) *openAICompatibleProvider {
	t.Helper()
	provider, err := runtime.instantiate(openAIFixtureInstance, config.Get().Providers[openAIFixtureInstance], caller)
	if err != nil {
		t.Fatal(err)
	}
	facade, ok := provider.(*openAICompatibleProvider)
	if !ok {
		t.Fatalf("provider=%T, want the OpenAI-compatible facade", provider)
	}
	return facade
}

// openAICatalogFixture is a facade that serves only a catalog at base,
// listed with a fixture key, as the facades the factory builds serve theirs.
func openAICatalogFixture(base string, timeout float64) Provider {
	return &openAICompatibleProvider{catalog: openAICatalog{base: base, header: openAIHeader("fixture"), timeout: timeout}}
}

// storeOpenAICatalog caches rows as the facade's caller's catalog.
func storeOpenAICatalog(provider *openAICompatibleProvider, rows ...ModelInfo) {
	provider.runtime.catalogs.store(catalogCacheKey(provider.instance, provider.caller), rows)
}

// Core sends the Chat body the transport sent, byte for byte: the payload
// buildOpenAIPayload built from withOpenAIOutputLimit's options, fields it
// did not forward dropped, HTML left unescaped. The fields the Chat handler
// never hands a provider, stream_options, parallel_tool_calls and thinking,
// are forwarded as the transport forwarded them for the Responses fallback.
func TestOpenAICompatibleChatBodyIsTheTransportsPayload(t *testing.T) {
	upstream, base := newOpenAIUpstream(t)
	provider := openAICompatibleFixture(t, &config.ProviderConfig{Type: "openai_compatible", BaseURL: base, APIKey: "fixture-key"})
	messages := []Message{{"role": "user", "content": "<b>fish & chips</b> \u2028 é"}, {"role": "assistant", "content": nil}}
	for name, kw := range map[string]Kwargs{
		"none": nil,
		"forwarded": {
			"temperature": 0.2, "top_p": json.Number("0.9"), "max_tokens": json.Number("64"), "stop": []any{"END"},
			"tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup"}}}, "tool_choice": "auto",
			"reasoning_effort": "high", "stream_options": map[string]any{"include_usage": true}, "metadata": map[string]any{"user": "fixture"},
			"parallel_tool_calls": false, "thinking": map[string]any{"type": "disabled"},
		},
		"dropped":        {"logprobs": true, "n": json.Number("2"), "output_config": map[string]any{"effort": "low"}, "temperature": nil, "_affinity_key": "fixture", "_force_api_support": false},
		"output limit":   {"_max_output_tokens": json.Number("128")},
		"explicit limit": {"_max_output_tokens": json.Number("128"), "max_tokens": json.Number("64")},
		"null limit":     {"_max_output_tokens": json.Number("128"), "max_completion_tokens": nil},
	} {
		for _, stream := range []bool{false, true} {
			if stream {
				iter, err := provider.Stream("chat-model", messages, kw)
				if err != nil {
					t.Fatalf("%s stream: %v", name, err)
				}
				_ = iter.Close()
			} else if _, err := provider.Complete("chat-model", messages, kw); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			want := copilotTransportBody(buildOpenAIPayload("chat-model", messages, stream, withOpenAIOutputLimit(kw)))
			calls := upstream.take()
			if len(calls) != 1 || calls[0].path != "/chat/completions" || calls[0].body != want ||
				calls[0].authorization != "Bearer fixture-key" || calls[0].accept != "" {
				t.Fatalf("%s stream=%v: calls=%+v\nwant=%s", name, stream, calls, want)
			}
		}
	}
}

// The facade declares what core's provider declares for its caller: Chat is
// preserved unless the configured adaptation serves the model over
// Responses, and Responses for a model whose cached row lists it, or any
// model of the openai entry. The transport labelled every Chat and
// Responses request native.
func TestOpenAICompatibleDeclaresCoresWirePreservation(t *testing.T) {
	_, base := newOpenAIUpstream(t)
	for _, fixture := range []struct {
		cfg                          *config.ProviderConfig
		model                        string
		chat, responses, anyResponse bool
	}{
		{&config.ProviderConfig{Type: "openai_compatible", BaseURL: base}, "chat-model", true, false, false},
		{&config.ProviderConfig{Type: "openai_compatible", BaseURL: base}, "both-model", true, true, false},
		{&config.ProviderConfig{Type: "openai_compatible", BaseURL: base, ForceApiSupport: true}, "responses-model", false, true, false},
		{&config.ProviderConfig{Type: "openai_compatible", BaseURL: base}, "responses-model", true, true, false},
		{&config.ProviderConfig{Type: "openai_compatible", RegistryID: "openai", BaseURL: base}, "chat-model", true, true, true},
		{&config.ProviderConfig{Type: "bedrock", BaseURL: base, ForceApiSupport: true}, "responses-model", false, true, false},
	} {
		provider := openAICompatibleFixture(t, fixture.cfg)
		storeOpenAICatalog(provider,
			ModelInfo{ID: "chat-model", SupportedSurfaces: []string{"/chat/completions"}},
			ModelInfo{ID: "both-model", SupportedSurfaces: []string{"/chat/completions", "/responses"}},
			ModelInfo{ID: "responses-model", SupportedSurfaces: []string{"/responses"}},
		)
		if got := provider.PreservesWireNativeSurface(fixture.model, core.ModelSurfaceChatCompletions); got != fixture.chat {
			t.Errorf("%+v %s: Chat preserved=%v", fixture.cfg, fixture.model, got)
		}
		if got := provider.PreservesWireNativeSurface(fixture.model, core.ModelSurfaceResponses); got != fixture.responses {
			t.Errorf("%+v %s: Responses preserved=%v", fixture.cfg, fixture.model, got)
		}
		if got := provider.PreservesWireNativeSurface("uncatalogued-model", core.ModelSurfaceResponses); got != fixture.anyResponse {
			t.Errorf("%+v: Responses of an uncatalogued model preserved=%v", fixture.cfg, got)
		}
		if provider.PreservesWireNativeSurface(fixture.model, core.ModelSurfaceMessages) {
			t.Errorf("%+v: Messages preserved", fixture.cfg)
		}
	}
}

// The endpoints the gateway proxies to an OpenAI-compatible or Bedrock
// instance go to its base URL with the key the factory resolved, as the
// transport's auth prepared them, and a Bedrock instance without a base URL
// has its region's endpoint.
func TestOpenAICompatibleProxyTargetIsTheFactorysKey(t *testing.T) {
	for _, fixture := range []struct {
		cfg                 *config.ProviderConfig
		base, authorization string
	}{
		{&config.ProviderConfig{Type: "openai_compatible", BaseURL: "https://api.example.test/v1/", APIKey: "Bearer fixture-key"}, "https://api.example.test/v1", "Bearer fixture-key"},
		{&config.ProviderConfig{Type: "litellm", BaseURL: "http://127.0.0.1:4000", APIKey: "free"}, "http://127.0.0.1:4000", ""},
		{&config.ProviderConfig{Type: "bedrock", Region: "eu-central-1", APIKey: "fixture-key"}, "https://bedrock-runtime.eu-central-1.amazonaws.com/v1", "Bearer fixture-key"},
		{&config.ProviderConfig{Type: "bedrock", BaseURL: " https://bedrock-mantle.example.test/v1/ "}, "https://bedrock-mantle.example.test/v1", ""},
	} {
		provider := openAICompatibleFixture(t, fixture.cfg)
		base, headers, ok := provider.runtime.ProviderHTTPTarget(openAIFixtureInstance, gatewayCaller())
		if !ok || base != fixture.base || headers.Get("Authorization") != fixture.authorization || headers.Get("Content-Type") != "application/json" {
			t.Fatalf("%+v: target=%q %v ok=%v", fixture.cfg, base, headers, ok)
		}
	}
}

// A Bedrock instance whose region is no region name builds no facade, so its
// key never reaches a host the region did not name, where the transport
// templated any region into its endpoint. Nor does a base URL that is not
// absolute, where the transport sent requests that could not reach it.
func TestOpenAIFacadesRefuseAnEndpointCoreCannotServe(t *testing.T) {
	for _, cfg := range []*config.ProviderConfig{
		{Type: "bedrock", Region: "attacker.example.test/v1#", APIKey: "fixture-key"},
		{Type: "openai_compatible", BaseURL: "api.example.test/v1", APIKey: "fixture-key"},
	} {
		runtime := openAIFixtureRuntime(t, cfg)
		if _, err := runtime.instantiate(openAIFixtureInstance, cfg, gatewayCaller()); !IsConfig(err) {
			t.Fatalf("%+v: err=%v, want a configuration error", cfg, err)
		}
	}
}

// Adaptation serves Chat as the transport's plan did: a Responses-only model
// over Responses, the answer converted back and marked, a sampling
// parameter the model refuses stripped and the request sent once more, and
// a reasoning model's max_tokens as max_completion_tokens. A request's own
// force_api_support turns it on or off.
func TestOpenAICompatibleAdaptsChatAsTheTransportDid(t *testing.T) {
	upstream, base := newOpenAIUpstream(t)
	upstream.setAnswer(func(w http.ResponseWriter, call openAICall) {
		if call.path == "/responses" && strings.Contains(call.body, `"temperature"`) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"Unsupported parameter: temperature"}}`)
			return
		}
		answerOpenAI(w, call)
	})
	messages := []Message{{"role": "user", "content": "hi"}}
	rows := []ModelInfo{
		{ID: "responses-model", SupportedSurfaces: []string{"/responses"}},
		{ID: "reasoning-model", SupportedSurfaces: []string{"/chat/completions"}, Capabilities: map[string]any{"reasoning_effort": []string{"low"}}},
	}
	forced := openAICompatibleFixture(t, &config.ProviderConfig{Type: "openai_compatible", BaseURL: base, ForceApiSupport: true})
	storeOpenAICatalog(forced, rows...)

	kw := Kwargs{"max_tokens": float64(32), "temperature": 0.2}
	response, err := forced.Complete("responses-model", messages, kw)
	calls := upstream.take()
	stripped := chatToResponsesWithReport("responses-model", messages, Kwargs{"max_tokens": float64(32)}).Value
	if err != nil || len(calls) != 2 || calls[1].path != "/responses" || calls[1].body != copilotTransportBody(stripped) ||
		fmt.Sprint(response["forced_support"]) != "map[req_api:chat resp_api:responses]" {
		t.Fatalf("over Responses: response=%v err=%v calls=%+v", response, err, calls)
	}
	stream, err := forced.Stream("responses-model", messages, nil)
	chunks, err := drainZenStream(t, stream, err)
	if calls = upstream.take(); err != nil || len(chunks) == 0 || len(calls) != 1 || calls[0].path != "/responses" || strings.Contains(calls[0].body, `"stream":true`) {
		t.Fatalf("stream over Responses: chunks=%q err=%v calls=%+v", chunks, err, calls)
	}
	if _, err := forced.Complete("responses-model", messages, Kwargs{"_force_api_support": false}); err != nil {
		t.Fatal(err)
	}
	if calls = upstream.take(); len(calls) != 1 || calls[0].path != "/chat/completions" {
		t.Fatalf("a request that turns adaptation off: calls=%+v", calls)
	}
	kw = Kwargs{"max_tokens": float64(64)}
	if _, err := forced.Complete("reasoning-model", messages, kw); err != nil {
		t.Fatal(err)
	}
	want := copilotTransportBody(buildOpenAIPayload("reasoning-model", messages, false, withRenamedMaxTokens(kw)))
	if calls = upstream.take(); len(calls) != 1 || calls[0].body != want {
		t.Fatalf("reasoning model: calls=%+v, want %s", calls, want)
	}

	plain := openAICompatibleFixture(t, &config.ProviderConfig{Type: "openai_compatible", BaseURL: base})
	storeOpenAICatalog(plain, rows...)
	if _, err := plain.Complete("reasoning-model", messages, kw); err != nil {
		t.Fatal(err)
	}
	if calls = upstream.take(); len(calls) != 1 || !strings.Contains(calls[0].body, `"max_tokens":64`) {
		t.Fatalf("without adaptation: calls=%+v", calls)
	}
	response, err = plain.Complete("responses-model", messages, Kwargs{"_force_api_support": true})
	if calls = upstream.take(); err != nil || len(calls) != 1 || calls[0].path != "/responses" || response["forced_support"] == nil {
		t.Fatalf("a request that turns adaptation on: response=%v err=%v calls=%+v", response, err, calls)
	}
}
