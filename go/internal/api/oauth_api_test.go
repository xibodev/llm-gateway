package api

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"llmgw/internal/codexauth"
	"llmgw/internal/config"
	"llmgw/internal/copilotauth"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
)

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
			_, _ = w.Write([]byte(`{"token":"fake-copilot-session-token","expires_at":4102444800,"endpoints":{"api":"https://example.test/copilot"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer mock.Close()
	oldDevice, oldAccess, oldSession := copilotauth.DeviceCodeURL, copilotauth.AccessTokenURL, copilotauth.SessionTokenURL
	copilotauth.DeviceCodeURL = mock.URL + "/device"
	copilotauth.AccessTokenURL = mock.URL + "/token"
	copilotauth.SessionTokenURL = mock.URL + "/session"
	t.Cleanup(func() {
		copilotauth.DeviceCodeURL, copilotauth.AccessTokenURL, copilotauth.SessionTokenURL = oldDevice, oldAccess, oldSession
	})

	oldSSOEnabled := config.Get().SSOEnabled
	oldSSOSecret := config.Get().SSOSharedSecret
	oldAutoProvision := config.Get().SSOAutoProvision
	oldCredentialKey := config.Get().CredentialEncryptionKey
	oldProviders := config.Get().Providers
	oldCacheDir := config.Get().GithubCopilotCacheDir
	t.Cleanup(func() {
		config.Update(func(s *config.Settings) {
			s.SSOEnabled, s.SSOSharedSecret, s.SSOAutoProvision = oldSSOEnabled, oldSSOSecret, oldAutoProvision
			s.CredentialEncryptionKey, s.Providers, s.GithubCopilotCacheDir = oldCredentialKey, oldProviders, oldCacheDir
		})
	})
	key := make([]byte, 32)
	config.Update(func(s *config.Settings) {
		s.SSOEnabled = true
		s.SSOSharedSecret = "proxy-secret"
		s.SSOAutoProvision = true
		s.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(key)
		s.GithubCopilotCacheDir = t.TempDir()
		s.Providers = map[string]*config.ProviderConfig{"copilot": {Type: "github_copilot", RegistryID: "github_copilot"}}
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(NewServer())
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

	status, refreshed := ssoConnectionRequest(t, server.URL, "oauth-user", http.MethodPost, "/user/api/connections/copilot/oauth/refresh", map[string]any{})
	if status != http.StatusOK || refreshed["status"] != "refreshed" {
		t.Fatalf("refresh status=%d payload=%+v", status, refreshed)
	}
	encoded, _ = json.Marshal(refreshed)
	if strings.Contains(string(encoded), "fake-github-access-token") || strings.Contains(string(encoded), "fake-copilot-session-token") {
		t.Fatalf("OAuth refresh leaked a token: %s", encoded)
	}
}

func TestUserCodexOAuthUsesOfficialMockedDeviceAndPKCEExchange(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
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
			if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || form.Get("grant_type") != "authorization_code" || form.Get("code") != "codex-auth-code" || form.Get("code_verifier") != "server-verifier" || form.Get("redirect_uri") != codexauth.DeviceAuthRedirectURI || len(form) != 5 {
				t.Fatalf("exchange form=%v", form)
			}
			_, _ = w.Write([]byte(`{"access_token":"codex-access-token","refresh_token":"codex-refresh-token","id_token":"codex-id-token","expires_in":120,"account_id":"workspace-42","account_label":"Fixture workspace"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	oldUser, oldDevice, oldToken := codexauth.UserCodeURL, codexauth.DeviceTokenURL, codexauth.OAuthTokenURL
	codexauth.UserCodeURL, codexauth.DeviceTokenURL, codexauth.OAuthTokenURL = server.URL+"/usercode", server.URL+"/device-token", server.URL+"/oauth-token"
	t.Cleanup(func() {
		codexauth.UserCodeURL, codexauth.DeviceTokenURL, codexauth.OAuthTokenURL = oldUser, oldDevice, oldToken
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
	key := make([]byte, 32)
	config.Update(func(s *config.Settings) {
		s.SSOEnabled, s.SSOSharedSecret, s.SSOAutoProvision = true, "proxy-secret", true
		s.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(key)
		s.Providers = map[string]*config.ProviderConfig{}
		s.OpenAICodexClientID = "fixture-codex-client"
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	gateway := httptest.NewServer(NewServer())
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
