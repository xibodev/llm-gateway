package providers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"

	gcpauth "github.com/xibodev/llm-provider-auth/gcp"
	core "github.com/xibodev/llmgw-core"
)

var googleChat = []Message{{"role": "user", "content": "hi"}}

// An answer the transport could not use keeps the transport's message and
// routing: an answer that is not the JSON it should be counts against the
// provider without a retry, and a request that got no complete answer may
// repeat.
func TestGoogleFacadeKeepsTheTransportsFailures(t *testing.T) {
	answers := map[string]func(http.ResponseWriter){
		"not-json":     func(w http.ResponseWriter) { _, _ = io.WriteString(w, "not json") },
		"empty-object": func(w http.ResponseWriter) { _, _ = io.WriteString(w, "{}") },
		"cut-short": func(w http.ResponseWriter) {
			w.Header().Set("Content-Length", "64")
			_, _ = io.WriteString(w, `{"candidates":`)
		},
		"too-large": func(w http.ResponseWriter) {
			_, _ = io.Copy(w, io.LimitReader(zeroReader{}, inferenceMaxResponseBytes+1))
		},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		answers[strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/models/"), ":generateContent")](w)
	}))
	defer server.Close()
	provider := studioFixture(t, server.URL, "k")
	for model, fixture := range map[string]struct {
		want               string
		retryable, circuit bool
	}{
		"not-json":     {"ai_studio: invalid JSON in upstream response", false, true},
		"empty-object": {"ai_studio: invalid JSON in upstream response", false, true},
		"cut-short":    {"ai_studio: response body transport error: unexpected EOF", true, true},
		"too-large":    {"ai_studio: response body exceeded the size limit", false, true},
	} {
		_, err := provider.Complete(model, googleChat, nil)
		if err == nil || err.Error() != fixture.want || UpstreamStatus(err) != 0 ||
			InvocationRetryable(err) != fixture.retryable || InvocationCircuitFailure(err) != fixture.circuit {
			t.Fatalf("%s: err=%v retryable=%v circuit=%v, want %q", model, err, InvocationRetryable(err), InvocationCircuitFailure(err), fixture.want)
		}
	}
	server.Close()
	_, err := provider.Complete("m", googleChat, nil)
	if prefix := `ai_studio: Post "` + server.URL + `/models/m:generateContent": `; err == nil ||
		!strings.HasPrefix(err.Error(), prefix) || !InvocationRetryable(err) || !InvocationCircuitFailure(err) {
		t.Fatalf("unreachable: err=%v, want a retryable error starting %q", err, prefix)
	}
}

// What the transport refused before sending anything, the facade refuses
// before it resolves a credential or sends anything, with the same error.
func TestGoogleFacadeRefusesBeforeSending(t *testing.T) {
	upstream := &googleUpstream{}
	server := httptest.NewServer(upstream)
	defer server.Close()
	studio := studioFixture(t, server.URL, "k")
	blank := &ConfigError{Msg: "a model name is required"}
	for name, fixture := range map[string]struct {
		call func() error
		want error
	}{
		"blank chat model":    {func() error { _, err := studio.Complete(" ", googleChat, nil); return err }, blank},
		"bare models/ prefix": {func() error { _, err := studio.Complete("models/", googleChat, nil); return err }, blank},
		"blank embeddings":    {func() error { _, err := studio.Embed(context.Background(), "", "hi"); return err }, blank},
		"blank image model":   {func() error { _, _, err := studio.GenerateImages("", "a crane", 1); return err }, blank},
		"blank prompt": {
			func() error { _, _, err := studio.GenerateImages("m", " ", 1); return err },
			&InvocationError{Msg: "ai_studio: a prompt is required"},
		},
		"blank stream model": {func() error { _, err := studio.Stream(" ", googleChat, nil); return err }, blank},
	} {
		if err := fixture.call(); err == nil || err.Error() != fixture.want.Error() || IsConfig(err) != IsConfig(fixture.want) {
			t.Fatalf("%s: err=%#v, want %#v", name, err, fixture.want)
		}
	}
	vertex := vertexFixture(t, server.URL, "k", "", "global")
	if _, err := vertex.Complete("m", googleChat, nil); !IsConfig(err) || err.Error() != "vertex_ai: project is required (set 'project' on the provider)" {
		t.Fatalf("Vertex AI without a project: err=%v", err)
	}
	if calls := upstream.count(); calls != 0 {
		t.Fatalf("refusals sent %d requests", calls)
	}
}

// A service account whose token exchange fails reports it as the transport
// did, before anything is sent: a refusal keeps its status and permits
// failover unless it is 401 or 403, and a token endpoint out of reach may
// repeat.
func TestGoogleFacadeTokenFailuresKeepTheTransportsErrors(t *testing.T) {
	setupVertexIAM(t)
	upstream := vertexModelServer(t)
	var status atomic.Int32
	tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(int(status.Load()))
		_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"fixture refusal"}`)
	}))
	defer tokens.Close()
	routeGoogleTokenEndpoint(t, tokens)
	if _, err := iam.PutSystemProviderConnection("vertex_ai", gcpauth.CredentialKind, serviceAccountFixture(t)); err != nil {
		t.Fatal(err)
	}
	provider, err := GetProviderForPrincipal("vertex_ai", gatewayCaller())
	if err != nil {
		t.Fatal(err)
	}
	const want = "provider 'vertex_ai': service account token refresh failed"
	for _, fixture := range []struct {
		status              int
		retryable, failover bool
	}{
		{http.StatusBadRequest, false, true},
		{http.StatusServiceUnavailable, true, true},
		{http.StatusUnauthorized, false, false},
	} {
		status.Store(int32(fixture.status))
		_, err := provider.Complete("gemini-test", googleChat, nil)
		if err == nil || err.Error() != want || UpstreamStatus(err) != fixture.status ||
			InvocationRetryable(err) != fixture.retryable || InvocationFailoverEligible(err) != fixture.failover {
			t.Fatalf("token endpoint %d: err=%v status=%d retryable=%v failover=%v", fixture.status, err, UpstreamStatus(err),
				InvocationRetryable(err), InvocationFailoverEligible(err))
		}
	}
	tokens.Close()
	if _, err := provider.Complete("gemini-test", googleChat, nil); err == nil || err.Error() != want ||
		UpstreamStatus(err) != 0 || !InvocationRetryable(err) {
		t.Fatalf("unreachable token endpoint: err=%v", err)
	}
	if calls := upstream.count(); calls != 0 {
		t.Fatalf("%d requests were sent without a token", calls)
	}
}

// Chat, embeddings and images resolve the credential of each request
// through Google's store, so a cached facade never sends one its connection
// no longer holds.
func TestGoogleFacadeResolvesEachRequestsCredential(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	InstallForTests(t)
	image := base64.StdEncoding.EncodeToString([]byte{0x89, 'P', 'N', 'G'})
	var mu sync.Mutex
	var sent []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sent = append(sent, r.Header.Get("x-goog-api-key"))
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, ":embedContent") {
			_, _ = io.WriteString(w, `{"embedding":{"values":[0.5]}}`)
			return
		}
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"ok"},{"inlineData":{"mimeType":"image/png","data":"`+image+`"}}]}}]}`)
	}))
	defer server.Close()
	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{"studio": {Type: "ai_studio", BaseURL: server.URL}}
	})
	if _, err := iam.PutSystemProviderConnection("studio", "api_key", "system-key"); err != nil {
		t.Fatal(err)
	}
	human, _ := iam.CreatePrincipal("human", "authentik:google-requests", "", "Owner")
	connection, err := iam.PutProviderConnection(iam.ProviderConnectionCreate{
		PrincipalID: human.ID, ProviderID: "studio", Kind: "api_key", Secret: "personal-key", MakeDefault: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	provider, err := GetProviderForPrincipal("studio", core.Caller{ID: human.ID, Kind: core.CallerHuman})
	if err != nil {
		t.Fatal(err)
	}
	embedder, _ := AsEmbeddingProvider(provider)
	generator, _ := AsImageGenerator(provider)
	expect := func(want string) {
		t.Helper()
		mu.Lock()
		sent = nil
		mu.Unlock()
		_, chatErr := provider.Complete("gemini-fixture", googleChat, nil)
		_, embedErr := embedder.Embed(context.Background(), "gemini-embedding-fixture", "hi")
		_, _, imageErr := generator.GenerateImages("gemini-image-fixture", "a crane", 1)
		mu.Lock()
		got := strings.Join(sent, ",")
		mu.Unlock()
		if chatErr != nil || embedErr != nil || imageErr != nil || got != want+","+want+","+want {
			t.Fatalf("sent %s (chat %v, embeddings %v, images %v), want %q for each", got, chatErr, embedErr, imageErr, want)
		}
	}
	expect("personal-key")
	if err := iam.RevokeProviderConnection(human.ID, connection.ID); err != nil {
		t.Fatal(err)
	}
	expect("system-key")
}

// Core's answers reach the caller as the transport's did: embeddings keep
// their order, values and usage, under the model without its prefix.
func TestGoogleFacadeEmbeddingsKeepTheirValues(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"embedding":{"values":[0.1,-0.25,3e-7]},"usageMetadata":{"promptTokenCount":2}}`)
	}))
	defer server.Close()
	result, err := studioFixture(t, server.URL, "k").Embed(context.Background(), "models/gemini-embedding-001", []any{"hi"})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(result)
	if want := `{"data":[{"embedding":[0.1,-0.25,3e-7],"index":0,"object":"embedding"}],"model":"gemini-embedding-001",` +
		`"object":"list","usage":{"prompt_tokens":2,"total_tokens":2}}`; string(encoded) != want {
		t.Fatalf("embeddings = %s\nwant %s", encoded, want)
	}
}

// googleToolFixture is a Chat request's tool and tool choice, as the API
// layer hands them on.
var googleToolFixture = Kwargs{
	"tools": []any{map[string]any{"type": "function", "function": map[string]any{
		"name": "lookup", "parameters": map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}},
	}}},
	"tool_choice": "required", "temperature": 0.2, "max_completion_tokens": json.Number("64"), "top_p": 0.9,
}

// googleBodies is a synthetic Gemini API that records each request's path,
// query and body, and answers a stream with stream and anything else with
// answer.
func googleBodies(t *testing.T, answer, stream string) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, r.URL.Path+"?"+r.URL.RawQuery+" "+string(body))
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, ":streamGenerateContent") {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, stream)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, answer)
	}))
	t.Cleanup(server.Close)
	return server, func() []string {
		mu.Lock()
		defer mu.Unlock()
		taken := seen
		seen = nil
		return taken
	}
}

// A Chat request's tools and tool choice reach Gemini as function
// declarations and its function calling config, and Gemini's function call
// comes back as a tool call that finishes the answer.
func TestGoogleFacadeSendsToolsAndReturnsToolCalls(t *testing.T) {
	server, take := googleBodies(t, `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{"q":"crane"}},`+
		`"thoughtSignature":"c2ln"}]},"finishReason":"STOP"}],"responseId":"resp-fixture","usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3,"totalTokenCount":8}}`, "")
	completion, err := studioFixture(t, server.URL, "k").Complete("gemini-fixture", googleChat, googleToolFixture)
	if err != nil {
		t.Fatal(err)
	}
	want := `/models/gemini-fixture:generateContent? {"contents":[{"parts":[{"text":"hi"}],"role":"user"}],"generationConfig":{"maxOutputTokens":64,"temperature":0.2},` +
		`"toolConfig":{"functionCallingConfig":{"mode":"ANY"}},"tools":[{"functionDeclarations":[{"name":"lookup",` +
		`"parametersJsonSchema":{"properties":{"q":{"type":"string"}},"type":"object"}}]}]}`
	if seen := take(); len(seen) != 1 || seen[0] != want {
		t.Fatalf("upstream = %q\nwant %q", seen, want)
	}
	choice := completion["choices"].([]any)[0].(map[string]any)
	calls, _ := choice["message"].(map[string]any)["tool_calls"].([]any)
	if choice["finish_reason"] != "tool_calls" || len(calls) != 1 {
		t.Fatalf("completion = %+v", completion)
	}
	call := calls[0].(map[string]any)
	if call["id"] != "call_resp-fixture_0" || call["function"].(map[string]any)["arguments"] != `{"q":"crane"}` ||
		call["extra_content"].(map[string]any)["google"].(map[string]any)["thought_signature"] != "c2ln" {
		t.Fatalf("tool call = %+v", call)
	}
}

// A Chat stream reaches Gemini's streamGenerateContent, and its chunks reach
// the API layer as data, with the usage the client asked for and without
// [DONE].
func TestGoogleFacadeStreamsThroughCore(t *testing.T) {
	server, take := googleBodies(t, "", "data: {\"candidates\": [{\"content\": {\"parts\": [{\"text\": \"Hel\"}],\"role\": \"model\"}}],\"modelVersion\": \"gemini-fixture-001\"}\r\n\r\n"+
		"data: {\"candidates\": [{\"content\": {\"parts\": [{\"text\": \"lo\"}],\"role\": \"model\"},\"finishReason\": \"STOP\"}],"+
		"\"usageMetadata\": {\"promptTokenCount\": 2,\"candidatesTokenCount\": 1,\"totalTokenCount\": 3},\"modelVersion\": \"gemini-fixture-001\"}\r\n\r\n")
	kw := Kwargs{"max_tokens": 32.0, "stream_options": map[string]any{"include_usage": true}}
	stream, err := studioFixture(t, server.URL, "k").Stream("gemini-fixture", googleChat, kw)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var chunks []string
	for {
		chunk, ok := stream.Next()
		if !ok {
			break
		}
		chunks = append(chunks, chunk)
	}
	if err := stream.Err(); err != nil {
		t.Fatal(err)
	}
	head := `{"choices":[{"delta":`
	tail := `,"index":0}],"id":"chatcmpl-google","model":"gemini-fixture-001","object":"chat.completion.chunk"}`
	want := []string{
		head + `{"content":"Hel","role":"assistant"},"finish_reason":null` + tail,
		head + `{"content":"lo"},"finish_reason":null` + tail,
		head + `{},"finish_reason":"stop"` + tail,
		`{"choices":[],"id":"chatcmpl-google","model":"gemini-fixture-001","object":"chat.completion.chunk","usage":{"completion_tokens":1,"prompt_tokens":2,"total_tokens":3}}`,
	}
	if strings.Join(chunks, "\n") != strings.Join(want, "\n") {
		t.Fatalf("chunks:\n%s\nwant:\n%s", strings.Join(chunks, "\n"), strings.Join(want, "\n"))
	}
	if seen := take(); len(seen) != 1 || seen[0] != `/models/gemini-fixture:streamGenerateContent?alt=sse {"contents":[{"parts":[{"text":"hi"}],"role":"user"}],"generationConfig":{"maxOutputTokens":32}}` {
		t.Fatalf("upstream = %q", seen)
	}
}

// A stream fails as the gateway's other streams fail: a refusal before the
// first chunk keeps its status and the wait Google asked for, an error
// Google sends in the stream keeps its status, a stream cut short counts
// against the provider, and a record over the size limit is the gateway's
// StreamRecordTooLargeError.
func TestGoogleFacadeStreamFailures(t *testing.T) {
	refusal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"quota",`+
			`"details":[{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"9s"}]}}`)
	}))
	defer refusal.Close()
	if _, err := studioFixture(t, refusal.URL, "k").Stream("gemini-fixture", googleChat, nil); UpstreamStatus(err) != http.StatusTooManyRequests ||
		!InvocationRetryable(err) || InvocationRetryAfter(err) != "9" {
		t.Fatalf("refused stream: err=%v status=%d retry-after=%q", err, UpstreamStatus(err), InvocationRetryAfter(err))
	}
	const hello = "data: {\"candidates\": [{\"content\": {\"parts\": [{\"text\": \"Hello\"}],\"role\": \"model\"}}]}\r\n\r\n"
	for name, fixture := range map[string]struct {
		stream string
		check  func(error) bool
	}{
		"error event": {hello + "data: {\"error\": {\"code\": 503,\"status\": \"UNAVAILABLE\",\"message\": \"overloaded\"}}\r\n\r\n", func(err error) bool {
			return UpstreamStatus(err) == http.StatusServiceUnavailable && InvocationRetryable(err)
		}},
		"cut short": {hello, func(err error) bool {
			return err.Error() == "the ai_studio stream ended before the model finished" && InvocationCircuitFailure(err) && !InvocationRetryable(err)
		}},
		"record too large": {hello + "data: " + strings.Repeat("x", maxStreamRecordWireSize) + "\r\n\r\n", func(err error) bool {
			var tooLarge *StreamRecordTooLargeError
			return errors.As(err, &tooLarge)
		}},
	} {
		server, _ := googleBodies(t, "", fixture.stream)
		stream, err := studioFixture(t, server.URL, "k").Stream("gemini-fixture", googleChat, nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var chunks int
		for _, ok := stream.Next(); ok; _, ok = stream.Next() {
			chunks++
		}
		_ = stream.Close()
		if err := stream.Err(); chunks != 1 || err == nil || !fixture.check(err) {
			t.Fatalf("%s: chunks=%d err=%#v", name, chunks, err)
		}
	}
}
