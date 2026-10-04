package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
	"llmgw/internal/router"
)

func TestAdminCreatesProviderFromRegistry(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	router.ResetSavingsState()
	router.ResetTelemetryState()
	t.Cleanup(func() {
		iam.ResetForTests()
		router.ResetSavingsState()
		router.ResetTelemetryState()
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	previousEncryptionKey := config.Get().CredentialEncryptionKey
	t.Cleanup(func() {
		config.Update(func(s *config.Settings) { s.CredentialEncryptionKey = previousEncryptionKey })
	})
	config.Update(func(s *config.Settings) {
		s.APIKey = "admin-secret"
		s.AllowUnauthenticatedAPI = false
		// Without credential encryption the key is kept in secrets.json.
		s.CredentialEncryptionKey = ""
		s.Providers = map[string]*config.ProviderConfig{}
		s.Endpoints = map[string]*config.EndpointConfig{}
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)

	server := httptest.NewServer(NewServer(Runtime{}))
	defer server.Close()
	status, created := jsonRequest(t, server.URL+"/admin/api/providers", http.MethodPost, "admin-secret", map[string]any{
		"registry_id": "gemini",
		"api_key":     "gemini-test-secret",
	})
	if status != http.StatusOK || created["id"] != "gemini" {
		t.Fatalf("create registry provider: %d %+v", status, created)
	}
	cfg := config.Get().Providers["gemini"]
	if cfg == nil || cfg.RegistryID != "gemini" || cfg.Type != "openai_compatible" ||
		cfg.BaseURL != "https://generativelanguage.googleapis.com/v1beta/openai" {
		t.Fatalf("Gemini config=%+v", cfg)
	}
	if got := config.LoadSecrets()["gemini"]; got != "gemini-test-secret" {
		t.Fatalf("Gemini secret=%q", got)
	}
	status, created = jsonRequest(t, server.URL+"/admin/api/providers", http.MethodPost, "admin-secret", map[string]any{
		"registry_id": "codex",
	})
	if status != http.StatusOK || created["id"] != "codex" {
		t.Fatalf("create aliased Codex provider: %d %+v", status, created)
	}
	codex := config.Get().Providers["codex"]
	if codex == nil || codex.RegistryID != "openai_codex" {
		t.Fatalf("Codex registry id was not canonicalized: %+v", codex)
	}
	status, _ = jsonRequest(t, server.URL+"/admin/api/providers", http.MethodPost, "admin-secret", map[string]any{
		"registry_id": "vertex_ai",
		"api_key":     "vertex-test-secret",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("Vertex without project status=%d, want 400", status)
	}
	status, created = jsonRequest(t, server.URL+"/admin/api/providers", http.MethodPost, "admin-secret", map[string]any{
		"registry_id": "vertex_ai",
		"api_key":     "vertex-test-secret",
		"project":     "project-a",
	})
	if status != http.StatusOK || created["id"] != "vertex_ai" {
		t.Fatalf("create Vertex provider: %d %+v", status, created)
	}
	vertex := config.Get().Providers["vertex_ai"]
	if vertex == nil || vertex.Project != "project-a" || vertex.Location != "global" {
		t.Fatalf("Vertex config=%+v", vertex)
	}

	status, state := jsonRequest(t, server.URL+"/admin/api/state", http.MethodGet, "admin-secret", nil)
	if status != http.StatusOK {
		t.Fatalf("state: %d %+v", status, state)
	}
	registry, ok := state["provider_registry"].([]any)
	if !ok || len(registry) < 10 {
		t.Fatalf("provider registry missing from state: %#v", state["provider_registry"])
	}
	statuses, ok := state["provider_statuses"].([]any)
	if !ok || len(statuses) < 10 {
		t.Fatalf("provider snapshots missing from state: %#v", state["provider_statuses"])
	}
	serialized, _ := json.Marshal(state)
	if strings.Contains(string(serialized), "gemini-test-secret") {
		t.Fatalf("provider state leaked a credential: %s", serialized)
	}
	if strings.Contains(string(serialized), "vertex-test-secret") {
		t.Fatalf("provider state leaked the Vertex credential: %s", serialized)
	}
	if !strings.Contains(string(serialized), `"project":"project-a"`) ||
		!strings.Contains(string(serialized), `"location":"global"`) {
		t.Fatalf("provider state omitted safe Vertex setup fields: %s", serialized)
	}
}

func TestProviderUpsertRejectsEndpointNameCollision(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	old := *config.Get()
	t.Cleanup(func() { config.Update(func(settings *config.Settings) { *settings = old }) })
	config.Update(func(settings *config.Settings) {
		settings.APIKey = "admin-secret"
		settings.Providers = map[string]*config.ProviderConfig{}
		settings.Endpoints = map[string]*config.EndpointConfig{
			"Coding": {Failover: []config.EndpointMember{{Provider: "echo", Model: "echo-default"}}},
		}
	})
	server := httptest.NewServer(NewServer(Runtime{}))
	defer server.Close()
	status, body := jsonRequest(t, server.URL+"/admin/api/providers", http.MethodPost, "admin-secret", map[string]any{
		"id": "coding", "type": "edge_tts",
	})
	if status != http.StatusConflict || body["error"] == nil || config.Get().Providers["coding"] != nil {
		t.Fatalf("status=%d body=%+v providers=%+v", status, body, config.Get().Providers)
	}
}

func TestOAuthProviderCreationRejectsEndpointNameCollision(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	old := *config.Get()
	t.Cleanup(func() { config.Update(func(settings *config.Settings) { *settings = old }) })
	config.Update(func(settings *config.Settings) {
		settings.Providers = map[string]*config.ProviderConfig{}
		settings.Endpoints = map[string]*config.EndpointConfig{
			"CODEX": {Failover: []config.EndpointMember{{Provider: "echo", Model: "echo-default"}}},
		}
	})
	if err := ensureOAuthProviderConfig("codex", "openai_codex"); err == nil {
		t.Fatal("OAuth setup accepted a provider colliding with an endpoint")
	}
	if _, exists := config.Provider("codex"); exists {
		t.Fatal("OAuth setup created a provider colliding with an endpoint")
	}
}

// An OAuth step whose configuration change the file does not take is the
// gateway's failure: the start answers 500, and neither the change nor a
// rollback of it is published.
func TestOAuthStartReportsConfigurationSaveFailures(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	t.Setenv("LLMGW_CONFIG", filepath.Join(config.StateDir(), "missing", "config.yaml"))
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	old := *config.Get()
	t.Cleanup(func() { config.Update(func(s *config.Settings) { *s = old }) })
	config.Update(func(s *config.Settings) {
		s.APIKey = "admin-secret"
		s.Providers = map[string]*config.ProviderConfig{}
		s.Endpoints = map[string]*config.EndpointConfig{}
		s.OpenAICodexClientID = "fixture-previous-client"
	})
	owner, err := iam.CreatePrincipal("human", "fixture:oauth-save-failure", "", "OAuth save failure")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(NewServer(Runtime{}))
	defer server.Close()
	unchanged := func(step string, change func() error) {
		t.Helper()
		before, generation := config.Snapshot()
		if err := change(); err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		if current, currentGeneration := config.Snapshot(); current != before || currentGeneration != generation {
			t.Fatalf("%s published a change the file did not take", step)
		}
	}

	unchanged("client ID save", func() error {
		status, body := jsonRequest(t, server.URL+"/admin/api/principals/"+owner.ID+"/connections/codex/oauth/start",
			http.MethodPost, "admin-secret", map[string]any{"client_id": "fixture-new-client", "flow": "device_code"})
		if status != http.StatusInternalServerError || !strings.Contains(fmt.Sprint(body["error"]), "Codex OAuth client ID") {
			return fmt.Errorf("status=%d body=%+v", status, body)
		}
		return nil
	})
	unchanged("provider setup", func() error {
		if err := ensureOAuthProviderConfig("codex", "openai_codex"); !errors.As(err, new(*configSaveError)) {
			return fmt.Errorf("err=%v", err)
		}
		return nil
	})
	config.Update(func(s *config.Settings) { s.OpenAICodexClientID = "fixture-new-client" })
	unchanged("client ID rollback", func() error {
		if err := restoreCodexClientID("fixture-new-client", "fixture-previous-client"); !errors.As(err, new(*configSaveError)) {
			return fmt.Errorf("err=%v", err)
		}
		return nil
	})
}

func TestProviderUpsertAcceptsAndPreservesPublicOAuthClientID(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	t.Setenv("LLMGW_CREDENTIAL_ENCRYPTION_KEY", base64.RawURLEncoding.EncodeToString(make([]byte, 32)))
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	config.Update(func(s *config.Settings) {
		s.APIKey = "admin-secret"
		s.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
		s.Providers = map[string]*config.ProviderConfig{}
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(NewServer(Runtime{}))
	defer server.Close()

	status, body := jsonRequest(t, server.URL+"/admin/api/providers", http.MethodPost, "admin-secret", map[string]any{
		"registry_id": "google_antigravity", "public_oauth_client_id": "public-client",
	})
	if status != http.StatusOK {
		t.Fatalf("create status=%d body=%+v", status, body)
	}
	status, body = jsonRequest(t, server.URL+"/admin/api/providers", http.MethodPost, "admin-secret", map[string]any{
		"id": "google-antigravity", "registry_id": "google_antigravity", "region": "updated-region",
	})
	if status != http.StatusOK {
		t.Fatalf("update status=%d body=%+v", status, body)
	}
	provider := config.Get().Providers["google-antigravity"]
	if provider == nil || provider.PublicOAuthClientID != "public-client" || provider.Region != "updated-region" {
		t.Fatalf("provider=%+v", provider)
	}
	reloaded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if provider := reloaded.Providers["google-antigravity"]; provider == nil || provider.PublicOAuthClientID != "public-client" {
		t.Fatalf("reloaded provider=%+v", provider)
	}
	owner, err := iam.CreatePrincipal("human", "fixture:oauth-client-clear", "", "OAuth client clear")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
		PrincipalID: owner.ID, ProviderID: "google-antigravity", Kind: "google_antigravity_oauth",
		AccessToken: "fixture-access", RefreshToken: "fixture-refresh", OAuthClientID: "public-client",
	}); err != nil {
		t.Fatal(err)
	}
	config.Update(func(settings *config.Settings) { settings.APIKey = "admin-secret" })
	clear := true
	status, body = jsonRequest(t, server.URL+"/admin/api/providers", http.MethodPost, "admin-secret", map[string]any{
		"id": "google-antigravity", "registry_id": "google_antigravity",
		"clear_public_oauth_client_id": clear,
	})
	if status != http.StatusConflict {
		t.Fatalf("bound clear status=%d body=%+v", status, body)
	}
	connections, err := iam.ListProviderConnections(owner.ID, "google-antigravity")
	if err != nil || len(connections) != 1 {
		t.Fatalf("connections=%+v err=%v", connections, err)
	}
	current, stored, ok, err := iam.OAuthProviderConnectionSecret(owner.ID, "google-antigravity", connections[0].Name)
	if err != nil || !ok {
		t.Fatalf("load OAuth connection: ok=%v err=%v", ok, err)
	}
	if _, err := iam.ReplaceOAuthProviderConnectionIfCurrent(stored, current, iam.OAuthConnectionCreate{
		PrincipalID: owner.ID, ProviderID: "google-antigravity", Name: stored.Name, Kind: stored.Kind,
		AccessToken: current.AccessToken, RefreshToken: current.RefreshToken, Status: "active",
	}); err != nil {
		t.Fatal(err)
	}
	status, body = jsonRequest(t, server.URL+"/admin/api/providers", http.MethodPost, "admin-secret", map[string]any{
		"id": "google-antigravity", "registry_id": "google_antigravity",
		"clear_public_oauth_client_id": clear,
	})
	if status != http.StatusConflict {
		t.Fatalf("legacy bound clear status=%d body=%+v", status, body)
	}
	connections, err = iam.ListProviderConnections(owner.ID, "google-antigravity")
	if err := iam.RevokeProviderConnection(owner.ID, connections[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := iam.PutProviderCredential(owner.ID, "google-antigravity", "google_antigravity_oauth", "legacy-access"); err != nil {
		t.Fatal(err)
	}
	status, body = jsonRequest(t, server.URL+"/admin/api/providers", http.MethodPost, "admin-secret", map[string]any{
		"id": "google-antigravity", "registry_id": "google_antigravity",
		"clear_public_oauth_client_id": clear,
	})
	if status != http.StatusConflict {
		t.Fatalf("legacy credential clear status=%d body=%+v", status, body)
	}
	if err := iam.RevokeProviderCredential(owner.ID, "google-antigravity"); err != nil {
		t.Fatal(err)
	}
	status, body = jsonRequest(t, server.URL+"/admin/api/providers", http.MethodPost, "admin-secret", map[string]any{
		"id": "google-antigravity", "registry_id": "google_antigravity",
		"clear_public_oauth_client_id": clear,
	})
	if status != http.StatusOK {
		t.Fatalf("clear status=%d body=%+v", status, body)
	}
	if provider := config.Get().Providers["google-antigravity"]; provider == nil || provider.PublicOAuthClientID != "" {
		t.Fatalf("public OAuth client ID was not cleared: %+v", provider)
	}
}

func TestAdminSetupTokenRequiresEncryptedStorageAndRemovesLegacySecret(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	config.Update(func(s *config.Settings) {
		s.APIKey = "admin-secret"
		s.CredentialEncryptionKey = ""
		s.Providers = map[string]*config.ProviderConfig{}
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(NewServer(Runtime{}))
	defer server.Close()
	token := AnthropicSetupTokenPrefix + strings.Repeat("a", 80)
	status, _ := jsonRequest(t, server.URL+"/admin/api/providers", http.MethodPost, "admin-secret", map[string]any{
		"registry_id": "anthropic", "api_key": token,
	})
	if status != http.StatusBadRequest || config.Get().Providers["anthropic"] != nil {
		t.Fatalf("setup token without encryption status=%d provider=%v", status, config.Get().Providers["anthropic"])
	}
	if err := config.SaveSecret("anthropic", "legacy-api-key"); err != nil {
		t.Fatal(err)
	}
	config.Update(func(s *config.Settings) {
		s.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	})
	status, body := jsonRequest(t, server.URL+"/admin/api/providers", http.MethodPost, "admin-secret", map[string]any{
		"registry_id": "anthropic", "api_key": token, "credential_kind": "api_key",
	})
	if status != http.StatusOK {
		t.Fatalf("setup token status=%d body=%v", status, body)
	}
	if _, exists := config.LoadSecrets()["anthropic"]; exists {
		t.Fatal("legacy Anthropic secret remained after setup-token rotation")
	}
	secret, connection, found, err := iam.SystemProviderConnectionSecret("anthropic")
	if err != nil || !found {
		t.Fatalf("encrypted setup-token connection found=%v err=%v", found, err)
	}
	if connection.Kind != "setup_token" || secret != token {
		t.Fatalf("stored connection kind=%q secret matches=%v", connection.Kind, secret == token)
	}
}

// With credential encryption the provider form keeps the key only in the
// encrypted store, and drops the plaintext key an earlier save left behind.
func TestAdminProviderKeyStaysOutOfSecretsFileWithEncryption(t *testing.T) {
	state := t.TempDir()
	t.Setenv("LLMGW_STATE_DIR", state)
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	old := *config.Get()
	t.Cleanup(func() { config.Update(func(s *config.Settings) { *s = old }) })
	config.Update(func(s *config.Settings) {
		s.APIKey = "admin-secret"
		s.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
		s.Providers = map[string]*config.ProviderConfig{}
		s.Endpoints = map[string]*config.EndpointConfig{}
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)
	if err := config.SaveSecret("gemini", "fixture-earlier-key"); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(NewServer(Runtime{}))
	defer server.Close()

	status, body := jsonRequest(t, server.URL+"/admin/api/providers", http.MethodPost, "admin-secret", map[string]any{
		"registry_id": "gemini", "api_key": "fixture-current-key",
	})
	if status != http.StatusOK {
		t.Fatalf("create status=%d body=%+v", status, body)
	}
	secret, connection, found, err := iam.SystemProviderConnectionSecret("gemini")
	if err != nil || !found || secret != "fixture-current-key" || connection.Source != iam.ConnectionSourceAdmin {
		t.Fatalf("system connection=%+v matches=%v found=%v err=%v", connection, secret == "fixture-current-key", found, err)
	}
	if _, err := os.Stat(filepath.Join(state, "secrets.json")); !os.IsNotExist(err) {
		t.Fatalf("secrets.json was kept beside the encrypted key: %v", err)
	}
}

// When provider keys are stored in plaintext, the key a config file sets is
// resolved before secrets.json. Editing the provider without a key keeps the
// configured one; a key entered in the console replaces it and, after a
// restart, is still the key in use.
func TestConsoleKeyReplacesTheConfiguredKeyAcrossRestarts(t *testing.T) {
	state := t.TempDir()
	t.Setenv("LLMGW_STATE_DIR", state)
	t.Setenv("LLMGW_CONFIG", filepath.Join(state, "config.yaml"))
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	old := *config.Get()
	t.Cleanup(func() { config.Update(func(s *config.Settings) { *s = old }) })
	const file = `providers:
  compat:
    type: openai_compatible
    base_url: https://compat.example.test/v1
    api_key: fixture-file-secret
`
	if err := os.WriteFile(config.ConfigFilePath(), []byte(file), 0o600); err != nil {
		t.Fatal(err)
	}
	restart := func() {
		t.Helper()
		if _, err := config.Load(); err != nil {
			t.Fatal(err)
		}
		config.Update(func(s *config.Settings) {
			s.APIKey = "admin-secret"
			s.AllowUnauthenticatedAPI = false
			s.CredentialEncryptionKey = ""
		})
	}
	restart()
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)
	server := httptest.NewServer(NewServer(Runtime{}))
	defer server.Close()
	upsert := func(fields map[string]any) {
		t.Helper()
		body := map[string]any{"id": "compat", "type": "openai_compatible"}
		for key, value := range fields {
			body[key] = value
		}
		if status, response := jsonRequest(t, server.URL+"/admin/api/providers", http.MethodPost, "admin-secret", body); status != http.StatusOK {
			t.Fatalf("upsert status=%d body=%+v", status, response)
		}
	}
	resolved := func() string {
		return config.ResolveProviderAPIKey("compat", config.Get().Providers["compat"])
	}

	upsert(map[string]any{"base_url": "https://compat-moved.example.test/v1"})
	if got := resolved(); got != "fixture-file-secret" {
		t.Fatalf("editing without a key lost the configured key: %q", got)
	}
	restart()
	if got := resolved(); got != "fixture-file-secret" {
		t.Fatalf("after a restart the configured key is gone: %q", got)
	}

	upsert(map[string]any{"base_url": "https://compat-moved.example.test/v1", "api_key": "fixture-console-secret"})
	if got := resolved(); got != "fixture-console-secret" {
		t.Fatalf("the console key is not in use: %q", got)
	}
	restart()
	if got := resolved(); got != "fixture-console-secret" {
		t.Fatalf("after a restart the configured key replaced the console key: %q", got)
	}
}

// When secrets.json cannot be updated, saving a provider key fails, with or
// without credential encryption, and deleting a provider reports the key it
// left behind instead of claiming success.
func TestProviderKeyStorageFailuresAreReported(t *testing.T) {
	old := *config.Get()
	t.Cleanup(func() { config.Update(func(s *config.Settings) { *s = old }) })
	env := newCredentialTestEnv(t)
	defer env.server.Close()
	// A directory where secrets.json belongs can be neither read nor replaced.
	if err := os.Mkdir(filepath.Join(config.StateDir(), "secrets.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	route := env.server.URL + "/admin/api/providers"
	for _, encryptionKey := range []string{config.Get().CredentialEncryptionKey, ""} {
		config.Update(func(s *config.Settings) { s.CredentialEncryptionKey = encryptionKey })
		status, body := jsonRequest(t, route, http.MethodPost, "admin-secret", map[string]any{
			"registry_id": "gemini", "api_key": "fixture-unsaved-key",
		})
		if status != http.StatusInternalServerError || config.Get().Providers["gemini"] != nil {
			t.Fatalf("encrypted=%v: status=%d provider=%+v", encryptionKey != "", status, config.Get().Providers["gemini"])
		}
		if strings.Contains(stringifyAny(body), "fixture-unsaved-key") {
			t.Fatal("the failure response repeats the key")
		}
	}
	status, _ := jsonRequest(t, route+"/vertex-prod", http.MethodDelete, "admin-secret", nil)
	if status != http.StatusInternalServerError || config.Get().Providers["vertex-prod"] != nil {
		t.Fatalf("delete status=%d provider=%+v", status, config.Get().Providers["vertex-prod"])
	}
}

func TestAdminRejectsSetupTokenLookingCredentialForNonAnthropicProvider(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	config.Update(func(s *config.Settings) {
		s.APIKey = "admin-secret"
		s.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
		s.Providers = map[string]*config.ProviderConfig{}
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(NewServer(Runtime{}))
	defer server.Close()
	token := AnthropicSetupTokenPrefix + strings.Repeat("a", 80)
	status, _ := jsonRequest(t, server.URL+"/admin/api/providers", http.MethodPost, "admin-secret", map[string]any{
		"registry_id": "gemini", "api_key": token,
	})
	if status != http.StatusBadRequest {
		t.Fatalf("non-Anthropic setup token status=%d, want 400", status)
	}
	if config.Get().Providers["gemini"] != nil || config.LoadSecrets()["gemini"] != "" {
		t.Fatal("rejected setup token changed Gemini provider or plaintext secrets")
	}
	if exists, err := iam.SystemProviderConnectionExists("gemini"); err != nil || exists {
		t.Fatalf("rejected setup token encrypted connection exists=%v err=%v", exists, err)
	}
}

func TestAdminRejectsPlannedRegistryProvider(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	router.ResetSavingsState()
	router.ResetTelemetryState()
	t.Cleanup(func() {
		router.ResetSavingsState()
		router.ResetTelemetryState()
	})
	config.Update(func(s *config.Settings) {
		s.APIKey = "admin-secret"
		s.AllowUnauthenticatedAPI = false
		s.Providers = map[string]*config.ProviderConfig{}
	})
	server := httptest.NewServer(NewServer(Runtime{}))
	defer server.Close()
	status, _ := jsonRequest(t, server.URL+"/admin/api/providers", http.MethodPost, "admin-secret", map[string]any{
		"registry_id": "claude_code",
	})
	if status != http.StatusConflict {
		t.Fatalf("planned provider status=%d, want %d", status, http.StatusConflict)
	}
}

func TestProviderStatusKeepsAliasNamedCustomProviderSeparate(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	settings := config.Defaults()
	settings.Providers = map[string]*config.ProviderConfig{
		"claude": {Type: "anthropic", BaseURL: "https://example.invalid"},
	}
	snapshots, err := providerStatusSnapshots(
		settings, map[string]string{"claude": "fake-key"}, nil, "",
	)
	if err != nil {
		t.Fatal(err)
	}
	var custom, client map[string]any
	for _, snapshot := range snapshots {
		switch snapshot["id"] {
		case "claude":
			custom = snapshot
		case "claude_code":
			client = snapshot
		}
	}
	if custom == nil || custom["configured"] != true || custom["description"] != "Custom configured provider." {
		t.Fatalf("custom alias-named provider snapshot=%+v", custom)
	}
	if client == nil || client["configured"] != false || client["status"] != "client_setup" {
		t.Fatalf("Claude Code registry snapshot=%+v", client)
	}
}

func TestProviderProbeReturnsExplicitSafeDetails(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	config.Update(func(s *config.Settings) {
		s.APIKey = "admin-secret"
		s.AllowUnauthenticatedAPI = false
		s.Providers = map[string]*config.ProviderConfig{"echo": {Type: "echo"}}
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)
	server := httptest.NewServer(NewServer(Runtime{}))
	defer server.Close()

	for _, operation := range []string{"test", "repair", "refresh"} {
		status, payload := jsonRequest(t, server.URL+"/admin/api/providers/echo/"+operation, http.MethodPost, "admin-secret", map[string]any{})
		if status != http.StatusOK || payload["success"] != true || payload["status"] != "passed" {
			t.Fatalf("%s probe status=%d payload=%+v", operation, status, payload)
		}
		if detail, _ := payload["details"].(string); detail == "" {
			t.Fatalf("%s probe did not return details: %+v", operation, payload)
		}
	}
}

func TestProviderUpsertInvalidatesCatalogAndLifecycleEvidence(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"fixture-model"}]}`))
	}))
	defer upstream.Close()
	config.Update(func(s *config.Settings) {
		s.APIKey = "admin-secret"
		s.Providers = map[string]*config.ProviderConfig{
			"fixture": {
				Type: "openai_compatible", BaseURL: upstream.URL, APIKey: "fixture",
			},
		}
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)
	if models := providers.RefreshCatalog("fixture"); len(models) != 1 {
		t.Fatalf("catalog models=%+v", models)
	}
	generation, err := iam.ProviderCheckGeneration("fixture", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := iam.RecordProviderCheck(iam.ProviderCheck{
		ProviderID: "fixture", Operation: iam.CheckVerify,
		Generation: generation, Success: true, Detail: "Provider check passed.",
	}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(NewServer(Runtime{}))
	defer server.Close()
	status, result := jsonRequest(
		t, server.URL+"/admin/api/providers", http.MethodPost, "admin-secret",
		map[string]any{
			"id": "fixture", "type": "openai_compatible",
			"base_url": upstream.URL,
		},
	)
	if status != http.StatusOK || result["ok"] != true {
		t.Fatalf("upsert status=%d result=%+v", status, result)
	}
	if models, _ := providers.CatalogCached("fixture"); len(models) != 0 {
		t.Fatalf("upsert retained catalog: %+v", models)
	}
	checks, err := iam.LastProviderChecks("")
	if err != nil {
		t.Fatal(err)
	}
	if len(checks["fixture"]) != 0 {
		t.Fatalf("upsert retained checks: %+v", checks)
	}
	current, err := iam.ProviderCheckGeneration("fixture", "")
	if err != nil || current <= generation {
		t.Fatalf("generation current=%d previous=%d err=%v", current, generation, err)
	}
}

func TestPortalProviderStatusUsesOnlyItsPrincipalCatalog(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	settings := config.Defaults()
	settings.Providers = map[string]*config.ProviderConfig{
		"echo": {Type: "echo"},
	}
	config.Update(func(current *config.Settings) { *current = *settings })
	t.Cleanup(func() {
		defaults := config.Defaults()
		config.Update(func(current *config.Settings) { *current = *defaults })
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)
	first, err := iam.CreatePrincipal("human", "authentik:first-catalog", "", "First")
	if err != nil {
		t.Fatal(err)
	}
	second, err := iam.CreatePrincipal("human", "authentik:second-catalog", "", "Second")
	if err != nil {
		t.Fatal(err)
	}
	firstPrincipal := &config.Principal{PrincipalID: first.ID, PrincipalKind: first.Kind}
	if models := providers.RefreshCatalogForPrincipal("echo", callerOf(firstPrincipal)); len(models) == 0 {
		t.Fatal("first principal catalog did not refresh")
	}
	firstRows, err := providerStatusSnapshots(settings, nil, nil, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	secondRows, err := providerStatusSnapshots(settings, nil, nil, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	modelCount := func(rows []map[string]any) int {
		for _, row := range rows {
			if row["id"] == "echo" {
				return row["model_count"].(int)
			}
		}
		return -1
	}
	if got := modelCount(firstRows); got == 0 {
		t.Fatalf("first principal model count=%d", got)
	}
	if got := modelCount(secondRows); got != 0 {
		t.Fatalf("second principal saw another catalog: %d", got)
	}
}

// Portal mode renders one human's view, and the not-discoverable verdict must
// be settled inside that scope. checks and cached models are already narrowed
// to the selected principal, but the final comparison reached for the
// operator-wide CatalogSnapshot, which reports the freshest catalog across ALL
// scopes — so another principal's newer catalog cleared this user's
// not_discoverable state while this user's own credential still could not list
// a thing.
func TestPortalNotDiscoverableSurvivesAnotherPrincipalsNewerCatalog(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	settings := config.Defaults()
	settings.Providers = map[string]*config.ProviderConfig{
		"echo": {Type: "echo"},
	}
	config.Update(func(current *config.Settings) { *current = *settings })
	t.Cleanup(func() {
		defaults := config.Defaults()
		config.Update(func(current *config.Settings) { *current = *defaults })
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)

	viewer, err := iam.CreatePrincipal("human", "fixture:portal-viewer", "", "Viewer")
	if err != nil {
		t.Fatal(err)
	}
	other, err := iam.CreatePrincipal("human", "fixture:other-principal", "", "Other")
	if err != nil {
		t.Fatal(err)
	}
	// The viewer's own catalog-facing check found the catalog unlistable.
	generation, err := iam.ProviderCheckGeneration("echo", viewer.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := iam.RecordProviderCheck(iam.ProviderCheck{
		ProviderID: "echo", Operation: iam.CheckCatalogSync, ScopeKey: viewer.ID,
		Generation: generation, Success: false,
		Detail:    providerCheckDetail(false, "catalog_not_discoverable"),
		CheckedAt: time.Now().Add(-10 * time.Minute).Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	// Another principal then listed models. That catalog is newer than the
	// viewer's failure and belongs to somebody else's credential.
	otherPrincipal := &config.Principal{PrincipalID: other.ID, PrincipalKind: other.Kind}
	if models := providers.RefreshCatalogForPrincipal("echo", callerOf(otherPrincipal)); len(models) == 0 {
		t.Fatal("other principal catalog did not refresh")
	}

	rows, err := providerStatusSnapshots(settings, nil, nil, viewer.ID)
	if err != nil {
		t.Fatal(err)
	}
	var row map[string]any
	for _, candidate := range rows {
		if candidate["id"] == "echo" {
			row = candidate
			break
		}
	}
	if row == nil {
		t.Fatalf("echo row missing from %+v", rows)
	}
	if got := stringOf(row["catalog_state"]); got != "not_discoverable" {
		t.Fatalf("portal catalog_state=%q, want \"not_discoverable\" — another principal's catalog cleared it", got)
	}
	instances, ok := row["instances"].([]map[string]any)
	if !ok || len(instances) != 1 {
		t.Fatalf("instances=%#v, want exactly one", row["instances"])
	}
	if got := stringOf(instances[0]["catalog_state"]); got != "not_discoverable" {
		t.Fatalf("portal instance catalog_state=%q, want \"not_discoverable\"", got)
	}
}
