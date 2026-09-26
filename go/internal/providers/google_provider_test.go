package providers

import (
	"context"
	"encoding/base64"
	"encoding/json"
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
		"stream": {
			func() error { _, err := studio.Stream("m", googleChat, nil); return err },
			&ConfigError{Msg: "ai_studio: streaming is not implemented for this provider yet; use a non-streaming request"},
		},
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
	if _, err := iam.PutSystemProviderConnection("vertex_ai", gcpauth.CredentialKind, serviceAccountFixture(t, tokens.URL)); err != nil {
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
