package providers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"

	antigravityauth "github.com/xibodev/llm-provider-auth/antigravity"
	core "github.com/xibodev/llmgw-core"
	coreproviders "github.com/xibodev/llmgw-core/providers"
)

func TestAntigravityChatIsNotWireNative(t *testing.T) {
	provider := &antigravityProvider{}
	if PreservesWireNativeSurface(provider, "model-a", core.ModelSurfaceChatCompletions) {
		t.Fatal("Antigravity Chat adaptation was certified as wire-native")
	}
}

func TestAntigravityPersistsDiscoveredProjectWithoutRefresh(t *testing.T) {
	human := setupAntigravityConnectionTest(t, "current-access", "refresh-token", 0)
	if err := persistAntigravityProject(human.ID, "antigravity", "personal", "current-access", "discovered-project"); err != nil {
		t.Fatal(err)
	}
	envelope, _, ok, err := iam.OAuthProviderConnectionSecret(human.ID, "antigravity", "personal")
	if err != nil || !ok {
		t.Fatalf("load persisted connection: ok=%v err=%v", ok, err)
	}
	if envelope.ProjectID != "discovered-project" || envelope.AccessToken != "current-access" || envelope.RefreshToken != "refresh-token" {
		t.Fatalf("envelope=%+v", envelope)
	}
}

func TestAntigravityProjectPersistenceDoesNotOverwriteConcurrentReauthorization(t *testing.T) {
	human := setupAntigravityConnectionTest(t, "old-access", "old-refresh", 0)
	if _, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
		PrincipalID: human.ID, ProviderID: "antigravity", Name: "personal", Kind: "google_antigravity_oauth",
		AccessToken: "reauthorized-access", RefreshToken: "reauthorized-refresh", Status: "active",
	}); err != nil {
		t.Fatal(err)
	}
	if err := persistAntigravityProject(human.ID, "antigravity", "personal", "old-access", "stale-project"); err != nil {
		t.Fatal(err)
	}
	envelope, _, ok, err := iam.OAuthProviderConnectionSecret(human.ID, "antigravity", "personal")
	if err != nil || !ok {
		t.Fatalf("load reauthorized connection: ok=%v err=%v", ok, err)
	}
	if envelope.AccessToken != "reauthorized-access" || envelope.RefreshToken != "reauthorized-refresh" || envelope.ProjectID != "" {
		t.Fatalf("concurrent reauthorization was overwritten: %+v", envelope)
	}
}

func TestAntigravityUnauthorizedRefreshIsDeduplicated(t *testing.T) {
	human := setupAntigravityConnectionTest(t, "rejected-access", "refresh-token", time.Now().Add(time.Hour).Unix())
	var refreshes atomic.Int32
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			refreshes.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "new-access", "expires_in": 3600})
		case "/load":
			_ = json.NewEncoder(w).Encode(map[string]any{"cloudaicompanionProject": "project"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer tokenServer.Close()
	setAntigravityOAuthTestConfig(t, tokenServer)

	start := make(chan struct{})
	errs := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for range 2 {
		go func() {
			ready.Done()
			<-start
			errs <- refreshAntigravityConnection(context.Background(), human.ID, "antigravity", "personal", "rejected-access")
		}()
	}
	ready.Wait()
	close(start)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if refreshes.Load() != 1 {
		t.Fatalf("refresh requests=%d, want 1", refreshes.Load())
	}
}

func TestAntigravityGatewayCompletionRecoversUnauthorized(t *testing.T) {
	human := setupAntigravityConnectionTest(t, "rejected-access", "refresh-token", time.Now().Add(time.Hour).Unix())
	if err := persistAntigravityProject(human.ID, "antigravity", "personal", "rejected-access", "project"); err != nil {
		t.Fatal(err)
	}
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "recovered-access", "expires_in": 3600})
		case "/load":
			_ = json.NewEncoder(w).Encode(map[string]any{"cloudaicompanionProject": "project", "paidTier": map[string]any{"id": "paid"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer tokenServer.Close()
	setAntigravityOAuthTestConfig(t, tokenServer)

	var completions atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		completions.Add(1)
		switch r.Header.Get("Authorization") {
		case "Bearer rejected-access":
			w.WriteHeader(http.StatusUnauthorized)
		case "Bearer recovered-access":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"recovered\"}]}}]}}\n\n"))
		default:
			t.Fatalf("authorization=%q", r.Header.Get("Authorization"))
		}
	}))
	defer upstream.Close()

	tokenSource := func(context.Context) (string, string, error) {
		envelope, _, ok, err := iam.OAuthProviderConnectionSecret(human.ID, "antigravity", "personal")
		if err != nil || !ok {
			return "", "", err
		}
		return envelope.AccessToken, envelope.ProjectID, nil
	}
	inner := coreproviders.NewExperimentalAntigravityProvider(tokenSource, upstream.Client(), upstream.URL)
	inner.SetUnauthorizedHandler(func(ctx context.Context, rejected string) error {
		return refreshAntigravityConnection(ctx, human.ID, "antigravity", "personal", rejected)
	})
	provider := &antigravityProvider{inner: inner, principalID: human.ID, providerID: "antigravity"}
	response, err := provider.CompleteContext(context.Background(), "model", []Message{{"role": "user", "content": "hello"}}, nil)
	if err != nil || response == nil || completions.Load() != 2 {
		t.Fatalf("response=%v err=%v completion requests=%d", response, err, completions.Load())
	}
}

func setupAntigravityConnectionTest(t *testing.T, accessToken, refreshToken string, expiresAt int64) iam.Principal {
	t.Helper()
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	config.Update(func(s *config.Settings) {
		s.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
		s.GoogleAntigravityClientID = "client-id"
		s.GoogleAntigravityClientSecret = "client-secret"
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	human, err := iam.CreatePrincipal("human", "fixture:antigravity", "", "Antigravity")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
		PrincipalID: human.ID, ProviderID: "antigravity", Name: "personal", Kind: "google_antigravity_oauth",
		AccessToken: accessToken, RefreshToken: refreshToken, ExpiresAt: expiresAt, Status: "active",
		OAuthProfile: antigravityOAuthProfileRuntimeSecret, OAuthClientID: "client-id",
	}); err != nil {
		t.Fatal(err)
	}
	return human
}

func setAntigravityOAuthTestConfig(t *testing.T, server *httptest.Server) {
	t.Helper()
	previousConfig := newGoogleAntigravityOAuthConfig
	newGoogleAntigravityOAuthConfig = func(string) antigravityauth.Config {
		return antigravityauth.Config{
			ClientID: "client-id", ClientSecret: "client-secret", ClientAuthMode: antigravityauth.ClientAuthModeClientSecretPost, HTTPClient: server.Client(),
			Endpoints: antigravityauth.Endpoints{TokenURL: server.URL + "/token", LoadCodeAssistURL: server.URL + "/load"},
		}
	}
	t.Cleanup(func() { newGoogleAntigravityOAuthConfig = previousConfig })
}

func TestAntigravityGatewayAdapterDiscoversCatalogWithoutFallbackProject(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/v1internal:loadCodeAssist":
			_ = json.NewEncoder(w).Encode(map[string]any{"cloudaicompanionProject": "discovered-project"})
		case "/v1internal:fetchAvailableModels":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["project"] != "discovered-project" {
				t.Fatalf("project=%v", body["project"])
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"models": map[string]any{"model-a": map[string]any{"displayName": "Model A"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	provider := &antigravityProvider{inner: coreproviders.NewExperimentalAntigravityProvider(func(context.Context) (string, string, error) {
		return "access-token", "", nil
	}, server.Client(), server.URL)}
	models, _, err := provider.ListModelsWithError()
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "model-a" || models[0].TypedCapabilities == nil || models[0].TypedCapabilities.Streaming != core.SupportUnsupported {
		t.Fatalf("models=%+v", models)
	}
	if len(paths) != 2 || paths[0] != "/v1internal:loadCodeAssist" {
		t.Fatalf("paths=%v", paths)
	}
}

func TestAntigravityGatewayAdvertisesAndGeneratesImagesOnlyForRosterMembers(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("image-bytes"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1internal:fetchAvailableModels":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models":                  map[string]any{"chat-model": map[string]any{}, "image-model": map[string]any{}},
				"imageGenerationModelIds": []string{"image-model", "orphan-image-model"},
			})
		case "/v1internal:streamGenerateContent":
			_, _ = fmt.Fprintf(w, "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"inlineData\":{\"mimeType\":\"image/png\",\"data\":%q}}]}}]}}\n\n", encoded)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	provider := &antigravityProvider{inner: coreproviders.NewExperimentalAntigravityProvider(func(context.Context) (string, string, error) {
		return "access-token", "project", nil
	}, server.Client(), server.URL)}
	models, _, err := provider.ListModelsWithError()
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 || len(models[0].SupportedSurfaces) != 1 || len(models[1].SupportedSurfaces) != 2 ||
		models[1].SupportedSurfaces[1] != "/v1/images/generations" || models[1].Capabilities != nil {
		t.Fatalf("models=%+v", models)
	}
	if models[1].TypedCapabilities == nil || models[1].TypedCapabilities.Operations.Image != core.SupportSupported {
		t.Fatalf("image capabilities=%+v", models[1].TypedCapabilities)
	}
	generator, ok := AsImageGenerator(provider)
	if !ok {
		t.Fatal("Antigravity adapter did not implement ImageGenerator")
	}
	contextual, ok := generator.(ContextImageGenerator)
	if !ok {
		t.Fatal("Antigravity adapter did not implement ContextImageGenerator")
	}
	images, _, err := contextual.GenerateImagesContext(context.Background(), "image-model", "draw", 1)
	if err != nil || len(images) != 1 || string(images[0].Data) != "image-bytes" || images[0].MimeType != "image/png" {
		t.Fatalf("images=%+v err=%v", images, err)
	}
}

func TestAntigravityGatewayImageFailurePreservesOnlySafeHTTPMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1internal:fetchAvailableModels":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models": map[string]any{"image-model": map[string]any{}}, "imageGenerationModelIds": []string{"image-model"},
			})
		case "/v1internal:streamGenerateContent":
			w.Header().Set("Retry-After", "17")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"private quota prose"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	provider := &antigravityProvider{inner: coreproviders.NewExperimentalAntigravityProvider(func(context.Context) (string, string, error) {
		return "access-token", "project", nil
	}, server.Client(), server.URL)}
	_, _, err := provider.GenerateImagesContext(context.Background(), "image-model", "draw", 1)
	if err == nil || UpstreamStatus(err) != http.StatusTooManyRequests || InvocationRetryAfter(err) != "17" || !InvocationRetryable(err) {
		t.Fatalf("status=%d Retry-After=%q retryable=%v err=%v", UpstreamStatus(err), InvocationRetryAfter(err), InvocationRetryable(err), err)
	}
	if strings.Contains(err.Error(), "RESOURCE_EXHAUSTED") || strings.Contains(err.Error(), "private quota prose") {
		t.Fatalf("upstream body escaped redaction boundary: %v", err)
	}
}

func TestAntigravityCatalogAcceptsProjectPersistenceFromSameOperation(t *testing.T) {
	human := setupAntigravityConnectionTest(t, "access-token", "refresh-token", time.Now().Add(time.Hour).Unix())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1internal:loadCodeAssist":
			_ = json.NewEncoder(w).Encode(map[string]any{"cloudaicompanionProject": "discovered-project"})
		case "/v1internal:fetchAvailableModels":
			_ = json.NewEncoder(w).Encode(map[string]any{"models": map[string]any{"model-a": map[string]any{"displayName": "Model A"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	principal := core.Caller{ID: human.ID, Kind: core.CallerHuman}
	provider, err := newAntigravityProvider("antigravity", principal)
	if err != nil {
		t.Fatal(err)
	}
	antigravity := provider.(*antigravityProvider)
	antigravity.inner = coreproviders.NewExperimentalAntigravityProvider(func(context.Context) (string, string, error) {
		envelope, _, ok, loadErr := iam.OAuthProviderConnectionSecret(human.ID, "antigravity", "personal")
		if loadErr != nil || !ok {
			return "", "", loadErr
		}
		return envelope.AccessToken, envelope.ProjectID, nil
	}, server.Client(), server.URL)
	antigravity.inner.SetProjectObserver(func(_ context.Context, accessToken, projectID string) error {
		return persistAntigravityProject(human.ID, "antigravity", "personal", accessToken, projectID)
	})
	cacheMu.Lock()
	cache[providerCacheKey("antigravity", principal)] = antigravity
	cacheMu.Unlock()

	models, _, err := RefreshCatalogForPrincipalWithError("antigravity", principal)
	if err != nil || len(models) != 1 || models[0].ID != "model-a" {
		t.Fatalf("models=%+v err=%v", models, err)
	}
	cached, refreshedAt := CatalogCachedForPrincipal("antigravity", principal)
	if len(cached) != 1 || cached[0].ID != "model-a" || refreshedAt.IsZero() {
		t.Fatalf("cached=%+v refreshedAt=%v", cached, refreshedAt)
	}
}

func TestAntigravityCatalogAcceptsCredentialRefreshFromSameOperation(t *testing.T) {
	human := setupAntigravityConnectionTest(t, "expired-access", "refresh-token", time.Now().Add(-time.Hour).Unix())
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "fresh-access", "expires_in": 3600})
		case "/load":
			_ = json.NewEncoder(w).Encode(map[string]any{"cloudaicompanionProject": "project"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer tokenServer.Close()
	setAntigravityOAuthTestConfig(t, tokenServer)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fresh-access" {
			t.Fatalf("authorization=%q", r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"models": map[string]any{"model-a": map[string]any{"displayName": "Model A"}}})
	}))
	defer upstream.Close()

	principal := core.Caller{ID: human.ID, Kind: core.CallerHuman}
	provider, err := newAntigravityProvider("antigravity", principal)
	if err != nil {
		t.Fatal(err)
	}
	antigravity := provider.(*antigravityProvider)
	tokenSource := func(ctx context.Context) (string, string, error) {
		envelope, connection, ok, loadErr := iam.OAuthProviderConnectionSecret(human.ID, "antigravity", "personal")
		if loadErr != nil || !ok {
			return "", "", loadErr
		}
		if envelope.ExpiresAt <= time.Now().Add(time.Minute).Unix() {
			if refreshErr := refreshAntigravityConnection(ctx, human.ID, "antigravity", connection.Name, envelope.AccessToken); refreshErr != nil {
				return "", "", refreshErr
			}
			envelope, _, ok, loadErr = iam.OAuthProviderConnectionSecret(human.ID, "antigravity", connection.Name)
			if loadErr != nil || !ok {
				return "", "", loadErr
			}
		}
		return envelope.AccessToken, envelope.ProjectID, nil
	}
	antigravity.inner = coreproviders.NewExperimentalAntigravityProvider(tokenSource, upstream.Client(), upstream.URL)
	cacheMu.Lock()
	cache[providerCacheKey("antigravity", principal)] = antigravity
	cacheMu.Unlock()

	models, _, err := RefreshCatalogForPrincipalWithError("antigravity", principal)
	if err != nil || len(models) != 1 || models[0].ID != "model-a" {
		t.Fatalf("models=%+v err=%v", models, err)
	}
	cached, refreshedAt := CatalogCachedForPrincipal("antigravity", principal)
	if len(cached) != 1 || cached[0].ID != "model-a" || refreshedAt.IsZero() {
		t.Fatalf("cached=%+v refreshedAt=%v", cached, refreshedAt)
	}
}

func TestAntigravityGatewayAdapterCompletesTypedMessages(t *testing.T) {
	var request map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1internal:streamGenerateContent" {
			http.NotFound(w, r)
			return
		}
		var envelope map[string]any
		if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil {
			t.Fatal(err)
		}
		request, _ = envelope["request"].(map[string]any)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hello\"}]},\"finishReason\":\"STOP\"}]}}\n\n"))
	}))
	defer server.Close()
	provider := &antigravityProvider{inner: coreproviders.NewExperimentalAntigravityProvider(func(context.Context) (string, string, error) {
		return "access-token", "project", nil
	}, server.Client(), server.URL)}
	response, err := provider.CompleteContext(context.Background(), "model-a", []Message{{"role": "user", "content": "hi"}}, Kwargs{
		"max_tokens": 32, "temperature": 0.2,
		"_fallback_timeout_ms": 1000, "_affinity_key": "private", "_force_api_support": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response["choices"].([]any)) != 1 {
		t.Fatalf("response=%v", response)
	}
	if request == nil {
		t.Fatal("upstream request was not captured")
	}
	encoded, _ := json.Marshal(request)
	for _, private := range []string{"_fallback_timeout_ms", "_affinity_key", "_force_api_support", "private"} {
		if strings.Contains(string(encoded), private) {
			t.Fatalf("gateway-private option reached Antigravity: %s", encoded)
		}
	}
}

func TestAntigravityRefreshUsesRevisionSafeReplacement(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/load" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"cloudaicompanionProject": "discovered-project", "paidTier": map[string]any{"id": "paid-tier"},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "new-access", "expires_in": 3600})
	}))
	defer tokenServer.Close()
	previousConfig := newGoogleAntigravityOAuthConfig
	newGoogleAntigravityOAuthConfig = func(string) antigravityauth.Config {
		return antigravityauth.Config{
			ClientID: "client-id", ClientSecret: "client-secret", ClientAuthMode: antigravityauth.ClientAuthModeClientSecretPost, HTTPClient: tokenServer.Client(),
			Endpoints: antigravityauth.Endpoints{TokenURL: tokenServer.URL + "/token", LoadCodeAssistURL: tokenServer.URL + "/load"},
		}
	}
	t.Cleanup(func() { newGoogleAntigravityOAuthConfig = previousConfig })
	config.Update(func(s *config.Settings) {
		s.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
		s.GoogleAntigravityClientID = "client-id"
		s.GoogleAntigravityClientSecret = "client-secret"
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	human, err := iam.CreatePrincipal("human", "fixture:antigravity", "", "Antigravity")
	if err != nil {
		t.Fatal(err)
	}
	_, err = iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
		PrincipalID: human.ID, ProviderID: "antigravity", Name: "personal", Kind: "google_antigravity_oauth",
		AccessToken: "old-access", RefreshToken: "refresh-token", ExpiresAt: time.Now().Add(-time.Hour).Unix(), Status: "active",
		OAuthProfile: antigravityOAuthProfileRuntimeSecret, OAuthClientID: "client-id",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	envelope, _, err := RefreshAntigravityOAuthConnection(context.Background(), human.ID, "antigravity", "personal")
	if err != nil {
		t.Fatal(err)
	}
	if envelope.AccessToken != "new-access" || envelope.RefreshToken != "refresh-token" || envelope.ProjectID != "discovered-project" {
		t.Fatalf("envelope=%+v", envelope)
	}
}

func TestAntigravityLegacyRefreshWithoutProfileFailsClosed(t *testing.T) {
	human := setupAntigravityConnectionTest(t, "old-access", "refresh-token", time.Now().Add(-time.Hour).Unix())
	current, connection, ok, err := iam.OAuthProviderConnectionSecret(human.ID, "antigravity", "personal")
	if err != nil || !ok {
		t.Fatalf("load connection: ok=%v err=%v", ok, err)
	}
	if _, err := iam.ReplaceOAuthProviderConnectionIfCurrent(connection, current, iam.OAuthConnectionCreate{
		PrincipalID: human.ID, ProviderID: "antigravity", Name: connection.Name, Kind: connection.Kind,
		Source: connection.Source, MakeDefault: connection.IsDefault, AccessToken: current.AccessToken,
		RefreshToken: current.RefreshToken, ExpiresAt: current.ExpiresAt, Status: "active",
	}); err != nil {
		t.Fatal(err)
	}
	err = refreshAntigravityConnection(context.Background(), human.ID, "antigravity", "personal", "old-access")
	if err == nil || !strings.Contains(err.Error(), "reauthorize") {
		t.Fatalf("legacy refresh error=%v", err)
	}
}

func TestAntigravityPublicPKCERefreshUsesPersistedProfile(t *testing.T) {
	human := setupAntigravityConnectionTest(t, "old-access", "refresh-token", time.Now().Add(-time.Hour).Unix())
	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{
			"antigravity": {Type: "google_antigravity", PublicOAuthClientID: "public-client"},
		}
	})
	current, connection, ok, err := iam.OAuthProviderConnectionSecret(human.ID, "antigravity", "personal")
	if err != nil || !ok {
		t.Fatalf("load connection: ok=%v err=%v", ok, err)
	}
	if _, err := iam.ReplaceOAuthProviderConnectionIfCurrent(connection, current, iam.OAuthConnectionCreate{
		PrincipalID: human.ID, ProviderID: "antigravity", Name: connection.Name, Kind: connection.Kind,
		Source: connection.Source, MakeDefault: connection.IsDefault, AccessToken: current.AccessToken,
		RefreshToken: current.RefreshToken, ExpiresAt: current.ExpiresAt, Status: "active",
		OAuthProfile: antigravityOAuthProfilePublicPKCE, OAuthClientID: "public-client",
	}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("client_id") != "public-client" || r.Form.Has("client_secret") {
			t.Fatalf("refresh form=%v", r.Form)
		}
		_, _ = w.Write([]byte(`{"access_token":"new-access","expires_in":3600}`))
	}))
	defer server.Close()
	oldFactory := newGoogleAntigravityOAuthConfig
	newGoogleAntigravityOAuthConfig = func(string) antigravityauth.Config {
		return antigravityauth.Config{Endpoints: antigravityauth.Endpoints{TokenURL: server.URL}, HTTPClient: server.Client()}
	}
	t.Cleanup(func() { newGoogleAntigravityOAuthConfig = oldFactory })
	if err := refreshAntigravityConnection(context.Background(), human.ID, "antigravity", "personal", "old-access"); err != nil {
		t.Fatal(err)
	}
	refreshed, _, ok, err := iam.OAuthProviderConnectionSecret(human.ID, "antigravity", "personal")
	if err != nil || !ok || refreshed.AccessToken != "new-access" || refreshed.OAuthProfile != antigravityOAuthProfilePublicPKCE || refreshed.OAuthClientID != "public-client" {
		t.Fatalf("refreshed=%+v ok=%v err=%v", refreshed, ok, err)
	}
}

func TestAntigravityConsumerManualRefreshUsesBoundClient(t *testing.T) {
	envelope := iam.OAuthTokenEnvelope{
		OAuthProfile:  antigravityOAuthProfileConsumerManual,
		OAuthClientID: "fixture-client", OAuthClientSecret: "fixture-secret",
		OAuthClientMode: "confidential", OAuthRedirectURI: "https://callback.example.test/oauth",
	}
	oauth, err := antigravityOAuthConfigForEnvelope("antigravity", envelope)
	if err != nil {
		t.Fatal(err)
	}
	if oauth.ClientID != envelope.OAuthClientID || oauth.ClientSecret != envelope.OAuthClientSecret ||
		oauth.RedirectURI != envelope.OAuthRedirectURI || oauth.ClientAuthMode != antigravityauth.ClientAuthModeClientSecretPost {
		t.Fatalf("manual refresh config=%+v", oauth)
	}

	envelope.OAuthClientMode = "public"
	envelope.OAuthClientSecret = ""
	oauth, err = antigravityOAuthConfigForEnvelope("antigravity", envelope)
	if err != nil || oauth.ClientAuthMode != antigravityauth.ClientAuthModePublicPKCE || oauth.ClientSecret != "" {
		t.Fatalf("public manual refresh config=%+v err=%v", oauth, err)
	}
}

func TestAntigravityRuntimeRefreshUsesClientSecretPost(t *testing.T) {
	human := setupAntigravityConnectionTest(t, "old-access", "refresh-token", time.Now().Add(-time.Hour).Unix())
	current, connection, ok, err := iam.OAuthProviderConnectionSecret(human.ID, "antigravity", "personal")
	if err != nil || !ok {
		t.Fatalf("load connection: ok=%v err=%v", ok, err)
	}
	if _, err := iam.ReplaceOAuthProviderConnectionIfCurrent(connection, current, iam.OAuthConnectionCreate{
		PrincipalID: human.ID, ProviderID: "antigravity", Name: connection.Name, Kind: connection.Kind,
		Source: connection.Source, MakeDefault: connection.IsDefault, AccessToken: current.AccessToken,
		RefreshToken: current.RefreshToken, ExpiresAt: current.ExpiresAt, Status: "active",
		OAuthProfile: antigravityOAuthProfileRuntimeSecret, OAuthClientID: "client-id",
	}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("client_id") != "client-id" || r.Form.Get("client_secret") != "client-secret" {
			t.Fatalf("refresh form=%v", r.Form)
		}
		_, _ = w.Write([]byte(`{"access_token":"new-access","expires_in":3600}`))
	}))
	defer server.Close()
	oldFactory := newGoogleAntigravityOAuthConfig
	newGoogleAntigravityOAuthConfig = func(string) antigravityauth.Config {
		return antigravityauth.Config{
			ClientID: "client-id", ClientSecret: "client-secret", ClientAuthMode: antigravityauth.ClientAuthModeClientSecretPost,
			Endpoints: antigravityauth.Endpoints{TokenURL: server.URL}, HTTPClient: server.Client(),
		}
	}
	t.Cleanup(func() { newGoogleAntigravityOAuthConfig = oldFactory })
	if err := refreshAntigravityConnection(context.Background(), human.ID, "antigravity", "personal", "old-access"); err != nil {
		t.Fatal(err)
	}
}
