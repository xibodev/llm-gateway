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
	if first.Get("x-opencode-client") != "llmgw" || first.Get("User-Agent") != "llm-gateway/test-version" {
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
