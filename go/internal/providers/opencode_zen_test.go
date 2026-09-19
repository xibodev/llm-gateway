package providers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"llmgw/internal/buildinfo"
)

func TestAnonymousOpenCodeHeadersMatchCLIContract(t *testing.T) {
	oldVersion := buildinfo.Version
	buildinfo.Version = "test-version"
	t.Cleanup(func() { buildinfo.Version = oldVersion })

	auth, err := newBearerAuth("https://opencode.ai/zen/v1", "", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	base, first, err := auth.Prepare()
	if err != nil {
		t.Fatal(err)
	}
	_, second, err := auth.Prepare()
	if err != nil {
		t.Fatal(err)
	}
	if base != "https://opencode.ai/zen/v1" || first.Get("Authorization") != "Bearer public" {
		t.Fatalf("base=%q authorization=%q", base, first.Get("Authorization"))
	}
	for _, key := range []string{"x-opencode-project", "x-opencode-session", "x-opencode-request"} {
		if first.Get(key) == "" {
			t.Fatalf("missing %s", key)
		}
	}
	if first.Get("x-opencode-project") != second.Get("x-opencode-project") ||
		first.Get("x-opencode-session") == second.Get("x-opencode-session") ||
		first.Get("x-opencode-request") == second.Get("x-opencode-request") {
		t.Fatalf("unexpected correlation lifetimes: first=%v second=%v", first, second)
	}
	if first.Get("x-opencode-client") != "cli" || first.Get("User-Agent") != openCodeAnonymousUserAgent {
		t.Fatalf("client headers=%v", first)
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
	if headers.Get("x-opencode-client") != "llmgw" || headers.Get("User-Agent") != "llm-gateway/test-version" {
		t.Fatalf("keyed client headers=%v", headers)
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
			{"id":"big-pickle","owned_by":"opencode"},
			{"id":"muse-spark-1.3-contributor-free","owned_by":"opencode"},
			{"id":"deepseek-v4-flash-free","owned_by":"opencode"},
			{"id":"paid-model","owned_by":"opencode"}
		]}`)
	}))
	defer upstream.Close()

	metadata := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"opencode":{"npm":"@ai-sdk/openai-compatible","models":{
			"big-pickle":{"id":"big-pickle","name":"Big Pickle","cost":{"input":0,"output":0},"family":"pickle","reasoning":true,"tool_call":true,"limit":{"context":200000,"output":32000}},
			"muse-spark-1.3-contributor-free":{"id":"muse-spark-1.3-contributor-free","name":"Muse Spark 1.3 Free","provider":{"npm":"@ai-sdk/openai"},"cost":{"input":0,"output":0},"structured_output":true,"tool_call":true,"limit":{"context":1048576,"output":131072}},
			"deepseek-v4-flash-free":{"id":"deepseek-v4-flash-free","name":"Deprecated","status":"deprecated","cost":{"input":0,"output":0}},
			"alpha-free":{"id":"alpha-free","name":"Experimental","status":"alpha","cost":{"input":0,"output":0}},
			"paid-model":{"id":"paid-model","name":"Paid","cost":{"input":1,"output":1}},
			"metadata-only":{"id":"metadata-only","name":"Not upstream","cost":{"input":0,"output":0}}
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
	if len(rows) != 2 {
		t.Fatalf("rows=%+v", rows)
	}
	if got := []string{rows[0].ID, rows[1].ID}; !slices.Equal(got, []string{"big-pickle", "muse-spark-1.3-contributor-free"}) {
		t.Fatalf("rows=%+v", rows)
	}
	if !rows[0].Free || !slices.Equal(rows[0].SupportedSurfaces, []string{"/chat/completions"}) {
		t.Fatalf("chat row=%+v", rows[0])
	}
	if !rows[1].Free || !slices.Equal(rows[1].SupportedSurfaces, []string{"/responses"}) ||
		rows[1].Label != "Muse Spark 1.3 Free" {
		t.Fatalf("responses row=%+v", rows[1])
	}
}

func TestZenMuseChatUsesResponses(t *testing.T) {
	var path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if r.URL.Path == "/responses" {
			if payload["model"] != "muse-spark-1.3-contributor-free" || payload["messages"] != nil || payload["input"] == nil {
				t.Fatalf("payload=%+v", payload)
			}
			_, _ = fmt.Fprint(w, `{"id":"resp_1","object":"response","status":"completed","model":"muse-spark-1.3-contributor-free","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}]}`)
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
		"muse-spark-1.3-contributor-free",
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

func TestAdaptAnonymousZenMessages(t *testing.T) {
	// Case 1: empty messages
	empty := adaptAnonymousZenMessages(nil)
	if len(empty) != 1 || empty[0]["role"] != "system" || empty[0]["content"] != openCodeAnonymousPreamble {
		t.Fatalf("empty messages: %+v", empty)
	}

	// Case 2: bare user message
	userOnly := []Message{{"role": "user", "content": "hello"}}
	adapted := adaptAnonymousZenMessages(userOnly)
	if len(adapted) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(adapted))
	}
	if adapted[0]["role"] != "system" || adapted[0]["content"] != openCodeAnonymousPreamble {
		t.Fatalf("unexpected system preamble: %+v", adapted[0])
	}
	if adapted[1]["role"] != "user" || adapted[1]["content"] != "hello" {
		t.Fatalf("unexpected user message: %+v", adapted[1])
	}

	// Case 3: existing system message is prepended
	existingSys := []Message{
		{"role": "system", "content": "You are a coding assistant."},
		{"role": "user", "content": "write code"},
	}
	adaptedSys := adaptAnonymousZenMessages(existingSys)
	if len(adaptedSys) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(adaptedSys))
	}
	sysContent, _ := adaptedSys[0]["content"].(string)
	if !strings.HasPrefix(sysContent, openCodeAnonymousPreamble) || !strings.HasSuffix(sysContent, "You are a coding assistant.") {
		t.Fatalf("expected combined preamble, got: %q", sysContent)
	}

	// Case 4: already has preamble -> no duplicate
	alreadyAdapted := []Message{
		{"role": "system", "content": openCodeAnonymousPreamble + "\n\nExtra instructions"},
		{"role": "user", "content": "hi"},
	}
	notReAdapted := adaptAnonymousZenMessages(alreadyAdapted)
	if len(notReAdapted) != 2 || notReAdapted[0]["content"] != alreadyAdapted[0]["content"] {
		t.Fatalf("preamble duplicated: %+v", notReAdapted)
	}
}

func TestAnonymousZenCompleteAdaptsMessages(t *testing.T) {
	var capturedMessages []any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if msgs, ok := payload["messages"].([]any); ok {
			capturedMessages = msgs
		}
		_, _ = fmt.Fprint(w, `{"id":"chat_1","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"hello"}}]}`)
	}))
	defer server.Close()

	// Anonymous Zen provider -> must adapt messages
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
	if len(capturedMessages) != 2 {
		t.Fatalf("expected 2 messages in payload, got %d: %+v", len(capturedMessages), capturedMessages)
	}
	firstMsg, ok := capturedMessages[0].(map[string]any)
	if !ok || firstMsg["role"] != "system" || firstMsg["content"] != openCodeAnonymousPreamble {
		t.Fatalf("expected anonymous preamble in first message, got: %+v", firstMsg)
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
