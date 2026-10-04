package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
	"llmgw/internal/router"
)

// streamFixture is one upstream wire's stream: the records of an answer
// that completes, of which the first cut reach a client of a stream that
// fails.
type streamFixture struct {
	records []string
	cut     int
}

var streamFixtures = map[string]streamFixture{
	"/chat/completions": {cut: 1, records: []string{
		`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello there"},"finish_reason":null}]}` + "\n\n",
		`data: {"id":"chatcmpl_1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}` + "\n\n",
		"data: [DONE]\n\n",
	}},
	"/responses": {cut: 2, records: []string{
		`data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_1","object":"response","status":"in_progress","model":"m","output":[]}}` + "\n\n",
		`data: {"type":"response.output_text.delta","sequence_number":1,"item_id":"msg_1","output_index":0,"content_index":0,"delta":"Hello there"}` + "\n\n",
		`data: {"type":"response.completed","sequence_number":2,"response":{"id":"resp_1","object":"response","status":"completed","model":"m","output":[],"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18}}}` + "\n\n",
	}},
	"/v1/messages": {cut: 3, records: []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"m\",\"content\":[],\"usage\":{\"input_tokens\":11,\"output_tokens\":0}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello there\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":7}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}},
}

// usageField is the usage an upstream reports in a record of the fixtures.
var usageField = regexp.MustCompile(`,"usage":\{[^}]*\}`)

// newStreamOutcomeUpstream streams each request as its model names: model
// "complete" ends the stream as the upstream's wire ends one, "unmetered"
// does too without reporting usage, "broken" drops the connection after the
// first records, and "cut" closes the body cleanly after them, as a proxy
// timing a connection out does.
func newStreamOutcomeUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The whole body is read, so dropping the connection never resets
		// it before the client read the records already sent.
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(raw, &body)
		fixture, ok := streamFixtures[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		records := fixture.records
		if body.Model != "complete" && body.Model != "unmetered" {
			records = records[:fixture.cut]
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, record := range records {
			if body.Model == "unmetered" {
				record = usageField.ReplaceAllString(record, "")
			}
			_, _ = io.WriteString(w, record)
		}
		w.(http.Flusher).Flush()
		if body.Model == "broken" {
			panic(http.ErrAbortHandler)
		}
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

// setupStreamOutcomeTest serves the gateway with three providers on one
// upstream: "native" with native Responses, "chat" without, which serves
// Responses through the Chat fallback, and "claude" on Anthropic's wire.
func setupStreamOutcomeTest(t *testing.T, upstreamURL string) *httptest.Server {
	t.Helper()
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	router.ResetSavingsState()
	router.ResetTelemetryState()
	old := *config.Get()
	config.Update(func(settings *config.Settings) {
		settings.AllowUnauthenticatedAPI = true
		settings.APIKey = ""
		settings.APIKeys = nil
		settings.Providers = map[string]*config.ProviderConfig{
			"native": {Type: "openai_compatible", RegistryID: "openai", BaseURL: upstreamURL, APIKey: "fixture"},
			"chat":   {Type: "openai_compatible", BaseURL: upstreamURL, APIKey: "fixture"},
			"claude": {Type: "anthropic", BaseURL: upstreamURL, APIKey: "fixture"},
		}
		settings.Endpoints = map[string]*config.EndpointConfig{}
		settings.Policies.Defaults = config.ProviderPolicy{RetryMaxAttempts: 1}
		settings.Policies.Overrides = map[string]config.ProviderPolicy{}
	})
	for _, id := range []string{"native", "chat", "claude"} {
		providers.ForgetCatalog(id)
	}
	providers.ResetProviders()
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	gateway := httptest.NewServer(NewServer(Runtime{}))
	t.Cleanup(func() {
		gateway.Close()
		providers.ResetProviders()
		iam.ResetForTests()
		router.ResetSavingsState()
		router.ResetTelemetryState()
		config.Update(func(settings *config.Settings) { *settings = old })
	})
	return gateway
}

func streamRequestBody(path, model string) string {
	switch path {
	case "/v1/responses":
		return `{"model":"` + model + `","stream":true,"input":"Say hello"}`
	case "/v1/messages":
		return `{"model":"` + model + `","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"Say hello"}]}`
	}
	return `{"model":"` + model + `","stream":true,"messages":[{"role":"user","content":"Say hello"}]}`
}

func streamThroughGateway(t *testing.T, gateway *httptest.Server, path, body string) string {
	t.Helper()
	response, err := http.Post(gateway.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status=%d content-type=%q body=%s", response.StatusCode, response.Header.Get("Content-Type"), raw)
	}
	return string(raw)
}

// streamUsageRow is the one usage event a request recorded.
type streamUsageRow struct {
	status, input, output, credits int
	code, provider, model          string
}

func readStreamUsageRow(t *testing.T) streamUsageRow {
	t.Helper()
	db, err := iam.DB()
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`SELECT status_code, input_tokens, output_tokens, credits_milli,
		COALESCE(error_code,''), COALESCE(provider,''), COALESCE(routed_model,'') FROM usage_events`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var found []streamUsageRow
	for rows.Next() {
		var row streamUsageRow
		if err := rows.Scan(&row.status, &row.input, &row.output, &row.credits, &row.code, &row.provider, &row.model); err != nil {
			t.Fatal(err)
		}
		found = append(found, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 {
		t.Fatalf("usage rows=%+v, want one", found)
	}
	return found[0]
}

// A stream succeeds only when its upstream ended it and the gateway wrote
// the surface's own terminal event. A stream its upstream broke off, or
// closed before its end, ends with the surface's error event instead, never
// with what tells a client the answer is complete, and is recorded as a
// failed stream.
func TestStreamOutcomesFollowTheUpstreamsEnd(t *testing.T) {
	upstream := newStreamOutcomeUpstream(t)
	type surface struct {
		path, provider string
		// success is the terminal a complete stream ends with, failure what
		// a failed one ends with instead; never are what a failed stream
		// must not carry.
		success, failure string
		never            []string
	}
	chat := surface{path: "/v1/chat/completions", success: "data: [DONE]\n\n",
		failure: `data: {"error":{"code":"provider_invocation_failed","message":"Upstream provider request failed.","type":"provider_error"}}` + "\n\n",
		never:   []string{"[DONE]", `"finish_reason":"stop"`}}
	messages := surface{path: "/v1/messages", success: "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		failure: "event: error\ndata: {\"error\":{\"message\":\"Upstream provider stream failed.\",\"type\":\"api_error\"},\"type\":\"error\"}\n\n",
		never:   []string{"message_delta", "message_stop", "content_block_stop"}}
	responses := surface{path: "/v1/responses", success: "event: response.completed\n",
		failure: "event: response.failed\n", never: []string{"response.completed", `"status":"completed"`}}
	for _, tc := range []struct {
		name, provider string
		surface        surface
	}{
		{"Chat", "chat", chat},
		{"Chat from Anthropic", "claude", chat},
		{"Messages from Chat", "chat", messages},
		{"Messages from Anthropic", "claude", messages},
		{"native Responses", "native", responses},
		{"Responses from Chat", "chat", responses},
		{"Responses from Anthropic", "claude", responses},
	} {
		for _, outcome := range []string{"complete", "broken", "cut"} {
			t.Run(tc.name+" "+outcome, func(t *testing.T) {
				gateway := setupStreamOutcomeTest(t, upstream.URL)
				body := streamThroughGateway(t, gateway, tc.surface.path, streamRequestBody(tc.surface.path, tc.provider+"/"+outcome))
				if !strings.Contains(body, "Hello there") {
					t.Fatalf("the streamed text was lost: %q", body)
				}
				row := readStreamUsageRow(t)
				// Whatever the end, the stream consumed a model: its row
				// names what served it and charges the tokens consumed.
				if row.provider != tc.provider || row.model != outcome || row.input <= 0 || row.output <= 0 || row.credits != 1000 {
					t.Fatalf("usage=%+v", row)
				}
				if outcome == "complete" {
					if !strings.Contains(body, tc.surface.success) || strings.Contains(body, tc.surface.failure) ||
						row.status != http.StatusOK || row.code != "" {
						t.Fatalf("usage=%+v body=%q", row, body)
					}
					return
				}
				if !endsWithEvent(body, tc.surface.failure) {
					t.Fatalf("failure event missing or not last: %q", body)
				}
				for _, never := range tc.surface.never {
					if strings.Contains(body, never) {
						t.Fatalf("a failed stream carried %q: %q", never, body)
					}
				}
				if row.status != http.StatusBadGateway || row.code != "upstream_stream" {
					t.Fatalf("usage=%+v", row)
				}
			})
		}
	}
}

// endsWithEvent reports a stream whose last record is event, given whole or
// by its event line.
func endsWithEvent(body, event string) bool {
	if strings.HasSuffix(body, event) {
		return true
	}
	last := strings.LastIndex(body, "event: ")
	return strings.HasPrefix(event, "event: ") && last >= 0 && last == strings.LastIndex(body, event)
}

// An upstream that ends its Chat stream with [DONE] ended it on purpose,
// whether or not a chunk carried a finish reason.
func TestChatStreamEndsAtTheUpstreamsDone(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"chatcmpl_1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"Hello there"}}]}`+"\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(upstream.Close)
	gateway := setupStreamOutcomeTest(t, upstream.URL)
	body := streamThroughGateway(t, gateway, "/v1/chat/completions", streamRequestBody("/v1/chat/completions", "chat/model"))
	if !strings.HasSuffix(body, "data: [DONE]\n\n") || strings.Contains(body, `"error"`) {
		t.Fatalf("body=%q", body)
	}
	if row := readStreamUsageRow(t); row.status != http.StatusOK || row.code != "" {
		t.Fatalf("usage=%+v", row)
	}
}
