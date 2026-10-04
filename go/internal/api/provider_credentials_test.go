package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
	"llmgw/internal/router"
)

func TestServiceProviderCredentialControlsModelsAndRoutes(t *testing.T) {
	stateDir := t.TempDir()
	var chatRequests atomic.Int32
	daemon := http.NewServeMux()
	daemon.HandleFunc("GET /extension/v1/github_copilot/models", func(w http.ResponseWriter, r *http.Request) {
		if token := r.Header.Get("X-Credential-Token"); token != "shared-secret" && token != "human-secret" {
			t.Errorf("catalog credential %q is neither the bound nor the human's", token)
		}
		writeJSON(w, http.StatusOK, map[string]any{"models": []any{
			map[string]any{"id": "model-a"}, map[string]any{"id": "model-b"},
		}})
	})
	daemon.HandleFunc("POST /extension/v1/github_copilot/invoke", func(w http.ResponseWriter, r *http.Request) {
		if token := r.Header.Get("X-Credential-Token"); token != "shared-secret" {
			t.Errorf("service request credential %q is not the bound one", token)
		}
		chatRequests.Add(1)
		writeJSON(w, http.StatusOK, map[string]any{
			"id": "local-test", "choices": []any{map[string]any{
				"message": map[string]any{"role": "assistant", "content": "local-test"},
			}},
		})
	})
	serveExtensionDaemon(t, daemon)
	setupProviderCredentialAPI(t, stateDir)
	// An extension request loads its credential through the store the
	// Runtime opened when it was built, so build one over this test's state.
	providers.InstallForTests(t)
	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{
			"copilot": {Type: "github_copilot"},
		}
		s.Endpoints = map[string]*config.EndpointConfig{}
		s.AnthropicDiscoveryAliases = false
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)

	credential, _ := iam.PutGatewayProviderCredential("copilot", "github_oauth", "shared-secret")
	authorizedToken, authorizedPrincipal := issueProviderTestKey(
		t, "authorized", "service:authorized", "service", credential.ID, true,
	)
	unauthorizedToken, _ := issueProviderTestKey(
		t, "unauthorized", "service:unauthorized", "service", credential.ID, false,
	)
	humanToken, humanPrincipal := issueHumanProviderTestKey(t, "human-secret")
	server := httptest.NewServer(NewServer(Runtime{}))
	defer server.Close()

	assertModelIDs(t, server.URL, unauthorizedToken, nil)
	status, _ := jsonRequest(t, server.URL+"/v1/chat/completions", http.MethodPost, unauthorizedToken, map[string]any{
		"model": "copilot/model-a", "messages": []map[string]any{{"role": "user", "content": "test"}},
	})
	if status != http.StatusForbidden {
		t.Fatalf("unbound service route status=%d, want 403", status)
	}
	if chatRequests.Load() != 0 {
		t.Fatal("unbound service reached provider route")
	}
	if rows := providers.RefreshCatalogForPrincipal("copilot", callerOf(authorizedPrincipal)); len(rows) != 2 {
		t.Fatalf("service project refresh returned %+v", rows)
	}
	if rows := providers.RefreshCatalogForPrincipal("copilot", callerOf(humanPrincipal)); len(rows) != 2 {
		t.Fatalf("human refresh returned %+v", rows)
	}

	assertModelIDs(t, server.URL, authorizedToken, []string{"copilot/model-a"})
	assertModelIDs(t, server.URL, humanToken, []string{"copilot/model-a"})
	status, _ = jsonRequest(t, server.URL+"/v1/chat/completions", http.MethodPost, authorizedToken, map[string]any{
		"model": "copilot/model-b", "messages": []map[string]any{{"role": "user", "content": "test"}},
	})
	if status != http.StatusForbidden || chatRequests.Load() != 0 {
		t.Fatalf("key-restricted route status=%d requests=%d", status, chatRequests.Load())
	}
	status, response := jsonRequest(t, server.URL+"/v1/chat/completions", http.MethodPost, authorizedToken, map[string]any{
		"model": "copilot/model-a", "messages": []map[string]any{{"role": "user", "content": "test"}},
	})
	if status != http.StatusOK || response["model"] != "model-a" || chatRequests.Load() != 1 {
		t.Fatalf("authorized service route status=%d response=%+v requests=%d", status, response, chatRequests.Load())
	}
	if err := iam.SetProviderCredentialStatus(credential.ID, "revoked"); err != nil {
		t.Fatal(err)
	}
	assertModelIDs(t, server.URL, authorizedToken, nil)
	status, _ = jsonRequest(t, server.URL+"/v1/chat/completions", http.MethodPost, authorizedToken, map[string]any{
		"model": "copilot/model-a", "messages": []map[string]any{{"role": "user", "content": "test"}},
	})
	if status != http.StatusForbidden {
		t.Fatalf("revoked credential route status=%d, want 403", status)
	}
	if chatRequests.Load() != 1 {
		t.Fatal("revoked credential reached provider route")
	}
}

func TestAdminSharedCredentialBindingIsSecretFreeAndAudited(t *testing.T) {
	stateDir := t.TempDir()
	setupProviderCredentialAPI(t, stateDir)
	secret := "configured-global-provider-secret"
	config.Update(func(s *config.Settings) {
		s.GithubCopilotOAuthToken = secret
		s.Providers = map[string]*config.ProviderConfig{
			"copilot": {Type: "github_copilot"},
		}
	})
	server := httptest.NewServer(NewServer(Runtime{}))
	defer server.Close()

	status, imported := jsonRequest(
		t, server.URL+"/admin/api/providers/copilot/shared-credential/import",
		http.MethodPost, "admin-secret", map[string]any{"source": "configured"},
	)
	if status != http.StatusOK {
		t.Fatalf("import status=%d payload=%+v", status, imported)
	}
	credential := imported["credential"].(map[string]any)
	if credential["id"] == "" || imported["token"] != nil || imported["secret"] != nil {
		t.Fatalf("unsafe import payload=%+v", imported)
	}
	status, project := jsonRequest(t, server.URL+"/admin/api/projects", http.MethodPost, "admin-secret", map[string]any{
		"slug": "binding", "name": "Binding",
	})
	if status != http.StatusCreated {
		t.Fatalf("project status=%d payload=%+v", status, project)
	}
	status, binding := jsonRequest(
		t, server.URL+"/admin/api/projects/"+project["id"].(string)+"/provider-credential-bindings",
		http.MethodPost, "admin-secret", map[string]any{
			"provider_id": "copilot", "principal_kind": "service",
			"credential_id": credential["id"],
		},
	)
	if status != http.StatusOK || binding["status"] != "active" {
		t.Fatalf("binding status=%d payload=%+v", status, binding)
	}
	status, state := jsonRequest(t, server.URL+"/admin/api/state", http.MethodGet, "admin-secret", nil)
	if status != http.StatusOK {
		t.Fatalf("state status=%d", status)
	}
	encoded, _ := json.Marshal(state)
	if string(encoded) == "" || containsSecret(encoded, secret) {
		t.Fatal("admin state serialized provider secret")
	}
	rawDB, err := os.ReadFile(filepath.Join(stateDir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	if containsSecret(rawDB, secret) {
		t.Fatal("gateway database contains plaintext provider secret")
	}
	status, audit := jsonRequest(t, server.URL+"/admin/api/audit", http.MethodGet, "admin-secret", nil)
	if status != http.StatusOK || containsSecret(mustJSON(t, audit), secret) {
		t.Fatal("audit unavailable or contains provider secret")
	}
}

func TestServiceBindingPreservesUnrelatedHumanCatalog(t *testing.T) {
	stateDir := t.TempDir()
	setupProviderCredentialAPI(t, stateDir)
	var upstreamAvailable atomic.Bool
	upstreamAvailable.Store(true)
	daemon := http.NewServeMux()
	daemon.HandleFunc("GET /extension/v1/github_copilot/models", func(w http.ResponseWriter, r *http.Request) {
		if !upstreamAvailable.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"models": []any{map[string]any{"id": "existing-human-model"}},
		})
	})
	serveExtensionDaemon(t, daemon)
	config.Update(func(s *config.Settings) {
		s.GithubCopilotOAuthToken = "shared-secret"
		s.Providers = map[string]*config.ProviderConfig{
			"copilot": {Type: "github_copilot"},
		}
		s.Endpoints = map[string]*config.EndpointConfig{}
		s.AnthropicDiscoveryAliases = false
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)
	humanToken, humanPrincipal := issueHumanProviderTestKeyForModels(
		t, "human-secret", []string{"copilot/existing-human-model"},
	)
	server := httptest.NewServer(NewServer(Runtime{}))
	defer server.Close()
	if rows := providers.RefreshCatalogForPrincipal("copilot", callerOf(humanPrincipal)); len(rows) != 1 {
		t.Fatalf("human refresh returned %+v", rows)
	}
	assertModelIDs(t, server.URL, humanToken, []string{"copilot/existing-human-model"})
	upstreamAvailable.Store(false)

	status, imported := jsonRequest(
		t, server.URL+"/admin/api/providers/copilot/shared-credential/import",
		http.MethodPost, "admin-secret", map[string]any{"source": "configured"},
	)
	if status != http.StatusOK {
		t.Fatalf("import status=%d payload=%+v", status, imported)
	}
	status, project := jsonRequest(t, server.URL+"/admin/api/projects", http.MethodPost, "admin-secret", map[string]any{
		"slug": "service-binding", "name": "Service Binding",
	})
	if status != http.StatusCreated {
		t.Fatalf("project status=%d payload=%+v", status, project)
	}
	credential := imported["credential"].(map[string]any)
	status, binding := jsonRequest(
		t, server.URL+"/admin/api/projects/"+project["id"].(string)+"/provider-credential-bindings",
		http.MethodPost, "admin-secret", map[string]any{
			"provider_id": "copilot", "principal_kind": "service",
			"credential_id": credential["id"],
		},
	)
	if status != http.StatusOK {
		t.Fatalf("binding status=%d payload=%+v", status, binding)
	}
	assertModelIDs(t, server.URL, humanToken, []string{"copilot/existing-human-model"})
}

func setupProviderCredentialAPI(t *testing.T, stateDir string) {
	t.Helper()
	t.Setenv("LLMGW_STATE_DIR", stateDir)
	iam.ResetForTests()
	router.ResetSavingsState()
	router.ResetTelemetryState()
	t.Cleanup(func() {
		iam.ResetForTests()
		router.ResetSavingsState()
		router.ResetTelemetryState()
	})
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	config.Update(func(s *config.Settings) {
		s.APIKey = "admin-secret"
		s.APIKeys = nil
		s.AllowUnauthenticatedAPI = false
		s.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(key)
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
}

func issueProviderTestKey(
	t *testing.T, slug, subject, kind, credentialID string, bind bool,
) (string, *config.Principal) {
	t.Helper()
	principal, _ := iam.CreatePrincipal(kind, subject, "", slug)
	project, _ := iam.CreateProject(slug, slug)
	_ = iam.SetMembership(project.ID, principal.ID, "member")
	_, _ = iam.SetProjectPolicy(project.ID, iam.KeyPolicy{
		AllowedModels: []string{"copilot/model-a", "copilot/model-b"}, AllowedProviders: []string{"copilot"},
	})
	if bind {
		_, _ = iam.SetProviderCredentialBinding(project.ID, "copilot", kind, credentialID)
	}
	issued, err := iam.IssueKey(iam.KeyCreate{
		ProjectID: project.ID, PrincipalID: principal.ID, Name: slug,
		Policy: iam.KeyPolicy{AllowedModels: []string{"copilot/model-a"}, AllowedProviders: []string{"copilot"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return issued.Token, &config.Principal{
		PrincipalID: principal.ID, PrincipalKind: principal.Kind,
		ProjectID: project.ID, Project: project.Slug,
	}
}

func issueHumanProviderTestKey(t *testing.T, secret string) (string, *config.Principal) {
	return issueHumanProviderTestKeyForModels(t, secret, []string{"copilot/model-a"})
}

func issueHumanProviderTestKeyForModels(
	t *testing.T, secret string, models []string,
) (string, *config.Principal) {
	t.Helper()
	human, _ := iam.CreatePrincipal("human", "authentik:human-provider", "", "Human")
	project, _ := iam.CreateProject("human-provider", "Human")
	_ = iam.SetMembership(project.ID, human.ID, "member")
	_, _ = iam.SetProjectPolicy(project.ID, iam.KeyPolicy{
		AllowedModels: models, AllowedProviders: []string{"copilot"},
	})
	if _, err := iam.PutProviderCredential(human.ID, "copilot", "github_oauth", secret); err != nil {
		t.Fatal(err)
	}
	issued, err := iam.IssueKey(iam.KeyCreate{
		ProjectID: project.ID, PrincipalID: human.ID, Name: "human",
		Policy: iam.KeyPolicy{AllowedModels: models, AllowedProviders: []string{"copilot"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return issued.Token, &config.Principal{
		PrincipalID: human.ID, PrincipalKind: human.Kind,
		ProjectID: project.ID, Project: project.Slug,
	}
}

// serveExtensionDaemon serves mux as the extension daemon, which serves
// Copilot and Codex for the gateway.
func serveExtensionDaemon(t *testing.T, mux *http.ServeMux) {
	t.Helper()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	t.Setenv("LLMGW_EXTENSION_URL", server.URL)
}

func assertModelIDs(t *testing.T, baseURL, token string, want []string) {
	t.Helper()
	status, payload := jsonRequest(t, baseURL+"/v1/models", http.MethodGet, token, nil)
	if status != http.StatusOK {
		t.Fatalf("models status=%d payload=%+v", status, payload)
	}
	rows := payload["data"].([]any)
	got := make([]string, 0, len(rows))
	for _, row := range rows {
		got = append(got, row.(map[string]any)["id"].(string))
	}
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if string(mustJSON(t, got)) != string(mustJSON(t, want)) {
		t.Fatalf("models=%v want=%v", got, want)
	}
}

func containsSecret(payload []byte, secret string) bool {
	return len(secret) > 0 && string(payload) != "" && bytes.Contains(payload, []byte(secret))
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}
