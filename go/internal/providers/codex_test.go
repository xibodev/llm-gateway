package providers

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"llmgw/internal/codexauth"
	"llmgw/internal/config"
	"llmgw/internal/iam"
)

func TestCodexCatalogRefreshStoresModelsAfterCredentialRotation(t *testing.T) {
	setupCodexProviderTest(t)
	human, err := iam.CreatePrincipal("human", "authentik:codex-catalog-refresh", "", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	oldClientID := config.Get().OpenAICodexClientID
	oldProviders := config.Get().Providers
	config.Update(func(s *config.Settings) {
		s.OpenAICodexClientID = "fixture-client"
		s.Providers = map[string]*config.ProviderConfig{
			"codex": {Type: "openai_compatible", RegistryID: "openai_codex"},
		}
	})
	t.Cleanup(func() {
		config.Update(func(s *config.Settings) {
			s.OpenAICodexClientID, s.Providers = oldClientID, oldProviders
		})
	})
	if _, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
		PrincipalID: human.ID, ProviderID: "codex", Kind: "openai_codex_oauth",
		AccessToken: "old-access", RefreshToken: "refresh-token",
		ExpiresAt: time.Now().Add(-time.Minute).Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"new-access","refresh_token":"new-refresh"}`))
		case "/backend-api/codex/models":
			if r.Header.Get("Authorization") != "Bearer new-access" {
				t.Fatalf("model auth=%q", r.Header.Get("Authorization"))
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-codex"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	oldModels, oldToken := codexauth.ModelsURL, codexauth.OAuthTokenURL
	codexauth.ModelsURL = server.URL + "/backend-api/codex/models"
	codexauth.OAuthTokenURL = server.URL + "/oauth/token"
	t.Cleanup(func() {
		codexauth.ModelsURL, codexauth.OAuthTokenURL = oldModels, oldToken
	})
	principal := &config.Principal{PrincipalID: human.ID, PrincipalKind: human.Kind}
	models, observation, err := RefreshCatalogForPrincipalWithError("codex", principal)
	if err != nil || len(models) != 1 || observation == nil {
		t.Fatalf("models=%+v observation=%+v err=%v", models, observation, err)
	}
	cached, refreshed := CatalogCachedForPrincipal("codex", principal)
	if len(cached) != 1 || refreshed.IsZero() {
		t.Fatalf("cached=%+v refreshed=%v", cached, refreshed)
	}
}

func TestCodexProviderUsesResponsesRefreshesOnceAndCatalogsWithClientVersion(t *testing.T) {
	setupCodexProviderTest(t)
	human, err := iam.CreatePrincipal("human", "authentik:codex-owner", "", "Codex Owner")
	if err != nil {
		t.Fatal(err)
	}
	oldClientID := config.Get().OpenAICodexClientID
	oldProviders := config.Get().Providers
	t.Cleanup(func() {
		config.Update(func(s *config.Settings) { s.OpenAICodexClientID, s.Providers = oldClientID, oldProviders })
	})
	config.Update(func(s *config.Settings) {
		s.OpenAICodexClientID = "fixture-client"
		s.Providers = map[string]*config.ProviderConfig{"codex": {Type: "openai_compatible", RegistryID: "openai_codex"}}
	})
	originalExpiresAt := time.Now().Add(time.Hour).Unix()
	if _, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
		PrincipalID: human.ID, ProviderID: "codex", Kind: "openai_codex_oauth", Source: iam.ConnectionSourceUser,
		AccessToken: "old-access", RefreshToken: "refresh-token", IDToken: "old-id", TokenType: "Bearer",
		AccountID: "account-42", AccountLabel: "Fixture workspace", ExpiresAt: originalExpiresAt,
	}); err != nil {
		t.Fatal(err)
	}

	responses := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend-api/codex/responses":
			responses++
			if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("ChatGPT-Account-ID") != "account-42" {
				t.Fatalf("responses request method=%s content=%q account=%q", r.Method, r.Header.Get("Content-Type"), r.Header.Get("ChatGPT-Account-ID"))
			}
			if responses == 1 {
				if r.Header.Get("Authorization") != "Bearer old-access" {
					t.Fatalf("first auth=%q", r.Header.Get("Authorization"))
				}
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if r.Header.Get("Authorization") != "Bearer new-access" {
				t.Fatalf("retry auth=%q", r.Header.Get("Authorization"))
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Codex response"}]}],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}`))
		case "/oauth/token":
			body := map[string]string{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if r.Header.Get("Content-Type") != "application/json" || body["grant_type"] != "refresh_token" || body["client_id"] != "fixture-client" || body["refresh_token"] != "refresh-token" || len(body) != 3 {
				t.Fatalf("refresh body=%v", body)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"new-access","refresh_token":"new-refresh"}`))
		case "/backend-api/codex/models":
			if r.URL.Query().Get("client_version") != codexauth.ClientVersion {
				t.Fatalf("model query=%s", r.URL.RawQuery)
			}
			if r.Header.Get("Authorization") != "Bearer new-access" || r.Header.Get("ChatGPT-Account-ID") != "account-42" {
				t.Fatalf("model headers auth=%q account=%q", r.Header.Get("Authorization"), r.Header.Get("ChatGPT-Account-ID"))
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5-codex","owned_by":"openai","display_name":"GPT-5 Codex","supported_endpoints":["/responses"]}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	oldResponses, oldModels, oldToken := codexauth.ResponsesBaseURL, codexauth.ModelsURL, codexauth.OAuthTokenURL
	codexauth.ResponsesBaseURL = server.URL + "/backend-api/codex"
	codexauth.ModelsURL = server.URL + "/backend-api/codex/models"
	codexauth.OAuthTokenURL = server.URL + "/oauth/token"
	t.Cleanup(func() {
		codexauth.ResponsesBaseURL, codexauth.ModelsURL, codexauth.OAuthTokenURL = oldResponses, oldModels, oldToken
	})

	instance, err := GetProviderForPrincipal("codex", &config.Principal{PrincipalID: human.ID})
	if err != nil {
		t.Fatal(err)
	}
	codex, ok := instance.(CodexProvider)
	if !ok {
		t.Fatalf("provider type=%T", instance)
	}
	response, observation, err := codex.CompleteWithObservation(
		"gpt-5-codex", []Message{{"role": "user", "content": "hello"}}, Kwargs{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if responses != 2 || response["model"] != "gpt-5-codex" {
		t.Fatalf("responses=%d response=%+v", responses, response)
	}
	currentObservation, found, err := iam.ActiveProviderAccountObservation(human.ID, "codex")
	if err != nil || !found || observation == nil || *observation != currentObservation {
		t.Fatalf("observation=%+v current=%+v found=%v err=%v", observation, currentObservation, found, err)
	}
	refreshed, _, ok, err := iam.OAuthProviderConnectionSecret(human.ID, "codex", "")
	if err != nil || !ok || refreshed.AccessToken != "new-access" || refreshed.RefreshToken != "new-refresh" || refreshed.IDToken != "old-id" || refreshed.TokenType != "Bearer" || refreshed.AccountID != "account-42" || refreshed.AccountLabel != "Fixture workspace" || refreshed.ExpiresAt != 0 {
		t.Fatalf("refreshed=%+v ok=%v err=%v", refreshed, ok, err)
	}
	rows := codex.ListModels()
	if len(rows) != 1 || rows[0].ID != "gpt-5-codex" || rows[0].Label != "GPT-5 Codex" || len(rows[0].SupportedSurfaces) != 1 {
		t.Fatalf("Codex models=%+v", rows)
	}
}

func TestCodexInvalidRefreshRevokesPrivateConnection(t *testing.T) {
	setupCodexProviderTest(t)
	human, err := iam.CreatePrincipal("human", "authentik:codex-invalid", "", "Codex Invalid")
	if err != nil {
		t.Fatal(err)
	}
	oldClientID := config.Get().OpenAICodexClientID
	t.Cleanup(func() { config.Update(func(s *config.Settings) { s.OpenAICodexClientID = oldClientID }) })
	config.Update(func(s *config.Settings) { s.OpenAICodexClientID = "fixture-client" })
	if _, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
		PrincipalID: human.ID, ProviderID: "codex", Kind: "openai_codex_oauth", AccessToken: "old", RefreshToken: "stale",
	}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer server.Close()
	oldToken := codexauth.OAuthTokenURL
	codexauth.OAuthTokenURL = server.URL
	t.Cleanup(func() { codexauth.OAuthTokenURL = oldToken })
	auth := codexAuth{principalID: human.ID, providerID: "codex", clientID: "fixture-client"}
	if err := auth.Refresh(); err == nil {
		t.Fatal("invalid refresh unexpectedly succeeded")
	}
	if _, _, ok, err := iam.OAuthProviderConnectionSecret(human.ID, "codex", ""); err != nil || ok {
		t.Fatalf("invalid refresh left active connection: ok=%v err=%v", ok, err)
	}
}

func TestCodexRefreshSerializesConcurrentRotation(t *testing.T) {
	setupCodexProviderTest(t)
	human, err := iam.CreatePrincipal("human", "authentik:codex-concurrent", "", "Codex Concurrent")
	if err != nil {
		t.Fatal(err)
	}
	oldClientID := config.Get().OpenAICodexClientID
	t.Cleanup(func() { config.Update(func(s *config.Settings) { s.OpenAICodexClientID = oldClientID }) })
	config.Update(func(s *config.Settings) { s.OpenAICodexClientID = "fixture-client" })
	if _, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
		PrincipalID: human.ID, ProviderID: "codex", Kind: "openai_codex_oauth", AccessToken: "old-access", RefreshToken: "old-refresh",
	}); err != nil {
		t.Fatal(err)
	}
	initial, connection, ok, err := iam.OAuthProviderConnectionSecret(human.ID, "codex", "")
	if err != nil || !ok {
		t.Fatalf("initial connection ok=%v err=%v", ok, err)
	}
	var refreshCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCalls.Add(1)
		body := map[string]string{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["refresh_token"] != "old-refresh" {
			t.Fatalf("refresh body=%+v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"new-access","refresh_token":"new-refresh"}`))
	}))
	defer server.Close()
	oldToken := codexauth.OAuthTokenURL
	codexauth.OAuthTokenURL = server.URL
	t.Cleanup(func() { codexauth.OAuthTokenURL = oldToken })

	auth := codexAuth{principalID: human.ID, providerID: "codex", clientID: "fixture-client"}
	start := make(chan struct{})
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			errs <- auth.refreshConnection(initial, connection)
		}()
	}
	close(start)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if refreshCalls.Load() != 1 {
		t.Fatalf("refresh calls=%d want 1", refreshCalls.Load())
	}
	current, _, ok, err := iam.OAuthProviderConnectionSecret(human.ID, "codex", "")
	if err != nil || !ok || current.AccessToken != "new-access" || current.RefreshToken != "new-refresh" {
		t.Fatalf("current=%+v ok=%v err=%v", current, ok, err)
	}
}

func TestCodexRefreshRejectsChangedAccount(t *testing.T) {
	setupCodexProviderTest(t)
	human, err := iam.CreatePrincipal("human", "authentik:codex-account-change", "", "Codex Account Change")
	if err != nil {
		t.Fatal(err)
	}
	oldClientID := config.Get().OpenAICodexClientID
	t.Cleanup(func() { config.Update(func(s *config.Settings) { s.OpenAICodexClientID = oldClientID }) })
	config.Update(func(s *config.Settings) { s.OpenAICodexClientID = "fixture-client" })
	if _, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
		PrincipalID: human.ID, ProviderID: "codex", Kind: "openai_codex_oauth", AccessToken: "old-access", RefreshToken: "old-refresh", AccountID: "account-a",
	}); err != nil {
		t.Fatal(err)
	}
	initial, connection, ok, err := iam.OAuthProviderConnectionSecret(human.ID, "codex", "")
	if err != nil || !ok {
		t.Fatalf("initial connection ok=%v err=%v", ok, err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"new-access","refresh_token":"new-refresh","account_id":"account-b"}`))
	}))
	defer server.Close()
	oldToken := codexauth.OAuthTokenURL
	codexauth.OAuthTokenURL = server.URL
	t.Cleanup(func() { codexauth.OAuthTokenURL = oldToken })

	auth := codexAuth{principalID: human.ID, providerID: "codex", clientID: "fixture-client"}
	err = auth.refreshConnection(initial, connection)
	if err == nil || !strings.Contains(err.Error(), "account changed") {
		t.Fatalf("refresh err=%v", err)
	}
	current, _, ok, err := iam.OAuthProviderConnectionSecret(human.ID, "codex", "")
	if err != nil || !ok || current.AccessToken != "old-access" || current.AccountID != "account-a" {
		t.Fatalf("current=%+v ok=%v err=%v", current, ok, err)
	}
}

func TestCodexPrepareRejectsConcurrentAccountReplacement(t *testing.T) {
	setupCodexProviderTest(t)
	human, err := iam.CreatePrincipal("human", "authentik:codex-account-replacement", "", "Codex Account Replacement")
	if err != nil {
		t.Fatal(err)
	}
	oldClientID := config.Get().OpenAICodexClientID
	t.Cleanup(func() { config.Update(func(s *config.Settings) { s.OpenAICodexClientID = oldClientID }) })
	config.Update(func(s *config.Settings) { s.OpenAICodexClientID = "fixture-client" })
	if _, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
		PrincipalID: human.ID, ProviderID: "codex", Kind: "openai_codex_oauth", AccessToken: "old-access", RefreshToken: "old-refresh", AccountID: "account-a", ExpiresAt: time.Now().Add(-time.Minute).Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	_, connection, ok, err := iam.OAuthProviderConnectionSecret(human.ID, "codex", "")
	if err != nil || !ok {
		t.Fatalf("initial connection ok=%v err=%v", ok, err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
			PrincipalID: human.ID, ProviderID: "codex", Name: connection.Name, Kind: connection.Kind, Source: connection.Source, MakeDefault: connection.IsDefault,
			AccessToken: "replacement-access", RefreshToken: "replacement-refresh", AccountID: "account-b", Status: "active",
		}); err != nil {
			t.Fatalf("replace connection: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"new-access","refresh_token":"new-refresh","account_id":"account-a"}`))
	}))
	defer server.Close()
	oldToken := codexauth.OAuthTokenURL
	codexauth.OAuthTokenURL = server.URL
	t.Cleanup(func() { codexauth.OAuthTokenURL = oldToken })

	auth := codexAuth{principalID: human.ID, providerID: "codex", clientID: "fixture-client"}
	if _, _, err = auth.Prepare(); err == nil || !strings.Contains(err.Error(), "account changed") {
		t.Fatalf("prepare err=%v", err)
	}
	current, _, ok, err := iam.OAuthProviderConnectionSecret(human.ID, "codex", "")
	if err != nil || !ok || current.AccessToken != "replacement-access" || current.AccountID != "account-b" {
		t.Fatalf("current=%+v ok=%v err=%v", current, ok, err)
	}
}

func TestCodexReusedRefreshDoesNotRevokeRotatedConnection(t *testing.T) {
	setupCodexProviderTest(t)
	human, err := iam.CreatePrincipal("human", "authentik:codex-reused", "", "Codex Reused")
	if err != nil {
		t.Fatal(err)
	}
	oldClientID := config.Get().OpenAICodexClientID
	t.Cleanup(func() { config.Update(func(s *config.Settings) { s.OpenAICodexClientID = oldClientID }) })
	config.Update(func(s *config.Settings) { s.OpenAICodexClientID = "fixture-client" })
	if _, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
		PrincipalID: human.ID, ProviderID: "codex", Kind: "openai_codex_oauth", AccessToken: "old-access", RefreshToken: "old-refresh",
	}); err != nil {
		t.Fatal(err)
	}
	initial, connection, ok, err := iam.OAuthProviderConnectionSecret(human.ID, "codex", "")
	if err != nil || !ok {
		t.Fatalf("initial connection ok=%v err=%v", ok, err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
			PrincipalID: human.ID, ProviderID: "codex", Name: connection.Name, Kind: connection.Kind, Source: connection.Source, MakeDefault: connection.IsDefault,
			AccessToken: "rotated-access", RefreshToken: "rotated-refresh", Status: "active",
		}); err != nil {
			t.Fatalf("rotate connection: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"refresh_token_reused"}`))
	}))
	defer server.Close()
	oldToken := codexauth.OAuthTokenURL
	codexauth.OAuthTokenURL = server.URL
	t.Cleanup(func() { codexauth.OAuthTokenURL = oldToken })

	auth := codexAuth{principalID: human.ID, providerID: "codex", clientID: "fixture-client"}
	if err := auth.refreshConnection(initial, connection); err == nil {
		t.Fatal("reused refresh unexpectedly succeeded")
	}
	current, _, ok, err := iam.OAuthProviderConnectionSecret(human.ID, "codex", "")
	if err != nil || !ok || current.AccessToken != "rotated-access" || current.RefreshToken != "rotated-refresh" {
		t.Fatalf("rotated connection was revoked: current=%+v ok=%v err=%v", current, ok, err)
	}
}

func TestExplicitCodexRefreshDoesNotRecreateRevokedConnection(t *testing.T) {
	setupCodexProviderTest(t)
	human, err := iam.CreatePrincipal("human", "authentik:codex-explicit-revoke", "", "Codex Explicit Revoke")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
		PrincipalID: human.ID, ProviderID: "codex", Kind: "openai_codex_oauth",
		AccessToken: "old-access", RefreshToken: "old-refresh", AccountID: "account-a",
	}); err != nil {
		t.Fatal(err)
	}
	_, connection, ok, err := iam.OAuthProviderConnectionSecret(human.ID, "codex", "")
	if err != nil || !ok {
		t.Fatalf("initial connection ok=%v err=%v", ok, err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"new-access","refresh_token":"new-refresh","account_id":"account-a"}`))
	}))
	defer server.Close()
	oldToken := codexauth.OAuthTokenURL
	codexauth.OAuthTokenURL = server.URL
	t.Cleanup(func() { codexauth.OAuthTokenURL = oldToken })

	errs := make(chan error, 1)
	go func() {
		_, _, refreshErr := RefreshCodexOAuthConnection(human.ID, "codex", "", "fixture-client")
		errs <- refreshErr
	}()
	<-started
	if err := iam.RevokeProviderConnection(human.ID, connection.ID); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-errs; err == nil {
		t.Fatal("explicit refresh unexpectedly recreated a revoked connection")
	}
	if _, _, ok, err := iam.OAuthProviderConnectionSecret(human.ID, "codex", ""); err != nil || ok {
		t.Fatalf("revoked connection became active: ok=%v err=%v", ok, err)
	}
}

func setupCodexProviderTest(t *testing.T) {
	t.Helper()
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	ResetProviders()
	oldKey := config.Get().CredentialEncryptionKey
	oldProviders := config.Get().Providers
	oldPolicies := config.Get().Policies
	t.Cleanup(func() {
		iam.ResetForTests()
		ResetProviders()
		config.Update(func(s *config.Settings) {
			s.CredentialEncryptionKey = oldKey
			s.Providers = oldProviders
			s.Policies = oldPolicies
		})
	})
	key := make([]byte, 32)
	config.Update(func(s *config.Settings) {
		s.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(key)
		s.Providers = map[string]*config.ProviderConfig{}
		s.Policies.Defaults = config.ProviderPolicy{}
		s.Policies.Overrides = map[string]config.ProviderPolicy{}
	})
}
