package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
	"llmgw/internal/router"
)

func nativeRecord(event, data string) string {
	return "event: " + event + "\ndata: " + data + "\n\n"
}

// nativeStreamRecords are a Messages stream as Anthropic writes it: pings,
// one between message_delta and message_stop, a thinking block with its
// signature, a text block, and usage that counts cached input. Its JSON
// keeps Anthropic's key order and spacing, which a stream decoded and
// encoded again would not.
var nativeStreamRecords = []string{
	nativeRecord("message_start", `{"type":"message_start","message":{"id":"msg_fixture","type":"message","role":"assistant","model":"claude-fixture","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":12,"cache_creation_input_tokens":200,"cache_read_input_tokens":3000,"output_tokens":1}}}`),
	nativeRecord("ping", `{"type": "ping"}`),
	nativeRecord("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`),
	nativeRecord("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"A greeting is wanted."}}`),
	nativeRecord("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"fixture-signature"}}`),
	nativeRecord("content_block_stop", `{"type":"content_block_stop","index":0}`),
	nativeRecord("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`),
	nativeRecord("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Hello there"}}`),
	nativeRecord("content_block_stop", `{"type":"content_block_stop","index":1}`),
	nativeRecord("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":42}}`),
	nativeRecord("ping", `{"type": "ping"}`),
	nativeRecord("message_stop", `{"type":"message_stop"}`),
}

const (
	// nativeStreamInput is the prompt nativeStreamRecords report: its input
	// with the input written to and read from the cache.
	nativeStreamInput = 12 + 200 + 3000
	// nativeStreamOutput is the output their message_delta reports.
	nativeStreamOutput = 42
	// nativeStreamedOutput estimates the thinking and the text they stream
	// before their message_delta: 21 bytes, five tokens, and 11, two.
	nativeStreamedOutput = 7
	// nativeStreamText is how many of them reach the end of the text, and
	// nativeStreamDelta how many come before message_delta.
	nativeStreamText  = 8
	nativeStreamDelta = 9
)

// gatewayStreamError is the error event the gateway ends a stream with when
// its upstream sent none.
const gatewayStreamError = "event: error\ndata: {\"error\":{\"message\":\"Upstream provider stream failed.\",\"type\":\"api_error\"},\"type\":\"error\"}\n\n"

// nativeStreamUpstream is the upstream of the native stream tests. answer
// answers each Messages request; the upstream keeps the last body each path
// received and how many requests it received.
type nativeStreamUpstream struct {
	*httptest.Server
	mu     sync.Mutex
	bodies map[string]map[string]any
	calls  map[string]int
}

func newNativeStreamUpstream(t *testing.T, answer func(w http.ResponseWriter, r *http.Request)) *nativeStreamUpstream {
	t.Helper()
	upstream := &nativeStreamUpstream{bodies: map[string]map[string]any{}, calls: map[string]int{}}
	upstream.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		// The whole body is read first, so the upstream notices a client
		// that leaves, and dropping the connection never resets it before
		// the client read the records already sent.
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		upstream.mu.Lock()
		upstream.bodies[r.URL.Path] = body
		upstream.calls[r.URL.Path]++
		upstream.mu.Unlock()
		answer(w, r)
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

func (u *nativeStreamUpstream) body(path string) map[string]any {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.bodies[path]
}

func (u *nativeStreamUpstream) callsTo(path string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls[path]
}

func (u *nativeStreamUpstream) totalCalls() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	total := 0
	for _, calls := range u.calls {
		total += calls
	}
	return total
}

// writeRecords sends records as the start of a stream and flushes them.
func writeRecords(w http.ResponseWriter, records ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, record := range records {
		_, _ = io.WriteString(w, record)
	}
	w.(http.Flusher).Flush()
}

// setupNativeStreamTest serves the gateway without authentication. Its
// providers are "first" and "second", on Anthropic's wire, and "adapted",
// which is not, each under its own path of upstream. Endpoint "native"
// routes to first and second, "mixed" to first and adapted.
func setupNativeStreamTest(t *testing.T, upstreamURL string) *httptest.Server {
	t.Helper()
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	router.ResetSavingsState()
	router.ResetTelemetryState()
	old := *config.Get()
	config.Update(func(s *config.Settings) {
		*s = *config.Defaults()
		s.AllowUnauthenticatedAPI = true
		s.Providers = map[string]*config.ProviderConfig{
			"first":   {Type: "anthropic", BaseURL: upstreamURL + "/first", APIKey: "fixture"},
			"second":  {Type: "anthropic", BaseURL: upstreamURL + "/second", APIKey: "fixture"},
			"adapted": {Type: "openai_compatible", BaseURL: upstreamURL + "/adapted", APIKey: "fixture"},
		}
		s.Endpoints = map[string]*config.EndpointConfig{
			"native": {Failover: []config.EndpointMember{{Provider: "first", Model: "model-a"}, {Provider: "second", Model: "model-b"}}},
			"mixed":  {Failover: []config.EndpointMember{{Provider: "first", Model: "model-a"}, {Provider: "adapted", Model: "model-c"}}},
		}
		s.Policies.Defaults = config.ProviderPolicy{RetryMaxAttempts: 1}
	})
	for _, id := range []string{"first", "second", "adapted"} {
		providers.ForgetCatalog(id)
	}
	providers.ResetProviders()
	providers.ResetCircuit("")
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	gateway := httptest.NewServer(NewServer(Runtime{}))
	t.Cleanup(func() {
		gateway.Close()
		providers.ResetProviders()
		providers.ResetCircuit("")
		iam.ResetForTests()
		router.ResetSavingsState()
		router.ResetTelemetryState()
		config.Update(func(s *config.Settings) { *s = old })
	})
	return gateway
}

// nativeStreamRequest is a Messages stream request with Claude Code's
// defaults, prompt caching and enabled thinking, which translation refuses.
func nativeStreamRequest(model string) map[string]any {
	return map[string]any{
		"model": model, "stream": true, "max_tokens": 2048,
		"system":   []any{map[string]any{"type": "text", "text": "Answer briefly.", "cache_control": map[string]any{"type": "ephemeral"}}},
		"thinking": map[string]any{"type": "enabled", "budget_tokens": 1024},
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "Say hello", "cache_control": map[string]any{"type": "ephemeral"}},
		}}},
		"metadata": map[string]any{"user_id": "fixture-user"},
	}
}

// postMessages posts body to the gateway's /v1/messages and returns the
// response and all of its body.
func postMessages(t *testing.T, gateway *httptest.Server, body map[string]any) (*http.Response, string) {
	t.Helper()
	response, err := http.Post(gateway.URL+"/v1/messages", "application/json", strings.NewReader(jsonStr(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response, string(raw)
}

// A stream whose targets all serve Messages natively is sent as the client
// sent it, prompt caching and thinking included, with only the gateway's
// changes: the resolved model, the stream flag, the gateway preamble, and
// without the gateway's routing controls. Every record the upstream sends
// reaches the client unchanged, and the stream is recorded with the usage
// those records report, cached input included.
func TestNativeMessagesStreamReachesTheClientAsSent(t *testing.T) {
	upstream := newNativeStreamUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		writeRecords(w, nativeStreamRecords...)
	})
	gateway := setupNativeStreamTest(t, upstream.URL)
	config.Update(func(s *config.Settings) { s.GatewayPreamble = "Follow the policy." })
	request := nativeStreamRequest("first/claude-fixture")
	request["fallback_timeout_ms"], request["affinity_key"] = 60000, "fixture-affinity"
	body := streamThroughGateway(t, gateway, "/v1/messages", jsonStr(request))
	if want := strings.Join(nativeStreamRecords, ""); body != want {
		t.Fatalf("the client read\n%q\nthe upstream sent\n%q", body, want)
	}

	want := nativeStreamRequest("claude-fixture")
	system := want["system"].([]any)
	want["system"] = append([]any{map[string]any{"type": "text", "text": "Follow the policy."}}, system...)
	var sent map[string]any
	if err := json.Unmarshal([]byte(jsonStr(want)), &sent); err != nil {
		t.Fatal(err)
	}
	if got := upstream.body("/first/v1/messages"); !reflect.DeepEqual(got, sent) {
		t.Fatalf("the upstream received %s, want %s", jsonStr(got), jsonStr(sent))
	}
	row := readStreamUsageRow(t)
	if row.status != http.StatusOK || row.code != "" || row.provider != "first" || row.model != "claude-fixture" ||
		row.input != nativeStreamInput || row.output != nativeStreamOutput {
		t.Fatalf("usage=%+v, want %d/%d tokens", row, nativeStreamInput, nativeStreamOutput)
	}
}

// A native stream that its upstream fails ends with an error event and is
// recorded as a failed stream, charged what it consumed: the upstream's own
// error event, relayed as it was sent, or the gateway's when the upstream
// sent none. It never carries message_stop, nor the message_delta that
// tells how the answer stopped.
func TestNativeMessagesStreamFailuresEndWithAnErrorEvent(t *testing.T) {
	upstreamError := nativeRecord("error", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
	for _, tc := range []struct {
		name string
		// send streams the records the upstream sends; abort drops the
		// connection after them instead of closing it.
		send   []string
		abort  bool
		want   string
		output int
	}{
		{
			name:   "error event",
			send:   append(append([]string{}, nativeStreamRecords[:nativeStreamText]...), upstreamError),
			want:   strings.Join(nativeStreamRecords[:nativeStreamText], "") + upstreamError,
			output: nativeStreamedOutput,
		},
		{
			name:   "closed before message_stop",
			send:   nativeStreamRecords[:len(nativeStreamRecords)-1],
			want:   strings.Join(nativeStreamRecords[:nativeStreamDelta], "") + gatewayStreamError,
			output: nativeStreamOutput,
		},
		{
			name:   "connection dropped",
			send:   nativeStreamRecords[:nativeStreamText],
			abort:  true,
			want:   strings.Join(nativeStreamRecords[:nativeStreamText], "") + gatewayStreamError,
			output: nativeStreamedOutput,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := newNativeStreamUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
				writeRecords(w, tc.send...)
				if tc.abort {
					panic(http.ErrAbortHandler)
				}
			})
			gateway := setupNativeStreamTest(t, upstream.URL)
			body := streamThroughGateway(t, gateway, "/v1/messages", jsonStr(nativeStreamRequest("first/claude-fixture")))
			if body != tc.want {
				t.Fatalf("the client read\n%q\nwant\n%q", body, tc.want)
			}
			for _, never := range []string{"message_stop", "message_delta"} {
				if strings.Contains(body, never) {
					t.Fatalf("a failed stream carried %s: %q", never, body)
				}
			}
			row := readStreamUsageRow(t)
			if row.status != http.StatusBadGateway || row.code != "upstream_stream" || row.provider != "first" ||
				row.input != nativeStreamInput || row.output != tc.output {
				t.Fatalf("usage=%+v, want a failed stream of %d/%d tokens", row, nativeStreamInput, tc.output)
			}
		})
	}
}

// A native stream moves to the next target only before its first byte, and
// only past a failure the non-streaming chain moves past: an overloaded
// target is left for the next, an invalid request is not, and a stream that
// broke after it opened ends as a failed stream of the target that opened
// it.
func TestNativeMessagesStreamFailsOverOnlyBeforeTheFirstByte(t *testing.T) {
	for _, tc := range []struct {
		name   string
		first  func(w http.ResponseWriter)
		status int
		body   string
		// served is the provider the usage row names, and usage its status.
		served     string
		usage      int
		secondCall int
	}{
		{
			name: "overloaded before the first byte",
			first: func(w http.ResponseWriter) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(529)
				_, _ = io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
			},
			status: http.StatusOK, body: strings.Join(nativeStreamRecords, ""),
			served: "second", usage: http.StatusOK, secondCall: 1,
		},
		{
			name: "invalid request",
			first: func(w http.ResponseWriter) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"invalid"}}`)
			},
			status: http.StatusBadRequest, usage: http.StatusBadRequest,
		},
		{
			name: "broken after the first byte",
			first: func(w http.ResponseWriter) {
				writeRecords(w, nativeStreamRecords[0])
				panic(http.ErrAbortHandler)
			},
			status: http.StatusOK, body: nativeStreamRecords[0] + gatewayStreamError,
			served: "first", usage: http.StatusBadGateway,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := newNativeStreamUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/first/") {
					tc.first(w)
					return
				}
				writeRecords(w, nativeStreamRecords...)
			})
			gateway := setupNativeStreamTest(t, upstream.URL)
			response, body := postMessages(t, gateway, nativeStreamRequest("native"))
			if response.StatusCode != tc.status || (tc.body != "" && body != tc.body) {
				t.Fatalf("status=%d body=%q", response.StatusCode, body)
			}
			if tc.status != http.StatusOK && (response.Header.Get("Content-Type") != "application/json" || !strings.Contains(body, `"type":"invalid_request_error"`)) {
				t.Fatalf("a refusal before the first byte answered %q with %q", response.Header.Get("Content-Type"), body)
			}
			if calls := upstream.callsTo("/first/v1/messages"); calls != 1 {
				t.Fatalf("first target calls=%d, want 1", calls)
			}
			if calls := upstream.callsTo("/second/v1/messages"); calls != tc.secondCall {
				t.Fatalf("second target calls=%d, want %d", calls, tc.secondCall)
			}
			row := readStreamUsageRow(t)
			if row.status != tc.usage || row.provider != tc.served {
				t.Fatalf("usage=%+v, want status %d served by %q", row, tc.usage, tc.served)
			}
		})
	}
}

// A stream on an endpoint with an adapted member keeps the strict
// translation: a field it cannot carry is refused before any target is
// asked, and an answer it can carry is rendered by the gateway, not relayed.
func TestMixedMessagesStreamKeepsTheStrictTranslation(t *testing.T) {
	upstream := newNativeStreamUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		writeRecords(w, nativeStreamRecords...)
	})
	gateway := setupNativeStreamTest(t, upstream.URL)
	response, body := postMessages(t, gateway, nativeStreamRequest("mixed"))
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(body, "Streaming cannot preserve Anthropic request") || upstream.totalCalls() != 0 {
		t.Fatalf("status=%d calls=%d body=%s", response.StatusCode, upstream.totalCalls(), body)
	}

	plain := map[string]any{"model": "mixed", "stream": true, "max_tokens": 64, "messages": []any{map[string]any{"role": "user", "content": "Say hello"}}}
	body = streamThroughGateway(t, gateway, "/v1/messages", jsonStr(plain))
	if upstream.callsTo("/first/v1/messages") != 1 || !strings.Contains(body, "Hello there") || !strings.HasSuffix(body, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n") {
		t.Fatalf("calls=%d body=%q", upstream.callsTo("/first/v1/messages"), body)
	}
	// The gateway renders a translated stream itself, with its own message ID.
	if strings.Contains(body, "msg_fixture") || strings.Contains(body, `{"type": "ping"}`) {
		t.Fatalf("a translated stream was relayed: %q", body)
	}
}

// A client that leaves a native stream, or that can no longer be written
// to, stops its upstream and the chain: no other target is tried, and the
// stream is recorded as cancelled, charged the input its upstream reported.
func TestNativeMessagesStreamCancellationStopsTheUpstream(t *testing.T) {
	for _, tc := range []struct {
		name   string
		writer func(cancel context.CancelFunc) *streamTestWriter
		body   string
	}{
		{
			// The client leaves once the first record reached it.
			name: "client left",
			writer: func(cancel context.CancelFunc) *streamTestWriter {
				return &streamTestWriter{cancel: cancel, cancelAfter: 1}
			},
			body: nativeStreamRecords[0],
		},
		{
			name:   "write failed",
			writer: func(context.CancelFunc) *streamTestWriter { return &streamTestWriter{writeErrAt: 1} },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			left := make(chan struct{}, 1)
			upstream := newNativeStreamUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				writeRecords(w, nativeStreamRecords[0])
				select {
				case <-r.Context().Done():
					left <- struct{}{}
				case <-time.After(5 * time.Second):
				}
			})
			setupNativeStreamTest(t, upstream.URL)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			writer := tc.writer(cancel)
			request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(jsonStr(nativeStreamRequest("native")))).WithContext(ctx)
			request.Header.Set("Content-Type", "application/json")
			done := make(chan struct{})
			go func() {
				NewServer(Runtime{}).ServeHTTP(writer, request)
				close(done)
			}()
			for _, wait := range []chan struct{}{done, left} {
				select {
				case <-wait:
				case <-time.After(5 * time.Second):
					t.Fatal("the stream did not end after its client left")
				}
			}
			if got := writer.body.String(); got != tc.body {
				t.Fatalf("body=%q, want %q", got, tc.body)
			}
			if calls := upstream.callsTo("/second/v1/messages"); calls != 0 {
				t.Fatalf("second target calls=%d after the client left", calls)
			}
			row := readStreamUsageRow(t)
			if row.status != 499 || row.code != "client_cancelled" || row.provider != "first" || row.model != "model-a" || row.input != nativeStreamInput {
				t.Fatalf("usage=%+v", row)
			}
		})
	}
}

// Every native record is read as core's Anthropic reads it: its data lines,
// joined, as one JSON object. A record whose data is not one carries no
// event.
func TestAnthropicRecordEventReadsTheRecordData(t *testing.T) {
	for record, want := range map[string]map[string]any{
		"event: ping\r\ndata: {\"type\": \"ping\"}\r\n\r\n":                   {"type": "ping"},
		"event: message_stop\ndata: {\"type\":\ndata: \"message_stop\"}\n\n":  {"type": "message_stop"},
		": comment\nevent: error\ndata\ndata: {\"type\":\"error\"}\n\n":       {"type": "error"},
		"event: message_stop\ndata: [\"message_stop\"]\n\n":                   nil,
		"event: message_stop\ndata: {\"type\":\"message_stop\"} trailing\n\n": nil,
	} {
		if got := anthropicRecordEvent(record); !reflect.DeepEqual(got, want) {
			t.Errorf("anthropicRecordEvent(%q)=%v, want %v", record, got, want)
		}
	}
}
