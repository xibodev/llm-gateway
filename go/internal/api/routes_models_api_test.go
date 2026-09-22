package api

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
	"llmgw/internal/router"

	codexauth "github.com/xibodev/llm-provider-auth/codex"
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
	status, _ = jsonRequest(t, server.URL+"/admin/api/endpoints", http.MethodPost, "admin-secret", map[string]any{
		"name": "ECHO", "failover": []map[string]any{{"provider": "echo", "model": "echo-small"}},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("provider case collision status=%d", status)
	}
	route := config.Get().Endpoints["coding"]
	if route == nil || len(route.Failover) != 2 || route.Failover[0].Model != "echo-strong" || route.Failover[1].Model != "echo-small" {
		t.Fatalf("saved route=%+v", route)
	}
}

func TestEndpointDeleteIsCaseInsensitive(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	old := *config.Get()
	t.Cleanup(func() { config.Update(func(settings *config.Settings) { *settings = old }) })
	config.Update(func(settings *config.Settings) {
		settings.APIKey = "admin-secret"
		settings.Endpoints = map[string]*config.EndpointConfig{"coding": {Failover: []config.EndpointMember{{Provider: "echo", Model: "echo-default"}}}}
	})
	if err := config.Save(); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(NewServer())
	defer server.Close()
	status, _ := jsonRequest(t, server.URL+"/admin/api/endpoints/CODING", http.MethodDelete, "admin-secret", nil)
	if status != http.StatusOK || config.Get().Endpoints["coding"] != nil {
		t.Fatalf("status=%d endpoints=%+v", status, config.Get().Endpoints)
	}
}

func TestEndpointDeleteRejectsAmbiguousCaseFold(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	old := *config.Get()
	t.Cleanup(func() { config.Update(func(settings *config.Settings) { *settings = old }) })
	config.Update(func(settings *config.Settings) {
		settings.APIKey = "admin-secret"
		settings.Endpoints = map[string]*config.EndpointConfig{
			"coding": {Failover: []config.EndpointMember{{Provider: "echo", Model: "echo-default"}}},
			"CODING": {Failover: []config.EndpointMember{{Provider: "echo", Model: "echo-strong"}}},
		}
	})
	server := httptest.NewServer(NewServer())
	defer server.Close()
	status, _ := jsonRequest(t, server.URL+"/admin/api/endpoints/CoDiNg", http.MethodDelete, "admin-secret", nil)
	if status != http.StatusConflict || len(config.Get().Endpoints) != 2 {
		t.Fatalf("status=%d endpoints=%+v", status, config.Get().Endpoints)
	}
	status, _ = jsonRequest(t, server.URL+"/admin/api/endpoints/coding", http.MethodDelete, "admin-secret", nil)
	if status != http.StatusOK || config.Get().Endpoints["coding"] != nil || config.Get().Endpoints["CODING"] == nil {
		t.Fatalf("exact delete status=%d endpoints=%+v", status, config.Get().Endpoints)
	}
}

func TestRouteSaveFailureRestoresMemoryAndPublication(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	t.Setenv("LLMGW_CONFIG", filepath.Join(config.StateDir(), "missing", "config.yaml"))
	iam.ResetForTests()
	providers.ResetProviders()
	t.Cleanup(func() { iam.ResetForTests(); providers.ResetProviders() })
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	profile := providers.AnonymousProviderProfiles()[0]
	old := *config.Get()
	t.Cleanup(func() { config.Update(func(s *config.Settings) { *s = old }) })
	config.Update(func(s *config.Settings) {
		s.APIKey = "admin-secret"
		s.Providers = map[string]*config.ProviderConfig{profile.ProviderID: {
			Type: profile.RuntimeType, RegistryID: profile.RegistryID, BaseURL: profile.BaseURL,
		}}
		s.Endpoints = map[string]*config.EndpointConfig{}
	})
	if err := iam.MarkAnonymousProviderManaged(profile.ProviderID); err != nil {
		t.Fatal(err)
	}
	generation, err := iam.ProviderCheckGeneration(profile.ProviderID, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := iam.EnsureProviderModelUnverified(profile.ProviderID, "", "candidate", iam.ModelEvidenceCompletion, generation); err != nil {
		t.Fatal(err)
	}
	originalLookup := catalogLookupForPrincipal
	catalogLookupForPrincipal = func(providerID, modelID string, _ *config.Principal) (providers.ModelInfo, bool) {
		return providers.ModelInfo{ID: modelID}, providerID == profile.ProviderID && modelID == "candidate"
	}
	t.Cleanup(func() { catalogLookupForPrincipal = originalLookup })

	request := httptest.NewRequest(http.MethodPost, "/admin/api/endpoints", jsonBody(map[string]any{
		"name": "candidate-route", "failover": []map[string]any{{
			"provider": profile.ProviderID, "model": "candidate", "allow_unverified": true,
		}},
	}))
	request.Header.Set("Authorization", "Bearer admin-secret")
	response := httptest.NewRecorder()
	NewServer().ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if config.Get().Endpoints["candidate-route"] != nil {
		t.Fatal("failed save installed route in memory")
	}
	_, published, err := providers.AnonymousModelPublication(profile.ProviderID, "candidate")
	if err != nil || published {
		t.Fatalf("failed save publication=%v err=%v", published, err)
	}
}

func TestAdminRouteRequiresExplicitUnverifiedAnonymousOptIn(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	providers.ResetProviders()
	t.Cleanup(func() { iam.ResetForTests(); providers.ResetProviders() })
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	profile := providers.AnonymousProviderProfiles()[0]
	oldProviders, oldEndpoints := config.Get().Providers, config.Get().Endpoints
	oldKey := config.Get().APIKey
	t.Cleanup(func() {
		config.Update(func(s *config.Settings) {
			s.Providers, s.Endpoints, s.APIKey = oldProviders, oldEndpoints, oldKey
		})
	})
	config.Update(func(s *config.Settings) {
		s.APIKey = "admin-secret"
		s.Providers = map[string]*config.ProviderConfig{profile.ProviderID: {
			Type: profile.RuntimeType, RegistryID: profile.RegistryID, BaseURL: profile.BaseURL,
		}}
		s.Endpoints = map[string]*config.EndpointConfig{}
	})
	if err := iam.MarkAnonymousProviderManaged(profile.ProviderID); err != nil {
		t.Fatal(err)
	}
	generation, err := iam.ProviderCheckGeneration(profile.ProviderID, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := iam.EnsureProviderModelUnverified(profile.ProviderID, "", "candidate", iam.ModelEvidenceCompletion, generation); err != nil {
		t.Fatal(err)
	}
	originalCatalog := catalogModelsForPrincipal
	originalLookup := catalogLookupForPrincipal
	catalogModelsForPrincipal = func(providerID string, _ *config.Principal) []providers.ModelInfo {
		if providerID == profile.ProviderID {
			return []providers.ModelInfo{{ID: "candidate"}}
		}
		return nil
	}
	catalogLookupForPrincipal = func(providerID, modelID string, _ *config.Principal) (providers.ModelInfo, bool) {
		return providers.ModelInfo{ID: modelID}, providerID == profile.ProviderID && modelID == "candidate"
	}
	t.Cleanup(func() {
		catalogModelsForPrincipal = originalCatalog
		catalogLookupForPrincipal = originalLookup
	})
	server := httptest.NewServer(NewServer())
	defer server.Close()
	body := map[string]any{"name": "candidate-route", "failover": []map[string]any{{
		"provider": profile.ProviderID, "model": "candidate",
	}}}
	status, _ := jsonRequest(t, server.URL+"/admin/api/endpoints", http.MethodPost, "admin-secret", body)
	if status != http.StatusBadRequest {
		t.Fatalf("unverified route status=%d", status)
	}
	body["failover"] = []map[string]any{{
		"provider": profile.ProviderID, "model": "candidate", "allow_unverified": true,
	}}
	status, saved := jsonRequest(t, server.URL+"/admin/api/endpoints", http.MethodPost, "admin-secret", body)
	if status != http.StatusOK || saved["ok"] != true {
		t.Fatalf("opt-in route status=%d payload=%+v", status, saved)
	}
	_, published, err := providers.AnonymousModelPublication(profile.ProviderID, "candidate")
	if err != nil || published {
		t.Fatalf("route opt-in leaked to direct publication=%v err=%v", published, err)
	}
	for _, name := range []string{"candidate-route-2"} {
		body["name"] = name
		status, _ = jsonRequest(t, server.URL+"/admin/api/endpoints", http.MethodPost, "admin-secret", body)
		if status != http.StatusOK {
			t.Fatalf("second opt-in route status=%d", status)
		}
	}
	status, _ = jsonRequest(t, server.URL+"/admin/api/endpoints/candidate-route", http.MethodDelete, "admin-secret", nil)
	if status != http.StatusOK {
		t.Fatalf("delete first route status=%d", status)
	}
	resolution, err := router.ResolveForPrincipal("candidate-route-2", nil)
	if err != nil || len(resolution.Targets) != 1 || resolution.Targets[0].Model != "candidate" {
		t.Fatalf("remaining route lost opt-in: resolution=%+v err=%v", resolution, err)
	}
	status, _ = jsonRequest(t, server.URL+"/admin/api/endpoints/candidate-route-2", http.MethodDelete, "admin-secret", nil)
	if status != http.StatusOK {
		t.Fatalf("delete final route status=%d", status)
	}
	if _, err := router.ResolveForPrincipal("candidate-route-2", nil); err == nil {
		t.Fatal("deleted opt-in route remained resolvable")
	}
}

func TestRouteEditRemovesUnverifiedPublicationOptIn(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	providers.ResetProviders()
	t.Cleanup(func() { iam.ResetForTests(); providers.ResetProviders() })
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	profile := providers.AnonymousProviderProfiles()[0]
	old := *config.Get()
	t.Cleanup(func() { config.Update(func(settings *config.Settings) { *settings = old }) })
	config.Update(func(settings *config.Settings) {
		settings.APIKey = "admin-secret"
		settings.Providers = map[string]*config.ProviderConfig{profile.ProviderID: {
			Type: profile.RuntimeType, RegistryID: profile.RegistryID, BaseURL: profile.BaseURL,
		}}
		settings.Endpoints = map[string]*config.EndpointConfig{}
	})
	if err := iam.MarkAnonymousProviderManaged(profile.ProviderID); err != nil {
		t.Fatal(err)
	}
	generation, _ := iam.ProviderCheckGeneration(profile.ProviderID, "")
	if err := iam.EnsureProviderModelUnverified(profile.ProviderID, "", "candidate", iam.ModelEvidenceCompletion, generation); err != nil {
		t.Fatal(err)
	}
	if err := iam.RecordProviderModelEvidence(iam.ProviderModelEvidence{
		ProviderID: profile.ProviderID, Model: "verified", Operation: iam.ModelEvidenceCompletion,
		State: "verified", Generation: generation,
	}); err != nil {
		t.Fatal(err)
	}
	originalLookup := catalogLookupForPrincipal
	catalogLookupForPrincipal = func(providerID, modelID string, _ *config.Principal) (providers.ModelInfo, bool) {
		return providers.ModelInfo{ID: modelID}, providerID == profile.ProviderID && (modelID == "candidate" || modelID == "verified")
	}
	t.Cleanup(func() { catalogLookupForPrincipal = originalLookup })
	server := httptest.NewServer(NewServer())
	defer server.Close()
	body := map[string]any{"name": "candidate-route", "failover": []map[string]any{{
		"provider": profile.ProviderID, "model": "candidate", "allow_unverified": true,
	}}}
	status, _ := jsonRequest(t, server.URL+"/admin/api/endpoints", http.MethodPost, "admin-secret", body)
	if status != http.StatusOK {
		t.Fatalf("create status=%d", status)
	}
	body["failover"] = []map[string]any{{"provider": profile.ProviderID, "model": "verified"}}
	status, _ = jsonRequest(t, server.URL+"/admin/api/endpoints", http.MethodPost, "admin-secret", body)
	if status != http.StatusOK {
		t.Fatalf("edit status=%d", status)
	}
	resolution, err := router.ResolveForPrincipal("candidate-route", nil)
	if err != nil || len(resolution.Targets) != 1 || resolution.Targets[0].Model != "verified" {
		t.Fatalf("edited route failed: resolution=%+v err=%v", resolution, err)
	}
	if _, published, err := providers.AnonymousModelPublication(profile.ProviderID, "candidate"); err != nil || published {
		t.Fatalf("route edit leaked direct publication=%v err=%v", published, err)
	}
}

func TestVerifiedRouteMemberDoesNotPersistFutureUnverifiedOptIn(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	providers.ResetProviders()
	t.Cleanup(func() { iam.ResetForTests(); providers.ResetProviders() })
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	profile := providers.AnonymousProviderProfiles()[0]
	old := *config.Get()
	t.Cleanup(func() { config.Update(func(settings *config.Settings) { *settings = old }) })
	config.Update(func(settings *config.Settings) {
		settings.APIKey = "admin-secret"
		settings.Providers = map[string]*config.ProviderConfig{profile.ProviderID: {
			Type: profile.RuntimeType, RegistryID: profile.RegistryID, BaseURL: profile.BaseURL,
		}}
		settings.Endpoints = map[string]*config.EndpointConfig{}
	})
	if err := iam.MarkAnonymousProviderManaged(profile.ProviderID); err != nil {
		t.Fatal(err)
	}
	generation, _ := iam.ProviderCheckGeneration(profile.ProviderID, "")
	if err := iam.RecordProviderModelEvidence(iam.ProviderModelEvidence{
		ProviderID: profile.ProviderID, Model: "verified", Operation: iam.ModelEvidenceCompletion,
		State: "verified", Generation: generation,
	}); err != nil {
		t.Fatal(err)
	}
	originalLookup := catalogLookupForPrincipal
	catalogLookupForPrincipal = func(providerID, modelID string, _ *config.Principal) (providers.ModelInfo, bool) {
		return providers.ModelInfo{ID: modelID}, providerID == profile.ProviderID && modelID == "verified"
	}
	t.Cleanup(func() { catalogLookupForPrincipal = originalLookup })
	server := httptest.NewServer(NewServer())
	defer server.Close()
	status, body := jsonRequest(t, server.URL+"/admin/api/endpoints", http.MethodPost, "admin-secret", map[string]any{
		"name": "verified-route", "failover": []map[string]any{{
			"provider": profile.ProviderID, "model": "verified", "allow_unverified": true,
		}},
	})
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%+v", status, body)
	}
	member := config.Get().Endpoints["verified-route"].Failover[0]
	if member.AllowUnverified {
		t.Fatalf("verified member retained future waiver: %+v", member)
	}
}

func TestRejectedRouteDoesNotPublishEarlierUnverifiedMember(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	providers.ResetProviders()
	t.Cleanup(func() { iam.ResetForTests(); providers.ResetProviders() })
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	profile := providers.AnonymousProviderProfiles()[0]
	oldProviders, oldEndpoints, oldKey := config.Get().Providers, config.Get().Endpoints, config.Get().APIKey
	t.Cleanup(func() {
		config.Update(func(s *config.Settings) {
			s.Providers, s.Endpoints, s.APIKey = oldProviders, oldEndpoints, oldKey
		})
	})
	config.Update(func(s *config.Settings) {
		s.APIKey = "admin-secret"
		s.Providers = map[string]*config.ProviderConfig{profile.ProviderID: {
			Type: profile.RuntimeType, RegistryID: profile.RegistryID, BaseURL: profile.BaseURL,
		}}
		s.Endpoints = map[string]*config.EndpointConfig{}
	})
	if err := iam.MarkAnonymousProviderManaged(profile.ProviderID); err != nil {
		t.Fatal(err)
	}
	generation, err := iam.ProviderCheckGeneration(profile.ProviderID, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := iam.EnsureProviderModelUnverified(profile.ProviderID, "", "candidate", iam.ModelEvidenceCompletion, generation); err != nil {
		t.Fatal(err)
	}
	originalLookup := catalogLookupForPrincipal
	catalogLookupForPrincipal = func(providerID, modelID string, _ *config.Principal) (providers.ModelInfo, bool) {
		return providers.ModelInfo{ID: modelID}, providerID == profile.ProviderID && modelID == "candidate"
	}
	t.Cleanup(func() { catalogLookupForPrincipal = originalLookup })

	server := httptest.NewServer(NewServer())
	defer server.Close()
	body := map[string]any{"name": "rejected-route", "failover": []map[string]any{
		{"provider": profile.ProviderID, "model": "candidate", "allow_unverified": true},
		{"provider": profile.ProviderID, "model": "missing"},
	}}
	status, _ := jsonRequest(t, server.URL+"/admin/api/endpoints", http.MethodPost, "admin-secret", body)
	if status != http.StatusBadRequest {
		t.Fatalf("rejected route status=%d", status)
	}
	_, published, err := providers.AnonymousModelPublication(profile.ProviderID, "candidate")
	if err != nil || published {
		t.Fatalf("rejected route published candidate=%v err=%v", published, err)
	}
}

func TestStoreCategoryAtomicallyRejectsCaseCollisions(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
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
			_, err := storeEndpoint(name, members)
			results <- err
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
	owner, err := iam.EnsurePrincipalBySubject("human", "authentik:model-owner", "", "Model Owner")
	if err != nil {
		t.Fatal(err)
	}
	if rows := providers.RefreshCatalogForPrincipal("echo", &config.Principal{
		PrincipalID: owner.ID, PrincipalKind: owner.Kind,
	}); len(rows) == 0 {
		t.Fatal("owner refresh returned no models")
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
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5-codex","owned_by":"openai","supported_in_api":true,"visibility":"list"}]}`))
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
	status, refreshed := jsonRequest(t, server.URL+"/admin/api/providers/codex/refresh?principal_id="+owner.ID, http.MethodPost, "admin-secret", nil)
	if status != http.StatusOK || refreshed["success"] != true || modelCalls != 1 {
		t.Fatalf("owner refresh status=%d payload=%+v calls=%d", status, refreshed, modelCalls)
	}
	status, payload = jsonRequest(t, server.URL+"/admin/api/models?principal_id="+owner.ID, http.MethodGet, "admin-secret", nil)
	if status != http.StatusOK || !contains(payload, "codex/gpt-5-codex") || modelCalls != 1 {
		t.Fatalf("owner models status=%d payload=%+v calls=%d", status, payload, modelCalls)
	}
	if _, err := router.ResolveForPrincipal("codex/gpt-5-codex", &config.Principal{
		PrincipalID: owner.ID, PrincipalKind: owner.Kind,
	}); err != nil {
		t.Fatalf("owner direct resolution: %v", err)
	}
	if _, err := router.ResolveForPrincipal("codex/gpt-5-codex", &config.Principal{
		PrincipalID: other.ID, PrincipalKind: other.Kind,
	}); err == nil {
		t.Fatal("other principal resolved owner-scoped direct model")
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
	routeOnlyPayload, err := buildModelListWithDiagnostics(&config.Principal{
		PrincipalID: owner.ID, PrincipalKind: owner.Kind, RoutesOnly: true,
		AllowedRoutes: []string{"owner-codex"},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	routeOnlyFound := false
	for _, raw := range routeOnlyPayload["data"].([]any) {
		if raw.(map[string]any)["id"] == "owner-codex" {
			routeOnlyFound = true
		}
	}
	if !routeOnlyFound {
		t.Fatalf("route-only principal lost private route: %+v", routeOnlyPayload)
	}
	status, payload = jsonRequest(t, server.URL+"/admin/api/models?principal_id="+other.ID, http.MethodGet, "admin-secret", nil)
	if status != http.StatusOK || contains(payload, "owner-codex") {
		t.Fatalf("other principal saw owner-scoped route status=%d payload=%+v", status, payload)
	}
	status, denied = jsonRequest(t, server.URL+"/admin/api/categories?principal_id="+other.ID, http.MethodPost, "admin-secret", map[string]any{"name": "other-codex", "failover": member})
	if status != http.StatusBadRequest || denied["error"] == nil {
		t.Fatalf("other route status=%d payload=%+v", status, denied)
	}
	status, denied = jsonRequest(t, server.URL+"/admin/api/categories", http.MethodPost, "admin-secret", map[string]any{"name": "unscoped-codex", "failover": member})
	if status != http.StatusBadRequest || denied["error"] == nil {
		t.Fatalf("unscoped route status=%d payload=%+v", status, denied)
	}
	connections, err := iam.ListProviderConnections(owner.ID, "codex")
	if err != nil || len(connections) != 1 {
		t.Fatalf("owner connections=%+v err=%v", connections, err)
	}
	if err := iam.RevokeProviderConnection(owner.ID, connections[0].ID); err != nil {
		t.Fatal(err)
	}
	ownerPrincipal := &config.Principal{PrincipalID: owner.ID, PrincipalKind: owner.Kind}
	if _, err := router.ResolveForPrincipal("codex/gpt-5-codex", ownerPrincipal); err == nil {
		t.Fatal("revoked principal resolved cached direct model")
	}
	if _, err := router.ResolveForPrincipal("owner-codex", ownerPrincipal); err == nil {
		t.Fatal("revoked principal resolved cached private route")
	}
}
