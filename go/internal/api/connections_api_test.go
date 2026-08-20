package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
	"llmgw/internal/router"
)

func TestAdminProviderConnectionLifecycle(t *testing.T) {
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
			"gemini": {
				Type: "openai_compatible", RegistryID: "gemini",
				BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai",
			},
		}
		s.Endpoints = map[string]*config.EndpointConfig{}
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	human, err := iam.CreatePrincipal("human", "authentik:connection-api", "", "Connection API")
	if err != nil {
		t.Fatal(err)
	}
	cachedBeforeCreate, err := providers.GetProviderForPrincipal(
		"gemini", &config.Principal{PrincipalID: human.ID},
	)
	if err != nil {
		t.Fatalf("cache provider before connection: %v", err)
	}
	server := httptest.NewServer(NewServer())
	defer server.Close()

	status, created := jsonRequest(
		t, server.URL+"/admin/api/principals/"+human.ID+"/connections",
		http.MethodPost, "admin-secret", map[string]any{
			"provider_id": "gemini", "connection_name": "personal",
			"secret": "gemini-personal-key",
		},
	)
	if status != http.StatusCreated {
		t.Fatalf("create connection: %d %+v", status, created)
	}
	connection := created["connection"].(map[string]any)
	if connection["provider_id"] != "gemini" || connection["secret"] != nil ||
		connection["private_to_principal"] != true {
		t.Fatalf("connection response=%+v", connection)
	}
	connectionID, _ := connection["id"].(string)
	cachedAfterCreate, err := providers.GetProviderForPrincipal(
		"gemini", &config.Principal{PrincipalID: human.ID},
	)
	if err != nil {
		t.Fatalf("cache provider after connection: %v", err)
	}
	if cachedBeforeCreate == cachedAfterCreate {
		t.Fatal("credential creation reused a stale provider instance")
	}
	status, listed := jsonRequest(
		t, server.URL+"/admin/api/principals/"+human.ID+"/connections?provider_id=gemini",
		http.MethodGet, "admin-secret", nil,
	)
	if status != http.StatusOK {
		t.Fatalf("list connection: %d %+v", status, listed)
	}
	items := listed["connections"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["id"] != connectionID {
		t.Fatalf("listed connections=%+v", items)
	}
	status, revoked := jsonRequest(
		t, server.URL+"/admin/api/principals/"+human.ID+"/connections/"+connectionID,
		http.MethodDelete, "admin-secret", nil,
	)
	if status != http.StatusOK || revoked["ok"] != true {
		t.Fatalf("revoke connection: %d %+v", status, revoked)
	}
	if _, _, ok, err := iam.ProviderConnectionSecret(human.ID, "gemini", ""); err != nil || ok {
		t.Fatalf("revoked connection available: ok=%v err=%v", ok, err)
	}
	cachedAfterRevoke, err := providers.GetProviderForPrincipal(
		"gemini", &config.Principal{PrincipalID: human.ID},
	)
	if err != nil {
		t.Fatalf("cache provider after revoke: %v", err)
	}
	if cachedAfterCreate == cachedAfterRevoke {
		t.Fatal("credential revocation reused a stale provider instance")
	}
}

func TestUserConnectionsAreOwnerScoped(t *testing.T) {
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
		s.SSOEnabled = true
		s.SSOSharedSecret = "proxy-secret"
		s.SSOAutoProvision = true
		s.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(key)
		s.Providers = map[string]*config.ProviderConfig{
			"gemini": {
				Type: "openai_compatible", RegistryID: "gemini",
				BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai",
			},
		}
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	other, err := iam.CreatePrincipal(
		"human", "authentik:user-two", "", "User Two",
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := iam.PutProviderConnection(iam.ProviderConnectionCreate{
		PrincipalID: other.ID, ProviderID: "gemini", Name: "other",
		Kind: "api_key", Secret: "other-secret", Source: iam.ConnectionSourceUser,
	}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(NewServer())
	defer server.Close()

	status, initial := ssoConnectionRequest(
		t, server.URL, "user-one", http.MethodGet, "/user/api/connections", nil,
	)
	if status != http.StatusOK || len(initial["connections"].([]any)) != 0 {
		t.Fatalf("initial owner list: %d %+v", status, initial)
	}
	status, created := ssoConnectionRequest(
		t, server.URL, "user-one", http.MethodPost, "/user/api/connections",
		map[string]any{
			"provider_id": "gemini", "connection_name": "mine", "secret": "mine-secret",
		},
	)
	if status != http.StatusCreated {
		t.Fatalf("self-service create: %d %+v", status, created)
	}
	status, mine := ssoConnectionRequest(
		t, server.URL, "user-one", http.MethodGet, "/user/api/connections", nil,
	)
	items := mine["connections"].([]any)
	if status != http.StatusOK || len(items) != 1 {
		t.Fatalf("owner list: %d %+v", status, mine)
	}
	item := items[0].(map[string]any)
	if item["connection_name"] != "mine" || item["secret"] != nil {
		t.Fatalf("owner connection leaked or mismatched: %+v", item)
	}
	status, theirs := ssoConnectionRequest(
		t, server.URL, "user-two", http.MethodGet, "/user/api/connections", nil,
	)
	theirItems := theirs["connections"].([]any)
	if status != http.StatusOK || len(theirItems) != 1 ||
		theirItems[0].(map[string]any)["connection_name"] != "other" {
		t.Fatalf("other owner list: %d %+v", status, theirs)
	}
}

func ssoConnectionRequest(
	t *testing.T, baseURL, subject, method, path string, body any,
) (int, map[string]any) {
	t.Helper()
	var payload []byte
	if body != nil {
		payload, _ = json.Marshal(body)
	}
	request, err := http.NewRequest(method, baseURL+path, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(ssoSecretHeader, "proxy-secret")
	request.Header.Set(ssoSubjectHeader, subject)
	request.Header.Set(ssoUsernameHeader, subject)
	request.Header.Set("Origin", baseURL)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(response.Body).Decode(&out)
	return response.StatusCode, out
}
