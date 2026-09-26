package providers

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"llmgw/internal/config"
)

// openAIOperations are the four operations of a facade, by name.
func openAIOperations(provider *openAICompatibleProvider) map[string]func() error {
	hi := []Message{{"role": "user", "content": "hi"}}
	return map[string]func() error{
		"chat": func() error { _, err := provider.Complete("fixture-model", hi, nil); return err },
		"chat stream": func() error {
			_, err := provider.Stream("fixture-model", hi, nil)
			return err
		},
		"responses": func() error {
			_, _, err := provider.CompleteResponses("fixture-model", map[string]any{"input": "hi"})
			return err
		},
		"responses stream": func() error {
			_, _, err := provider.StreamResponses("fixture-model", map[string]any{"input": "hi"})
			return err
		},
	}
}

// Core's failures map back to the errors the transport returned, Bedrock's
// as the OpenAI-compatible instances', since both ran on it: a refusal keeps
// its status, its Retry-After and the transport's message with the
// upstream's words, and a request that got no answer keeps the transport's
// message, its cause and its disposition.
func TestOpenAICompatibleFailuresKeepTheTransportsErrors(t *testing.T) {
	for _, providerType := range []string{"openai_compatible", "bedrock"} {
		var status int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"error":{"message":"slow down","code":"fixture_code"}}`)
		}))
		t.Cleanup(server.Close)
		provider := openAICompatibleFixture(t, &config.ProviderConfig{Type: providerType, RegistryID: "openai", BaseURL: server.URL, APIKey: "fixture-key"})
		for name, call := range openAIOperations(provider) {
			want := "openai: upstream returned %d: slow down"
			if strings.HasPrefix(name, "responses") {
				want = "openai: responses endpoint returned %d: slow down"
			}
			for status0, transient := range map[int]bool{http.StatusTooManyRequests: true, http.StatusBadRequest: false} {
				status = status0
				err := call()
				if err == nil || err.Error() != fmt.Sprintf(want, status) || UpstreamStatus(err) != status ||
					InvocationRetryAfter(err) != "7" || InvocationRetryable(err) != transient || InvocationFailoverEligible(err) != transient {
					t.Fatalf("%s %s %d: err=%v retry-after=%q", providerType, name, status, err, InvocationRetryAfter(err))
				}
			}
		}

		closed := httptest.NewServer(http.NotFoundHandler())
		closed.Close()
		unanswered := openAICompatibleFixture(t, &config.ProviderConfig{Type: providerType, RegistryID: "openai", BaseURL: closed.URL})
		for name, call := range openAIOperations(unanswered) {
			prefix, path := map[string]string{
				"chat": "openai: upstream transport error: ", "chat stream": "openai: streaming transport error: ",
				"responses": "openai: responses transport error: ", "responses stream": "openai: responses streaming transport error: ",
			}[name], "/chat/completions"
			if strings.HasPrefix(name, "responses") {
				path = "/responses"
			}
			err := call()
			if err == nil || !strings.HasPrefix(err.Error(), prefix+`Post "`+closed.URL+path+`"`) || !InvocationRetryable(err) || !InvocationCircuitFailure(err) {
				t.Fatalf("%s %s unanswered: err=%v, want the prefix %q", providerType, name, err, prefix)
			}
		}
	}
}

// An answer that cannot be used keeps the transport's message, and counts
// against the circuit without being repeated.
func TestOpenAICompatibleUnusableAnswersKeepTheTransportsMessages(t *testing.T) {
	var body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }))
	t.Cleanup(server.Close)
	provider := openAICompatibleFixture(t, &config.ProviderConfig{Type: "openai_compatible", RegistryID: "openai", BaseURL: server.URL})
	operations := openAIOperations(provider)
	for _, fixture := range []struct{ operation, body, want string }{
		{"chat", `{"id":`, "openai: invalid JSON in upstream response"},
		{"chat", `{}`, "openai: invalid JSON in upstream response"},
		{"chat", `{"choices":[]}`, "openai: invalid chat response payload"},
		{"chat", `{"choices":["plain"]}`, "openai: invalid chat response payload"},
		{"responses", `{"id":`, "openai: invalid JSON in responses payload"},
		{"responses", `{"id":"resp_1"}`, "openai: invalid Responses payload"},
	} {
		body = fixture.body
		err := operations[fixture.operation]()
		if err == nil || err.Error() != fixture.want || InvocationRetryable(err) || !InvocationCircuitFailure(err) || UpstreamStatus(err) != 0 {
			t.Fatalf("%s %s: err=%v, want %q", fixture.operation, fixture.body, err, fixture.want)
		}
	}
}

// A model without native Responses is refused before anything is sent, and
// so is, after it, one whose Responses endpoint the upstream does not route:
// the router then serves the request over Chat.
func TestOpenAICompatibleResponsesFallBackToChat(t *testing.T) {
	upstream, base := newOpenAIUpstream(t)
	upstream.setAnswer(func(w http.ResponseWriter, call openAICall) {
		if strings.Contains(call.body, `"stream":true`) {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		http.NotFound(w, nil)
	})
	provider := openAICompatibleFixture(t, &config.ProviderConfig{Type: "openai_compatible", BaseURL: base, APIKey: "fixture-key"})
	storeOpenAICatalog(provider,
		ModelInfo{ID: "chat-model", SupportedSurfaces: []string{"/chat/completions"}},
		ModelInfo{ID: "responses-model", SupportedSurfaces: []string{"/chat/completions", "/responses"}},
	)
	payload := map[string]any{"input": "hi"}
	if _, observation, err := provider.CompleteResponses("chat-model", payload); !errors.Is(err, ErrResponsesUnsupported) || observation != nil || len(upstream.take()) != 0 {
		t.Fatalf("chat model: err=%v observation=%v", err, observation)
	}
	if _, _, err := provider.StreamResponses("chat-model", payload); !errors.Is(err, ErrResponsesUnsupported) || len(upstream.take()) != 0 {
		t.Fatalf("chat model stream: err=%v", err)
	}
	if _, _, err := provider.CompleteResponses("responses-model", payload); !errors.Is(err, ErrResponsesUnsupported) || len(upstream.take()) != 1 {
		t.Fatalf("unrouted Responses: err=%v", err)
	}
	if _, _, err := provider.StreamResponses("responses-model", payload); !errors.Is(err, ErrResponsesUnsupported) || len(upstream.take()) != 1 {
		t.Fatalf("unrouted Responses stream: err=%v", err)
	}
}

// Core relays the upstream's records byte for byte; the facade hands the API
// layer their data as the transport's stream did, and ends as it ended:
// without an error at the stream's end, the Responses stream's missing
// terminal event included, with a StreamRecordTooLargeError for a record
// over the size limit, and with the transport's streaming error, not
// repeated, when the stream broke.
func TestOpenAICompatibleStreamsRelayDataEvents(t *testing.T) {
	const first = `{"choices":[{"index":0,"delta":{"content":"a"}}]}`
	const second = `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`
	var response string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, response)
		if strings.HasSuffix(response, "break") {
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		}
	}))
	t.Cleanup(server.Close)
	provider := openAICompatibleFixture(t, &config.ProviderConfig{Type: "openai_compatible", RegistryID: "openai", BaseURL: server.URL})
	drain := func(responses bool) ([]string, error) {
		t.Helper()
		var stream StreamIter
		var err error
		if responses {
			stream, _, err = provider.StreamResponses("fixture-model", map[string]any{"input": "hi"})
		} else {
			stream, err = provider.Stream("fixture-model", []Message{{"role": "user", "content": "hi"}}, nil)
		}
		if err != nil {
			t.Fatal(err)
		}
		defer stream.Close()
		var chunks []string
		for chunk, ok := stream.Next(); ok; chunk, ok = stream.Next() {
			chunks = append(chunks, chunk)
		}
		return chunks, stream.Err()
	}
	for _, responses := range []bool{false, true} {
		response = ": keepalive\n\ndata: " + first + "\n\nevent: message\ndata: " + second + "\r\n\r\ndata: [DONE]\n\n"
		if chunks, err := drain(responses); err != nil || strings.Join(chunks, "|") != first+"|"+second {
			t.Fatalf("responses=%v: chunks=%q err=%v", responses, chunks, err)
		}
		response = "data: " + strings.Repeat("x", maxStreamRecordWireSize) + "\n\n"
		var sizeErr *StreamRecordTooLargeError
		if _, err := drain(responses); !errors.As(err, &sizeErr) {
			t.Fatalf("responses=%v oversized record: err=%#v", responses, err)
		}
		prefix := map[bool]string{false: "openai", true: "responses"}[responses] + ": streaming transport error: "
		response = "data: " + first + "\n\ndata: {\"choices\":[" + "break"
		if chunks, err := drain(responses); len(chunks) != 1 || !IsInvocation(err) || !strings.HasPrefix(err.Error(), prefix) || InvocationRetryable(err) {
			t.Fatalf("responses=%v broken stream: chunks=%q err=%v", responses, chunks, err)
		}
	}
}
