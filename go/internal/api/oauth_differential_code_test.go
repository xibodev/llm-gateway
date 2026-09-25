package api

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"

	codexauth "github.com/xibodev/llm-provider-auth/codex"
	"github.com/xibodev/llm-provider-auth/tokenstore"
	"github.com/xibodev/llmgw-core/oauthflow"
)

// fixtureRecordOf is the record a driver returns for result, which is what
// the adapter it replaced returned.
func fixtureRecordOf(result providers.ProviderAuthPoll) tokenstore.Record {
	return tokenstore.Record{
		AccessToken: result.AccessToken, RefreshToken: result.RefreshToken, IDToken: result.IDToken,
		TokenType: result.TokenType, Expiry: time.Unix(result.ExpiresAt, 0), AccountID: result.AccountID,
		Metadata: map[string]string{
			providers.OAuthMetadataAccountLabel: result.AccountLabel, providers.OAuthMetadataProjectID: result.ProjectID,
			providers.OAuthMetadataProfile: result.OAuthProfile, providers.OAuthMetadataClientID: result.OAuthClientID,
		},
	}
}

func stateOf(t *testing.T, started map[string]any) string {
	t.Helper()
	authorizationURL, err := url.Parse(stringValueForTest(started["authorization_url"]))
	if err != nil {
		t.Fatal(err)
	}
	return authorizationURL.Query().Get("state")
}

// codexBrowserCompletions completes a Codex browser sign-in the old way and
// the new, both against a token endpoint that answers tokens, for a provider
// the completion configures.
func codexBrowserCompletions(t *testing.T, tokens string, lifetime int64) (completionState, completionState) {
	t.Helper()
	ctx := context.Background()
	useDifferentialSettings(t, func(*config.Settings) {})
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(tokens))
	}))
	t.Cleanup(mock.Close)
	providers.SetCodexEndpointsForTests(t, providers.CodexEndpoints{
		OAuth: codexauth.Endpoints{OAuthTokenURL: mock.URL + "/token"}, BrowserAuthorizeURL: mock.URL + "/authorize",
	})
	twins := newCompletionTwins(t, "codex", "openai_codex_oauth")
	twins.lifetime = lifetime
	old := twins.run(t, 0, func() map[string]any {
		adapter := oauthAdapter(t, "openai_codex", "codex")
		manual := adapter.(providers.ManualProviderAuthAdapter)
		captured := providers.ProviderAuthManualConfig{ClientID: providers.EffectiveCodexClientID(), ClientMode: "public", RedirectURI: codexBrowserLoopbackRedirectURI}
		start, err := manual.StartManual(ctx, captured)
		if err != nil {
			t.Fatal(err)
		}
		result, err := manual.CompleteManual(ctx, "fixture-code", start.PrivateState, captured)
		return legacyManualCompletion(twins.principal, "openai_codex", "codex", legacyBrowserFlow{
			ConnectionName: "work", Kind: adapter.CredentialKind(), Source: iam.ConnectionSourceAdmin, ManualConfig: captured,
		}, result, err)
	})
	next := twins.run(t, 1, func() map[string]any {
		s := newServer(Runtime{}, time.Now)
		started, err := s.startCodexBrowserFlow(twins.principal, "openai_codex", "", true, "work", iam.ConnectionSourceAdmin)
		if err != nil {
			t.Fatal(err)
		}
		return s.completeManualOAuthFlow(twins.principal, "openai_codex", stringValueForTest(started["flow_id"]),
			codexBrowserLoopbackRedirectURI+"?code=fixture-code&state="+url.QueryEscape(stateOf(t, started)))
	})
	return old, next
}

// The browser and manual completions. Codex runs against fixture endpoints
// on both sides. Antigravity's endpoints are Google's, which no fixture
// reaches, so both sides store the same provider result there; the providers
// tests show its drivers return what its adapter did.
func TestOAuthCodeCompletionWritesWhatTheGatewayWrote(t *testing.T) {
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"owner@example.test","https://api.openai.com/auth":{"chatgpt_account_id":"fixture-account"}}`))
	tokens := `"access_token":"fixture-codex-access","refresh_token":"fixture-codex-refresh","token_type":"Bearer","id_token":"header.` + claims + `.signature"`
	t.Run("codex browser, provider created on completion", func(t *testing.T) {
		old, next := codexBrowserCompletions(t, `{`+tokens+`,"expires_in":3600}`, 3600)
		sameCompletion(t, old, next, "authorized")
	})
	// A token without a lifetime carries the zero time's second, which the
	// gateway has always refused to store: the completion fails alike.
	t.Run("codex browser, token without a lifetime", func(t *testing.T) {
		old, next := codexBrowserCompletions(t, `{`+tokens+`}`, 0)
		sameCompletion(t, old, next, "error")
	})
	t.Run("antigravity browser", func(t *testing.T) {
		useDifferentialSettings(t, func(*config.Settings) {})
		result := providers.ProviderAuthPoll{
			Status: "authorized", AccessToken: "fixture-ag-access", RefreshToken: "fixture-ag-refresh", TokenType: "Bearer",
			ExpiresAt: 1_900_000_000, ProjectID: "fixture-project", OAuthProfile: "runtime_client_secret_post", OAuthClientID: "fixture-client",
		}
		twins := newCompletionTwins(t, "google-antigravity", "google_antigravity_oauth")
		old := twins.run(t, 0, func() map[string]any {
			return legacyCallbackCompletion(legacyBrowserFlow{
				PrincipalID: twins.principal.ID, ProviderID: "google-antigravity", Kind: "google_antigravity_oauth",
				ConnectionName: "personal", Source: iam.ConnectionSourceUser,
			}, result, nil)
		})
		next := twins.run(t, 1, func() map[string]any {
			s := newServer(Runtime{}, time.Now)
			useOAuthDriver(s, oauthflow.MethodBrowser, &fixtureCodeDriver{exchange: func(oauthflow.Flow, string) (tokenstore.Record, error) {
				return fixtureRecordOf(result), nil
			}})
			flowID, state := startBrowser(t, s, twins.principal)
			request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:9791/oauth/callback/google_antigravity?code=fixture-code&state="+url.QueryEscape(state), nil)
			request.SetPathValue("provider_id", "google_antigravity")
			recorder := httptest.NewRecorder()
			s.handleOAuthBrowserCallback(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("callback status=%d body=%q", recorder.Code, recorder.Body.String())
			}
			return s.pollOAuthFlow(twins.principal, "google_antigravity", flowID, "", iam.ConnectionSourceUser)
		})
		sameCompletion(t, old, next, "authorized")
	})
	t.Run("antigravity consumer_manual, client saved and provider created", func(t *testing.T) {
		useDifferentialSettings(t, func(settings *config.Settings) { delete(settings.Providers, "google-antigravity") })
		input := providers.ProviderAuthManualConfig{ClientID: " fixture-manual-client ", ClientSecret: " fixture-manual-secret ", ClientMode: "Confidential", RedirectURI: "https://callback.example.test/oauth"}
		result := providers.ProviderAuthPoll{
			Status: "authorized", AccessToken: "fixture-manual-access", RefreshToken: "fixture-manual-refresh",
			ExpiresAt: 1_900_000_000, ProjectID: "fixture-project", OAuthProfile: antigravityManualProfile, OAuthClientID: "fixture-manual-client",
		}
		twins := newCompletionTwins(t, "google-antigravity", "google_antigravity_oauth")
		old := twins.run(t, 0, func() map[string]any {
			configured, err := antigravityManualConfig("google-antigravity", input, true)
			if err != nil {
				t.Fatal(err)
			}
			return legacyManualCompletion(twins.principal, "google_antigravity", "google-antigravity", legacyBrowserFlow{
				ConnectionName: "work", Kind: "google_antigravity_oauth", Source: iam.ConnectionSourceAdmin,
				PersistConfig: true, ManualConfig: configured,
			}, result, nil)
		})
		next := twins.run(t, 1, func() map[string]any {
			s := newServer(Runtime{}, time.Now)
			useOAuthDriver(s, oauthflow.MethodManual, &fixtureCodeDriver{exchange: func(oauthflow.Flow, string) (tokenstore.Record, error) {
				return fixtureRecordOf(result), nil
			}})
			started, err := s.startManualOAuthFlow(twins.principal, "google_antigravity", "work", iam.ConnectionSourceAdmin, input, true)
			if err != nil {
				t.Fatal(err)
			}
			return s.completeManualOAuthFlow(twins.principal, "google_antigravity", stringValueForTest(started["flow_id"]), "fixture-code")
		})
		sameCompletion(t, old, next, "authorized")
	})
}
