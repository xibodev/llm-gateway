package providers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"

	gcpauth "github.com/xibodev/llm-provider-auth/gcp"
	core "github.com/xibodev/llmgw-core"
)

// googleCall is one request a synthetic Google received: its credential
// header, "key <value>" for an API key, its Vertex AI request type and its
// path.
type googleCall struct{ credential, requestType, path string }

// googleUpstream is a synthetic AI Studio and Vertex AI that answers every
// generateContent with one line of text.
type googleUpstream struct {
	mu    sync.Mutex
	calls []googleCall
}

func (u *googleUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	credential := r.Header.Get("Authorization")
	if key := r.Header.Get("x-goog-api-key"); key != "" {
		credential = "key " + key
	}
	u.mu.Lock()
	u.calls = append(u.calls, googleCall{
		credential: credential, requestType: r.Header.Get("X-Vertex-AI-LLM-Request-Type"), path: r.URL.Path,
	})
	u.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"ok"}]}}]}`))
}

func (u *googleUpstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.calls)
}

func (u *googleUpstream) last() googleCall {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.calls) == 0 {
		return googleCall{}
	}
	return u.calls[len(u.calls)-1]
}

// googleRuntimeChat sends a Chat completion for caller on instance through
// the core Runtime, as the facade names a Google operation.
func googleRuntimeChat(runtime *Runtime, caller core.Caller, instance string) error {
	ctx := withCoreOperation(context.Background(), googleCoreType, caller)
	_, err := runtime.core.Invoke(ctx, caller, instance, core.Request{
		Surface: core.ModelSurfaceChatCompletions, Model: "gemini-fixture", ContentType: core.ContentTypeJSON,
		Body: []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
	})
	return err
}

// The core Runtime serves a Google instance with the credential the provider
// factory resolves for it, in the factory's order: the caller's connection,
// then the system connection, then the configured key; without any, AI
// Studio is sent none. Each request resolves its own.
func TestGoogleVerticalResolvesTheFactoryPrecedence(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	runtime := InstallForTests(t)
	upstream := &googleUpstream{}
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{"studio": {Type: "ai_studio", BaseURL: server.URL}}
	})
	human, err := iam.CreatePrincipal("human", "authentik:google-owner", "", "Google Owner")
	if err != nil {
		t.Fatal(err)
	}
	owner := core.Caller{ID: human.ID, Kind: core.CallerHuman}
	expect := func(caller core.Caller, want string) {
		t.Helper()
		if err := googleRuntimeChat(runtime, caller, "studio"); err != nil || upstream.last().credential != want {
			t.Fatalf("credential=%q err=%v, want %q", upstream.last().credential, err, want)
		}
	}

	expect(owner, "")
	config.Update(func(s *config.Settings) { s.Providers["studio"].APIKey = "configured-key" })
	expect(owner, "key configured-key")
	if _, err := iam.PutSystemProviderConnection("studio", "api_key", "system-key"); err != nil {
		t.Fatal(err)
	}
	expect(owner, "key system-key")
	connection, err := iam.PutProviderConnection(iam.ProviderConnectionCreate{
		PrincipalID: human.ID, ProviderID: "studio", Kind: "api_key", Secret: "personal-key", MakeDefault: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	expect(owner, "key personal-key")
	expect(gatewayCaller(), "key system-key")
	if err := iam.RevokeProviderConnection(human.ID, connection.ID); err != nil {
		t.Fatal(err)
	}
	expect(owner, "key system-key")

	// A connection of a kind Google does not take is refused before anything
	// is sent, and the kinds Google takes never refresh.
	oauth, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
		PrincipalID: human.ID, ProviderID: "studio", Kind: "openai_codex_oauth", MakeDefault: true,
		AccessToken: "oauth-access", ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	store := runtime.verticals[googleCoreType].credentials
	if _, err := store.Load(context.Background(), oauth.ID); !IsConfig(err) {
		t.Fatalf("Google's store loaded an OAuth connection: err=%v", err)
	}
	if sent := upstream.count(); googleRuntimeChat(runtime, owner, "studio") == nil || upstream.count() != sent {
		t.Fatal("an OAuth connection was sent to AI Studio")
	}
	if refresh := runtime.coreRefresh(config.Get(), "studio"); refresh != nil {
		t.Fatal("a Google credential would refresh")
	}
	if key, err := store.Resolve(context.Background(), gatewayCaller(), "missing"); !errors.Is(err, core.ErrNoCredential) {
		t.Fatalf("an instance without a credential resolved key=%q err=%v", key, err)
	}
}

// A Vertex AI instance the core Runtime serves exchanges a stored
// service-account key for the bearer it sends, bills the project the key
// names when none is configured, and keeps the configured request type.
func TestGoogleVerticalExchangesStoredServiceAccounts(t *testing.T) {
	setupCodexProviderTest(t)
	runtime := InstallForTests(t)
	upstream := &googleUpstream{}
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	tokens := stubTokenEndpoint(t)
	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{
			"vertex": {Type: "vertex_ai", BaseURL: server.URL + "/v1", VertexRequestType: "paygo"},
		}
	})
	if _, err := iam.PutSystemProviderConnection("vertex", gcpauth.CredentialKind, serviceAccountFixture(t, tokens.URL)); err != nil {
		t.Fatal(err)
	}
	if err := googleRuntimeChat(runtime, gatewayCaller(), "vertex"); err != nil {
		t.Fatal(err)
	}
	want := googleCall{
		credential: "Bearer ya29.stored-path", requestType: "shared",
		path: "/v1/projects/fixture-project/locations/global/publishers/google/models/gemini-fixture:generateContent",
	}
	if got := upstream.last(); got != want {
		t.Fatalf("upstream call=%+v, want %+v", got, want)
	}
}
