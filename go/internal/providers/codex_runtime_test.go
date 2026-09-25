package providers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"

	codexauth "github.com/xibodev/llm-provider-auth/codex"
	core "github.com/xibodev/llmgw-core"
)

const codexFixtureEvents = "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_fixture\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"gpt-codex\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}]}}\n\n"

type codexUpstreamCall struct{ path, authorization, account string }

// codexUpstream records every call and answers with handle.
type codexUpstream struct {
	mu     sync.Mutex
	calls  []codexUpstreamCall
	handle func(w http.ResponseWriter, r *http.Request, attempt int)
}

func (u *codexUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	u.calls = append(u.calls, codexUpstreamCall{r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("ChatGPT-Account-ID")})
	attempt := len(u.calls)
	u.mu.Unlock()
	u.handle(w, r, attempt)
}

func (u *codexUpstream) recorded() []codexUpstreamCall {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]codexUpstreamCall(nil), u.calls...)
}

// setupCodexRuntimeTest configures the installed Runtime's codex instance
// against upstream and returns the facade of a new human owner.
func setupCodexRuntimeTest(t *testing.T, upstream http.Handler) (CodexProvider, iam.Principal) {
	t.Helper()
	setupCodexProviderTest(t)
	configureCodexInstance(t, "codex")
	oldClientID := config.Get().OpenAICodexClientID
	t.Cleanup(func() { config.Update(func(s *config.Settings) { s.OpenAICodexClientID = oldClientID }) })
	config.Update(func(s *config.Settings) { s.OpenAICodexClientID = "fixture-client" })
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	SetCodexEndpointsForTests(t, CodexEndpoints{
		OAuth: codexauth.Endpoints{OAuthTokenURL: server.URL + "/oauth/token"}, ResponsesBaseURL: server.URL,
	})
	human, err := iam.CreatePrincipal("human", "authentik:"+strings.ToLower(t.Name()), "", "Codex Owner")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := Current().newCodexProvider("codex", core.Caller{ID: human.ID, Kind: core.CallerHuman}, 30, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	return provider, human
}

func putCodexConnection(t *testing.T, owner iam.Principal, access, account string) iam.ProviderConnection {
	t.Helper()
	connection, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
		PrincipalID: owner.ID, ProviderID: "codex", Kind: "openai_codex_oauth", MakeDefault: true,
		AccessToken: access, RefreshToken: access + "-refresh", AccountID: account,
		ExpiresAt: time.Now().Add(time.Hour).Unix(), OAuthProfile: codexOAuthProfileDevice, OAuthClientID: "fixture-client",
	})
	if err != nil {
		t.Fatal(err)
	}
	return connection
}

func TestCodexRequestWithoutConnectionFailsClosed(t *testing.T) {
	upstream := &codexUpstream{handle: func(w http.ResponseWriter, r *http.Request, _ int) {
		t.Errorf("a request without a credential reached %s", r.URL.Path)
	}}
	provider, _ := setupCodexRuntimeTest(t, upstream)
	_, observation, err := provider.CompleteResponses("gpt-codex", map[string]any{"input": "hello"})
	if !IsConfig(err) || !strings.Contains(err.Error(), "no active private Codex connection") || observation != nil {
		t.Fatalf("err=%v observation=%+v", err, observation)
	}
}

// After an upstream 401 the Runtime refreshes once. When that refresh fails,
// the Runtime returns the 401, but the Codex path reported the refresh
// failure: a grant the provider rejected for good revokes the connection and
// fails over with the token endpoint's status.
func TestCodexReplayReportsTheFailedRefresh(t *testing.T) {
	upstream := &codexUpstream{handle: func(w http.ResponseWriter, r *http.Request, _ int) {
		if r.URL.Path == "/oauth/token" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}}
	provider, owner := setupCodexRuntimeTest(t, upstream)
	connection := putCodexConnection(t, owner, "fixture-access", "account-fixture")
	cached := "codex@" + owner.ID
	putProvider(cached, EchoProvider{})

	_, observation, err := provider.CompleteResponses("gpt-codex", map[string]any{"input": "hello"})
	if UpstreamStatus(err) != http.StatusBadRequest || !InvocationFailoverEligible(err) || InvocationRetryable(err) {
		t.Fatalf("err=%v status=%d, want the failed refresh rather than the 401", err, UpstreamStatus(err))
	}
	if observation == nil || observation.ConnectionID != connection.ID || observation.CredentialRevision <= 0 {
		t.Fatalf("observation=%+v, want the connection the request used", observation)
	}
	calls := upstream.recorded()
	if len(calls) != 2 || calls[0].path != "/responses" || calls[1].path != "/oauth/token" {
		t.Fatalf("upstream calls=%+v", calls)
	}
	if _, _, ok, err := iam.OAuthProviderConnectionSecret(owner.ID, "codex", ""); err != nil || ok {
		t.Fatalf("the rejected grant left an active connection: ok=%v err=%v", ok, err)
	}
	if _, found := Current().instances.instances[cached]; found {
		t.Fatal("the revocation left the owner's provider cached")
	}
}

// The connection is signed in again, to another account, between a request
// and the 401 that makes the Runtime refresh it. The Codex path pinned the
// account within one credential fetch, not across the replay, so it replayed
// with the new sign-in. The pin's scope keeps that: see oauthCall.attempt.
func TestCodexReplayAfterSignInUsesTheNewSignIn(t *testing.T) {
	var owner iam.Principal
	upstream := &codexUpstream{}
	upstream.handle = func(w http.ResponseWriter, r *http.Request, attempt int) {
		if r.URL.Path != "/responses" {
			t.Errorf("unexpected upstream call to %s", r.URL.Path)
			return
		}
		if attempt == 1 {
			if _, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
				PrincipalID: owner.ID, ProviderID: "codex", Kind: "openai_codex_oauth", MakeDefault: true,
				AccessToken: "second-access", RefreshToken: "second-refresh", AccountID: "account-second",
				ExpiresAt: time.Now().Add(time.Hour).Unix(), OAuthProfile: codexOAuthProfileDevice, OAuthClientID: "fixture-client",
			}); err != nil {
				t.Errorf("sign in again: %v", err)
			}
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(codexFixtureEvents))
	}
	var provider CodexProvider
	provider, owner = setupCodexRuntimeTest(t, upstream)
	putCodexConnection(t, owner, "first-access", "account-first")

	if _, _, err := provider.CompleteResponses("gpt-codex", map[string]any{"input": "hello"}); err != nil {
		t.Fatal(err)
	}
	calls := upstream.recorded()
	want := []codexUpstreamCall{
		{"/responses", "Bearer first-access", "account-first"},
		{"/responses", "Bearer second-access", "account-second"},
	}
	if len(calls) != len(want) || calls[0] != want[0] || calls[1] != want[1] {
		t.Fatalf("upstream calls=%+v, want %+v", calls, want)
	}
	// Serving a request still marks the connection used for the console.
	if connections, err := iam.ListProviderConnections(owner.ID, "codex"); err != nil || len(connections) != 1 || connections[0].LastUsedAt == 0 {
		t.Fatalf("connections=%+v err=%v, want the connection marked used", connections, err)
	}
}
