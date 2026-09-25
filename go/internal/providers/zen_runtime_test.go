package providers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"

	core "github.com/xibodev/llmgw-core"
)

// An anonymous Responses completion is assembled from the stream Zen sends
// it, as the transport assembled it: the last response object, with the
// streamed text added as a message when its output has none.
func TestAnonymousZenResponsesAssembleTheTerminalResponse(t *testing.T) {
	completed := func(output string) string {
		return "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"output\":" + output + "}}\n\n"
	}
	delta := func(kind, text string) string {
		return "data: {\"type\":\"response." + kind + ".delta\",\"delta\":\"" + text + "\"}\n\n"
	}
	cases := map[string]struct {
		stream string
		types  []string
		text   string
	}{
		"never invents text": {stream: completed("[]")},
		"uses only output text deltas": {
			stream: delta("reasoning_summary_text", "reasoning") + delta("function_call_arguments", "arguments") +
				delta("output_text", "answer") + completed("[]"),
			types: []string{"message"}, text: "answer",
		},
		"keeps a function call": {stream: completed(`[{"type":"function_call","name":"bash","arguments":"{}"}]`), types: []string{"function_call"}},
		"keeps reasoning":       {stream: completed(`[{"type":"reasoning","summary":[]}]`), types: []string{"reasoning"}},
		"adds text beside reasoning": {
			stream: delta("output_text", "remembered") + completed(`[{"type":"reasoning","summary":[]}]`),
			types:  []string{"reasoning", "message"}, text: "remembered",
		},
	}
	for name, fixture := range cases {
		base := zenServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, fixture.stream)
		})
		response, _, err := zenFixture(t, base, "").CompleteResponses("muse-spark-fixture", map[string]any{"input": "hi"})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		output, _ := response["output"].([]any)
		var types []string
		text := ""
		for _, raw := range output {
			item, _ := raw.(map[string]any)
			types = append(types, fmt.Sprint(item["type"]))
			if content, _ := item["content"].([]any); item["type"] == "message" && len(content) > 0 {
				text = fmt.Sprint(content[0].(map[string]any)["text"])
			}
		}
		if strings.Join(types, ",") != strings.Join(fixture.types, ",") || text != fixture.text {
			t.Fatalf("%s: output types=%v text=%q, want %v %q", name, types, text, fixture.types, fixture.text)
		}
	}
}

// zenAuthorizations records the Authorization of each request and answers a
// Chat completion, streamed when the request streams.
type zenAuthorizations struct {
	mu   sync.Mutex
	seen []string
}

func (a *zenAuthorizations) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.seen = append(a.seen, r.Header.Get("Authorization"))
		a.mu.Unlock()
		if zenBody(t, r)["stream"] == true {
			_, _ = fmt.Fprint(w, zenChatStreamFixture)
			return
		}
		_, _ = fmt.Fprint(w, `{"id":"chat_1","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}
}

func (a *zenAuthorizations) last() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.seen) == 0 {
		return ""
	}
	return a.seen[len(a.seen)-1]
}

// Each request resolves the credential through Zen's store in the provider
// factory's order: the caller's connection, the system connection, the
// configured key, and without any the request is anonymous. The facade keeps
// the factory's observation, which only a personal connection makes.
func TestZenRequestsResolveTheFactoryPrecedence(t *testing.T) {
	setupCodexProviderTest(t)
	runtime := InstallForTests(t)
	upstream := &zenAuthorizations{}
	cfg := &config.ProviderConfig{Type: "openai_compatible", RegistryID: "opencode_zen", BaseURL: zenServer(t, upstream.handler(t))}
	config.Update(func(s *config.Settings) { s.Providers = map[string]*config.ProviderConfig{"zen": cfg} })
	human, err := iam.CreatePrincipal("human", "authentik:zen-owner", "", "Zen Owner")
	if err != nil {
		t.Fatal(err)
	}
	owner := core.Caller{ID: human.ID, Kind: core.CallerHuman}
	expect := func(caller core.Caller, want string) *zenProvider {
		t.Helper()
		provider := zenFacade(t, runtime, caller)
		if _, observation, err := provider.CompleteWithObservation("chat", []Message{{"role": "user", "content": "hi"}}, nil); err != nil || upstream.last() != want {
			t.Fatalf("authorization=%q err=%v, want %q", upstream.last(), err, want)
		} else if _, reference, _ := resolveAPIKeyObserved("zen", cfg, caller); fmt.Sprint(observation) != fmt.Sprint(reference) {
			t.Fatalf("observation=%+v, want the factory's %+v", observation, reference)
		}
		return provider
	}

	expect(owner, "Bearer public")
	config.Update(func(s *config.Settings) { s.Providers["zen"].APIKey = "configured-key" })
	expect(owner, "Bearer configured-key")
	if _, err := iam.PutSystemProviderConnection("zen", "api_key", "system-key"); err != nil {
		t.Fatal(err)
	}
	expect(owner, "Bearer system-key")
	connection, err := iam.PutProviderConnection(iam.ProviderConnectionCreate{
		PrincipalID: human.ID, ProviderID: "zen", Kind: "api_key", Secret: "personal-key", MakeDefault: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	personal := expect(owner, "Bearer personal-key")
	expect(gatewayCaller(), "Bearer system-key")

	// A request resolves its own credential, so one the connection no longer
	// holds is never sent, whatever facade the cache still holds.
	if err := iam.RevokeProviderConnection(human.ID, connection.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := personal.Complete("chat", []Message{{"role": "user", "content": "hi"}}, nil); err != nil || upstream.last() != "Bearer system-key" {
		t.Fatalf("authorization=%q err=%v after revocation", upstream.last(), err)
	}

	// A connection of another kind is refused as the factory refused it.
	oauth, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
		PrincipalID: human.ID, ProviderID: "zen", Kind: "openai_codex_oauth", MakeDefault: true,
		AccessToken: "oauth-access", ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.instantiate("zen", cfg, owner); !IsConfig(err) {
		t.Fatalf("an OAuth connection built a Zen facade: err=%v", err)
	}
	store := runtime.verticals[zenCoreType].credentials
	if _, err := store.Load(context.Background(), oauth.ID); !IsConfig(err) {
		t.Fatalf("Zen's store loaded an OAuth connection: err=%v", err)
	}
	if key, err := store.Resolve(context.Background(), gatewayCaller(), "missing"); !errors.Is(err, core.ErrNoCredential) {
		t.Fatalf("an instance without a credential resolved key=%q err=%v", key, err)
	}
}

func drainZenStream(t *testing.T, stream StreamIter, err error) ([]string, error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var chunks []string
	for {
		chunk, ok := stream.Next()
		if !ok {
			return chunks, stream.Err()
		}
		chunks = append(chunks, chunk)
	}
}

// Core relays Zen's records byte for byte; the facade hands the API layer
// their data as the transport's stream did. A Responses stream that ends
// before its terminal event ends without an error, so the API layer writes
// the response.failed that says so.
func TestZenStreamsRelayDataEvents(t *testing.T) {
	const first = `{"id":"c","choices":[{"index":0,"delta":{"content":"a"}}]}`
	const second = `{"id":"c","choices":[{"index":0,"delta":{"content":"b"},"finish_reason":"stop"}]}`
	const created = `{"type":"response.created","response":{"id":"resp_1"}}`
	const text = `{"type":"response.output_text.delta","delta":"x"}`
	var responsesTail string
	base := zenServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if r.URL.Path == "/responses" {
			_, _ = fmt.Fprint(w, "event: response.created\ndata: "+created+"\n\ndata: not json\n\ndata: "+text+"\n\n"+responsesTail)
			return
		}
		_, _ = fmt.Fprint(w, ": keepalive\n\ndata: "+first+"\n\nevent: message\ndata: "+second+"\r\n\r\ndata: [DONE]\n\n")
	})
	provider := zenFixture(t, base, "secret")
	provider.runtime.catalogs.store("zen", []ModelInfo{{ID: "responses-model", SupportedSurfaces: []string{"/responses"}}})

	chat, err := provider.Stream("chat-model", []Message{{"role": "user", "content": "hi"}}, nil)
	chunks, err := drainZenStream(t, chat, err)
	if err != nil || strings.Join(chunks, "|") != first+"|"+second {
		t.Fatalf("chat chunks=%q err=%v", chunks, err)
	}
	stream, _, err := provider.StreamResponses("responses-model", map[string]any{"input": "hi"})
	chunks, err = drainZenStream(t, stream, err)
	if err != nil || strings.Join(chunks, "|") != created+"|"+text {
		t.Fatalf("unterminated chunks=%q err=%v, want both events and no error", chunks, err)
	}
	responsesTail = "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"output\":[]}}\n\n"
	stream, _, err = provider.StreamResponses("responses-model", map[string]any{"input": "hi"})
	if chunks, err = drainZenStream(t, stream, err); err != nil || len(chunks) != 3 {
		t.Fatalf("terminated chunks=%q err=%v", chunks, err)
	}
}

// A model without native Responses is refused before anything is sent, and
// so is, after it, one whose Responses endpoint Zen does not route: the
// router then serves the request over Chat.
func TestZenResponsesFallBackToChat(t *testing.T) {
	requests := 0
	base := zenServer(t, func(w http.ResponseWriter, _ *http.Request) {
		requests++
		http.NotFound(w, nil)
	})
	provider := zenFixture(t, base, "secret")
	provider.runtime.catalogs.store("zen", []ModelInfo{
		{ID: "chat-model", SupportedSurfaces: []string{"/chat/completions"}},
		{ID: "responses-model", SupportedSurfaces: []string{"/responses"}},
	})
	if _, _, err := provider.CompleteResponses("chat-model", map[string]any{"input": "hi"}); !errors.Is(err, ErrResponsesUnsupported) || requests != 0 {
		t.Fatalf("chat model: err=%v requests=%d", err, requests)
	}
	if _, _, err := provider.CompleteResponses("responses-model", map[string]any{"input": "hi"}); !errors.Is(err, ErrResponsesUnsupported) || requests != 1 {
		t.Fatalf("unrouted Responses: err=%v requests=%d", err, requests)
	}
	if _, _, err := provider.StreamResponses("responses-model", map[string]any{"input": "hi"}); !errors.Is(err, ErrResponsesUnsupported) || requests != 2 {
		t.Fatalf("unrouted Responses stream: err=%v requests=%d", err, requests)
	}
}

// Zen's refusals keep the status, Retry-After and dispositions the router and
// the resilience wrapper decided on before.
func TestZenFailuresKeepTheirClassification(t *testing.T) {
	var status int
	requests := 0
	base := zenServer(t, func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, `{"error":{"message":"refused","code":"fixture_code"}}`)
	})
	keyed := zenFixture(t, base, "secret")
	chat := func(provider *zenProvider, model string, kw Kwargs) error {
		_, err := provider.Complete(model, []Message{{"role": "user", "content": "hi"}}, kw)
		return err
	}
	for _, fixture := range []struct {
		status                     int
		retryable, failover, trips bool
	}{
		{http.StatusTooManyRequests, true, true, true},
		{http.StatusServiceUnavailable, true, true, true},
		{http.StatusBadRequest, false, false, false},
		{http.StatusUnauthorized, false, false, false},
	} {
		status = fixture.status
		err := chat(keyed, "chat-model", nil)
		if UpstreamStatus(err) != fixture.status || InvocationRetryAfter(err) != "7" ||
			InvocationRetryable(err) != fixture.retryable || InvocationFailoverEligible(err) != fixture.failover ||
			InvocationCircuitFailure(err) != fixture.trips {
			t.Fatalf("status %d: err=%v retry-after=%q", fixture.status, err, InvocationRetryAfter(err))
		}
		if throttle := fixture.status == http.StatusTooManyRequests; IsThrottle(err) != throttle {
			t.Fatalf("status %d: throttle=%v, want %v", fixture.status, IsThrottle(err), throttle)
		}
	}

	// A Chat request Responses cannot carry is refused before it is sent.
	keyed.runtime.catalogs.store("zen", []ModelInfo{{ID: "responses-model", SupportedSurfaces: []string{"/responses"}}})
	sent := requests
	if err := chat(keyed, "responses-model", Kwargs{"stop": []any{"END"}}); !IsConfig(err) || !strings.Contains(err.Error(), "stop") || requests != sent {
		t.Fatalf("material conversion: err=%v requests=%d", err, requests-sent)
	}

	// Anonymous access is not sent for a model the catalog does not mark
	// free; the refusal keeps the 401 Zen answered it with.
	anonymous := zenFixture(t, base, "")
	anonymous.runtime.catalogs.store("zen", []ModelInfo{{ID: "paid-model", SupportedSurfaces: []string{"/chat/completions"}}})
	sent = requests
	err := chat(anonymous, "paid-model", nil)
	if UpstreamStatus(err) != http.StatusUnauthorized || InvocationFailoverEligible(err) || requests != sent ||
		!strings.Contains(err.Error(), "configure an OpenCode Zen API key") {
		t.Fatalf("anonymous paid model: err=%v requests=%d", err, requests-sent)
	}
}
