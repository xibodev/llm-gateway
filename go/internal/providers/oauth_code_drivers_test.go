package providers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	antigravityauth "github.com/xibodev/llm-provider-auth/antigravity"
	codexauth "github.com/xibodev/llm-provider-auth/codex"
	"github.com/xibodev/llmgw-core/oauthflow"
)

// withoutPKCE drops what each authorization attempt generates afresh, so two
// attempts can be compared.
func withoutPKCE(t *testing.T, raw string) string {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Del("state")
	query.Del("code_challenge")
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

// withVerifier replaces a code verifier in logged requests, so the requests
// of two attempts compare equal exactly when nothing else differs.
func withVerifier(requests []string, verifier string) []string {
	out := make([]string, len(requests))
	for index, request := range requests {
		out[index] = strings.ReplaceAll(request, url.QueryEscape(verifier), "VERIFIER")
	}
	return out
}

func privateState(t *testing.T, raw string) map[string]string {
	t.Helper()
	private := map[string]string{}
	if err := json.Unmarshal([]byte(raw), &private); err != nil {
		t.Fatal(err)
	}
	return private
}

func TestCodexBrowserDriverSendsWhatTheAdapterSends(t *testing.T) {
	log := &requestLog{}
	claims := `{"email":"owner@example.test","https://api.openai.com/auth":{"chatgpt_account_id":"fixture-account"}}`
	idToken := "header." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".signature"
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"fixture-access","refresh_token":"fixture-refresh","token_type":"Bearer","id_token":"` + idToken + `"}`))
	}))
	defer mock.Close()
	SetCodexEndpointsForTests(t, CodexEndpoints{
		OAuth: codexauth.Endpoints{OAuthTokenURL: mock.URL + "/token"}, BrowserAuthorizeURL: mock.URL + "/authorize",
	})
	ctx := context.Background()
	captured := ProviderAuthManualConfig{ClientID: "captured-client", ClientMode: "public", RedirectURI: codexBrowserRedirectURI}
	adapter := openAICodexAuthAdapter{clientID: EffectiveCodexClientID()}
	driver := oauthDriver(t, "openai_codex", oauthflow.MethodManual).(oauthflow.CodeDriver)

	start, err := adapter.StartManual(ctx, captured)
	if err != nil {
		t.Fatal(err)
	}
	authorization, err := driver.Start(ctx, oauthflow.StartRequest{Params: captured.OAuthParams()})
	if err != nil {
		t.Fatal(err)
	}
	if withoutPKCE(t, start.AuthorizationURL) != withoutPKCE(t, authorization.AuthorizationURL) ||
		authorization.Secrets.RedirectURI != codexBrowserRedirectURI || authorization.ExpiresIn != codeFlowTTL {
		t.Fatalf("authorization=%+v start=%+v", authorization, start)
	}
	private := privateState(t, start.PrivateState)
	result, err := adapter.CompleteManual(ctx, "fixture-code", start.PrivateState, captured)
	if err != nil {
		t.Fatal(err)
	}
	adapterRequests := withVerifier(log.take(), private["code_verifier"])
	record, err := driver.Exchange(ctx, oauthflow.Flow{Secrets: authorization.Secrets}, "fixture-code")
	if err != nil {
		t.Fatal(err)
	}
	sameRequests(t, "exchange", adapterRequests, withVerifier(log.take(), authorization.Secrets.Verifier))
	sameRecord(t, record, result)
	if record.AccountID != "fixture-account" || record.Metadata[OAuthMetadataClientID] != "captured-client" {
		t.Fatalf("record=%+v", record)
	}
}

func TestAntigravityDriversSendWhatTheAdapterSends(t *testing.T) {
	log := &requestLog{}
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.record(r)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/load" {
			_, _ = w.Write([]byte(`{"cloudaicompanionProject":"fixture-project"}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"fixture-access","refresh_token":"fixture-refresh","token_type":"Bearer"}`))
	}))
	defer mock.Close()
	endpoints := antigravityauth.Endpoints{AuthorizeURL: mock.URL + "/authorize", TokenURL: mock.URL + "/token", LoadCodeAssistURL: mock.URL + "/load"}
	oauth := antigravityauth.Config{
		ClientID: "fixture-client", ClientSecret: "fixture-secret", ClientAuthMode: antigravityauth.ClientAuthModeClientSecretPost,
		Endpoints: endpoints, HTTPClient: mock.Client(),
	}
	ctx := context.Background()
	redirectURI := "https://gateway.example.test/oauth/callback/google_antigravity"

	adapter := googleAntigravityAuthAdapter{oauth: &oauth}
	browser := antigravityBrowserDriver{adapter: adapter}
	start, err := adapter.StartBrowser(ctx, redirectURI)
	if err != nil {
		t.Fatal(err)
	}
	authorization, err := browser.Start(ctx, oauthflow.StartRequest{RedirectURI: redirectURI})
	if err != nil {
		t.Fatal(err)
	}
	private := privateState(t, start.PrivateState)
	if withoutPKCE(t, start.AuthorizationURL) != withoutPKCE(t, authorization.AuthorizationURL) ||
		authorization.Secrets.DriverData["oauth_profile"] != private["oauth_profile"] ||
		authorization.Secrets.DriverData["oauth_client_id"] != private["oauth_client_id"] {
		t.Fatalf("authorization=%+v start=%+v", authorization, start)
	}
	private["redirect_uri"] = redirectURI
	stored, _ := json.Marshal(private)
	result, err := adapter.CompleteBrowser(ctx, "fixture-code", string(stored))
	if err != nil {
		t.Fatal(err)
	}
	adapterRequests := withVerifier(log.take(), private["code_verifier"])
	flow := oauthflow.Flow{Secrets: authorization.Secrets}
	record, err := browser.Exchange(ctx, flow, "fixture-code")
	if err != nil {
		t.Fatal(err)
	}
	sameRequests(t, "browser exchange", adapterRequests, withVerifier(log.take(), authorization.Secrets.Verifier))
	sameRecord(t, record, result)

	changed := oauth
	changed.ClientID = "other-client"
	if _, err := (antigravityBrowserDriver{adapter: googleAntigravityAuthAdapter{oauth: &changed}}).Exchange(ctx, flow, "fixture-code"); err == nil || !strings.Contains(err.Error(), "profile") {
		t.Fatalf("changed client profile error=%v", err)
	}

	// The manual profile builds its client from the administrator's
	// settings. Its authorization matches the adapter's; the exchange runs
	// against the fixture's endpoints.
	input := ProviderAuthManualConfig{ClientID: " manual-client ", ClientSecret: "manual-secret", ClientMode: "confidential", RedirectURI: "https://callback.example.test/oauth"}
	manualStart, err := adapter.StartManual(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	manualAuthorization, err := oauthDriver(t, "google_antigravity", oauthflow.MethodManual).Start(ctx, oauthflow.StartRequest{Params: input.OAuthParams()})
	if err != nil {
		t.Fatal(err)
	}
	if withoutPKCE(t, manualStart.AuthorizationURL) != withoutPKCE(t, manualAuthorization.AuthorizationURL) ||
		manualAuthorization.Secrets.RedirectURI != input.RedirectURI {
		t.Fatalf("manual authorization=%+v start=%+v", manualAuthorization, manualStart)
	}
	manual := antigravityManualDriver{config: func(input ProviderAuthManualConfig) (antigravityauth.Config, error) {
		configured, err := antigravityManualConfig(input)
		configured.Endpoints, configured.HTTPClient = endpoints, mock.Client()
		return configured, err
	}}
	manualRecord, err := manual.Exchange(ctx, oauthflow.Flow{Secrets: oauthflow.Secrets{
		Verifier: manualAuthorization.Secrets.Verifier, Params: input.OAuthParams(),
	}}, "fixture-code")
	if err != nil {
		t.Fatal(err)
	}
	form := log.take()
	if len(form) == 0 || !strings.Contains(form[0], "client_id=manual-client&") || !strings.Contains(form[0], "client_secret=manual-secret") ||
		!strings.Contains(form[0], "redirect_uri="+url.QueryEscape(input.RedirectURI)) ||
		manualRecord.Metadata[OAuthMetadataProfile] != antigravityOAuthProfileConsumerManual ||
		manualRecord.Metadata[OAuthMetadataClientID] != "manual-client" || manualRecord.Metadata[OAuthMetadataProjectID] != "fixture-project" {
		t.Fatalf("manual requests=%v record=%+v", form, manualRecord)
	}
}
