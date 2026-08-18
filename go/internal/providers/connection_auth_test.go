package providers

import (
	"encoding/base64"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
)

func TestPersonalAPIKeyConnectionOverridesSystemProviderKey(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	ResetProviders()
	t.Cleanup(func() {
		iam.ResetForTests()
		ResetProviders()
	})
	key := make([]byte, 32)
	config.Update(func(s *config.Settings) {
		s.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(key)
		s.Providers = map[string]*config.ProviderConfig{
			"gemini": {
				Type: "openai_compatible", RegistryID: "gemini",
				BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai",
				APIKey:  "system-key",
			},
		}
	})
	human, err := iam.CreatePrincipal("human", "authentik:gemini-owner", "", "Gemini owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := iam.PutProviderConnection(iam.ProviderConnectionCreate{
		PrincipalID: human.ID, ProviderID: "gemini", Name: "personal",
		Kind: "api_key", Secret: "personal-key", Source: iam.ConnectionSourceUser,
	}); err != nil {
		t.Fatal(err)
	}
	baseURL, headers, ok := ProviderHTTPTarget(
		"gemini", &config.Principal{PrincipalID: human.ID},
	)
	if !ok {
		t.Fatal("personal Gemini provider target unavailable")
	}
	if baseURL != "https://generativelanguage.googleapis.com/v1beta/openai" ||
		headers.Get("Authorization") != "Bearer personal-key" {
		t.Fatalf("target base=%q authorization=%q", baseURL, headers.Get("Authorization"))
	}
	_, headers, ok = ProviderHTTPTarget("gemini", nil)
	if !ok || headers.Get("Authorization") != "Bearer system-key" {
		t.Fatalf("system authorization=%q ok=%v", headers.Get("Authorization"), ok)
	}
}

func TestAPIKeyRuntimeRejectsOAuthConnectionEnvelope(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	ResetProviders()
	t.Cleanup(func() {
		iam.ResetForTests()
		ResetProviders()
	})
	key := make([]byte, 32)
	config.Update(func(s *config.Settings) {
		s.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(key)
		s.Providers = map[string]*config.ProviderConfig{
			"custom": {Type: "openai_compatible", BaseURL: "https://example.invalid/v1"},
		}
	})
	human, err := iam.CreatePrincipal("human", "authentik:oauth-on-api-key", "", "OAuth Owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
		PrincipalID: human.ID, ProviderID: "custom", Kind: "fixture_oauth",
		AccessToken: "must-not-be-used-as-api-key",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := GetProviderForPrincipal(
		"custom", &config.Principal{PrincipalID: human.ID},
	); err == nil {
		t.Fatal("generic API-key runtime accepted an OAuth connection envelope")
	}
}
