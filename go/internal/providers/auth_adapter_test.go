package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"

	antigravityauth "github.com/xibodev/llm-provider-auth/antigravity"
	copilotauth "github.com/xibodev/llm-provider-auth/copilot"
)

func TestGoogleAntigravityBrowserOAuthUsesSharedFlowAndDiscoversProject(t *testing.T) {
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/load" {
			if r.Header.Get("Authorization") != "Bearer access-token" {
				t.Fatalf("authorization=%q", r.Header.Get("Authorization"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"cloudaicompanionProject": "project-id", "paidTier": map[string]any{"id": "paid-tier"}})
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("client_id") != "client-id" || r.Form.Get("client_secret") != "client-secret" || r.Form.Get("code_verifier") == "" {
			t.Fatalf("token form=%v", r.Form)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "access-token", "refresh_token": "refresh-token", "expires_in": 3600,
		})
	}))
	defer tokenServer.Close()
	oauth := antigravityauth.Config{
		ClientID: "client-id", ClientSecret: "client-secret", ClientAuthMode: antigravityauth.ClientAuthModeClientSecretPost, HTTPClient: tokenServer.Client(),
		Endpoints: antigravityauth.Endpoints{
			AuthorizeURL: tokenServer.URL + "/authorize", TokenURL: tokenServer.URL + "/token",
			LoadCodeAssistURL: tokenServer.URL + "/load",
		},
	}
	adapter := googleAntigravityAuthAdapter{oauth: &oauth}
	start, err := adapter.StartBrowser(context.Background(), "https://gateway.example.test/oauth/callback/google_antigravity")
	if err != nil {
		t.Fatal(err)
	}
	authorizationURL, _ := url.Parse(start.AuthorizationURL)
	if authorizationURL.Query().Get("code_challenge") == "" || authorizationURL.Query().Get("state") == "" {
		t.Fatalf("authorization URL=%s", start.AuthorizationURL)
	}
	var private map[string]string
	if json.Unmarshal([]byte(start.PrivateState), &private) != nil {
		t.Fatal("private state is not JSON")
	}
	private["redirect_uri"] = "https://gateway.example.test/oauth/callback/google_antigravity"
	raw, _ := json.Marshal(private)
	result, err := adapter.CompleteBrowser(context.Background(), "authorization-code", string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if result.AccessToken != "access-token" || result.AccountID != "" || result.AccountLabel != "" || result.ProjectID != "project-id" || result.OAuthProfile != antigravityOAuthProfileRuntimeSecret || result.OAuthClientID != "client-id" {
		t.Fatalf("result=%+v", result)
	}
}

func TestGoogleAntigravityBrowserOAuthPersistsNoFallbackProject(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/load" {
			_, _ = w.Write([]byte(`{"currentTier":{"id":"free-tier"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"access-token","expires_in":3600}`))
	}))
	defer server.Close()
	oauth := antigravityauth.Config{
		ClientID: "client-id", ClientSecret: "client-secret", ClientAuthMode: antigravityauth.ClientAuthModeClientSecretPost, HTTPClient: server.Client(),
		Endpoints: antigravityauth.Endpoints{AuthorizeURL: server.URL + "/authorize", TokenURL: server.URL + "/token", LoadCodeAssistURL: server.URL + "/load"},
	}
	adapter := googleAntigravityAuthAdapter{oauth: &oauth}
	start, err := adapter.StartBrowser(context.Background(), "https://gateway.example.test/oauth/callback/google_antigravity")
	if err != nil {
		t.Fatal(err)
	}
	private := map[string]string{}
	_ = json.Unmarshal([]byte(start.PrivateState), &private)
	private["redirect_uri"] = "https://gateway.example.test/oauth/callback/google_antigravity"
	raw, _ := json.Marshal(private)
	result, err := adapter.CompleteBrowser(context.Background(), "authorization-code", string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if result.ProjectID != "" {
		t.Fatalf("fallback project=%q", result.ProjectID)
	}
}

func TestGoogleAntigravityPublicPKCEProfileUsesConfiguredProviderClient(t *testing.T) {
	oldProviders := config.Get().Providers
	t.Cleanup(func() { config.Update(func(s *config.Settings) { s.Providers = oldProviders }) })
	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{
			"antigravity": {Type: "google_antigravity", PublicOAuthClientID: "public-client"},
		}
	})
	adapter := googleAntigravityAuthAdapter{providerID: "antigravity"}
	oauth := adapter.config("https://gateway.example.test/oauth/callback/google_antigravity")
	if oauth.ClientID != "public-client" || oauth.ClientSecret != "" || oauth.ClientAuthMode != antigravityauth.ClientAuthModePublicPKCE {
		t.Fatalf("public OAuth config=%+v", oauth)
	}
}

func TestGoogleAntigravityRuntimeClientUsesClientSecretPost(t *testing.T) {
	oldClientID := config.Get().GoogleAntigravityClientID
	oldSecret := config.Get().GoogleAntigravityClientSecret
	t.Cleanup(func() {
		config.Update(func(s *config.Settings) {
			s.GoogleAntigravityClientID, s.GoogleAntigravityClientSecret = oldClientID, oldSecret
		})
	})
	config.Update(func(s *config.Settings) {
		s.GoogleAntigravityClientID = "runtime-client"
		s.GoogleAntigravityClientSecret = "runtime-secret"
	})
	oauth := (googleAntigravityAuthAdapter{}).config("https://gateway.example.test/oauth/callback/google_antigravity")
	if oauth.ClientID != "runtime-client" || oauth.ClientSecret != "runtime-secret" || oauth.ClientAuthMode != antigravityauth.ClientAuthModeClientSecretPost {
		t.Fatalf("runtime OAuth config=%+v", oauth)
	}
}

func TestGoogleAntigravityBrowserOAuthRejectsChangedClientProfile(t *testing.T) {
	oauth := antigravityauth.Config{
		ClientID: "client-a", ClientSecret: "secret-a", ClientAuthMode: antigravityauth.ClientAuthModeClientSecretPost,
		RedirectURI: "https://gateway.example.test/oauth/callback/google_antigravity",
	}
	adapter := googleAntigravityAuthAdapter{oauth: &oauth}
	start, err := adapter.StartBrowser(context.Background(), oauth.RedirectURI)
	if err != nil {
		t.Fatal(err)
	}
	var private map[string]string
	if err := json.Unmarshal([]byte(start.PrivateState), &private); err != nil {
		t.Fatal(err)
	}
	private["redirect_uri"] = oauth.RedirectURI
	oauth.ClientID = "client-b"
	raw, _ := json.Marshal(private)
	if _, err := adapter.CompleteBrowser(context.Background(), "authorization-code", string(raw)); err == nil || !strings.Contains(err.Error(), "profile") {
		t.Fatalf("changed profile error=%v", err)
	}
}

type fixtureProviderAuthAdapter struct{}

func (fixtureProviderAuthAdapter) ID() string             { return "fixture_auth" }
func (fixtureProviderAuthAdapter) CredentialKind() string { return "fixture_oauth" }
func (fixtureProviderAuthAdapter) Capabilities() ProviderAuthCapabilities {
	return ProviderAuthCapabilities{TokenImport: true}
}

func TestSafeProviderAuthPollSanitizesBeforeRuneLimit(t *testing.T) {
	secret := "llmgw_" + strings.Repeat("a", 32)
	result := SafeProviderAuthPoll(ProviderAuthPoll{
		Status: "denied", Error: secret + " Bearer top-secret user@example.test?token=query-secret " + strings.Repeat("界", 400),
		AccessToken: "internal-access", RefreshToken: "internal-refresh", IDToken: "internal-id",
	})
	if result.Status != "denied" || result.AccessToken != "internal-access" || result.RefreshToken != "internal-refresh" || result.IDToken != "internal-id" {
		t.Fatalf("safe poll changed semantic or internal token fields: %+v", result)
	}
	if strings.Contains(result.Error, secret) || strings.Contains(result.Error, "top-secret") || strings.Contains(result.Error, "user@example.test") || strings.Contains(result.Error, "query-secret") {
		t.Fatalf("safe poll retained sensitive diagnostics: %q", result.Error)
	}
	if len([]rune(result.Error)) > maxProviderAuthDiagnosticChars {
		t.Fatalf("safe poll error has %d runes", len([]rune(result.Error)))
	}
}
func (fixtureProviderAuthAdapter) Import(
	context.Context, ProviderAuthImport,
) (ProviderAuthPoll, error) {
	return ProviderAuthPoll{Status: "authorized", AccessToken: "fixture"}, nil
}

func TestProviderAuthAdapterRegistryAndCapabilities(t *testing.T) {
	const id = "fixture_auth"
	unregisterProviderAuthAdapterFactoryForTests(id)
	t.Cleanup(func() { unregisterProviderAuthAdapterFactoryForTests(id) })
	if err := RegisterProviderAuthAdapterFactory(id, func(string) (ProviderAuthAdapter, error) {
		return fixtureProviderAuthAdapter{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	adapter, err := NewProviderAuthAdapter(id, "provider")
	if err != nil {
		t.Fatal(err)
	}
	if adapter.CredentialKind() != "fixture_oauth" || !adapter.Capabilities().TokenImport {
		t.Fatalf("adapter=%+v capabilities=%+v", adapter, adapter.Capabilities())
	}
	if _, ok := adapter.(ImportProviderAuthAdapter); !ok {
		t.Fatal("fixture adapter should expose token import")
	}
	if err := RegisterProviderAuthAdapterFactory(id, func(string) (ProviderAuthAdapter, error) {
		return fixtureProviderAuthAdapter{}, nil
	}); err == nil {
		t.Fatal("duplicate auth adapter should be rejected")
	}
}

func TestBuiltInProviderAuthAdapters(t *testing.T) {
	copilot, err := NewProviderAuthAdapter("github_copilot", "copilot")
	if err != nil {
		t.Fatal(err)
	}
	if copilot.CredentialKind() != "github_oauth" || !copilot.Capabilities().DeviceCode {
		t.Fatalf("Copilot adapter=%+v capabilities=%+v", copilot, copilot.Capabilities())
	}
	if _, ok := copilot.(DeviceProviderAuthAdapter); !ok {
		t.Fatal("Copilot adapter should expose device authorization")
	}
	if _, ok := copilot.(RefreshableProviderAuthAdapter); !ok {
		t.Fatal("Copilot adapter should expose refresh")
	}
	if _, ok := copilot.(RevocableProviderAuthAdapter); ok {
		t.Fatal("Copilot adapter must not claim unsupported upstream revocation")
	}
	if _, err := NewProviderAuthAdapter("missing", "provider"); err == nil {
		t.Fatal("unknown auth adapter should be rejected")
	}

	oldClientID := config.Get().OpenAICodexClientID
	t.Cleanup(func() {
		config.Update(func(s *config.Settings) { s.OpenAICodexClientID = oldClientID })
	})
	config.Update(func(s *config.Settings) { s.OpenAICodexClientID = "fixture-client" })
	codex, err := NewProviderAuthAdapter("openai_codex", "codex")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := codex.(GuardedRefreshProviderAuthAdapter); !ok {
		t.Fatal("Codex adapter should expose guarded connection refresh")
	}
	if _, ok := codex.(RefreshableProviderAuthAdapter); ok {
		t.Fatal("Codex adapter must not expose unguarded token-only refresh")
	}
	if _, ok := codex.(RevocableProviderAuthAdapter); !ok {
		t.Fatal("Codex adapter should expose upstream revocation")
	}
}

var _ ImportProviderAuthAdapter = fixtureProviderAuthAdapter{}
var _ RefreshableProviderAuthAdapter = githubCopilotAuthAdapter{}
var _ DeviceProviderAuthAdapter = githubCopilotAuthAdapter{}
var _ = iam.OAuthTokenEnvelope{}

func TestCopilotInvocationErrorPreservesRetryClassification(t *testing.T) {
	transport := copilotInvocationError(&copilotauth.AuthError{Msg: "transport", Transport: true})
	if !InvocationRetryable(transport) {
		t.Fatal("Copilot transport error should be retryable")
	}
	rejected := copilotInvocationError(&copilotauth.AuthError{Msg: "rejected", StatusCode: 401})
	if InvocationRetryable(rejected) || InvocationFailoverEligible(rejected) || UpstreamStatus(rejected) != 401 {
		t.Fatalf("Copilot rejection retry=%v failover=%v status=%d", InvocationRetryable(rejected), InvocationFailoverEligible(rejected), UpstreamStatus(rejected))
	}
}
