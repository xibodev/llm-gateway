package api

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"llmgw/internal/codexauth"
	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
)

func TestAdminRouteRejectsUnknownMembersAndPreservesOrder(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	providers.ResetProviders()
	t.Cleanup(func() { iam.ResetForTests(); providers.ResetProviders() })
	oldProviders, oldEndpoints := config.Get().Providers, config.Get().Endpoints
	oldKey, oldAllow := config.Get().APIKey, config.Get().AllowUnauthenticatedAPI
	t.Cleanup(func() {
		config.Update(func(s *config.Settings) {
			s.Providers, s.Endpoints = oldProviders, oldEndpoints
			s.APIKey, s.AllowUnauthenticatedAPI = oldKey, oldAllow
		})
	})
	config.Update(func(s *config.Settings) {
		s.APIKey = "admin-secret"
		s.AllowUnauthenticatedAPI = false
		s.Providers = map[string]*config.ProviderConfig{"echo": {Type: "echo"}}
		s.Endpoints = map[string]*config.EndpointConfig{}
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(NewServer())
	defer server.Close()

	status, _ := jsonRequest(t, server.URL+"/admin/api/categories", http.MethodPost, "admin-secret", map[string]any{
		"name": "bad-provider", "failover": []map[string]any{{"provider": "missing", "model": "x"}},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("unknown provider status=%d", status)
	}
	status, _ = jsonRequest(t, server.URL+"/admin/api/categories", http.MethodPost, "admin-secret", map[string]any{
		"name": "bad-model", "failover": []map[string]any{{"provider": "echo", "model": "missing"}},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("unknown model status=%d", status)
	}
	status, saved := jsonRequest(t, server.URL+"/admin/api/categories", http.MethodPost, "admin-secret", map[string]any{
		"name": "coding", "failover": []map[string]any{{"provider": "echo", "model": "echo-strong"}, {"provider": "echo", "model": "echo-small"}},
	})
	if status != http.StatusOK || saved["members"] != float64(2) {
		t.Fatalf("save status=%d payload=%+v", status, saved)
	}
	status, duplicate := jsonRequest(t, server.URL+"/admin/api/categories", http.MethodPost, "admin-secret", map[string]any{
		"name": "CODING", "failover": []map[string]any{{"provider": "echo", "model": "echo-small"}},
	})
	if status != http.StatusBadRequest || duplicate["error"] == nil {
		t.Fatalf("case-colliding route status=%d payload=%+v", status, duplicate)
	}
	route := config.Get().Endpoints["coding"]
	if route == nil || len(route.Failover) != 2 || route.Failover[0].Model != "echo-strong" || route.Failover[1].Model != "echo-small" {
		t.Fatalf("saved route=%+v", route)
	}
}

func TestStoreCategoryAtomicallyRejectsCaseCollisions(t *testing.T) {
	oldEndpoints := config.Get().Endpoints
	t.Cleanup(func() {
		config.Update(func(s *config.Settings) {
			s.Endpoints = oldEndpoints
		})
	})
	config.Update(func(s *config.Settings) {
		s.Endpoints = map[string]*config.EndpointConfig{}
	})

	members := []config.EndpointMember{{Provider: "echo", Model: "echo-strong"}}
	start := make(chan struct{})
	results := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for _, name := range []string{"coding", "CODING"} {
		go func(name string) {
			ready.Done()
			<-start
			results <- storeEndpoint(name, members)
		}(name)
	}
	ready.Wait()
	close(start)

	successes := 0
	for range 2 {
		if err := <-results; err == nil {
			successes++
		}
	}
	if successes != 1 || len(config.Get().Endpoints) != 1 {
		t.Fatalf("successes=%d categories=%+v", successes, config.Get().Endpoints)
	}
}

func TestUserModelsUsesOnlyOwnerScopedPortalEndpoint(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	providers.ResetProviders()
	t.Cleanup(func() { iam.ResetForTests(); providers.ResetProviders() })
	key := make([]byte, 32)
	oldSSO, oldSecret, oldAuto := config.Get().SSOEnabled, config.Get().SSOSharedSecret, config.Get().SSOAutoProvision
	oldCredentialKey, oldProviders := config.Get().CredentialEncryptionKey, config.Get().Providers
	t.Cleanup(func() {
		config.Update(func(s *config.Settings) {
			s.SSOEnabled, s.SSOSharedSecret, s.SSOAutoProvision = oldSSO, oldSecret, oldAuto
			s.CredentialEncryptionKey, s.Providers = oldCredentialKey, oldProviders
		})
	})
	config.Update(func(s *config.Settings) {
		s.SSOEnabled, s.SSOSharedSecret, s.SSOAutoProvision = true, "proxy-secret", true
		s.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(key)
		s.Providers = map[string]*config.ProviderConfig{"echo": {Type: "echo"}}
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(NewServer())
	defer server.Close()
	status, payload := ssoConnectionRequest(t, server.URL, "model-owner", http.MethodGet, "/user/api/models", nil)
	if status != http.StatusOK {
		t.Fatalf("models status=%d payload=%+v", status, payload)
	}
	rows := payload["data"].([]any)
	if len(rows) == 0 || rows[0].(map[string]any)["id"] == nil {
		t.Fatalf("models rows=%+v", rows)
	}
}

func TestAdminCodexCatalogAndRouteValidationArePrincipalScoped(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	providers.ResetProviders()
	t.Cleanup(func() { iam.ResetForTests(); providers.ResetProviders() })
	key := make([]byte, 32)
	oldProviders, oldEndpoints := config.Get().Providers, config.Get().Endpoints
	oldAPIKey, oldAllow := config.Get().APIKey, config.Get().AllowUnauthenticatedAPI
	oldCredentialKey, oldClientID := config.Get().CredentialEncryptionKey, config.Get().OpenAICodexClientID
	t.Cleanup(func() {
		config.Update(func(s *config.Settings) {
			s.Providers, s.Endpoints = oldProviders, oldEndpoints
			s.APIKey, s.AllowUnauthenticatedAPI = oldAPIKey, oldAllow
			s.CredentialEncryptionKey, s.OpenAICodexClientID = oldCredentialKey, oldClientID
		})
	})
	config.Update(func(s *config.Settings) {
		s.APIKey, s.AllowUnauthenticatedAPI = "admin-secret", false
		s.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(key)
		s.OpenAICodexClientID = "fixture-client"
		s.Providers = map[string]*config.ProviderConfig{"codex": {Type: "openai_compatible", RegistryID: "openai_codex"}}
		s.Endpoints = map[string]*config.EndpointConfig{}
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	owner, err := iam.CreatePrincipal("human", "authentik:catalog-owner", "", "Catalog Owner")
	if err != nil {
		t.Fatal(err)
	}
	other, err := iam.CreatePrincipal("human", "authentik:catalog-other", "", "Catalog Other")
	if err != nil {
		t.Fatal(err)
	}
	project, err := iam.EnsureProject("catalog-project", "Catalog Project")
	if err != nil {
		t.Fatal(err)
	}
	if err := iam.SetMembership(project.ID, owner.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if _, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
		PrincipalID: owner.ID, ProviderID: "codex", Kind: "openai_codex_oauth", AccessToken: "owner-access", RefreshToken: "owner-refresh",
	}); err != nil {
		t.Fatal(err)
	}
	modelCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		modelCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5-codex","owned_by":"openai"}]}`))
	}))
	defer upstream.Close()
	oldModels := codexauth.ModelsURL
	codexauth.ModelsURL = upstream.URL + "/models"
	t.Cleanup(func() { codexauth.ModelsURL = oldModels })
	server := httptest.NewServer(NewServer())
	defer server.Close()
	contains := func(payload map[string]any, want string) bool {
		for _, value := range payload["data"].([]any) {
			if value.(map[string]any)["id"] == want {
				return true
			}
		}
		return false
	}

	status, payload := jsonRequest(t, server.URL+"/admin/api/models", http.MethodGet, "admin-secret", nil)
	if status != http.StatusOK || contains(payload, "codex/gpt-5-codex") || modelCalls != 0 {
		t.Fatalf("unscoped models status=%d payload=%+v calls=%d", status, payload, modelCalls)
	}
	status, payload = jsonRequest(t, server.URL+"/admin/api/models?principal_id="+owner.ID, http.MethodGet, "admin-secret", nil)
	if status != http.StatusOK || !contains(payload, "codex/gpt-5-codex") || modelCalls != 1 {
		t.Fatalf("owner models status=%d payload=%+v calls=%d", status, payload, modelCalls)
	}
	status, payload = jsonRequest(t, server.URL+"/admin/api/models?principal_id="+owner.ID+"&project_id="+project.ID, http.MethodGet, "admin-secret", nil)
	if status != http.StatusOK || !contains(payload, "codex/gpt-5-codex") || modelCalls != 1 {
		t.Fatalf("owner project models status=%d payload=%+v calls=%d", status, payload, modelCalls)
	}
	status, denied := jsonRequest(t, server.URL+"/admin/api/models?principal_id="+other.ID+"&project_id="+project.ID, http.MethodGet, "admin-secret", nil)
	if status != http.StatusBadRequest || denied["error"] == nil {
		t.Fatalf("non-member project models status=%d payload=%+v", status, denied)
	}
	status, payload = jsonRequest(t, server.URL+"/admin/api/models?principal_id="+other.ID, http.MethodGet, "admin-secret", nil)
	if status != http.StatusOK || contains(payload, "codex/gpt-5-codex") || modelCalls != 1 {
		t.Fatalf("other models status=%d payload=%+v calls=%d", status, payload, modelCalls)
	}
	member := []map[string]any{{"provider": "codex", "model": "gpt-5-codex"}}
	status, saved := jsonRequest(t, server.URL+"/admin/api/categories?principal_id="+owner.ID, http.MethodPost, "admin-secret", map[string]any{"name": "owner-codex", "failover": member})
	if status != http.StatusOK || saved["members"] != float64(1) {
		t.Fatalf("owner route status=%d payload=%+v", status, saved)
	}
	status, denied = jsonRequest(t, server.URL+"/admin/api/categories?principal_id="+other.ID, http.MethodPost, "admin-secret", map[string]any{"name": "other-codex", "failover": member})
	if status != http.StatusBadRequest || denied["error"] == nil {
		t.Fatalf("other route status=%d payload=%+v", status, denied)
	}
	status, denied = jsonRequest(t, server.URL+"/admin/api/categories", http.MethodPost, "admin-secret", map[string]any{"name": "unscoped-codex", "failover": member})
	if status != http.StatusBadRequest || denied["error"] == nil {
		t.Fatalf("unscoped route status=%d payload=%+v", status, denied)
	}
}
