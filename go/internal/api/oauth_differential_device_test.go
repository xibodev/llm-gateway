package api

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"

	codexauth "github.com/xibodev/llm-provider-auth/codex"
)

// useDifferentialSettings configures what both completions of a case start
// from: an encryption key, the Codex client, and the providers configure
// changes.
func useDifferentialSettings(t *testing.T, configure func(*config.Settings)) {
	t.Helper()
	useOAuthProviders(t, func(settings *config.Settings) {
		settings.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
		settings.OpenAICodexClientID = "fixture-codex-client"
		settings.GoogleAntigravityOAuthProfile = ""
		configure(settings)
	})
}

func oauthAdapter(t *testing.T, adapterID, providerID string) providers.ProviderAuthAdapter {
	t.Helper()
	adapter, err := providers.NewProviderAuthAdapter(adapterID, providerID)
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

// The device completions: the old poll ran the adapter and stored its
// result; the new one runs the driver and the Service's key hook.
func TestOAuthDeviceCompletionWritesWhatTheGatewayWrote(t *testing.T) {
	ctx := context.Background()
	t.Run("copilot", func(t *testing.T) {
		useDifferentialSettings(t, func(*config.Settings) {})
		fixtureCopilot(t, func(int32) string { return `{"access_token":"fixture-copilot-access"}` })
		twins := newCompletionTwins(t, "copilot", "github_oauth")
		old := twins.run(t, 0, func() map[string]any {
			adapter := oauthAdapter(t, "github_copilot", "copilot")
			device := adapter.(providers.DeviceProviderAuthAdapter)
			start, err := device.StartDevice(ctx)
			if err != nil {
				t.Fatal(err)
			}
			result := providers.SafeProviderAuthPoll(device.PollDevice(ctx, start.DeviceCode, start.PrivateState))
			return legacyPollCompletion(twins.principal, "copilot", adapter.CredentialKind(), "work", iam.ConnectionSourceUser, result)
		})
		next := twins.run(t, 1, func() map[string]any {
			clock := newOAuthTestClock()
			s := newServer(Runtime{}, clock.Now)
			deviceCode := startDevice(t, s, twins.principal)
			clock.Advance(time.Second)
			return s.pollOAuthFlow(twins.principal, "copilot", deviceCode, "work", iam.ConnectionSourceUser)
		})
		sameCompletion(t, old, next, "authorized")
	})
	t.Run("codex", func(t *testing.T) {
		useDifferentialSettings(t, func(settings *config.Settings) {
			settings.Providers["codex"] = &config.ProviderConfig{Type: "openai_compatible", RegistryID: "openai_codex"}
		})
		mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/usercode":
				_, _ = w.Write([]byte(`{"device_auth_id":"fixture-device","user_code":"CODEX-1","interval":1,"expires_in":60}`))
			case "/device-token":
				_, _ = w.Write([]byte(`{"authorization_code":"fixture-code","code_verifier":"fixture-verifier"}`))
			default:
				_, _ = w.Write([]byte(`{"access_token":"fixture-codex-access","refresh_token":"fixture-codex-refresh","id_token":"fixture-id","token_type":"Bearer","expires_at":1900000000,"account_id":"fixture-account","account_label":"Fixture account"}`))
			}
		}))
		defer mock.Close()
		providers.SetCodexEndpointsForTests(t, providers.CodexEndpoints{OAuth: codexauth.Endpoints{
			UserCodeURL: mock.URL + "/usercode", DeviceTokenURL: mock.URL + "/device-token", OAuthTokenURL: mock.URL + "/token",
		}})
		twins := newCompletionTwins(t, "codex", "openai_codex_oauth")
		old := twins.run(t, 0, func() map[string]any {
			adapter := oauthAdapter(t, "openai_codex", "codex")
			device := adapter.(providers.DeviceProviderAuthAdapter)
			start, err := device.StartDevice(ctx)
			if err != nil {
				t.Fatal(err)
			}
			result := providers.SafeProviderAuthPoll(device.PollDevice(ctx, start.DeviceCode, start.PrivateState))
			return legacyPollCompletion(twins.principal, "codex", adapter.CredentialKind(), "work", iam.ConnectionSourceAdmin, result)
		})
		next := twins.run(t, 1, func() map[string]any {
			clock := newOAuthTestClock()
			s := newServer(Runtime{}, clock.Now)
			started, err := s.startOAuthFlow(twins.principal, "openai_codex", "", true,
				httptest.NewRequest(http.MethodPost, "http://127.0.0.1:9791/start", nil), "", iam.ConnectionSourceAdmin, "device_code")
			if err != nil {
				t.Fatal(err)
			}
			clock.Advance(time.Second)
			return s.pollOAuthFlow(twins.principal, "openai_codex", stringValueForTest(started["device_code"]), "work", iam.ConnectionSourceAdmin)
		})
		sameCompletion(t, old, next, "authorized")
	})
}
