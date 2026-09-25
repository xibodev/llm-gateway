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

	core "github.com/xibodev/llmgw-core"
	corezen "github.com/xibodev/llmgw-core/providers/zen"
)

func TestAnonymousOpenCodeHeadersMatchCLIContract(t *testing.T) {
	auth, err := newBearerAuth("https://opencode.ai/zen/v1", "", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	base, first, err := auth.Prepare()
	if err != nil {
		t.Fatal(err)
	}
	if base != "https://opencode.ai/zen/v1" || first.Get("Authorization") != "Bearer public" {
		t.Fatalf("base=%q authorization=%q", base, first.Get("Authorization"))
	}
	for _, key := range []string{"x-opencode-project", "x-opencode-session", "x-opencode-request", "x-opencode-client", "User-Agent"} {
		if first.Get(key) != "" {
			t.Fatalf("auth prepared invocation header %s=%q", key, first.Get(key))
		}
	}
	if first.Get("x-session-id") != "" {
		t.Fatalf("legacy x-session-id leaked: %q", first.Get("x-session-id"))
	}

	keyed, err := newBearerAuth("https://opencode.ai/zen/v1", "secret", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	_, headers, err := keyed.Prepare()
	if err != nil || headers.Get("Authorization") != "Bearer secret" {
		t.Fatalf("configured key changed: headers=%v err=%v", headers, err)
	}
	if headers.Get("x-opencode-client") != "" || headers.Get("User-Agent") != "" {
		t.Fatalf("keyed auth prepared invocation headers=%v", headers)
	}

	ordinary, err := newBearerAuth("https://example.com/v1", "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	_, headers, err = ordinary.Prepare()
	if err != nil || headers.Get("Authorization") != "" || headers.Get("x-opencode-session") != "" {
		t.Fatalf("ordinary provider gained OpenCode headers: %v, err=%v", headers, err)
	}
}

func TestZenInvocationHeadersPreserveCallerIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for key, want := range map[string]string{"x-opencode-project": "project supplied", "x-opencode-session": "session supplied", "x-opencode-request": "request supplied", "x-opencode-client": "desktop supplied", "User-Agent": corezen.AnonymousUserAgent} {
			if got := r.Header.Get(key); got != want {
				t.Errorf("%s=%q want %q", key, got, want)
			}
		}
		_, _ = fmt.Fprint(w, "data: {\"id\":\"chat_1\",\"object\":\"chat.completion.chunk\",\"model\":\"chat\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	auth, _ := newBearerAuth(server.URL, "", nil, true)
	provider := OpenAIProvider{auth: auth, Timeout: 2, providerID: "zen", registryID: "opencode_zen", anonymous: true}
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

func TestZenUnsupportedParameterRetryKeepsLogicalIdentity(t *testing.T) {
	var seen []http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Clone())
		if len(seen) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, `{"error":{"message":"Unsupported parameter: temperature"}}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"id":"resp_1","output":[]}`)
	}))
	defer server.Close()
	auth, _ := newBearerAuth(server.URL, "secret", nil, true)
	provider := OpenAIProvider{auth: auth, Timeout: 2, providerID: "zen", registryID: "opencode_zen"}
	identity := corezen.InvocationIdentity{Project: "project", Session: "session", Request: "request", Client: "desktop", UserAgent: corezen.AnonymousUserAgent}
	ctx := corezen.WithInvocationIdentity(context.Background(), identity)
	_, _, err := provider.callResponsesPayloadContext(ctx, map[string]any{"model": "model", "input": "hi", "temperature": 0.2}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 {
		t.Fatalf("attempts=%d", len(seen))
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
	var seen []http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Clone())
		if len(seen) == 1 {
			http.Error(w, "retry", http.StatusInternalServerError)
			return
		}
		_, _ = fmt.Fprint(w, "data: {\"id\":\"chat_1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	auth, _ := newBearerAuth(server.URL, "", nil, true)
	inner := OpenAIProvider{auth: auth, Timeout: 2, providerID: "zen", registryID: "opencode_zen", anonymous: true}
	provider := &ResilientProvider{inner: inner, name: "zen", policy: config.ProviderPolicy{RetryMaxAttempts: 2}}
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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Header.Get("x-opencode-request")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"chat_1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"title\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	auth, _ := newBearerAuth(server.URL, "", nil, true)
	provider := OpenAIProvider{auth: auth, Timeout: 2, providerID: "zen", registryID: "opencode_zen", anonymous: true}
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
	provider := OpenAIProvider{registryID: "opencode_zen", anonymous: true}
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
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer public" || r.Header.Get("x-opencode-session") == "" {
			t.Fatalf("missing anonymous OpenCode headers: %v", r.Header)
		}
		_, _ = fmt.Fprint(w, `{"data":[
			{"id":"big-pickle"},
			{"id":"muse-spark-1.3-contributor-free"},
			{"id":"beta-free"},
			{"id":"paid-model"},
			{"id":"missing-output"}
		]}`)
	}))
	defer upstream.Close()

	metadata := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
	}))
	defer metadata.Close()

	auth, err := newBearerAuth(upstream.URL, "", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	provider := OpenAIProvider{
		auth: auth, Timeout: 2, providerID: "zen", registryID: "opencode_zen",
		anonymous: true, metadataURL: metadata.URL,
	}
	rows, _, err := provider.ListModelsWithError()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows=%+v", rows)
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
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	resetCatalogForTest(t)
	const responsesModel = "constellation-free"
	Current().catalogs.store("zen", []ModelInfo{
		{ID: responsesModel, SupportedSurfaces: []string{"/responses"}},
		{ID: "big-pickle", SupportedSurfaces: []string{"/chat/completions"}},
	})
	var path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if r.URL.Path == "/responses" {
			if payload["model"] != responsesModel || payload["messages"] != nil || payload["input"] == nil {
				t.Fatalf("payload=%+v", payload)
			}
			_, _ = fmt.Fprint(w, `{"id":"resp_1","object":"response","status":"completed","model":"constellation-free","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}]}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"id":"chat_1","object":"chat.completion"}`)
	}))
	defer server.Close()

	provider := OpenAIProvider{
		auth: bearerAuth{base: server.URL}, Timeout: 2,
		providerID: "zen", registryID: "opencode_zen",
	}
	response, err := provider.Complete(
		responsesModel,
		[]Message{{"role": "user", "content": "hi"}},
		Kwargs{"max_tokens": 64},
	)
	if err != nil {
		t.Fatal(err)
	}
	if path != "/responses" {
		t.Fatalf("path=%q", path)
	}
	choices, _ := response["choices"].([]any)
	message, _ := choices[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "hi" {
		t.Fatalf("response=%+v", response)
	}

	path = ""
	_, err = provider.Complete("big-pickle", []Message{{"role": "user", "content": "hi"}}, nil)
	if err == nil || !strings.Contains(err.Error(), "invalid chat response payload") || path != "/chat/completions" {
		t.Fatalf("ordinary Zen path=%q err=%v", path, err)
	}
}

func TestKeyedZenNativeSurfaceUsesPersistedCatalog(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	resetCatalogForTest(t)
	Current().catalogs.store("zen", []ModelInfo{
		{ID: "constellation-free", SupportedSurfaces: []string{"/responses"}},
		{ID: "muse-spark-chat", SupportedSurfaces: []string{"/chat/completions"}},
	})
	provider := OpenAIProvider{
		auth: bearerAuth{base: "https://opencode.ai/zen/v1"}, providerID: "zen",
		registryID: "opencode_zen", anonymous: false,
	}
	if !provider.zenUsesResponses("constellation-free") || provider.zenUsesResponses("muse-spark-chat") {
		t.Fatal("keyed Zen routing did not follow persisted surfaces")
	}
	if !provider.PreservesWireNativeSurface("constellation-free", core.ModelSurfaceResponses) ||
		provider.PreservesWireNativeSurface("constellation-free", core.ModelSurfaceChatCompletions) ||
		!provider.PreservesWireNativeSurface("muse-spark-chat", core.ModelSurfaceChatCompletions) {
		t.Fatal("keyed Zen wire-native declaration did not follow persisted surfaces")
	}
}

func TestAnonymousZenCompletePreservesOrdinaryAndExplicitTitlePrompts(t *testing.T) {
	var capturedMessages []any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if msgs, ok := payload["messages"].([]any); ok {
			capturedMessages = msgs
		}
		if stream, _ := payload["stream"].(bool); stream {
			_, _ = fmt.Fprint(w, "data: {\"id\":\"chat_1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\ndata: [DONE]\n\n")
		} else {
			_, _ = fmt.Fprint(w, `{"id":"chat_1","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"hello"}}]}`)
		}
	}))
	defer server.Close()

	// Ordinary first turns retain their user content under the proven v0.6.6
	// admission preamble.
	anonZen := OpenAIProvider{
		auth:       bearerAuth{base: server.URL},
		Timeout:    2,
		providerID: "zen",
		registryID: "opencode_zen",
		anonymous:  true,
	}
	_, err := anonZen.Complete("big-pickle", []Message{{"role": "user", "content": "test"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(capturedMessages) != 2 || capturedMessages[0].(map[string]any)["content"] != corezen.AnonymousAssistantPreamble || capturedMessages[1].(map[string]any)["content"] != "test" {
		t.Fatalf("ordinary prompt changed: %+v", capturedMessages)
	}
	adaptedMessages, adaptedKw := adaptAnonymousZenChat([]Message{{"role": "user", "content": "test"}}, nil)
	if len(adaptedMessages) != 2 || adaptedMessages[0]["content"] != corezen.AnonymousAssistantPreamble || adaptedMessages[1]["content"] != "test" || adaptedKw["tool_choice"] != nil || adaptedKw["tools"] != nil {
		t.Fatalf("ordinary agent admission shape: messages=%+v kwargs=%+v", adaptedMessages, adaptedKw)
	}

	capturedMessages = nil
	title := []Message{{"role": "system", "content": "You are a title generator. Output one title."}, {"role": "user", "content": "test"}}
	if _, err = anonZen.Complete("big-pickle", title, nil); err != nil {
		t.Fatal(err)
	}
	if len(capturedMessages) != 2 || capturedMessages[0].(map[string]any)["content"] != title[0]["content"] {
		t.Fatalf("explicit title prompt changed: %+v", capturedMessages)
	}
	_, titleKw := adaptAnonymousZenChat(title, nil)
	if titleKw["tools"] != nil || titleKw["tool_choice"] != nil {
		t.Fatalf("explicit title request gained agent tools: %+v", titleKw)
	}

	// Keyed Zen provider -> must NOT adapt messages
	capturedMessages = nil
	keyedZen := OpenAIProvider{
		auth:       bearerAuth{base: server.URL},
		Timeout:    2,
		providerID: "zen",
		registryID: "opencode_zen",
		anonymous:  false,
	}
	_, err = keyedZen.Complete("big-pickle", []Message{{"role": "user", "content": "test"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(capturedMessages) != 1 {
		t.Fatalf("expected 1 unadapted message, got %d: %+v", len(capturedMessages), capturedMessages)
	}

	// Non-Zen anonymous provider -> must NOT adapt messages
	capturedMessages = nil
	nonZen := OpenAIProvider{
		auth:       bearerAuth{base: server.URL},
		Timeout:    2,
		providerID: "other",
		registryID: "kilo_code",
		anonymous:  true,
	}
	_, err = nonZen.Complete("kilo-free", []Message{{"role": "user", "content": "test"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(capturedMessages) != 1 {
		t.Fatalf("expected 1 unadapted message, got %d: %+v", len(capturedMessages), capturedMessages)
	}
}

func TestAnonymousZenResponsesPreservesOrdinaryAndExplicitTitleInstructions(t *testing.T) {
	ordinary := adaptAnonymousZenResponsesPayload(map[string]any{"input": "Explain this failure", "instructions": "Be concise"})
	if ordinary["instructions"] != corezen.AnonymousAssistantPreamble+"\n\nBe concise" {
		t.Fatalf("ordinary Responses instructions changed: %+v", ordinary)
	}
	if ordinary["tools"] != nil || ordinary["tool_choice"] != nil {
		t.Fatalf("ordinary Responses admission shape: %+v", ordinary)
	}
	title := adaptAnonymousZenResponsesPayload(map[string]any{"input": "Explain this failure", "instructions": "You are a title generator"})
	if title["instructions"] != "You are a title generator" {
		t.Fatalf("explicit title instructions changed: %+v", title)
	}
	if title["tools"] != nil || title["tool_choice"] != nil {
		t.Fatalf("explicit title request gained agent tools: %+v", title)
	}
}

func TestAnonymousZenRecognizedByBaseURL(t *testing.T) {
	// A provider configured as "zen" with base_url "https://opencode.ai/zen/v1"
	// and NO explicit registry_id must still be recognized as Zen.
	auth, err := newBearerAuth("https://opencode.ai/zen/v1", "", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	p := OpenAIProvider{
		auth:       auth,
		providerID: "zen",
		registryID: "",
		anonymous:  true,
		Timeout:    15,
	}
	if !p.isAnonymousZen() {
		t.Fatal("expected isAnonymousZen to be true based on base_url")
	}
	if !p.zenUsesResponses("muse-spark-1.2-contributor-free") {
		t.Fatal("cold-cache Muse fallback must preserve the v0.6.6 Responses surface")
	}
}

func TestAnonymousZenToolsAndMultiTurn(t *testing.T) {
	origMessages := []Message{
		{"role": "system", "content": "You are Pi, a personal AI coding agent."},
		{"role": "user", "content": "What is the weather?"},
	}
	customTool := map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "get_weather",
			"description": "get weather",
			"parameters":  map[string]any{"type": "object"},
		},
	}
	kw := Kwargs{"tools": []any{customTool}}

	adaptedMessages, adaptedKw := adaptAnonymousZenChat(origMessages, kw)

	// Ensure system prompt is preserved (NOT replaced with title generator preamble)
	if sys, _ := adaptedMessages[0]["content"].(string); !strings.Contains(sys, "You are Pi") {
		t.Fatalf("expected Pi system prompt preserved, got: %s", sys)
	}
	if sys, _ := adaptedMessages[0]["content"].(string); strings.Contains(sys, "You are a title generator") {
		t.Fatal("title generator preamble must NOT be present when tools are used")
	}

	// Caller tools and tool choice are preserved while compatibility tools return.
	kw["tool_choice"] = "required"
	_, adaptedKw = adaptAnonymousZenChat(origMessages, kw)
	tools, ok := adaptedKw["tools"].([]any)
	if !ok || len(tools) != 3 || adaptedKw["tool_choice"] != "required" {
		t.Fatalf("caller tool contract changed: %+v", adaptedKw)
	}
}
