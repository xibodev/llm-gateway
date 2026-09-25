package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"llmgw/internal/iam"

	antigravityauth "github.com/xibodev/llm-provider-auth/antigravity"
	core "github.com/xibodev/llmgw-core"
)

// setupAntigravityRefreshTest stores an expired connection of a new owner and
// points the settings OAuth client at upstream. It returns the owner.
func setupAntigravityRefreshTest(t *testing.T, upstream *antigravityUpstream) iam.Principal {
	t.Helper()
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	human := setupAntigravityConnectionTest(t, "old-access", "refresh-token", "", time.Now().Add(-time.Hour).Unix())
	setAntigravityOAuthTestConfig(t, server)
	return human
}

// refreshForm returns the form of the only refresh upstream received.
func refreshForm(t *testing.T, upstream *antigravityUpstream) url.Values {
	t.Helper()
	refreshes := upstream.to("/token")
	if len(refreshes) != 1 {
		t.Fatalf("refresh requests=%d, want 1", len(refreshes))
	}
	form, err := url.ParseQuery(refreshes[0].body)
	if err != nil {
		t.Fatal(err)
	}
	return form
}

// A project discovered with a token that a new sign-in replaced meanwhile is
// dropped: the new sign-in may act for another account.
func TestAntigravityProjectStoreDoesNotOverwriteConcurrentReauthorization(t *testing.T) {
	human := setupAntigravityConnectionTest(t, "old-access", "old-refresh", "", 0)
	_, connection := storedAntigravityConnection(t, human)
	record, err := Current().credentials.Load(context.Background(), connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	putAntigravityConnection(t, iam.OAuthConnectionCreate{
		PrincipalID: human.ID, AccessToken: "reauthorized-access", RefreshToken: "reauthorized-refresh",
	})
	Current().storeAntigravityProject(context.Background(), core.CredentialFromRecord(connection.ID, record), "stale-project")
	envelope, _ := storedAntigravityConnection(t, human)
	if envelope.AccessToken != "reauthorized-access" || envelope.RefreshToken != "reauthorized-refresh" || envelope.ProjectID != "" {
		t.Fatalf("concurrent reauthorization was overwritten: project=%q", envelope.ProjectID)
	}
}

// Two refreshes of one rejected token, standing for two processes whose
// credential store's lease is all they share, spend the grant once.
func TestAntigravityUnauthorizedRefreshIsDeduplicated(t *testing.T) {
	upstream := &antigravityUpstream{}
	human := setupAntigravityRefreshTest(t, upstream)
	_, connection := storedAntigravityConnection(t, human)
	record, err := Current().credentials.Load(context.Background(), connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	for range 2 {
		coordinator, err := Current().antigravityCoordinator("antigravity")
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			<-start
			_, err := coordinator.Rejected(context.Background(), connection.ID, record)
			errs <- err
		}()
	}
	close(start)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if refreshes := upstream.count("/token"); refreshes != 1 {
		t.Fatalf("refresh requests=%d, want 1", refreshes)
	}
}

func TestAntigravityRefreshUsesRevisionSafeReplacement(t *testing.T) {
	human := setupAntigravityRefreshTest(t, &antigravityUpstream{})
	envelope, _, err := RefreshAntigravityOAuthConnection(context.Background(), human.ID, "antigravity", "personal")
	if err != nil {
		t.Fatal(err)
	}
	if envelope.AccessToken != "refreshed-access" || envelope.RefreshToken != "refresh-token" || envelope.ProjectID != "discovered-project" {
		t.Fatalf("project=%q refreshed=%v", envelope.ProjectID, envelope.AccessToken == "refreshed-access")
	}
}

func TestAntigravityRuntimeRefreshUsesClientSecretPost(t *testing.T) {
	upstream := &antigravityUpstream{}
	human := setupAntigravityRefreshTest(t, upstream)
	if _, _, err := RefreshAntigravityOAuthConnection(context.Background(), human.ID, "antigravity", "personal"); err != nil {
		t.Fatal(err)
	}
	if form := refreshForm(t, upstream); form.Get("client_id") != "client-id" || form.Get("client_secret") != "client-secret" {
		t.Fatalf("refresh form carried client %q, secret %v", form.Get("client_id"), form.Has("client_secret"))
	}
}

func TestAntigravityPublicPKCERefreshUsesPersistedProfile(t *testing.T) {
	upstream := &antigravityUpstream{}
	server := httptest.NewServer(upstream)
	defer server.Close()
	human := setupAntigravityConnectionTest(t, "old-access", "refresh-token", "", time.Now().Add(-time.Hour).Unix())
	configureAntigravityInstance(t, "public-client")
	putAntigravityConnection(t, iam.OAuthConnectionCreate{
		PrincipalID: human.ID, AccessToken: "old-access", RefreshToken: "refresh-token", ExpiresAt: time.Now().Add(-time.Hour).Unix(),
		OAuthProfile: antigravityOAuthProfilePublicPKCE, OAuthClientID: "public-client",
	})
	replaceAntigravityOAuthConfig(t, func(string) antigravityauth.Config {
		return antigravityauth.Config{
			Endpoints:  antigravityauth.Endpoints{TokenURL: server.URL + "/token", LoadCodeAssistURL: server.URL + "/load"},
			HTTPClient: server.Client(),
		}
	})
	if _, _, err := RefreshAntigravityOAuthConnection(context.Background(), human.ID, "antigravity", "personal"); err != nil {
		t.Fatal(err)
	}
	if form := refreshForm(t, upstream); form.Get("client_id") != "public-client" || form.Has("client_secret") {
		t.Fatalf("refresh form carried client %q, secret %v", form.Get("client_id"), form.Has("client_secret"))
	}
	refreshed, _ := storedAntigravityConnection(t, human)
	if refreshed.AccessToken != "refreshed-access" || refreshed.OAuthProfile != antigravityOAuthProfilePublicPKCE || refreshed.OAuthClientID != "public-client" {
		t.Fatalf("profile=%q client=%q refreshed=%v", refreshed.OAuthProfile, refreshed.OAuthClientID, refreshed.AccessToken == "refreshed-access")
	}
}

// A consumer's own client refreshes the grant it issued, with its secret
// when it is confidential and with none when it is public.
func TestAntigravityConsumerManualRefreshUsesBoundClient(t *testing.T) {
	for _, client := range []struct{ mode, secret string }{{"confidential", "fixture-secret"}, {"public", ""}} {
		t.Run(client.mode, func(t *testing.T) {
			upstream := &antigravityUpstream{}
			human := setupAntigravityRefreshTest(t, upstream)
			putAntigravityConnection(t, iam.OAuthConnectionCreate{
				PrincipalID: human.ID, AccessToken: "old-access", RefreshToken: "refresh-token", ExpiresAt: time.Now().Add(-time.Hour).Unix(),
				OAuthProfile: antigravityOAuthProfileConsumerManual, OAuthClientID: "fixture-client", OAuthClientSecret: client.secret,
				OAuthClientMode: client.mode, OAuthRedirectURI: "https://callback.example.test/oauth",
			})
			if _, _, err := RefreshAntigravityOAuthConnection(context.Background(), human.ID, "antigravity", "personal"); err != nil {
				t.Fatal(err)
			}
			form := refreshForm(t, upstream)
			if form.Get("client_id") != "fixture-client" || form.Get("client_secret") != client.secret || form.Has("client_secret") != (client.secret != "") {
				t.Fatalf("refresh form carried client %q, secret %v", form.Get("client_id"), form.Has("client_secret"))
			}
		})
	}
}

// The gateway refuses to refresh the grants its Antigravity path refused,
// which core would send to a client they may not belong to, and sends none
// of them anywhere. Inference refreshes an expired one first, so it fails
// the same way before anything is sent.
func TestAntigravityLegacyRefreshWithoutProfileFailsClosed(t *testing.T) {
	for _, grant := range []struct {
		name, profile, clientID, mode, secret, want string
	}{
		{name: "no profile", want: "reauthorize"},
		{name: "runtime without client", profile: antigravityOAuthProfileRuntimeSecret, want: "reauthorize"},
		{name: "runtime with another client", profile: antigravityOAuthProfileRuntimeSecret, clientID: "other-client", want: "profile is unavailable"},
		{name: "public client no longer configured", profile: antigravityOAuthProfilePublicPKCE, clientID: "public-client", want: "profile is unavailable"},
		{name: "consumer without a mode", profile: antigravityOAuthProfileConsumerManual, clientID: "fixture-client", want: "public or confidential"},
		{name: "confidential consumer without a secret", profile: antigravityOAuthProfileConsumerManual, clientID: "fixture-client", mode: "confidential", want: "require a client secret"},
		{name: "unknown profile", profile: "fixture-profile", clientID: "client-id", want: "unsupported"},
	} {
		t.Run(grant.name, func(t *testing.T) {
			upstream := &antigravityUpstream{handle: func(w http.ResponseWriter, r *http.Request) bool {
				t.Errorf("a refused grant reached %s", r.URL.Path)
				return false
			}}
			provider, human := setupAntigravityRuntimeTest(t, upstream, "old-access", "refresh-token", "project", 0)
			putAntigravityConnection(t, iam.OAuthConnectionCreate{
				PrincipalID: human.ID, AccessToken: "old-access", RefreshToken: "refresh-token", ProjectID: "project",
				ExpiresAt: time.Now().Add(-time.Hour).Unix(), OAuthProfile: grant.profile, OAuthClientID: grant.clientID,
				OAuthClientMode: grant.mode, OAuthClientSecret: grant.secret,
			})
			_, _, err := RefreshAntigravityOAuthConnection(context.Background(), human.ID, "antigravity", "personal")
			if err == nil || !strings.Contains(err.Error(), grant.want) {
				t.Fatalf("refresh error=%v, want %q", err, grant.want)
			}
			if _, err := provider.Complete("model-a", antigravityHello, nil); !IsInvocation(err) || UpstreamStatus(err) != 0 {
				t.Fatalf("inference error=%v", err)
			}
		})
	}
}
