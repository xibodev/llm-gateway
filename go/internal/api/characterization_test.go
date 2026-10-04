package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
	"llmgw/internal/router"
)

// TestHTTPCharacterization replays fixed client requests through the full
// handler stack against one recording fixture upstream, and pins in
// testdata/characterization what each client saw and every request the
// gateway sent upstream. A golden diff is a behavior change; when it is
// intended, LLMGW_UPDATE_GOLDEN=1 rewrites the goldens for review.
func TestHTTPCharacterization(t *testing.T) {
	const (
		chat      = "/v1/chat/completions"
		messages  = "/v1/messages"
		responses = "/v1/responses"
		images    = "/v1/images/generations"
		hello     = `"messages":[{"role":"user","content":"Say hello"}]`
		hello64   = `"max_tokens":64,` + hello
		stream    = `,"stream":true`
		input     = `"input":"Say hello"`
		crane     = `"prompt":"an origami crane"`
		// briefly carries the markup an HTML-escaping encoder would change.
		briefly = `"max_tokens":64,"temperature":0.2,"messages":[{"role":"system","content":"Answer <briefly> & kindly."},{"role":"user","content":"Say hello"}]`
	)
	fixture := &charFixture{}
	upstream := httptest.NewServer(fixture)
	t.Cleanup(upstream.Close)
	gateway := setupCharacterization(t)
	u := upstream.URL
	// Each group is one gateway configuration, so a golden sees only its
	// group's providers and the model list does not grow with the suite.
	for _, group := range []struct {
		providers map[string]*config.ProviderConfig
		endpoints map[string]*config.EndpointConfig
		goldens   map[string][]charCase
	}{{
		providers: map[string]*config.ProviderConfig{
			"fixture":   {Type: "openai_compatible", BaseURL: u + "/f", APIKey: "fixture-key"},
			"fixture-a": {Type: "openai_compatible", BaseURL: u + "/a", APIKey: "fixture-key"},
			"fixture-b": {Type: "openai_compatible", BaseURL: u + "/b", APIKey: "fixture-key"},
			"native":    {Type: "anthropic", BaseURL: u + "/n", APIKey: "fixture-key"},
		},
		endpoints: map[string]*config.EndpointConfig{"fixture-failover": {Failover: []config.EndpointMember{
			{Provider: "fixture-a", Model: "model-a"}, {Provider: "fixture-b", Model: "model-b"},
		}}},
		goldens: map[string][]charCase{
			"openai-chat-native":               {charPost("Chat Completions, native OpenAI-compatible", chat, "fixture/chat-model", hello)},
			"openai-chat-native-stream":        {charPost("Chat Completions stream, native OpenAI-compatible", chat, "fixture/chat-model", hello+stream)},
			"anthropic-messages-native":        {charPost("Messages, native Anthropic", messages, "native/claude-fixture", hello64)},
			"anthropic-messages-native-stream": {charPost("Messages stream, native Anthropic", messages, "native/claude-fixture", hello64+stream)},
			"openai-messages-via-chat":         {charPost("Messages translated to a Chat-only model", messages, "fixture/chat-model", hello64)},
			"openai-messages-via-chat-stream":  {charPost("Messages stream translated to a Chat-only model", messages, "fixture/chat-model", hello64+stream)},
			"openai-responses-native":          {charPost("Responses, native OpenAI-compatible", responses, "fixture/responses-model", input)},
			"openai-responses-native-stream":   {charPost("Responses stream, native OpenAI-compatible", responses, "fixture/responses-model", input+stream)},
			"openai-responses-via-chat":        {charPost("Responses translated to a Chat-only model", responses, "fixture/chat-model", input)},
			"openai-responses-via-chat-stream": {charPost("Responses stream translated to a Chat-only model", responses, "fixture/chat-model", input+stream)},
			"failover-before-output-stream":    {charPost("First member fails before output; the second member serves", chat, "fixture-failover", hello+stream).failing("before")},
			"failover-after-output-stream":     {charPost("First member fails after output; no failover", chat, "fixture-failover", hello+stream).failing("after")},
			"models":                           {{title: "Model list", method: http.MethodGet, path: "/v1/models"}},
		},
	}, {
		providers: map[string]*config.ProviderConfig{
			"bedrock":      {Type: "bedrock", BaseURL: u + "/br", APIKey: "fixture-key"},
			"adapt":        {Type: "openai_compatible", BaseURL: u + "/ad", APIKey: "fixture-key", ForceApiSupport: true},
			"pollinations": {Type: "openai_compatible", RegistryID: "pollinations", BaseURL: u + "/po"},
		},
		goldens: map[string][]charCase{
			"bedrock": {
				charPost("Chat Completions, Bedrock", chat, "bedrock/chat-fixture", hello64),
				charPost("Chat Completions stream, Bedrock", chat, "bedrock/chat-fixture", hello64+stream),
				charPost("Responses, native Bedrock", responses, "bedrock/responses-fixture", input),
				charPost("Responses stream, native Bedrock", responses, "bedrock/responses-fixture", input+stream),
				charPost("Responses translated to a Chat-only model, Bedrock", responses, "bedrock/chat-fixture", input),
				charPost("Refused upstream, Bedrock", chat, "bedrock/throttled-fixture", hello64),
			},
			"openai-compatible-edges": {
				charPost("Chat Completions to a reasoning model, forced adaptation", chat, "adapt/reasoning-fixture", hello64),
				charPost("Chat Completions stream to a reasoning model, forced adaptation", chat, "adapt/reasoning-fixture", hello64+stream),
				charPost("Chat Completions, keyless Pollinations", chat, "pollinations/openai-fast", hello64),
				charPost("Stream refused upstream, keyless Pollinations", chat, "pollinations/throttled-fixture", hello64+stream),
			},
		},
	}, {
		providers: map[string]*config.ProviderConfig{
			"studio": {Type: "ai_studio", BaseURL: u + "/studio/v1beta", APIKey: "fixture-key"},
			"vertex": {Type: "vertex_ai", BaseURL: u + "/vertex/v1", APIKey: "fixture-key", Project: "fixture-project"},
		},
		goldens: map[string][]charCase{
			"google-chat": {
				charPost("Chat Completions, AI Studio", chat, "studio/gemini-fixture", briefly),
				charPost("Chat Completions stream, AI Studio", chat, "studio/gemini-fixture", briefly+stream),
				charPost("Chat Completions, Vertex AI", chat, "vertex/gemini-fixture", briefly),
				charPost("Messages translated to Chat, AI Studio", messages, "studio/gemini-fixture", `"system":"Answer briefly.",`+hello64),
				charPost("Responses translated to Chat, Vertex AI", responses, "vertex/gemini-fixture", input),
				charPost("Billing exhausted, Vertex AI", chat, "vertex/gemini-billing-fixture", briefly),
				charPost("Model not available, Vertex AI", chat, "vertex/missing-model-fixture", briefly),
				charPost("Credential rejected, AI Studio", chat, "studio/gemini-denied-fixture", briefly),
				charPost("Budget spent on reasoning, AI Studio", chat, "studio/gemini-thinking-fixture", briefly),
			},
			"google-embeddings": {
				charPost("Embeddings, AI Studio", "/v1/embeddings", "studio/gemini-embedding-fixture", input),
				charPost("Embeddings, Vertex AI", "/v1/embeddings", "vertex/text-embedding-fixture", `"input":["first","second"]`),
				charPost("Embeddings refused upstream, AI Studio", "/v1/embeddings", "studio/missing-embedding-fixture", input),
			},
			"google-images": {
				charPost("Image generation, Vertex AI", images, "vertex/gemini-image-fixture", crane),
				charPost("Image generation from a text model, AI Studio", images, "studio/gemini-fixture", crane),
				charPost("Image generation refused upstream, AI Studio", images, "studio/gemini-denied-fixture", crane),
			},
		},
	}} {
		config.Update(func(s *config.Settings) { s.Providers, s.Endpoints = group.providers, group.endpoints })
		providers.ResetProviders()
		// Catalog rows decide which surfaces a model serves natively, so they
		// are cached before any case runs.
		for id := range group.providers {
			providers.RefreshCatalog(id)
		}
		for _, name := range slices.Sorted(maps.Keys(group.goldens)) {
			t.Run(name, func(t *testing.T) {
				var got strings.Builder
				for _, c := range group.goldens[name] {
					got.WriteString(fixture.replay(t, gateway, c))
				}
				charGolden(t, name, got.String())
			})
		}
	}
}

// setupCharacterization configures a gateway with no retries and no circuit
// breaker, so every upstream request a case causes is one the golden shows.
func setupCharacterization(t *testing.T) *httptest.Server {
	t.Helper()
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	old := *config.Get()
	iam.ResetForTests()
	router.ResetSavingsState()
	router.ResetTelemetryState()
	config.Update(func(s *config.Settings) {
		*s = *config.Defaults()
		s.APIKey = "fixture-gateway-token"
		s.Policies.Defaults.RetryMaxAttempts = 1
		s.Policies.Defaults.CircuitFailureThreshold = 0
	})
	gateway := httptest.NewServer(NewServer(Runtime{}))
	t.Cleanup(func() {
		gateway.Close()
		iam.ResetForTests()
		router.ResetSavingsState()
		router.ResetTelemetryState()
		config.Update(func(s *config.Settings) { *s = old })
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	// Other tests cache catalogs under the same provider ids, and a catalog
	// that fails to refresh keeps its rows, so the suite has its own Runtime.
	providers.InstallForTests(t)
	return gateway
}

// charCase is one client request. fail makes the fixture's answer to the
// first upstream request the case causes a failure of that kind.
type charCase struct {
	title, method, path, body, fail string
}

func charPost(title, path, model, fields string) charCase {
	return charCase{title: title, method: http.MethodPost, path: path, body: fmt.Sprintf(`{"model":%q,%s}`, model, fields)}
}

func (c charCase) failing(kind string) charCase {
	c.fail = kind
	return c
}

// charFixture is every upstream the cases reach, told apart by path. It
// records every request but catalog reads, the only GETs, which follow the
// catalog cache rather than the case.
type charFixture struct {
	mu       sync.Mutex
	requests strings.Builder
	fail     string
}

func (f *charFixture) replay(t *testing.T, gateway *httptest.Server, c charCase) string {
	t.Helper()
	f.mu.Lock()
	f.requests.Reset()
	f.fail = c.fail
	f.mu.Unlock()
	request, err := http.NewRequest(c.method, gateway.URL+c.path, strings.NewReader(c.body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer fixture-gateway-token")
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	fmt.Fprintf(&out, "# %s\nrequest: %s %s\nstatus: %d\n", c.title, c.method, c.path, response.StatusCode)
	// The gateway's own headers and those that shape how a client reads the
	// body are pinned; the transport's, such as Date, are not.
	for _, name := range slices.Sorted(maps.Keys(response.Header)) {
		value := response.Header.Get(name)
		switch {
		case strings.HasPrefix(name, "X-Llmgw-") && strings.HasSuffix(name, "-Ms"):
			value = "<ms>"
		case name == "X-Request-Id":
			value, _ = charNormalize("request_id", value).(string)
		case strings.HasPrefix(name, "X-Llmgw-"), name == "Content-Type", name == "Cache-Control", name == "Connection":
		default:
			continue
		}
		fmt.Fprintf(&out, "header %s: %s\n", name, value)
	}
	f.mu.Lock()
	out.WriteString(f.requests.String())
	f.mu.Unlock()
	out.WriteString("body:\n")
	if !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		out.WriteString(charJSON(string(body), "  ") + "\n")
		return out.String()
	}
	for _, line := range strings.Split(strings.TrimRight(string(body), "\n"), "\n") {
		if data, ok := strings.CutPrefix(line, "data: "); ok && json.Valid([]byte(data)) {
			line = "data: " + charJSON(data, "")
		}
		out.WriteString(line + "\n")
	}
	return out.String()
}

// charJSON renders raw with sorted keys and unescaped markup, its times
// replaced by <time> and the identifiers the gateway generates by their
// prefix. Text that is not JSON is returned as it is.
func charJSON(raw, indent string) string {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return strings.TrimRight(raw, "\n")
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", indent)
	_ = encoder.Encode(charNormalize("", value))
	return strings.TrimSuffix(out.String(), "\n")
}

// charNormalize keeps an identifier that names a fixture, as every one the
// fixture upstream returns does, so only the gateway's own are replaced.
func charNormalize(key string, value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for k, v := range typed {
			typed[k] = charNormalize(k, v)
		}
	case []any:
		for i, v := range typed {
			typed[i] = charNormalize(key, v)
		}
	case string:
		if (key == "id" || key == "item_id" || key == "response_id" || key == "request_id") && !strings.Contains(typed, "fixture") {
			if cut := strings.IndexAny(typed, "_-"); cut >= 0 {
				return "<generated " + typed[:cut+1] + ">"
			}
		}
	}
	switch key {
	case "created", "created_at", "discovered_at":
		return "<time>"
	}
	return value
}

func charGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "characterization", name+".golden")
	if os.Getenv("LLMGW_UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("%s changed; if intended, rerun with LLMGW_UPDATE_GOLDEN=1\n--- got\n%s--- want\n%s", path, got, want)
	}
}

func (f *charFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	path := r.URL.Path
	model, _ := body["model"].(string)
	if _, rest, ok := strings.Cut(path, "/models/"); ok {
		model, _, _ = strings.Cut(rest, ":")
	}
	if r.Method == http.MethodGet {
		if catalog, ok := charCatalogs[path]; ok {
			charAnswer(w, http.StatusOK, catalog)
		} else {
			http.NotFound(w, r)
		}
		return
	}
	f.mu.Lock()
	fail := f.fail
	f.fail = ""
	fmt.Fprintf(&f.requests, "upstream: %s %s\nupstream body: %s\n", r.Method, path, charJSON(string(raw), ""))
	f.mu.Unlock()
	if refusal, ok := charRefusals[model]; ok {
		charAnswer(w, refusal.code, fmt.Sprintf(`{"error":{"code":%d,"message":%q,"status":%q}}`, refusal.code, refusal.message, refusal.status))
		return
	}
	stream := body["stream"] == true
	switch {
	case fail == "before":
		charAnswer(w, http.StatusServiceUnavailable, `{"error":{"message":"fixture outage"}}`)
	case fail == "after":
		charAnswer(w, http.StatusOK, charChunk(model, `{"role":"assistant","content":"Partial"}`, "null", ""))
		w.(http.Flusher).Flush()
		// Drop the connection after the first chunk, as a failing upstream does.
		panic(http.ErrAbortHandler)
	case strings.HasSuffix(path, ":generateContent"):
		charAnswer(w, http.StatusOK, charGemini[model])
	case strings.HasSuffix(path, ":embedContent"):
		charAnswer(w, http.StatusOK, `{"embedding":{"values":[0.25,-0.5]},"usageMetadata":{"promptTokenCount":2}}`)
	case strings.HasSuffix(path, ":predict"):
		charAnswer(w, http.StatusOK, `{"predictions":[{"embeddings":{"values":[0.75],"statistics":{"token_count":3}}}]}`)
	case strings.HasSuffix(path, "/messages") && stream:
		charAnswer(w, http.StatusOK, charMessageStream)
	case strings.HasSuffix(path, "/messages"):
		charAnswer(w, http.StatusOK, fmt.Sprintf(charMessage, model))
	case strings.HasSuffix(path, "/responses") && stream:
		charAnswer(w, http.StatusOK, fmt.Sprintf(charResponseStream, model))
	case strings.HasSuffix(path, "/responses"):
		charAnswer(w, http.StatusOK, fmt.Sprintf(charResponse, model))
	case strings.HasSuffix(path, "/chat/completions") && stream:
		charAnswer(w, http.StatusOK, charChunk(model, `{"role":"assistant","content":"Hello"}`, "null", "")+
			charChunk(model, `{"content":" from chat"}`, "null", "")+
			charChunk(model, `{}`, `"stop"`, `,"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}`)+
			"data: [DONE]\n\n")
	case strings.HasSuffix(path, "/chat/completions"):
		charAnswer(w, http.StatusOK, fmt.Sprintf(charChat, model))
	default:
		http.NotFound(w, r)
	}
}

// charAnswer writes body with status, as an event stream when it is one.
func charAnswer(w http.ResponseWriter, status int, body string) {
	contentType := "application/json"
	if strings.HasPrefix(body, "data: ") || strings.HasPrefix(body, "event: ") {
		contentType = "text/event-stream"
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func charChunk(model, delta, finish, extra string) string {
	return fmt.Sprintf(`data: {"id":"chatcmpl_fixture","object":"chat.completion.chunk","created":1700000000,"model":%q,"choices":[{"index":0,"delta":%s,"finish_reason":%s}]%s}`+"\n\n", model, delta, finish, extra)
}

var charCatalogs = map[string]string{
	"/f/models":    `{"data":[{"id":"chat-model","supported_endpoints":["/chat/completions"]},{"id":"responses-model","supported_endpoints":["/chat/completions","/responses"]}]}`,
	"/a/models":    `{"data":[{"id":"model-a","supported_endpoints":["/chat/completions"]}]}`,
	"/b/models":    `{"data":[{"id":"model-b","supported_endpoints":["/chat/completions"]}]}`,
	"/n/v1/models": `{"data":[{"id":"claude-fixture","display_name":"Claude Fixture","type":"model"}],"has_more":false}`,
	"/br/models":   `{"data":[{"id":"chat-fixture","supported_endpoints":["/chat/completions"]},{"id":"responses-fixture","supported_endpoints":["/chat/completions","/responses"]}]}`,
	"/ad/models":   `{"data":[{"id":"reasoning-fixture","supported_endpoints":["/chat/completions"],"capabilities":{"supports":{"reasoning_effort":["low","high"]}}}]}`,
}

// charRefusals are the models every fixture upstream refuses. The Google
// status decides how the gateway names the refusal.
var charRefusals = map[string]struct {
	code            int
	status, message string
}{
	"throttled-fixture":         {http.StatusTooManyRequests, "RESOURCE_EXHAUSTED", "fixture rate limit"},
	"gemini-billing-fixture":    {http.StatusTooManyRequests, "RESOURCE_EXHAUSTED", "Your prepayment credits are depleted."},
	"missing-model-fixture":     {http.StatusNotFound, "NOT_FOUND", "Publisher model was not found."},
	"missing-embedding-fixture": {http.StatusNotFound, "NOT_FOUND", "Model was not found."},
	"gemini-denied-fixture":     {http.StatusForbidden, "PERMISSION_DENIED", "denied"},
}

var charGemini = map[string]string{
	"gemini-fixture":          `{"candidates":[{"content":{"role":"model","parts":[{"text":"Hello from Gemini"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":4,"totalTokenCount":7}}`,
	"gemini-thinking-fixture": `{"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"MAX_TOKENS"}],"usageMetadata":{"promptTokenCount":3,"thoughtsTokenCount":32,"totalTokenCount":35}}`,
	"gemini-image-fixture":    `{"candidates":[{"content":{"role":"model","parts":[{"inlineData":{"mimeType":"image/png","data":"iVBORw0KGgo="}}]},"finishReason":"STOP"}]}`,
}

const (
	charChat = `{"id":"chatcmpl_fixture","object":"chat.completion","created":1700000000,"model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":"Hello from chat"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`

	charMessage       = `{"id":"msg_fixture","type":"message","role":"assistant","model":%q,"content":[{"type":"text","text":"Hello from messages"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":3,"output_tokens":4}}`
	charMessageStream = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_fixture\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-fixture\",\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\" from messages\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":4}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

	charResponseItem   = `{"id":"msg_fixture","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hello from responses","annotations":[]}]}`
	charResponse       = `{"id":"resp_fixture","object":"response","created_at":1700000000,"model":%[1]q,"status":"completed","instructions":null,"output":[` + charResponseItem + `],"usage":{"input_tokens":3,"output_tokens":4,"total_tokens":7}}`
	charResponseStream = "event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_fixture\",\"object\":\"response\",\"created_at\":1700000000,\"model\":%[1]q,\"status\":\"in_progress\",\"instructions\":null,\"output\":[]}}\n\n" +
		"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"sequence_number\":1,\"output_index\":0,\"item\":{\"id\":\"msg_fixture\",\"type\":\"message\",\"role\":\"assistant\",\"status\":\"in_progress\",\"content\":[]}}\n\n" +
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"sequence_number\":2,\"item_id\":\"msg_fixture\",\"output_index\":0,\"content_index\":0,\"delta\":\"Hello\"}\n\n" +
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"sequence_number\":3,\"item_id\":\"msg_fixture\",\"output_index\":0,\"content_index\":0,\"delta\":\" from responses\"}\n\n" +
		"event: response.output_text.done\ndata: {\"type\":\"response.output_text.done\",\"sequence_number\":4,\"item_id\":\"msg_fixture\",\"output_index\":0,\"content_index\":0,\"text\":\"Hello from responses\"}\n\n" +
		"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"sequence_number\":5,\"output_index\":0,\"item\":" + charResponseItem + "}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"sequence_number\":6,\"response\":{\"id\":\"resp_fixture\",\"object\":\"response\",\"created_at\":1700000000,\"model\":%[1]q,\"status\":\"completed\",\"instructions\":null,\"output\":[" + charResponseItem + "],\"usage\":{\"input_tokens\":3,\"output_tokens\":4,\"total_tokens\":7}}}\n\n"
)
