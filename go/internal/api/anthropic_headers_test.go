package api

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// A Messages client's anthropic-version and anthropic-beta reach a native
// Anthropic target, streamed or not, so beta features it asked for are
// served; a credential header the client sent never does, and a Chat
// request, which carries no such headers, sends none.
func TestAnthropicClientHeadersReachNativeMessagesTargets(t *testing.T) {
	var mu sync.Mutex
	var last http.Header
	var upstream *nativeStreamUpstream
	upstream = newNativeStreamUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		last = r.Header.Clone()
		mu.Unlock()
		if upstream.body(r.URL.Path)["stream"] == true {
			writeRecords(w, nativeStreamRecords...)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_fixture","type":"message","role":"assistant","model":"claude-fixture","content":[{"type":"text","text":"Hello"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`)
	})
	gateway := setupNativeStreamTest(t, upstream.URL)
	sentHeaders := func() http.Header {
		mu.Lock()
		defer mu.Unlock()
		sent := last
		last = nil
		return sent
	}

	for _, stream := range []bool{true, false} {
		body := nativeStreamRequest("first/model-a")
		body["stream"] = stream
		request, err := http.NewRequest(http.MethodPost, gateway.URL+"/v1/messages", strings.NewReader(jsonStr(body)))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("anthropic-version", "2023-06-01")
		request.Header.Add("anthropic-beta", "interleaved-thinking-2025-05-14")
		request.Header.Add("anthropic-beta", "context-1m-2025-08-07")
		request.Header.Set("x-api-key", "client-sent-key")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		answer, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("stream=%v: status=%d body=%s", stream, response.StatusCode, answer)
		}
		sent := sentHeaders()
		if sent == nil {
			t.Fatalf("stream=%v: the upstream received no request", stream)
		}
		betas := strings.Join(sent.Values("anthropic-beta"), ",")
		if !strings.Contains(betas, "interleaved-thinking-2025-05-14") || !strings.Contains(betas, "context-1m-2025-08-07") {
			t.Fatalf("stream=%v: anthropic-beta=%q", stream, betas)
		}
		if sent.Get("anthropic-version") != "2023-06-01" {
			t.Fatalf("stream=%v: anthropic-version=%q", stream, sent.Get("anthropic-version"))
		}
		if sent.Get("x-api-key") == "client-sent-key" {
			t.Fatalf("stream=%v: the client's key reached the upstream", stream)
		}
	}

	chat, err := http.Post(gateway.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"first/model-a","stream":true,"messages":[{"role":"user","content":"Say hello"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(chat.Body)
	chat.Body.Close()
	if sent := sentHeaders(); sent == nil || len(sent.Values("anthropic-beta")) != 0 {
		t.Fatalf("a Chat request sent anthropic-beta or nothing: %v", sent)
	}
}
