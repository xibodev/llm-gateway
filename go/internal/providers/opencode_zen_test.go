package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"

	core "github.com/xibodev/llmgw-core"
	corezen "github.com/xibodev/llmgw-core/providers/zen"
)

const zenChatStreamFixture = "data: {\"id\":\"chat_1\",\"object\":\"chat.completion.chunk\",\"model\":\"chat\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"

// zenFixture configures instance "zen" as OpenCode Zen at base with the
// configured key, which "" leaves anonymous, in a new installed Runtime, and
// returns the facade the provider factory builds for the gateway caller.
func zenFixture(t *testing.T, base, key string) *zenProvider {
	t.Helper()
	return zenFixtureConfig(t, &config.ProviderConfig{Type: "openai_compatible", RegistryID: "opencode_zen", BaseURL: base, APIKey: key})
}

func zenFixtureConfig(t *testing.T, cfg *config.ProviderConfig) *zenProvider {
	t.Helper()
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	runtime := InstallForTests(t)
	oldKey, oldProviders := config.Get().CredentialEncryptionKey, config.Get().Providers
	config.Update(func(s *config.Settings) {
		s.CredentialEncryptionKey = ""
		s.Providers = map[string]*config.ProviderConfig{"zen": cfg}
	})
	t.Cleanup(func() {
		config.Update(func(s *config.Settings) { s.CredentialEncryptionKey, s.Providers = oldKey, oldProviders })
	})
	return zenFacade(t, runtime, gatewayCaller())
}

func zenFacade(t *testing.T, runtime *Runtime, caller core.Caller) *zenProvider {
	t.Helper()
	provider, err := runtime.instantiate(config.Get(), "zen", config.Get().Providers["zen"], caller)
	if err != nil {
		t.Fatal(err)
	}
	zen, ok := provider.(*zenProvider)
	if !ok {
		t.Fatalf("provider=%T, want the Zen facade", provider)
	}
	return zen
}

// zenServer serves handler until the test ends and returns its URL.
func zenServer(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server.URL
}

// zenBody decodes a request body, which fails the test unless it is JSON.
func zenBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		t.Error(err)
	}
	return payload
}

// Proxied endpoints authenticate as the transport authenticated them: the
// public bearer without a key, the key with one, and no invocation identity.
func TestZenHTTPTargetMatchesCLIContract(t *testing.T) {
	for key, want := range map[string]string{"": "Bearer public", "free": "Bearer public", "secret": "Bearer secret"} {
		zen := zenFixture(t, "https://opencode.ai/zen/v1/", key)
		base, headers, ok := zen.runtime.ProviderHTTPTarget("zen", gatewayCaller())
		if !ok || base != "https://opencode.ai/zen/v1" || headers.Get("Authorization") != want {
			t.Fatalf("key %q: ok=%v base=%q authorization=%q", key, ok, base, headers.Get("Authorization"))
		}
		for _, name := range []string{"x-opencode-project", "x-opencode-session", "x-opencode-request", "x-opencode-client", "User-Agent", "x-session-id"} {
			if headers.Get(name) != "" {
				t.Fatalf("key %q: proxied header %s=%q", key, name, headers.Get(name))
			}
		}
	}
	headers := openAIHeader("")
	if headers.Get("Authorization") != "" || headers.Get("x-opencode-session") != "" {
		t.Fatalf("ordinary provider gained OpenCode headers: %v", headers)
	}
}

func TestZenInvocationHeadersPreserveCallerIdentity(t *testing.T) {
	base := zenServer(t, func(w http.ResponseWriter, r *http.Request) {
		for key, want := range map[string]string{"x-opencode-project": "project supplied", "x-opencode-session": "session supplied", "x-opencode-request": "request supplied", "x-opencode-client": "desktop supplied", "User-Agent": corezen.AnonymousUserAgent, "Authorization": "Bearer public"} {
			if got := r.Header.Get(key); got != want {
				t.Errorf("%s=%q want %q", key, got, want)
			}
		}
		_, _ = fmt.Fprint(w, zenChatStreamFixture)
	})
	provider := zenFixture(t, base, "")
	headers := http.Header{}
	headers.Set("x-opencode-project", "project supplied")
	headers.Set("x-opencode-session", "session supplied")
	headers.Set("x-opencode-request", "request supplied")
	headers.Set("x-opencode-client", "desktop supplied")
	headers.Set("User-Agent", "caller-agent/1")
	identity, err := corezen.NewInvocationIdentity(headers, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := corezen.WithInvocationIdentity(context.Background(), identity)
	if _, err = provider.CompleteContext(ctx, "chat", []Message{{"role": "user", "content": "hi"}}, nil); err != nil {
		t.Fatal(err)
	}
}

// A keyed Chat request for a Responses-only model goes over Responses, and a
// 400 naming a sampling parameter is retried once without it, as the same
// logical invocation.
func TestZenUnsupportedParameterRetryKeepsLogicalIdentity(t *testing.T) {
	var mu sync.Mutex
	var seen []http.Header
	var bodies []map[string]any
	base := zenServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen, bodies = append(seen, r.Header.Clone()), append(bodies, zenBody(t, r))
		attempt := len(seen)
		mu.Unlock()
		if attempt == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, `{"error":{"message":"Unsupported parameter: temperature"}}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"id":"resp_1","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`)
	})
	provider := zenFixture(t, base, "secret")
	provider.runtime.catalogs.store("zen", []ModelInfo{{ID: "model", SupportedSurfaces: []string{"/responses"}}})
	identity := corezen.InvocationIdentity{Project: "project", Session: "session", Request: "request", Client: "desktop", UserAgent: corezen.AnonymousUserAgent}
	ctx := corezen.WithInvocationIdentity(context.Background(), identity)
	if _, err := provider.CompleteContext(ctx, "model", []Message{{"role": "user", "content": "hi"}}, Kwargs{"temperature": 0.2}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || bodies[0]["temperature"] != 0.2 || bodies[1]["temperature"] != nil {
		t.Fatalf("attempts=%d bodies=%+v", len(seen), bodies)
	}
	if got := seen[0].Get("User-Agent"); got != corezen.AnonymousUserAgent {
		t.Fatalf("User-Agent=%q, want %q", got, corezen.AnonymousUserAgent)
	}
	for _, key := range []string{"x-opencode-project", "x-opencode-session", "x-opencode-request", "x-opencode-client", "User-Agent"} {
		if seen[0].Get(key) == "" || seen[0].Get(key) != seen[1].Get(key) {
			t.Fatalf("%s changed: %q -> %q", key, seen[0].Get(key), seen[1].Get(key))
		}
	}
}

func TestZenProviderRetryKeepsLogicalIdentity(t *testing.T) {
	var mu sync.Mutex
	var seen []http.Header
	base := zenServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Clone())
		attempt := len(seen)
		mu.Unlock()
		if attempt == 1 {
			http.Error(w, "retry", http.StatusInternalServerError)
			return
		}
		_, _ = fmt.Fprint(w, zenChatStreamFixture)
	})
	provider := &ResilientProvider{inner: zenFixture(t, base, ""), name: "zen", policy: config.ProviderPolicy{RetryMaxAttempts: 2}}
	if _, err := provider.CompleteContext(context.Background(), "chat", []Message{{"role": "user", "content": "hi"}}, nil); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 {
		t.Fatalf("attempts=%d", len(seen))
	}
	for _, key := range []string{"x-opencode-project", "x-opencode-session", "x-opencode-request", "x-opencode-client", "User-Agent"} {
		if seen[0].Get(key) == "" || seen[0].Get(key) != seen[1].Get(key) {
			t.Fatalf("%s changed: %q -> %q", key, seen[0].Get(key), seen[1].Get(key))
		}
	}
}

func TestZenConcurrentTitleInvocationsDoNotShareIdentity(t *testing.T) {
	requests := make(chan string, 2)
	base := zenServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Header.Get("x-opencode-request")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"chat_1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"title\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	})
	provider := zenFixture(t, base, "")
	var wg sync.WaitGroup
	for _, requestID := range []string{"title-one", "title-two"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			identity := corezen.InvocationIdentity{Project: "project", Session: "session", Request: id, Client: "cli", UserAgent: corezen.AnonymousUserAgent}
			ctx := corezen.WithInvocationIdentity(context.Background(), identity)
			_, _ = provider.CompleteContext(ctx, "chat", []Message{{"role": "system", "content": "You are a title generator."}, {"role": "user", "content": "hello"}}, nil)
		}(requestID)
	}
	wg.Wait()
	close(requests)
	got := []string{}
	for id := range requests {
		got = append(got, id)
	}
	slices.Sort(got)
	if !slices.Equal(got, []string{"title-one", "title-two"}) {
		t.Fatalf("requests=%v", got)
	}
}

func TestAnonymousAPIKeyRecognizesSentinels(t *testing.T) {
	for _, value := range []string{"", "free", "none", "public", "Bearer public"} {
		if !AnonymousAPIKey(value) {
			t.Fatalf("%q should be anonymous", value)
		}
	}
	for _, value := range []string{"secret", "Bearer secret"} {
		if AnonymousAPIKey(value) {
			t.Fatalf("%q should be credentialed", value)
		}
	}
}

func TestAnonymousZenSurfacesAreNotWireNative(t *testing.T) {
	provider := zenFixture(t, "https://opencode.ai/zen/v1", "public")
	for _, surface := range []core.ModelSurface{core.ModelSurfaceChatCompletions, core.ModelSurfaceResponses} {
		if PreservesWireNativeSurface(provider, "muse-spark-fixture", surface) {
			t.Fatalf("anonymous Zen surface %q was certified as wire-native", surface)
		}
	}
}

func TestZenModeClassificationSeparatesAccessAndRequestIntent(t *testing.T) {
	if got := zenAccessMode(true, true); got != zenAccessAnonymous {
		t.Fatalf("anonymous access mode=%q", got)
	}
	if got := zenAccessMode(true, false); got != zenAccessKeyed {
		t.Fatalf("keyed access mode=%q", got)
	}
	if got := zenAccessMode(false, true); got != "not_zen" {
		t.Fatalf("non-Zen access mode=%q", got)
	}

	ordinary := []Message{{"role": "user", "content": "Explain this failure"}}
	if got := zenChatRequestMode(ordinary); got != zenRequestOrdinary {
		t.Fatalf("ordinary first turn mode=%q", got)
	}
	title := []Message{
		{"role": "system", "content": "You are a title generator. Output one title."},
		{"role": "user", "content": "Explain this failure"},
	}
	if got := zenChatRequestMode(title); got != zenRequestTitle {
		t.Fatalf("explicit title turn mode=%q", got)
	}
	if got := zenResponsesRequestMode(map[string]any{"input": "Explain this failure"}); got != zenRequestOrdinary {
		t.Fatalf("ordinary Responses mode=%q", got)
	}
	if got := zenResponsesRequestMode(map[string]any{"instructions": "You are a title generator"}); got != zenRequestTitle {
		t.Fatalf("title Responses mode=%q", got)
	}
}

func TestAnonymousZenCatalogUsesActiveZeroCostMetadata(t *testing.T) {
	upstream := zenServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer public" || r.Header.Get("x-opencode-session") == "" {
			t.Errorf("missing anonymous OpenCode headers: %v", r.Header)
		}
		_, _ = fmt.Fprint(w, `{"data":[
			{"id":"big-pickle"},
			{"id":"muse-spark-1.3-contributor-free"},
			{"id":"beta-free"},
			{"id":"paid-model"},
			{"id":"missing-output"}
		]}`)
	})
	metadata := zenServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"opencode":{"npm":"@ai-sdk/openai-compatible","models":{
			"big-pickle":{"id":"big-pickle","name":"Big Pickle","status":"active","cost":{"input":0,"output":0},"family":"pickle","reasoning":true,"tool_call":true,"limit":{"context":200000,"output":32000}},
			"muse-spark-1.3-contributor-free":{"id":"muse-spark-1.3-contributor-free","name":"Muse Spark 1.3 Free","status":"active","provider":{"npm":"@ai-sdk/openai"},"cost":{"input":0,"output":0},"structured_output":true,"tool_call":true,"limit":{"context":1048576,"output":131072}},
			"beta-free":{"id":"beta-free","name":"Beta","status":"beta","cost":{"input":0,"output":0}},
			"deepseek-v4-flash-free":{"id":"deepseek-v4-flash-free","name":"Deprecated","status":"deprecated","cost":{"input":0,"output":0}},
			"alpha-free":{"id":"alpha-free","name":"Experimental","status":"alpha","cost":{"input":0,"output":0}},
			"paid-model":{"id":"paid-model","name":"Paid","status":"active","cost":{"input":1,"output":1}},
			"missing-output":{"id":"missing-output","name":"Incomplete cost","status":"active","cost":{"input":0}},
			"metadata-only":{"id":"metadata-only","name":"Not upstream","status":"active","cost":{"input":0,"output":0}}
		}}}`)
	})
	provider := zenFixture(t, upstream, "")
	provider.metadataURL = metadata
	rows, observation, err := provider.ListModelsWithError()
	if err != nil || observation != nil || len(rows) != 3 {
		t.Fatalf("rows=%+v observation=%+v err=%v", rows, observation, err)
	}
	if got := []string{rows[0].ID, rows[1].ID, rows[2].ID}; !slices.Equal(got, []string{"beta-free", "big-pickle", "muse-spark-1.3-contributor-free"}) {
		t.Fatalf("rows=%+v", rows)
	}
	if !rows[1].Free || !slices.Equal(rows[1].SupportedSurfaces, []string{"/chat/completions"}) {
		t.Fatalf("chat row=%+v", rows[1])
	}
	if !rows[2].Free || !slices.Equal(rows[2].SupportedSurfaces, []string{"/responses"}) ||
		rows[2].Label != "Muse Spark 1.3 Free" {
		t.Fatalf("responses row=%+v", rows[2])
	}
	if rows[2].TypedCapabilities == nil ||
		rows[2].TypedCapabilities.Provenance.Source != core.ModelCapabilitySourceModelsDev ||
		rows[2].TypedCapabilities.Surfaces.Responses != core.SupportSupported {
		t.Fatalf("shared capability evidence=%+v", rows[2].TypedCapabilities)
	}
}

func TestZenChatUsesPersistedCatalogSurface(t *testing.T) {
	const responsesModel = "constellation-free"
	var path string
	base := zenServer(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		payload := zenBody(t, r)
		if r.URL.Path == "/responses" {
			if payload["model"] != responsesModel || payload["messages"] != nil || payload["input"] == nil {
				t.Errorf("payload=%+v", payload)
			}
			_, _ = fmt.Fprint(w, `{"id":"resp_1","object":"response","status":"completed","model":"constellation-free","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}]}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"id":"chat_1","object":"chat.completion"}`)
	})
	provider := zenFixture(t, base, "secret")
	provider.runtime.catalogs.store("zen", []ModelInfo{
		{ID: responsesModel, SupportedSurfaces: []string{"/responses"}},
		{ID: "big-pickle", SupportedSurfaces: []string{"/chat/completions"}},
	})
	response, err := provider.Complete(responsesModel, []Message{{"role": "user", "content": "hi"}}, Kwargs{"max_tokens": 64})
	if err != nil {
		t.Fatal(err)
	}
	if path != "/responses" {
		t.Fatalf("path=%q", path)
	}
	choices, _ := response["choices"].([]any)
	message, _ := choices[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "hi" || response["forced_support"] == nil {
		t.Fatalf("response=%+v, want the answer marked as served over Responses", response)
	}

	path = ""
	_, err = provider.Complete("big-pickle", []Message{{"role": "user", "content": "hi"}}, nil)
	if !InvocationCircuitFailure(err) || InvocationRetryable(err) || path != "/chat/completions" {
		t.Fatalf("ordinary Zen path=%q err=%v, want a completion without choices refused", path, err)
	}
}

func TestKeyedZenNativeSurfaceUsesPersistedCatalog(t *testing.T) {
	provider := zenFixture(t, "https://opencode.ai/zen/v1", "secret")
	provider.runtime.catalogs.store("zen", []ModelInfo{
		{ID: "constellation-free", SupportedSurfaces: []string{"/responses"}},
		{ID: "muse-spark-chat", SupportedSurfaces: []string{"/chat/completions"}},
	})
	if !provider.overResponses("constellation-free") || provider.overResponses("muse-spark-chat") {
		t.Fatal("keyed Zen routing did not follow persisted surfaces")
	}
	if !provider.PreservesWireNativeSurface("constellation-free", core.ModelSurfaceResponses) ||
		provider.PreservesWireNativeSurface("constellation-free", core.ModelSurfaceChatCompletions) ||
		!provider.PreservesWireNativeSurface("muse-spark-chat", core.ModelSurfaceChatCompletions) {
		t.Fatal("keyed Zen wire-native declaration did not follow persisted surfaces")
	}
}

func TestAnonymousZenCompletePreservesOrdinaryAndExplicitTitlePrompts(t *testing.T) {
	var captured map[string]any
	base := zenServer(t, func(w http.ResponseWriter, r *http.Request) {
		captured = zenBody(t, r)
		if stream, _ := captured["stream"].(bool); stream {
			_, _ = fmt.Fprint(w, "data: {\"id\":\"chat_1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\ndata: [DONE]\n\n")
		} else {
			_, _ = fmt.Fprint(w, `{"id":"chat_1","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"hello"}}]}`)
		}
	})
	messagesOf := func() []any { messages, _ := captured["messages"].([]any); return messages }

	// Ordinary first turns retain their user content under the proven v0.6.6
	// admission preamble, and gain no agent tools.
	anonymous := zenFixture(t, base, "")
	if _, err := anonymous.Complete("big-pickle", []Message{{"role": "user", "content": "test"}}, nil); err != nil {
		t.Fatal(err)
	}
	if messages := messagesOf(); len(messages) != 2 || messages[0].(map[string]any)["content"] != corezen.AnonymousAssistantPreamble || messages[1].(map[string]any)["content"] != "test" {
		t.Fatalf("ordinary prompt changed: %+v", captured)
	}
	if captured["tools"] != nil || captured["tool_choice"] != nil {
		t.Fatalf("ordinary agent admission shape: %+v", captured)
	}
	title := []Message{{"role": "system", "content": "You are a title generator. Output one title."}, {"role": "user", "content": "test"}}
	if _, err := anonymous.Complete("big-pickle", title, nil); err != nil {
		t.Fatal(err)
	}
	if messages := messagesOf(); len(messages) != 2 || messages[0].(map[string]any)["content"] != title[0]["content"] || captured["tools"] != nil || captured["tool_choice"] != nil {
		t.Fatalf("explicit title prompt changed: %+v", captured)
	}

	// Keyed Zen must not adapt messages.
	if _, err := zenFixture(t, base, "secret").Complete("big-pickle", []Message{{"role": "user", "content": "test"}}, nil); err != nil {
		t.Fatal(err)
	}
	if messages := messagesOf(); len(messages) != 1 || captured["stream"] != false {
		t.Fatalf("keyed request was adapted: %+v", captured)
	}

	// A non-Zen anonymous provider must not adapt messages.
	nonZen := openAICompatibleFixture(t, &config.ProviderConfig{Type: "openai_compatible", RegistryID: "kilo_code", BaseURL: base})
	if _, err := nonZen.Complete("kilo-free", []Message{{"role": "user", "content": "test"}}, nil); err != nil {
		t.Fatal(err)
	}
	if messages := messagesOf(); len(messages) != 1 {
		t.Fatalf("non-Zen request was adapted: %+v", captured)
	}
}

const zenCompletedFixture = "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}]}}\n\n"

func TestAnonymousZenResponsesPreservesOrdinaryAndExplicitTitleInstructions(t *testing.T) {
	var captured map[string]any
	base := zenServer(t, func(w http.ResponseWriter, r *http.Request) {
		captured = zenBody(t, r)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, zenCompletedFixture)
	})
	provider := zenFixture(t, base, "")
	if _, _, err := provider.CompleteResponses("muse-spark-fixture", map[string]any{"input": "Explain this failure", "instructions": "Be concise"}); err != nil {
		t.Fatal(err)
	}
	if captured["instructions"] != corezen.AnonymousAssistantPreamble+"\n\nBe concise" || captured["stream"] != true {
		t.Fatalf("ordinary Responses instructions changed: %+v", captured)
	}
	if captured["tools"] != nil || captured["tool_choice"] != nil {
		t.Fatalf("ordinary Responses admission shape: %+v", captured)
	}
	if _, _, err := provider.CompleteResponses("muse-spark-fixture", map[string]any{"input": "Explain this failure", "instructions": "You are a title generator"}); err != nil {
		t.Fatal(err)
	}
	if captured["instructions"] != "You are a title generator" || captured["tools"] != nil || captured["tool_choice"] != nil {
		t.Fatalf("explicit title request changed: %+v", captured)
	}
}

func TestAnonymousZenRecognizedByBaseURL(t *testing.T) {
	// A provider configured as "zen" with base_url "https://opencode.ai/zen/v1"
	// and NO explicit registry_id must still be recognized as Zen.
	provider := zenFixtureConfig(t, &config.ProviderConfig{Type: "openai_compatible", BaseURL: "https://opencode.ai/zen/v1"})
	if !provider.anonymous {
		t.Fatal("expected anonymous Zen access based on base_url")
	}
	if !provider.overResponses("muse-spark-1.2-contributor-free") {
		t.Fatal("cold-cache Muse fallback must preserve the v0.6.6 Responses surface")
	}
}

func TestAnonymousZenToolsAndMultiTurn(t *testing.T) {
	var captured map[string]any
	base := zenServer(t, func(w http.ResponseWriter, r *http.Request) {
		captured = zenBody(t, r)
		_, _ = fmt.Fprint(w, zenChatStreamFixture)
	})
	provider := zenFixture(t, base, "")
	messages := []Message{
		{"role": "system", "content": "You are Pi, a personal AI coding agent."},
		{"role": "user", "content": "What is the weather?"},
	}
	customTool := map[string]any{
		"type": "function",
		"function": map[string]any{
			"name": "get_weather", "description": "get weather", "parameters": map[string]any{"type": "object"},
		},
	}
	stream := func(kw Kwargs) {
		t.Helper()
		iterator, err := provider.Stream("big-pickle", messages, kw)
		if err != nil {
			t.Fatal(err)
		}
		_ = iterator.Close()
	}

	stream(Kwargs{"tools": []any{customTool}})
	// The system prompt is preserved, not replaced with the title preamble.
	sent, _ := captured["messages"].([]any)
	system, _ := sent[0].(map[string]any)["content"].(string)
	if !strings.Contains(system, "You are Pi") || strings.Contains(system, "You are a title generator") {
		t.Fatalf("system prompt changed: %q", system)
	}

	// Caller tools and tool choice are preserved while compatibility tools return.
	stream(Kwargs{"tools": []any{customTool}, "tool_choice": "required"})
	if tools, ok := captured["tools"].([]any); !ok || len(tools) != 3 || captured["tool_choice"] != "required" {
		t.Fatalf("caller tool contract changed: %+v", captured)
	}
}
