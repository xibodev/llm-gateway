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

	"llmgw/internal/iam"
)

// A stream whose upstream reported usage is recorded with exactly those
// counts; one whose upstream reported none is recorded with an estimate: a
// token per four bytes of the prompt sent, and the streamed text as
// llm-translate estimates it.
func TestStreamUsageIsReportedOrEstimated(t *testing.T) {
	upstream := newStreamOutcomeUpstream(t)
	// The Chat and Messages prompt is [{"content":"Say hello","role":"user"}],
	// 39 bytes; the Responses prompt is the input "Say hello", 11 bytes. The
	// one streamed text, "Hello there", is 11 bytes.
	for _, tc := range []struct {
		name, path, provider string
		estimatedInput       int
	}{
		{"Chat", "/v1/chat/completions", "chat", 10},
		{"Messages", "/v1/messages", "chat", 10},
		{"native Responses", "/v1/responses", "native", 3},
		{"Responses from Chat", "/v1/responses", "chat", 3},
	} {
		for _, model := range []string{"complete", "unmetered"} {
			t.Run(tc.name+" "+model, func(t *testing.T) {
				gateway := setupStreamOutcomeTest(t, upstream.URL)
				streamThroughGateway(t, gateway, tc.path, streamRequestBody(tc.path, tc.provider+"/"+model))
				row := readStreamUsageRow(t)
				wantInput, wantOutput := 11, 7
				if model == "unmetered" {
					wantInput, wantOutput = tc.estimatedInput, 2
				}
				if row.status != http.StatusOK || row.input != wantInput || row.output != wantOutput || row.credits != 1000 {
					t.Fatalf("usage=%+v, want %d/%d tokens", row, wantInput, wantOutput)
				}
			})
		}
	}
}

// The output estimate counts every text a stream sends a client, each piece
// at a quarter of its bytes and at least one token: content, refusals,
// reasoning once however the upstream names it, and tool arguments, but not
// encoded audio. A usage the upstream reports replaces the estimate, except
// for a zero, which some upstreams send before their last chunk.
func TestStreamUsageEstimatesTheStreamedText(t *testing.T) {
	chat := newStreamUsage("openai.chat", "m", nil, nil, time.Now(), 40)
	for _, chunk := range []string{
		`{"choices":[{"delta":{"content":"abcdefgh"}}],"usage":{"prompt_tokens":0,"completion_tokens":0}}`,
		`{"choices":[{"delta":{"refusal":"no"}}]}`,
		`{"choices":[{"delta":{"reasoning_content":"abcdefgh","reasoning":"abcdefgh"}}]}`,
		`{"choices":[{"delta":{"reasoning":"abcd"}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"lookup","arguments":"{\"q\":1}"}}]}}]}`,
		`not JSON`,
	} {
		chat.chatChunk(chunk)
	}
	// 2 + 1 + 2 + 1 + 1 output tokens; ten for 40 prompt bytes.
	if input, output := chat.tokens(); input != 10 || output != 7 {
		t.Fatalf("chat estimate=%d/%d, want 10/7", input, output)
	}
	chat.chatChunk(`{"choices":[],"usage":{"prompt_tokens":123,"completion_tokens":45}}`)
	if input, output := chat.tokens(); input != 123 || output != 45 {
		t.Fatalf("chat reported=%d/%d, want 123/45", input, output)
	}

	responses := newStreamUsage("openai.responses", "m", nil, nil, time.Now(), 3)
	for _, event := range []map[string]any{
		{"type": "response.created", "response": map[string]any{"usage": nil}},
		{"type": "response.output_text.delta", "delta": "abcdefgh"},
		{"type": "response.function_call_arguments.delta", "delta": `{"q":1}`},
		{"type": "response.audio.delta", "delta": strings.Repeat("A", 4000)},
		{"type": "response.output_text.done", "text": "abcdefgh"},
	} {
		responses.responsesEvent(event)
	}
	if input, output := responses.tokens(); input != 1 || output != 3 {
		t.Fatalf("responses estimate=%d/%d, want 1/3", input, output)
	}
	responses.responsesEvent(map[string]any{"type": "response.completed", "response": map[string]any{
		"usage": map[string]any{"input_tokens": 9.0, "output_tokens": 8.0},
	}})
	if input, output := responses.tokens(); input != 9 || output != 8 {
		t.Fatalf("responses reported=%d/%d, want 9/8", input, output)
	}
}

// A Chat client that asks for its stream's usage gets it: stream_options
// reaches the upstream, whose usage chunk reaches the client, and the
// stream is recorded with exactly that usage. A request that does not
// stream never sends stream_options, which upstreams refuse there.
func TestChatStreamForwardsStreamOptions(t *testing.T) {
	var mu sync.Mutex
	var sent map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		sent = body
		mu.Unlock()
		if body["stream"] != true {
			writeJSON(w, http.StatusOK, map[string]any{
				"id": "chatcmpl_1", "object": "chat.completion", "model": "model",
				"choices": []any{map[string]any{"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": "Hello there"}}},
				"usage":   map[string]any{"prompt_tokens": 21, "completion_tokens": 5, "total_tokens": 26},
			})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"chatcmpl_1","object":"chat.completion.chunk","model":"model","choices":[{"index":0,"delta":{"content":"Hello there"},"finish_reason":"stop"}]}`+"\n\n")
		if options, _ := body["stream_options"].(map[string]any); options["include_usage"] == true {
			_, _ = io.WriteString(w, `data: {"id":"chatcmpl_1","object":"chat.completion.chunk","model":"model","choices":[],"usage":{"prompt_tokens":21,"completion_tokens":5,"total_tokens":26}}`+"\n\n")
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(upstream.Close)
	const messages = `"messages":[{"role":"user","content":"Say hello"}]`
	for _, tc := range []struct {
		name, body          string
		stream, wantOptions bool
		wantInput, wantOut  int
	}{
		// Without usage the stream is estimated: ten prompt tokens for 39
		// bytes, two for the 11 streamed.
		{"stream", `{"model":"chat/model","stream":true,` + messages + `}`, true, false, 10, 2},
		{"stream with usage", `{"model":"chat/model","stream":true,"stream_options":{"include_usage":true},` + messages + `}`, true, true, 21, 5},
		{"no stream", `{"model":"chat/model","stream_options":{"include_usage":true},` + messages + `}`, false, false, 21, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gateway := setupStreamOutcomeTest(t, upstream.URL)
			if tc.stream {
				body := streamThroughGateway(t, gateway, "/v1/chat/completions", tc.body)
				if got := strings.Contains(body, `"completion_tokens":5`); got != tc.wantOptions {
					t.Fatalf("usage chunk relayed=%v body=%q", got, body)
				}
			} else {
				response, err := http.Post(gateway.URL+"/v1/chat/completions", "application/json", strings.NewReader(tc.body))
				if err != nil {
					t.Fatal(err)
				}
				_, _ = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
				if response.StatusCode != http.StatusOK {
					t.Fatalf("status=%d", response.StatusCode)
				}
			}
			mu.Lock()
			options, forwarded := sent["stream_options"]
			mu.Unlock()
			if forwarded != tc.wantOptions || (forwarded && !reflect.DeepEqual(options, map[string]any{"include_usage": true})) {
				t.Fatalf("upstream stream_options=%#v, want forwarded %v", options, tc.wantOptions)
			}
			if row := readStreamUsageRow(t); row.status != http.StatusOK || row.input != tc.wantInput || row.output != tc.wantOut {
				t.Fatalf("usage=%+v, want %d/%d tokens", row, tc.wantInput, tc.wantOut)
			}
		})
	}
}

// A client that leaves a stream midway still consumed what the upstream
// streamed until then: the stream is recorded as cancelled, charged those
// tokens, their credit and their cost, against the model that served it.
func TestCancelledStreamIsChargedWhatItConsumed(t *testing.T) {
	const chunk = `data: {"id":"chatcmpl_1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"content":"abcdefgh"},"finish_reason":null}]}` + "\n\n"
	upstreamDone := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, strings.Repeat(chunk, 3))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		upstreamDone <- struct{}{}
	}))
	t.Cleanup(upstream.Close)
	setupStreamOutcomeTest(t, upstream.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The client leaves once the third chunk reached it.
	writer := &streamTestWriter{cancel: cancel, cancelAfter: 3}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(streamRequestBody("/v1/chat/completions", "chat/gpt-4o"))).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	done := make(chan struct{})
	go func() {
		NewServer(Runtime{}).ServeHTTP(writer, request)
		close(done)
	}()
	for _, wait := range []chan struct{}{done, upstreamDone} {
		select {
		case <-wait:
		case <-time.After(5 * time.Second):
			t.Fatal("the stream did not end after its client left")
		}
	}
	if got := strings.Count(writer.body.String(), "abcdefgh"); got != 3 || strings.Contains(writer.body.String(), "[DONE]") {
		t.Fatalf("body=%q", writer.body.String())
	}
	row := readStreamUsageRow(t)
	// The prompt is 39 bytes, ten tokens; each chunk streamed 8 bytes, two.
	if row.status != 499 || row.code != "client_cancelled" || row.provider != "chat" || row.model != "gpt-4o" ||
		row.input != 10 || row.output != 6 || row.credits != 1000 {
		t.Fatalf("usage=%+v", row)
	}
	db, err := iam.DB()
	if err != nil {
		t.Fatal(err)
	}
	var cost int64
	if err := db.QueryRow(`SELECT cost_microusd FROM usage_events`).Scan(&cost); err != nil || cost <= 0 {
		t.Fatalf("cost=%d micro-USD err=%v, want the served model's price", cost, err)
	}
}
