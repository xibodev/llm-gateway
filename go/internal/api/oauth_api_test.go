package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"

	codexauth "github.com/xibodev/llm-provider-auth/codex"
	copilotauth "github.com/xibodev/llm-provider-auth/copilot"
)

func TestOAuthCallbackURLRequiresConfiguredOriginOutsideLoopback(t *testing.T) {
	config.Update(func(settings *config.Settings) { settings.OAuthPublicBaseURL = "" })
	t.Cleanup(func() { config.Update(func(settings *config.Settings) { settings.OAuthPublicBaseURL = "" }) })

	request := httptest.NewRequest(http.MethodPost, "https://gateway.example.test/start", nil)
	request.Host = "gateway.example.test"
	request.Header.Set("X-Forwarded-Proto", "https")
	if _, err := oauthCallbackURL(request, "google_antigravity"); err == nil || !strings.Contains(err.Error(), "LLMGW_OAUTH_PUBLIC_BASE_URL") {
		t.Fatalf("unconfigured public callback accepted: %v", err)
	}

	config.Update(func(settings *config.Settings) { settings.OAuthPublicBaseURL = "https://approved.example.test" })
	callback, err := oauthCallbackURL(request, "google_antigravity")
	if err != nil || callback != "https://approved.example.test/oauth/callback/google_antigravity" {
		t.Fatalf("callback=%q err=%v", callback, err)
	}
	config.Update(func(settings *config.Settings) { settings.OAuthPublicBaseURL = "https://approved.example.test/base" })
	if _, err := oauthCallbackURL(request, "google_antigravity"); err == nil {
		t.Fatal("public OAuth origin with a path was accepted")
	}
}

func TestOAuthCallbackURLAllowsLocalLoopbackWithoutPublicBase(t *testing.T) {
	config.Update(func(settings *config.Settings) { settings.OAuthPublicBaseURL = "" })
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9791/start", nil)
	callback, err := oauthCallbackURL(request, "google_antigravity")
	if err != nil || callback != "http://127.0.0.1:9791/oauth/callback/google_antigravity" {
		t.Fatalf("callback=%q err=%v", callback, err)
	}
}

func TestOAuthAdapterPrefersExactConfiguredProviderID(t *testing.T) {
	oldProviders, oldClientID := config.Get().Providers, config.Get().OpenAICodexClientID
	t.Cleanup(func() {
		config.Update(func(s *config.Settings) {
			s.Providers, s.OpenAICodexClientID = oldProviders, oldClientID
		})
	})
	config.Update(func(s *config.Settings) {
		s.OpenAICodexClientID = "fixture-client"
		s.Providers = map[string]*config.ProviderConfig{
			"codex-work":     {Type: "openai_compatible", RegistryID: "codex"},
			"codex-personal": {Type: "openai_compatible", RegistryID: "openai_codex"},
		}
	})

	providerID, kind, _, err := oauthAdapterFor("codex-work")
	if err != nil || providerID != "codex-work" || kind != "openai_codex_oauth" {
		t.Fatalf("exact provider resolution id=%q kind=%q err=%v", providerID, kind, err)
	}
	if _, _, _, err := oauthAdapterFor("codex"); err == nil {
		t.Fatal("ambiguous registry alias should require a configured provider id")
	}

	config.Update(func(s *config.Settings) {
		delete(s.Providers, "codex-personal")
	})
	providerID, kind, _, err = oauthAdapterFor("codex")
	if err != nil || providerID != "codex-work" || kind != "openai_codex_oauth" {
		t.Fatalf("single alias resolution id=%q kind=%q err=%v", providerID, kind, err)
	}

	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{
			"codex": {Type: "openai_compatible"},
		}
	})
	if _, _, _, err := oauthAdapterFor("codex"); err == nil {
		t.Fatal("an exact custom provider named codex must not inherit the registry alias")
	}

	config.Update(func(s *config.Settings) {
		s.Providers["codex-work"] = &config.ProviderConfig{
			Type: "openai_compatible", RegistryID: "openai_codex",
		}
	})
	providerID, kind, _, err = oauthAdapterFor("openai_codex")
	if err != nil || providerID != "codex-work" || kind != "openai_codex_oauth" {
		t.Fatalf("registry fallback included alias-named custom provider id=%q kind=%q err=%v", providerID, kind, err)
	}

	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{
			"github_copilot": {Type: "openai_compatible", RegistryID: "github_copilot"},
		}
	})
	if _, _, _, err := oauthAdapterFor("github_copilot"); err == nil {
		t.Fatal("OAuth adapter should reject a registry/runtime mismatch")
	}
}

func TestAdminCanPersistCodexClientIDForOAuth(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LLMGW_STATE_DIR", dir)
	t.Setenv("LLMGW_CONFIG", dir+"/config.yaml")
	oldSettings := *config.Get()
	t.Cleanup(func() {
		config.Update(func(settings *config.Settings) {
			*settings = oldSettings
		})
	})
	config.Update(func(settings *config.Settings) {
		settings.Providers = map[string]*config.ProviderConfig{}
		settings.OpenAICodexClientID = ""
	})
	if err := configureOAuthClientID("openai_codex", "client-a", false); err == nil {
		t.Fatal("non-admin client ID update should fail")
	}
	if err := configureOAuthClientID("openai_codex", "client-a", true); err != nil {
		t.Fatal(err)
	}
	if got := config.Load().OpenAICodexClientID; got != "client-a" {
		t.Fatalf("persisted client ID=%q", got)
	}
}

func TestOAuthStartSurfacesProviderPersistenceFailure(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LLMGW_STATE_DIR", dir)
	t.Setenv("LLMGW_CONFIG", dir+"/config.yaml")
	t.Setenv("LLMGW_OPENAI_CODEX_CLIENT_ID", "fixture-codex-client")
	if err := os.WriteFile(config.ConfigFilePath(), []byte("providers: invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The device start precedes persistence, so it must be served locally: the
	// real endpoint rejects a fixture client ID.
	var starts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		starts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"device_auth_id":"fixture-device","user_code":"FIXTURE-CODE","interval":5,"expires_in":600}`))
	}))
	defer server.Close()
	providers.SetCodexEndpointsForTests(t, providers.CodexEndpoints{OAuth: codexauth.Endpoints{UserCodeURL: server.URL}})
	oldSettings := *config.Get()
	t.Cleanup(func() { config.Update(func(settings *config.Settings) { *settings = oldSettings }) })
	config.Update(func(settings *config.Settings) {
		settings.Providers = map[string]*config.ProviderConfig{}
		settings.OpenAICodexClientID = "fixture-codex-client"
	})

	_, err := startOAuthFlow(
		iam.Principal{ID: "owner", Kind: "human"}, "openai_codex", "", true,
		httptest.NewRequest(http.MethodPost, "http://127.0.0.1/oauth/start", nil),
		"personal", iam.ConnectionSourceUser, "device_code",
	)
	if err == nil || err.Error() != "could not persist the OAuth provider configuration" {
		t.Fatalf("start error=%v", err)
	}
	if starts.Load() != 1 {
		t.Fatalf("device starts=%d, want 1", starts.Load())
	}
	if config.Get().Providers["codex"] != nil {
		t.Fatal("failed persistence installed the provider in memory")
	}
}

func TestUserOAuthStartCannotCreateGlobalProvider(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LLMGW_STATE_DIR", dir)
	t.Setenv("LLMGW_CONFIG", dir+"/config.yaml")
	oldSettings := *config.Get()
	t.Cleanup(func() { config.Update(func(settings *config.Settings) { *settings = oldSettings }) })
	config.Update(func(settings *config.Settings) {
		settings.Providers = map[string]*config.ProviderConfig{}
		settings.OpenAICodexClientID = "fixture-client"
	})
	_, err := startOAuthFlow(
		iam.Principal{ID: "owner", Kind: "human"}, "openai_codex", "", false,
		httptest.NewRequest(http.MethodPost, "http://127.0.0.1/oauth/start", nil),
		"personal", iam.ConnectionSourceUser, "device_code",
	)
	if err == nil || !strings.Contains(err.Error(), "administrator") {
		t.Fatalf("start error=%v", err)
	}
	if config.Get().Providers["codex"] != nil {
		t.Fatal("self-service OAuth created a global provider")
	}
}

func TestAdminCodexClientIDRollsBackWhenFlowStartFails(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LLMGW_STATE_DIR", dir)
	t.Setenv("LLMGW_CONFIG", dir+"/config.yaml")
	oldSettings := *config.Get()
	t.Cleanup(func() { config.Update(func(settings *config.Settings) { *settings = oldSettings }) })
	config.Update(func(settings *config.Settings) {
		settings.Providers = map[string]*config.ProviderConfig{
			"codex": {Type: "openai_compatible", RegistryID: "openai_codex"},
		}
		settings.OpenAICodexClientID = "previous-client"
	})
	if err := config.Save(); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "failed", http.StatusBadGateway)
	}))
	defer server.Close()
	providers.SetCodexEndpointsForTests(t, providers.CodexEndpoints{OAuth: codexauth.Endpoints{UserCodeURL: server.URL}})

	_, err := startOAuthFlow(
		iam.Principal{ID: "owner", Kind: "human"}, "codex", "replacement-client", true,
		httptest.NewRequest(http.MethodPost, "http://127.0.0.1/oauth/start", nil),
		"personal", iam.ConnectionSourceAdmin, "device_code",
	)
	if err == nil {
		t.Fatal("failed upstream start unexpectedly succeeded")
	}
	if got := config.Get().OpenAICodexClientID; got != "previous-client" {
		t.Fatalf("runtime client ID=%q", got)
	}
	if got := config.Load().OpenAICodexClientID; got != "previous-client" {
		t.Fatalf("persisted client ID=%q", got)
	}
}

func TestCodexOAuthStartUsesRequestedBrowserFlow(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LLMGW_STATE_DIR", dir)
	t.Setenv("LLMGW_CONFIG", dir+"/config.yaml")
	oldSettings := *config.Get()
	t.Cleanup(func() { config.Update(func(settings *config.Settings) { *settings = oldSettings }) })
	config.Update(func(settings *config.Settings) {
		settings.Providers = map[string]*config.ProviderConfig{
			"codex": {Type: "openai_compatible", RegistryID: "openai_codex"},
		}
		settings.OpenAICodexClientID = "fixture-client"
	})
	response, err := startCodexBrowserFlow(
		iam.Principal{ID: "owner", Kind: "human"}, "codex", "fixture-client", true, "personal", iam.ConnectionSourceAdmin,
	)
	if err != nil {
		t.Fatal(err)
	}
	if response["flow"] != codexBrowserManualProfile || response["authorization_url"] == "" || response["flow_id"] == "" {
		t.Fatalf("response=%+v", response)
	}
	authorizationURL, _ := url.Parse(response["authorization_url"].(string))
	if got := authorizationURL.Query().Get("redirect_uri"); got != "http://localhost:1455/auth/callback" {
		t.Fatalf("redirect_uri=%q", got)
	}
}

func TestOAuthFlowStoreCapsOutstandingFlowsPerPrincipal(t *testing.T) {
	oauthFlows.Lock()
	oldFlows := oauthFlows.values
	oldGeneration := oauthFlows.nextGeneration
	oauthFlows.values = map[string]oauthFlowState{}
	oauthFlows.nextGeneration = 0
	oauthFlows.Unlock()
	t.Cleanup(func() {
		oauthFlows.Lock()
		oauthFlows.values = oldFlows
		oauthFlows.nextGeneration = oldGeneration
		oauthFlows.Unlock()
	})
	now := time.Now().Unix()
	for index := 0; index < maxOAuthFlowsPerPrincipal+1; index++ {
		key := oauthFlowKey("principal", "copilot", fmt.Sprintf("device-%d", index))
		storeOAuthFlow(key, oauthFlowState{
			PrincipalID: "principal", ProviderID: "copilot",
			StartedAt: now + int64(index), ExpiresAt: now + 3600,
		})
	}
	oauthFlows.Lock()
	defer oauthFlows.Unlock()
	count := 0
	for _, flow := range oauthFlows.values {
		if flow.PrincipalID == "principal" {
			count++
		}
	}
	if count != maxOAuthFlowsPerPrincipal {
		t.Fatalf("outstanding flow count=%d want %d", count, maxOAuthFlowsPerPrincipal)
	}
	if _, exists := oauthFlows.values[oauthFlowKey("principal", "copilot", "device-0")]; exists {
		t.Fatal("oldest outstanding OAuth flow was not evicted")
	}
}

func TestBrowserOAuthFlowStoreCapsOutstandingFlowsPerPrincipal(t *testing.T) {
	browserOAuthFlows.Lock()
	oldFlows := browserOAuthFlows.values
	oldGeneration := browserOAuthFlows.nextGeneration
	browserOAuthFlows.values = map[string]browserOAuthFlowState{}
	browserOAuthFlows.nextGeneration = 0
	browserOAuthFlows.Unlock()
	t.Cleanup(func() {
		browserOAuthFlows.Lock()
		browserOAuthFlows.values = oldFlows
		browserOAuthFlows.nextGeneration = oldGeneration
		browserOAuthFlows.Unlock()
	})
	now := time.Now().Unix()
	for index := 0; index < maxOAuthFlowsPerPrincipal+1; index++ {
		storeBrowserOAuthFlow(fmt.Sprintf("state-%d", index), browserOAuthFlowState{
			PrincipalID: "principal", ProviderID: "google-antigravity", StartedAt: now + int64(index), ExpiresAt: now + 3600,
		})
	}
	browserOAuthFlows.Lock()
	defer browserOAuthFlows.Unlock()
	count := 0
	for _, flow := range browserOAuthFlows.values {
		if flow.PrincipalID == "principal" {
			count++
		}
	}
	if count != maxOAuthFlowsPerPrincipal {
		t.Fatalf("outstanding browser flow count=%d want %d", count, maxOAuthFlowsPerPrincipal)
	}
	if _, exists := browserOAuthFlows.values["state-0"]; exists {
		t.Fatal("oldest outstanding browser OAuth flow was not evicted")
	}
}

func TestAntigravityOAuthUsesStableRegistryCallbackForCustomInstance(t *testing.T) {
	oldProviders := config.Get().Providers
	t.Cleanup(func() { config.Update(func(s *config.Settings) { s.Providers = oldProviders }) })
	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{"antigravity-work": {Type: "google_antigravity", RegistryID: "google_antigravity"}}
	})
	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9791/start", nil)
	callback, err := oauthCallbackURL(request, oauthRegistryIDForRef("antigravity-work"))
	if err != nil || callback != "http://127.0.0.1:9791/oauth/callback/google_antigravity" {
		t.Fatalf("callback=%q err=%v", callback, err)
	}
}

func TestBrowserOAuthWrongCallbackPathDoesNotConsumeFlow(t *testing.T) {
	browserOAuthFlows.Lock()
	oldFlows := browserOAuthFlows.values
	browserOAuthFlows.values = map[string]browserOAuthFlowState{
		"state": {
			ExpectedState: "state", CallbackID: "google_antigravity", ExpiresAt: time.Now().Add(time.Minute).Unix(),
		},
	}
	browserOAuthFlows.Unlock()
	t.Cleanup(func() {
		browserOAuthFlows.Lock()
		browserOAuthFlows.values = oldFlows
		browserOAuthFlows.Unlock()
	})
	request := httptest.NewRequest(http.MethodGet, "/oauth/callback/wrong?state=state", nil)
	request.SetPathValue("provider_id", "wrong")
	recorder := httptest.NewRecorder()
	handleOAuthBrowserCallback(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", recorder.Code)
	}
	browserOAuthFlows.Lock()
	flow := browserOAuthFlows.values["state"]
	browserOAuthFlows.Unlock()
	if flow.Completing {
		t.Fatal("wrong callback path consumed browser OAuth flow")
	}
}

func TestOAuthPollCannotResurrectEvictedFlow(t *testing.T) {
	oauthFlows.Lock()
	oldFlows := oauthFlows.values
	oldGeneration := oauthFlows.nextGeneration
	oauthFlows.values = map[string]oauthFlowState{}
	oauthFlows.nextGeneration = 0
	oauthFlows.Unlock()
	t.Cleanup(func() {
		oauthFlows.Lock()
		oauthFlows.values = oldFlows
		oauthFlows.nextGeneration = oldGeneration
		oauthFlows.Unlock()
	})

	key := oauthFlowKey("principal", "copilot", "device")
	storeOAuthFlow(key, oauthFlowState{
		PrincipalID: "principal", ProviderID: "copilot",
		StartedAt: time.Now().Unix(), ExpiresAt: time.Now().Add(time.Hour).Unix(),
		Interval: 5,
	})
	oauthFlows.Lock()
	observed := oauthFlows.values[key]
	delete(oauthFlows.values, key)
	oauthFlows.Unlock()

	if applyOAuthPollResult(
		key, observed, providers.ProviderAuthPoll{Status: "pending"}, time.Now().Unix(),
	) {
		t.Fatal("evicted OAuth flow was reinserted")
	}
	oauthFlows.Lock()
	_, exists := oauthFlows.values[key]
	oauthFlows.Unlock()
	if exists {
		t.Fatal("evicted OAuth flow remains stored")
	}
}

func TestOAuthProjectionSanitizesBeforeRuneLimitAndPreservesStatuses(t *testing.T) {
	secret := "llmgw_" + strings.Repeat("a", 32)
	detail := secret + " Bearer top-secret user@example.test?token=query-secret " + strings.Repeat("界", 400)
	for _, status := range []string{"pending", "slow_down", "authorized", "expired", "denied", "error"} {
		response := safeOAuthPollResponse(status, detail)
		if response["status"] != status {
			t.Fatalf("status %q projected as %+v", status, response)
		}
		projected := response["error"].(string)
		if strings.Contains(projected, secret) || strings.Contains(projected, "top-secret") || strings.Contains(projected, "user@example.test") || strings.Contains(projected, "query-secret") {
			t.Fatalf("status %q retained sensitive diagnostics: %q", status, projected)
		}
		if len([]rune(projected)) > maxOAuthDiagnosticChars {
			t.Fatalf("status %q error has %d runes", status, len([]rune(projected)))
		}
	}
}

func TestOAuthHandlersSanitizeMaliciousPollDiagnostics(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	secret := "llmgw_" + strings.Repeat("b", 32)
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/device" {
			_, _ = w.Write([]byte(`{"device_code":"safe-device","user_code":"SAFE-CODE","verification_uri":"https://example.test/verify","interval":1,"expires_in":60}`))
			return
		}
		_, _ = fmt.Fprintf(w, `{"error":%q}`, secret+" Bearer top-secret user@example.test?token=query-secret "+strings.Repeat("界", 400))
	}))
	defer mock.Close()
	t.Cleanup(providers.SetCopilotEndpointsForTests(copilotauth.Endpoints{
		DeviceCodeURL: mock.URL + "/device", AccessTokenURL: mock.URL + "/token",
	}))

	oldSettings := *config.Get()
	t.Cleanup(func() { config.Update(func(settings *config.Settings) { *settings = oldSettings }) })
	config.Update(func(settings *config.Settings) {
		settings.APIKey = "admin-secret"
		settings.SSOEnabled, settings.SSOSharedSecret, settings.SSOAutoProvision = true, "proxy-secret", true
		settings.Providers = map[string]*config.ProviderConfig{
			"copilot": {Type: "github_copilot", RegistryID: "github_copilot"},
		}
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	principal, err := iam.CreatePrincipal("human", "admin-oauth-user", "", "Admin OAuth User")
	if err != nil {
		t.Fatal(err)
	}
	gateway := httptest.NewServer(NewServer(Runtime{}))
	defer gateway.Close()

	checks := []struct {
		name, startPath, pollPath, token string
		user                             bool
		generic                          bool
	}{
		{name: "generic user", startPath: "/user/api/connections/copilot/oauth/start", pollPath: "/user/api/connections/copilot/oauth/poll", user: true, generic: true},
		{name: "generic admin", startPath: "/admin/api/principals/" + principal.ID + "/connections/copilot/oauth/start", pollPath: "/admin/api/principals/" + principal.ID + "/connections/copilot/oauth/poll", token: "admin-secret", generic: true},
		{name: "legacy user", startPath: "/user/api/copilot/login/start", pollPath: "/user/api/copilot/login/poll", user: true},
		{name: "legacy admin", startPath: "/admin/api/copilot/login/start", pollPath: "/admin/api/copilot/login/poll", token: "admin-secret"},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			request := func(path string, body map[string]any) (int, map[string]any) {
				if check.user {
					return ssoConnectionRequest(t, gateway.URL, "oauth-user", http.MethodPost, path, body)
				}
				return jsonRequest(t, gateway.URL+path, http.MethodPost, check.token, body)
			}
			status, started := request(check.startPath, map[string]any{})
			if status != http.StatusOK || started["device_code"] != "safe-device" || started["user_code"] != "SAFE-CODE" || started["verification_uri"] != "https://example.test/verify" || started["interval"] != float64(1) || started["expires_in"] != float64(60) {
				t.Fatalf("start status=%d payload=%+v", status, started)
			}
			if check.generic {
				oauthFlows.Lock()
				for key, flow := range oauthFlows.values {
					if strings.HasSuffix(key, "|safe-device") {
						flow.NextPollAt = 0
						oauthFlows.values[key] = flow
					}
				}
				oauthFlows.Unlock()
			}
			status, response := request(check.pollPath, map[string]any{"device_code": "safe-device"})
			if status != http.StatusOK || response["status"] != "denied" {
				t.Fatalf("poll status=%d payload=%+v", status, response)
			}
			encoded, _ := json.Marshal(response)
			text := string(encoded)
			for _, sensitive := range []string{secret, "top-secret", "user@example.test", "query-secret", "access_token", "refresh_token", "id_token"} {
				if strings.Contains(text, sensitive) {
					t.Fatalf("response leaked %q: %s", sensitive, text)
				}
			}
			detail, _ := response["error"].(string)
			if len([]rune(detail)) > maxOAuthDiagnosticChars {
				t.Fatalf("error has %d runes: %q", len([]rune(detail)), detail)
			}
		})
	}
}

func TestLegacyAdminCopilotPollPersistsAuthorizedTokenWithoutProjectingIt(t *testing.T) {
	cacheDir := t.TempDir()
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"authorized-cache-token"}`))
	}))
	defer mock.Close()
	t.Cleanup(providers.SetCopilotEndpointsForTests(copilotauth.Endpoints{AccessTokenURL: mock.URL}))

	oldSettings := *config.Get()
	t.Cleanup(func() { config.Update(func(settings *config.Settings) { *settings = oldSettings }) })
	config.Update(func(settings *config.Settings) {
		settings.APIKey = "admin-secret"
		settings.GithubCopilotCacheDir = cacheDir
		settings.GithubCopilotOAuthToken = ""
		settings.GithubCopilotUseGhCLI = false
	})
	gateway := httptest.NewServer(NewServer(Runtime{}))
	defer gateway.Close()

	status, response := jsonRequest(
		t, gateway.URL+"/admin/api/copilot/login/poll", http.MethodPost, "admin-secret",
		map[string]any{"device_code": "authorized-device"},
	)
	if status != http.StatusOK || response["status"] != "authorized" {
		t.Fatalf("poll status=%d payload=%+v", status, response)
	}
	encoded, _ := json.Marshal(response)
	if strings.Contains(string(encoded), "authorized-cache-token") || response["access_token"] != nil || response["refresh_token"] != nil || response["id_token"] != nil {
		t.Fatalf("authorized poll projected a token: %s", encoded)
	}
	authStatus := providers.CopilotAuth().AuthStatus()
	if authStatus["active_source"] != "cache" || authStatus["cache_present"] != true {
		t.Fatalf("authorized poll did not authenticate from cache: %+v", authStatus)
	}
	resolved, err := providers.CopilotAuth().ResolveOAuthToken()
	if err != nil || resolved != "authorized-cache-token" {
		t.Fatalf("persisted token resolution token=%q err=%v", resolved, err)
	}
}

func TestOAuthPollRejectsUntrackedDeviceCode(t *testing.T) {
	oldProviders := config.Get().Providers
	t.Cleanup(func() {
		config.Update(func(s *config.Settings) { s.Providers = oldProviders })
	})
	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{
			"copilot": {Type: "github_copilot", RegistryID: "github_copilot"},
		}
	})
	response := pollOAuthFlow(
		iam.Principal{ID: "principal", Kind: "human"},
		"copilot", "untracked-device", "", iam.ConnectionSourceUser,
	)
	if response["status"] != "expired" {
		t.Fatalf("untracked poll response=%+v", response)
	}
}

func TestUserOAuthDeviceFlowUsesMockedEndpointsAndNeverReturnsTokens(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	var tokenPolls atomic.Int32
	var sessionExchanges atomic.Int32
	var catalogRequests atomic.Int32
	var mockURL string
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/device":
			_, _ = w.Write([]byte(`{"device_code":"fake-device-code","user_code":"ABCD-1234","verification_uri":"https://example.test/verify","interval":1,"expires_in":30}`))
		case "/token":
			if tokenPolls.Add(1) == 1 {
				_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
			} else {
				_, _ = w.Write([]byte(`{"access_token":"fake-github-access-token"}`))
			}
		case "/session":
			sessionExchanges.Add(1)
			if r.Header.Get("Authorization") != "token fake-github-access-token" {
				t.Fatalf("session exchange used unexpected authorization")
			}
			_, _ = fmt.Fprintf(w, `{"token":"fake-copilot-session-token","expires_at":4102444800,"endpoints":{"api":%q}}`, mockURL)
		case "/models":
			catalogRequests.Add(1)
			if r.Header.Get("Authorization") != "Bearer fake-copilot-session-token" {
				t.Fatalf("catalog used unexpected authorization")
			}
			_, _ = w.Write([]byte(`{"data":[{"id":"copilot-fixture-model","owned_by":"github"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer mock.Close()
	mockURL = mock.URL
	t.Cleanup(providers.SetCopilotEndpointsForTests(copilotauth.Endpoints{
		DeviceCodeURL:   mock.URL + "/device",
		AccessTokenURL:  mock.URL + "/token",
		SessionTokenURL: mock.URL + "/session",
	}))

	oldSSOEnabled := config.Get().SSOEnabled
	oldSSOSecret := config.Get().SSOSharedSecret
	oldAutoProvision := config.Get().SSOAutoProvision
	oldCredentialKey := config.Get().CredentialEncryptionKey
	oldProviders := config.Get().Providers
	oldCacheDir := config.Get().GithubCopilotCacheDir
	oldAllowCopilotProxy := config.Get().AllowCopilotProxy
	t.Cleanup(func() {
		config.Update(func(s *config.Settings) {
			s.SSOEnabled, s.SSOSharedSecret, s.SSOAutoProvision = oldSSOEnabled, oldSSOSecret, oldAutoProvision
			s.CredentialEncryptionKey, s.Providers, s.GithubCopilotCacheDir = oldCredentialKey, oldProviders, oldCacheDir
			s.AllowCopilotProxy = oldAllowCopilotProxy
		})
	})
	key := make([]byte, 32)
	config.Update(func(s *config.Settings) {
		s.SSOEnabled = true
		s.SSOSharedSecret = "proxy-secret"
		s.SSOAutoProvision = true
		s.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(key)
		s.GithubCopilotCacheDir = t.TempDir()
		s.AllowCopilotProxy = true
		s.Providers = map[string]*config.ProviderConfig{"copilot": {Type: "github_copilot", RegistryID: "github_copilot"}}
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(NewServer(Runtime{}))
	defer server.Close()

	status, started := ssoConnectionRequest(t, server.URL, "oauth-user", http.MethodPost, "/user/api/connections/copilot/oauth/start", map[string]any{})
	if status != http.StatusOK || started["user_code"] != "ABCD-1234" || started["access_token"] != nil {
		t.Fatalf("start status=%d payload=%+v", status, started)
	}
	deviceCode, _ := started["device_code"].(string)
	if deviceCode == "" {
		t.Fatalf("missing device code: %+v", started)
	}

	status, throttled := ssoConnectionRequest(t, server.URL, "oauth-user", http.MethodPost, "/user/api/connections/copilot/oauth/poll", map[string]any{"device_code": deviceCode})
	if status != http.StatusOK || throttled["status"] != "slow_down" || tokenPolls.Load() != 0 {
		t.Fatalf("initial interval was not enforced: status=%d payload=%+v polls=%d", status, throttled, tokenPolls.Load())
	}
	time.Sleep(1100 * time.Millisecond)
	status, pending := ssoConnectionRequest(t, server.URL, "oauth-user", http.MethodPost, "/user/api/connections/copilot/oauth/poll", map[string]any{"device_code": deviceCode})
	if status != http.StatusOK || pending["status"] != "pending" || tokenPolls.Load() != 1 {
		t.Fatalf("pending poll status=%d payload=%+v polls=%d", status, pending, tokenPolls.Load())
	}
	status, slowed := ssoConnectionRequest(t, server.URL, "oauth-user", http.MethodPost, "/user/api/connections/copilot/oauth/poll", map[string]any{"device_code": deviceCode})
	if status != http.StatusOK || slowed["status"] != "slow_down" || tokenPolls.Load() != 1 {
		t.Fatalf("slow_down poll status=%d payload=%+v polls=%d", status, slowed, tokenPolls.Load())
	}
	time.Sleep(1100 * time.Millisecond)
	status, authorized := ssoConnectionRequest(t, server.URL, "oauth-user", http.MethodPost, "/user/api/connections/copilot/oauth/poll", map[string]any{"device_code": deviceCode})
	if status != http.StatusOK || authorized["status"] != "authorized" || tokenPolls.Load() != 2 {
		t.Fatalf("authorized poll status=%d payload=%+v polls=%d", status, authorized, tokenPolls.Load())
	}
	encoded, _ := json.Marshal(authorized)
	if strings.Contains(string(encoded), "fake-github-access-token") || strings.Contains(string(encoded), "fake-copilot-session-token") {
		t.Fatalf("OAuth poll leaked a token: %s", encoded)
	}

	status, listed := ssoConnectionRequest(t, server.URL, "oauth-user", http.MethodGet, "/user/api/connections?provider_id=copilot", nil)
	if status != http.StatusOK {
		t.Fatalf("list status=%d payload=%+v", status, listed)
	}
	connections := listed["connections"].([]any)
	if len(connections) != 1 {
		t.Fatalf("connections=%+v", connections)
	}
	connection := connections[0].(map[string]any)
	if connection["oauth_status"] != "active" || connection["access_token"] != nil || connection["refresh_token"] != nil {
		t.Fatalf("safe connection response=%+v", connection)
	}

	status, tested := ssoConnectionRequest(t, server.URL, "oauth-user", http.MethodPost, "/user/api/providers/copilot/test", map[string]any{})
	if status != http.StatusOK || tested["success"] != true || tested["model_count"] != float64(1) || tested["owner_scope"] != "human_owner" {
		t.Fatalf("catalog test status=%d payload=%+v", status, tested)
	}
	if sessionExchanges.Load() != 1 || catalogRequests.Load() != 1 {
		t.Fatalf("session exchanges=%d catalog requests=%d", sessionExchanges.Load(), catalogRequests.Load())
	}

	status, refreshed := ssoConnectionRequest(t, server.URL, "oauth-user", http.MethodPost, "/user/api/connections/copilot/oauth/refresh", map[string]any{})
	if status != http.StatusOK || refreshed["status"] != "refreshed" {
		t.Fatalf("refresh status=%d payload=%+v", status, refreshed)
	}
	encoded, _ = json.Marshal(refreshed)
	if strings.Contains(string(encoded), "fake-github-access-token") || strings.Contains(string(encoded), "fake-copilot-session-token") {
		t.Fatalf("OAuth refresh leaked a token: %s", encoded)
	}
}

func TestUserCodexOAuthStoresBoundProfileAndSurvivesReload(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LLMGW_STATE_DIR", dir)
	t.Setenv("LLMGW_CONFIG", dir+"/config.yaml")
	t.Setenv("LLMGW_OPENAI_CODEX_CLIENT_ID", "fixture-codex-client")
	t.Setenv("LLMGW_SSO_ENABLED", "true")
	t.Setenv("LLMGW_SSO_SHARED_SECRET", "proxy-secret")
	t.Setenv("LLMGW_SSO_AUTO_PROVISION", "true")
	credentialKey := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	t.Setenv("LLMGW_CREDENTIAL_ENCRYPTION_KEY", credentialKey)
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	deviceCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/usercode":
			body := map[string]string{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["client_id"] != "fixture-codex-client" {
				t.Fatalf("usercode body=%+v", body)
			}
			_, _ = w.Write([]byte(`{"device_auth_id":"codex-device","usercode":"CODEX-123","interval":"1","expires_in":60}`))
		case "/device-token":
			deviceCalls++
			_, _ = w.Write([]byte(`{"authorization_code":"codex-auth-code","code_challenge":"server-challenge","code_verifier":"server-verifier"}`))
		case "/oauth-token":
			raw := make([]byte, r.ContentLength)
			_, _ = r.Body.Read(raw)
			form, _ := url.ParseQuery(string(raw))
			if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || form.Get("grant_type") != "authorization_code" || form.Get("client_id") != "fixture-codex-client" || form.Get("code") != "codex-auth-code" || form.Get("code_verifier") != "server-verifier" || form.Get("redirect_uri") != codexauth.DeviceAuthRedirectURI || len(form) != 5 {
				t.Fatalf("exchange form=%v", form)
			}
			_, _ = w.Write([]byte(`{"access_token":"codex-access-token","refresh_token":"codex-refresh-token","id_token":"codex-id-token","expires_in":120,"account_id":"workspace-42","account_label":"Fixture workspace"}`))
		case "/models":
			if r.URL.Query().Get("client_version") == "" {
				t.Fatal("catalog request omitted client_version")
			}
			if r.Header.Get("Authorization") != "Bearer codex-access-token" || r.Header.Get("ChatGPT-Account-ID") != "workspace-42" {
				t.Fatalf("catalog headers auth=%q account=%q", r.Header.Get("Authorization"), r.Header.Get("ChatGPT-Account-ID"))
			}
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5-codex","owned_by":"openai","description":"GPT-5 Codex","supported_in_api":true,"visibility":"list","supported_endpoints":["/responses"]}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	providers.SetCodexEndpointsForTests(t, providers.CodexEndpoints{
		OAuth: codexauth.Endpoints{
			UserCodeURL: server.URL + "/usercode", DeviceTokenURL: server.URL + "/device-token",
			OAuthTokenURL: server.URL + "/oauth-token",
		},
		ModelsURL: server.URL + "/models",
	})

	oldSSOEnabled, oldSSOSecret := config.Get().SSOEnabled, config.Get().SSOSharedSecret
	oldAutoProvision, oldCredentialKey := config.Get().SSOAutoProvision, config.Get().CredentialEncryptionKey
	oldProviders, oldClientID := config.Get().Providers, config.Get().OpenAICodexClientID
	t.Cleanup(func() {
		config.Update(func(s *config.Settings) {
			s.SSOEnabled, s.SSOSharedSecret, s.SSOAutoProvision = oldSSOEnabled, oldSSOSecret, oldAutoProvision
			s.CredentialEncryptionKey, s.Providers, s.OpenAICodexClientID = oldCredentialKey, oldProviders, oldClientID
		})
	})
	config.Update(func(s *config.Settings) {
		s.SSOEnabled, s.SSOSharedSecret, s.SSOAutoProvision = true, "proxy-secret", true
		s.CredentialEncryptionKey = credentialKey
		s.Providers = map[string]*config.ProviderConfig{
			"codex": {Type: "openai_compatible", RegistryID: "openai_codex"},
		}
		s.OpenAICodexClientID = "fixture-codex-client"
	})
	if err := config.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	gateway := httptest.NewServer(NewServer(Runtime{}))
	defer gateway.Close()

	status, started := ssoConnectionRequest(t, gateway.URL, "codex-user", http.MethodPost, "/user/api/connections/openai_codex/oauth/start", map[string]any{})
	if status != http.StatusOK || started["provider_id"] != "codex" || started["user_code"] != "CODEX-123" || started["verification_uri"] != codexauth.DeviceVerificationURL {
		t.Fatalf("start status=%d payload=%+v", status, started)
	}
	deviceCode, _ := started["device_code"].(string)
	oauthFlows.Lock()
	flowKey := oauthFlowKey("", "", "")
	for key, flow := range oauthFlows.values {
		if strings.Contains(key, deviceCode) {
			flow.NextPollAt = 0
			oauthFlows.values[key] = flow
			flowKey = key
			break
		}
	}
	oauthFlows.Unlock()
	if flowKey == oauthFlowKey("", "", "") {
		t.Fatal("Codex flow state was not retained server-side")
	}
	status, authorized := ssoConnectionRequest(t, gateway.URL, "codex-user", http.MethodPost, "/user/api/connections/openai_codex/oauth/poll", map[string]any{"device_code": deviceCode})
	if status != http.StatusOK || authorized["status"] != "authorized" || deviceCalls != 1 {
		t.Fatalf("poll status=%d payload=%+v calls=%d", status, authorized, deviceCalls)
	}
	serialized, _ := json.Marshal(authorized)
	for _, secret := range []string{"codex-access-token", "codex-refresh-token", "codex-id-token"} {
		if strings.Contains(string(serialized), secret) {
			t.Fatalf("OAuth response leaked %q: %s", secret, serialized)
		}
	}
	status, tested := ssoConnectionRequest(t, gateway.URL, "codex-user", http.MethodPost, "/user/api/providers/codex/test", map[string]any{})
	if status != http.StatusOK || tested["success"] != true || tested["model_count"] != float64(1) {
		t.Fatalf("catalog test status=%d payload=%+v", status, tested)
	}
	status, catalog := ssoConnectionRequest(t, gateway.URL, "codex-user", http.MethodGet, "/user/api/models", nil)
	if status != http.StatusOK {
		t.Fatalf("catalog status=%d payload=%+v", status, catalog)
	}
	rows := catalog["data"].([]any)
	var row map[string]any
	for _, raw := range rows {
		candidate := raw.(map[string]any)
		if candidate["id"] == "codex/gpt-5-codex" {
			row = candidate
			break
		}
	}
	if row == nil || row["owned_by"] != "codex" || row["display_name"] != "GPT-5 Codex" {
		t.Fatalf("Codex model projection=%+v", row)
	}
	status, listed := ssoConnectionRequest(t, gateway.URL, "codex-user", http.MethodGet, "/user/api/connections?provider_id=codex", nil)
	if status != http.StatusOK {
		t.Fatalf("list status=%d payload=%+v", status, listed)
	}
	connections := listed["connections"].([]any)
	if len(connections) != 1 {
		t.Fatalf("connections=%+v", connections)
	}
	connection := connections[0].(map[string]any)
	if connection["oauth_account_id"] != "workspace-42" || connection["access_token"] != nil || connection["refresh_token"] != nil {
		t.Fatalf("safe Codex connection=%+v", connection)
	}
	envelope, _, ok, err := iam.OAuthProviderConnectionSecret(stringValueForTest(connection["principal_id"]), "codex", "")
	if err != nil || !ok || envelope.OAuthProfile != "device_client_id" || envelope.OAuthClientID != "fixture-codex-client" {
		t.Fatalf("stored Codex profile=%+v ok=%v err=%v", envelope, ok, err)
	}

	config.Load()
	if provider := config.Get().Providers["codex"]; provider == nil || provider.RegistryID != "openai_codex" {
		t.Fatalf("reloaded provider=%+v", provider)
	}
	reloadedConnections, err := iam.ListProviderConnections(stringValueForTest(connection["principal_id"]), "codex")
	if err != nil {
		t.Fatal(err)
	}
	if len(reloadedConnections) != 1 || reloadedConnections[0].OAuthAccountID != "workspace-42" {
		t.Fatalf("reloaded connections=%+v", reloadedConnections)
	}
}

func stringValueForTest(value any) string {
	text, _ := value.(string)
	return text
}

func TestCodexOAuthFlowExpiresBeforePoll(t *testing.T) {
	oldClientID := config.Get().OpenAICodexClientID
	config.Update(func(s *config.Settings) { s.OpenAICodexClientID = "fixture-codex-client" })
	t.Cleanup(func() { config.Update(func(s *config.Settings) { s.OpenAICodexClientID = oldClientID }) })
	principal := iam.Principal{ID: "prn-expire", Kind: "human"}
	oauthFlows.Lock()
	oauthFlows.values[oauthFlowKey(principal.ID, "codex", "expired-device")] = oauthFlowState{PrincipalID: principal.ID, ProviderID: "codex", ExpiresAt: time.Now().Add(-time.Second).Unix()}
	oauthFlows.Unlock()
	response := pollOAuthFlow(principal, "codex", "expired-device", "", iam.ConnectionSourceUser)
	if response["status"] != "expired" {
		t.Fatalf("expired flow response=%+v", response)
	}
}

func TestManualAuthorizationCodeValidatesURLShape(t *testing.T) {
	code, state, fromURL, err := manualAuthorizationCode("https://callback.example.test/oauth?code=fixture-code&state=fixture-state")
	if err != nil || code != "fixture-code" || state != "fixture-state" || !fromURL {
		t.Fatalf("code=%q state=%q fromURL=%v err=%v", code, state, fromURL, err)
	}
	code, state, fromURL, err = manualAuthorizationCode("fixture-code")
	if err != nil || code != "fixture-code" || state != "" || fromURL {
		t.Fatalf("plain code=%q state=%q fromURL=%v err=%v", code, state, fromURL, err)
	}
	for _, invalid := range []string{"", "https://callback.example.test/oauth?code=only", "code=not-a-plain-code"} {
		if _, _, _, err := manualAuthorizationCode(invalid); err == nil {
			t.Fatalf("accepted invalid authorization response %q", invalid)
		}
	}
}

func TestConsumerManualFlowCreatesProviderOnlyAfterSuccessfulCompletion(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LLMGW_STATE_DIR", dir)
	t.Setenv("LLMGW_CONFIG", dir+"/config.yaml")
	t.Setenv("LLMGW_CREDENTIAL_ENCRYPTION_KEY", base64.RawURLEncoding.EncodeToString(make([]byte, 32)))
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	oldSettings := *config.Get()
	t.Cleanup(func() { config.Update(func(settings *config.Settings) { *settings = oldSettings }) })
	config.Update(func(settings *config.Settings) {
		settings.Providers = map[string]*config.ProviderConfig{}
		settings.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
		settings.GoogleAntigravityOAuthProfile = antigravityManualProfile
		settings.GoogleAntigravityClientID = "fixture-client"
		settings.GoogleAntigravityClientMode = "public"
		settings.GoogleAntigravityRedirectURI = "https://callback.example.test/oauth"
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	principal, err := iam.CreatePrincipal("human", "fixture:manual-owner", "", "Manual Owner")
	if err != nil {
		t.Fatal(err)
	}
	oldStart, oldComplete := startManualProviderOAuth, completeManualProviderOAuth
	t.Cleanup(func() { startManualProviderOAuth, completeManualProviderOAuth = oldStart, oldComplete })
	startManualProviderOAuth = func(_ context.Context, _ providers.ManualProviderAuthAdapter, input providers.ProviderAuthManualConfig) (providers.ProviderAuthBrowserStart, error) {
		if input.ClientMode != "public" || input.RedirectURI != "https://callback.example.test/oauth" {
			t.Fatalf("manual config=%+v", input)
		}
		return providers.ProviderAuthBrowserStart{
			AuthorizationURL: "https://accounts.example.test/authorize?state=fixture-state&code_challenge=fixture-challenge",
			PrivateState:     `{"state":"fixture-state","code_verifier":"fixture-verifier"}`, ExpiresIn: 600,
		}, nil
	}
	completeManualProviderOAuth = func(_ context.Context, _ providers.ManualProviderAuthAdapter, code, privateState string, input providers.ProviderAuthManualConfig) (providers.ProviderAuthPoll, error) {
		if code != "fixture-code" || !strings.Contains(privateState, "fixture-verifier") || input.ClientID != "fixture-client" {
			t.Fatalf("completion code=%q state=%q config=%+v", code, privateState, input)
		}
		return providers.ProviderAuthPoll{Status: "authorized", AccessToken: "fixture-access", RefreshToken: "fixture-refresh", ProjectID: "fixture-project"}, nil
	}
	started, err := startManualOAuthFlow(principal, "google_antigravity", "personal", iam.ConnectionSourceAdmin, providers.ProviderAuthManualConfig{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if config.Get().Providers["google-antigravity"] != nil {
		t.Fatal("manual start created the provider before authorization")
	}
	connections, err := iam.ListProviderConnections(principal.ID, "google-antigravity")
	if err != nil || len(connections) != 0 {
		t.Fatalf("manual start created connections=%+v err=%v", connections, err)
	}
	wrong := completeManualOAuthFlow(principal, "google_antigravity", stringValueForTest(started["flow_id"]), "https://callback.example.test/oauth?code=fixture-code&state=wrong")
	if wrong["status"] != "error" || config.Get().Providers["google-antigravity"] != nil {
		t.Fatalf("wrong-state completion=%+v provider=%+v", wrong, config.Get().Providers["google-antigravity"])
	}
	if _, found, err := iam.OAuthClientProfileByName("google_antigravity", antigravityManualProfile); err != nil || found {
		t.Fatalf("failed authorization persisted client profile: found=%v err=%v", found, err)
	}
	connections, err = iam.ListProviderConnections(principal.ID, "google-antigravity")
	if err != nil || len(connections) != 0 {
		t.Fatalf("wrong-state completion created connections=%+v err=%v", connections, err)
	}
	authorized := completeManualOAuthFlow(principal, "google_antigravity", stringValueForTest(started["flow_id"]), "https://callback.example.test/oauth?code=fixture-code&state=fixture-state")
	if authorized["status"] != "authorized" || config.Get().Providers["google-antigravity"] == nil {
		t.Fatalf("authorized=%+v provider=%+v", authorized, config.Get().Providers["google-antigravity"])
	}
	envelope, _, ok, err := iam.OAuthProviderConnectionSecret(principal.ID, "google-antigravity", "personal")
	if err != nil || !ok || envelope.OAuthProfile != antigravityManualProfile || envelope.OAuthClientMode != "public" || envelope.OAuthRedirectURI != "https://callback.example.test/oauth" || envelope.ProjectID != "fixture-project" {
		t.Fatalf("envelope=%+v ok=%v err=%v", envelope, ok, err)
	}
}

func TestConsumerManualCompletionRollsBackConnectionWhenProviderPersistenceFails(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LLMGW_STATE_DIR", dir)
	t.Setenv("LLMGW_CONFIG", dir+"/config.yaml")
	t.Setenv("LLMGW_CREDENTIAL_ENCRYPTION_KEY", base64.RawURLEncoding.EncodeToString(make([]byte, 32)))
	if err := os.WriteFile(config.ConfigFilePath(), []byte("providers: invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	oldSettings := *config.Get()
	t.Cleanup(func() { config.Update(func(settings *config.Settings) { *settings = oldSettings }) })
	config.Update(func(settings *config.Settings) {
		settings.Providers = map[string]*config.ProviderConfig{}
		settings.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
		settings.GoogleAntigravityOAuthProfile = antigravityManualProfile
		settings.GoogleAntigravityClientID = "fixture-client"
		settings.GoogleAntigravityClientMode = "public"
		settings.GoogleAntigravityRedirectURI = "https://callback.example.test/oauth"
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	principal, err := iam.CreatePrincipal("human", "fixture:rollback-owner", "", "Rollback Owner")
	if err != nil {
		t.Fatal(err)
	}
	oldStart, oldComplete := startManualProviderOAuth, completeManualProviderOAuth
	t.Cleanup(func() { startManualProviderOAuth, completeManualProviderOAuth = oldStart, oldComplete })
	startManualProviderOAuth = func(context.Context, providers.ManualProviderAuthAdapter, providers.ProviderAuthManualConfig) (providers.ProviderAuthBrowserStart, error) {
		return providers.ProviderAuthBrowserStart{AuthorizationURL: "https://accounts.example.test/authorize", PrivateState: `{"state":"rollback-state","code_verifier":"fixture-verifier"}`, ExpiresIn: 600}, nil
	}
	completeManualProviderOAuth = func(context.Context, providers.ManualProviderAuthAdapter, string, string, providers.ProviderAuthManualConfig) (providers.ProviderAuthPoll, error) {
		return providers.ProviderAuthPoll{Status: "authorized", AccessToken: "fixture-access"}, nil
	}
	started, err := startManualOAuthFlow(principal, "google_antigravity", "personal", iam.ConnectionSourceAdmin, providers.ProviderAuthManualConfig{}, false)
	if err != nil {
		t.Fatal(err)
	}
	response := completeManualOAuthFlow(principal, "google_antigravity", stringValueForTest(started["flow_id"]), "fixture-code")
	if response["status"] != "error" || config.Get().Providers["google-antigravity"] != nil {
		t.Fatalf("completion=%+v provider=%+v", response, config.Get().Providers["google-antigravity"])
	}
	if _, _, ok, err := iam.OAuthProviderConnectionSecret(principal.ID, "google-antigravity", "personal"); err != nil || ok {
		t.Fatalf("failed completion retained active connection ok=%v err=%v", ok, err)
	}
}
