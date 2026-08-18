package api

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
)

func TestUserCanTestProviderWithOwnPrincipal(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	providers.ResetProviders()
	t.Cleanup(func() {
		iam.ResetForTests()
		providers.ResetProviders()
	})

	oldSSOEnabled := config.Get().SSOEnabled
	oldSSOSecret := config.Get().SSOSharedSecret
	oldAutoProvision := config.Get().SSOAutoProvision
	oldCredentialKey := config.Get().CredentialEncryptionKey
	oldProviders := config.Get().Providers
	t.Cleanup(func() {
		config.Update(func(s *config.Settings) {
			s.SSOEnabled = oldSSOEnabled
			s.SSOSharedSecret = oldSSOSecret
			s.SSOAutoProvision = oldAutoProvision
			s.CredentialEncryptionKey = oldCredentialKey
			s.Providers = oldProviders
		})
	})
	key := make([]byte, 32)
	config.Update(func(s *config.Settings) {
		s.SSOEnabled = true
		s.SSOSharedSecret = "proxy-secret"
		s.SSOAutoProvision = true
		s.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(key)
		s.Providers = map[string]*config.ProviderConfig{
			"echo": {Type: "echo"},
		}
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	owner, err := iam.EnsurePrincipalBySubject(
		"human", "authentik:provider-test-user", "", "Provider Test User",
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := iam.PutProviderConnection(iam.ProviderConnectionCreate{
		PrincipalID: owner.ID, ProviderID: "echo", Name: "personal",
		Kind: "api_key", Secret: "fixture-secret", Source: iam.ConnectionSourceUser,
	}); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(NewServer())
	defer server.Close()
	status, response := ssoConnectionRequest(
		t, server.URL, "provider-test-user", http.MethodPost,
		"/user/api/providers/echo/test", map[string]any{},
	)
	if status != http.StatusOK {
		t.Fatalf("test provider status=%d response=%+v", status, response)
	}
	if response["success"] != true || response["status"] != "passed" {
		t.Fatalf("test provider response=%+v", response)
	}
	if count, ok := response["model_count"].(float64); !ok || count < 1 {
		t.Fatalf("test provider model_count=%v", response["model_count"])
	}

	status, response = ssoConnectionRequest(
		t, server.URL, "provider-test-other", http.MethodPost,
		"/user/api/providers/echo/test", map[string]any{},
	)
	if status != http.StatusForbidden {
		t.Fatalf("unauthorized provider test status=%d response=%+v", status, response)
	}
}
