package providers

import (
	"bufio"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llmgw/internal/config"
)

// pacedUpstream answers every request with frames, one every gap, and then
// ends the response, or, when stall is set, holds it open without sending
// anything more. It reports on left when the caller leaves early.
func pacedUpstream(t *testing.T, gap time.Duration, stall bool, left chan<- struct{}, frames ...string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
		for index, frame := range frames {
			if index > 0 {
				select {
				case <-r.Context().Done():
					left <- struct{}{}
					return
				case <-time.After(gap):
				}
			}
			_, _ = io.WriteString(w, frame)
			w.(http.Flusher).Flush()
		}
		if stall {
			select {
			case <-r.Context().Done():
				left <- struct{}{}
			case <-time.After(5 * time.Second):
			}
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// silentUpstream reads each request and sends no response until the caller
// leaves, which it reports on left.
func silentUpstream(t *testing.T, left chan<- struct{}) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// The server notices the caller leave only once it has read the body.
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
			left <- struct{}{}
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func chatFrames(pieces int) []string {
	frames := make([]string, 0, pieces+1)
	for range pieces {
		frames = append(frames, `data: {"choices":[{"index":0,"delta":{"content":"piece"},"finish_reason":null}]}`+"\n\n")
	}
	return append(frames, "data: [DONE]\n\n")
}

func anthropicFrames(pieces int) []string {
	frames := []string{`data: {"type":"message_start","message":{"id":"msg_1","model":"model","role":"assistant","content":[],"usage":{"input_tokens":2}}}` + "\n\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n"}
	for range pieces {
		frames = append(frames, `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"piece"}}`+"\n\n")
	}
	return append(frames, `data: {"type":"content_block_stop","index":0}`+"\n\n"+
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`+"\n\n"+
		`data: {"type":"message_stop"}`+"\n\n")
}

func ollamaFrames(pieces int) []string {
	frames := make([]string, 0, pieces+1)
	for range pieces {
		frames = append(frames, `{"message":{"role":"assistant","content":"piece"},"done":false}`+"\n")
	}
	return append(frames, `{"done":true}`+"\n")
}

// A provider's timeout bounds waiting, not answers: a stream that keeps
// arriving runs past it on every type that streams, however long it lasts.
func TestProviderStreamsOutlastTheirTimeout(t *testing.T) {
	const pieces = 6
	timeout, gap := 0.3, 100*time.Millisecond
	messages := []Message{{"role": "user", "content": "hi"}}
	for _, test := range []struct {
		name   string
		frames []string
		open   func(t *testing.T, base string) (StreamIter, error)
	}{
		{"openai_compatible", chatFrames(pieces), func(t *testing.T, base string) (StreamIter, error) {
			return openAICompatibleFixture(t, &config.ProviderConfig{Type: "openai_compatible", BaseURL: base, Timeout: &timeout}).Stream("model", messages, nil)
		}},
		{"azure_openai", chatFrames(pieces), func(t *testing.T, base string) (StreamIter, error) {
			return azureFixture(t, &config.ProviderConfig{Type: "azure_openai", BaseURL: base, APIKey: "fixture-key", Timeout: &timeout}).Stream("model", messages, nil)
		}},
		{"bedrock", chatFrames(pieces), func(t *testing.T, base string) (StreamIter, error) {
			return openAICompatibleFixture(t, &config.ProviderConfig{Type: "bedrock", BaseURL: base, Timeout: &timeout}).Stream("model", messages, nil)
		}},
		{"anthropic", anthropicFrames(pieces), func(t *testing.T, base string) (StreamIter, error) {
			return anthropicFixture(t, &config.ProviderConfig{Type: "anthropic", BaseURL: base, Timeout: &timeout}).Stream("model", messages, nil)
		}},
		{"ollama", ollamaFrames(pieces), func(t *testing.T, base string) (StreamIter, error) {
			return ollamaFixtureTimeout(t, base, timeout).Stream("model", messages, nil)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := pacedUpstream(t, gap, false, make(chan struct{}, 1), test.frames...)
			started := time.Now()
			stream, err := test.open(t, base)
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			var text strings.Builder
			for chunk, more := stream.Next(); more; chunk, more = stream.Next() {
				text.WriteString(chunk)
			}
			if stream.Err() != nil || strings.Count(text.String(), "piece") != pieces {
				t.Fatalf("err=%v chunks=%s, want the whole stream", stream.Err(), text.String())
			}
			if elapsed := time.Since(started); elapsed <= time.Duration(timeout*float64(time.Second)) {
				t.Fatalf("the stream took %v, not longer than the timeout", elapsed)
			}
		})
	}
}

// A stream that sends nothing for its provider's timeout ends with an error,
// and its upstream request is closed.
func TestProviderStreamThatStallsEnds(t *testing.T) {
	timeout := 0.3
	left := make(chan struct{}, 1)
	base := pacedUpstream(t, 0, true, left, chatFrames(1)[0])
	stream, err := openAICompatibleFixture(t, &config.ProviderConfig{Type: "openai_compatible", BaseURL: base, Timeout: &timeout}).
		Stream("model", []Message{{"role": "user", "content": "hi"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	started := time.Now()
	chunks := 0
	for _, more := stream.Next(); more; _, more = stream.Next() {
		chunks++
	}
	if chunks != 1 || stream.Err() == nil || !strings.Contains(stream.Err().Error(), "sent nothing") {
		t.Fatalf("chunks=%d err=%v, want the stall to end the stream", chunks, stream.Err())
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("the stalled stream ended after %v", elapsed)
	}
	select {
	case <-left:
	case <-time.After(5 * time.Second):
		t.Fatal("the stalled upstream request was not closed")
	}
}

// An upstream that never sends its response headers fails the request at its
// provider's timeout, as a transport failure another try may get past.
func TestProviderRequestWithoutHeadersTimesOut(t *testing.T) {
	timeout := 0.3
	left := make(chan struct{}, 2)
	provider := openAICompatibleFixture(t, &config.ProviderConfig{Type: "openai_compatible", BaseURL: silentUpstream(t, left), Timeout: &timeout})
	messages := []Message{{"role": "user", "content": "hi"}}
	for name, call := range map[string]func() error{
		"complete": func() error { _, err := provider.Complete("model", messages, nil); return err },
		"stream": func() error {
			stream, err := provider.Stream("model", messages, nil)
			if stream != nil {
				_ = stream.Close()
			}
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			started := time.Now()
			if err := call(); err == nil || !InvocationRetryable(err) {
				t.Fatalf("err=%v, want a retryable transport failure", err)
			}
			if elapsed := time.Since(started); elapsed > 3*time.Second {
				t.Fatalf("the request waited %v for headers", elapsed)
			}
			select {
			case <-left:
			case <-time.After(5 * time.Second):
				t.Fatal("the upstream request was not closed")
			}
		})
	}
}

// Only time spent waiting for the upstream counts against the timeout: a
// reader that pauses longer between reads keeps its stream.
func TestProviderClientCountsOnlyTheWaitForTheUpstream(t *testing.T) {
	base := pacedUpstream(t, 0, false, make(chan struct{}, 1), "first\n", "second\n")
	response, err := providerClient(0.1).Get(base)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	if line, err := reader.ReadString('\n'); err != nil || line != "first\n" {
		t.Fatalf("line=%q err=%v", line, err)
	}
	time.Sleep(300 * time.Millisecond)
	rest, err := io.ReadAll(reader)
	if err != nil || string(rest) != "second\n" {
		t.Fatalf("rest=%q err=%v", rest, err)
	}
}

// A body closed while a read waits ends that read with the close, not with
// an idle timeout it never reached.
func TestIdleBodyCloseEndsAWaitingRead(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	body := newIdleBody(reader, time.Hour)
	done := make(chan error, 1)
	go func() {
		_, err := body.Read(make([]byte, 8))
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	_ = body.Close()
	select {
	case err := <-done:
		var idle *idleTimeoutError
		if err == nil || errors.As(err, &idle) {
			t.Fatalf("err=%v, want the closed body's error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("closing the body did not end the read")
	}
}

// A provider client with no timeout waits indefinitely, as a zero
// Client.Timeout did, on the shared transport.
func TestProviderClientWithoutTimeout(t *testing.T) {
	for _, timeout := range []float64{0, -1} {
		client := providerClient(timeout)
		if client.Timeout != 0 || client.Transport != nil {
			t.Fatalf("timeout %v: client=%+v", timeout, client)
		}
	}
}
