package api

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
	"llmgw/internal/router"
)

// A provider added from the legacy admin page carries no registry id — its
// addProvider() posts {id, type, base_url} only — so every such provider is
// judged by its runtime type alone. azure_openai declares
// auth_methods: ["api_key"] in the registry manifest, and must therefore accept
// an API-key connection on that path too, not only when a registry id happens
// to be present. Types that declare no api_key method must still be refused.
func TestAPIKeyConnectionFollowsTheManifestForRegistrylessProviders(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	router.ResetSavingsState()
	router.ResetTelemetryState()
	providers.ResetProviders()
	t.Cleanup(func() {
		iam.ResetForTests()
		router.ResetSavingsState()
		router.ResetTelemetryState()
		providers.ResetProviders()
	})
	key := make([]byte, 32)
	config.Update(func(s *config.Settings) {
		s.APIKey = "admin-secret"
		s.AllowUnauthenticatedAPI = false
		s.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(key)
		s.Providers = map[string]*config.ProviderConfig{
			// No RegistryID on either: this is exactly what the legacy admin
			// page writes.
			"my-azure":  {Type: "azure_openai", BaseURL: "https://azure.invalid/openai/v1"},
			"my-ollama": {Type: "ollama", BaseURL: "http://ollama.invalid"},
		}
		s.Endpoints = map[string]*config.EndpointConfig{}
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	human, err := iam.CreatePrincipal("human", "authentik:apikey-types", "", "API Key Types")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(NewServer())
	defer server.Close()

	status, body := jsonRequest(
		t, server.URL+"/admin/api/principals/"+human.ID+"/connections",
		http.MethodPost, "admin-secret", map[string]any{
			"provider_id": "my-azure", "connection_name": "personal",
			"secret": "azure-personal-key",
		},
	)
	if status != http.StatusCreated {
		t.Fatalf("custom azure_openai refused an API key: %d %+v", status, body)
	}

	status, body = jsonRequest(
		t, server.URL+"/admin/api/principals/"+human.ID+"/connections",
		http.MethodPost, "admin-secret", map[string]any{
			"provider_id": "my-ollama", "connection_name": "personal",
			"secret": "not-a-thing",
		},
	)
	if status != http.StatusBadRequest {
		t.Fatalf("credential-free runtime accepted an API key: %d %+v", status, body)
	}
}

func TestRuntimeTypeAcceptsAPIKey(t *testing.T) {
	for _, tc := range []struct {
		runtimeType string
		want        bool
	}{
		{"azure_openai", true},
		{"vertex_ai", true},
		{"anthropic", true},
		{"bedrock", true},
		{"ai_studio", true},
		{"openai_compatible", true},
		{"OpenAI", true},
		{" litellm ", true},
		{"ollama", false},
		{"edge_tts", false},
		{"echo", false},
		{"", false},
	} {
		if got := runtimeTypeAcceptsAPIKey(tc.runtimeType); got != tc.want {
			t.Errorf("runtimeTypeAcceptsAPIKey(%q) = %v, want %v", tc.runtimeType, got, tc.want)
		}
	}
}
