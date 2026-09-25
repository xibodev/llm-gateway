package providers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"

	antigravityauth "github.com/xibodev/llm-provider-auth/antigravity"
	core "github.com/xibodev/llmgw-core"
	coreproviders "github.com/xibodev/llmgw-core/providers"
)

const antigravityFixtureCompletion = "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hello\"}]},\"finishReason\":\"STOP\"}]}}\n\n"

type antigravityCall struct{ path, authorization, body string }

// antigravityUpstream is a synthetic Cloud Code Assist that also serves
// Google's token endpoint at /token and account discovery at /load. It
// records every call and answers with handle, or else as a healthy upstream
// that accepts every token; see antigravityAnswer.
type antigravityUpstream struct {
	mu     sync.Mutex
	calls  []antigravityCall
	handle func(w http.ResponseWriter, r *http.Request) bool
}

func (u *antigravityUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	u.mu.Lock()
	u.calls = append(u.calls, antigravityCall{r.URL.Path, r.Header.Get("Authorization"), string(body)})
	u.mu.Unlock()
	if u.handle == nil || !u.handle(w, r) {
		antigravityAnswer(w, r)
	}
}

func (u *antigravityUpstream) recorded() []antigravityCall {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]antigravityCall(nil), u.calls...)
}

// to returns the calls that reached path.
func (u *antigravityUpstream) to(path string) []antigravityCall {
	var calls []antigravityCall
	for _, call := range u.recorded() {
		if call.path == path {
			calls = append(calls, call)
		}
	}
	return calls
}

func (u *antigravityUpstream) count(path string) int { return len(u.to(path)) }

func antigravityAnswer(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/token":
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "refreshed-access", "expires_in": 3600})
	case "/load", "/v1internal:loadCodeAssist":
		_ = json.NewEncoder(w).Encode(map[string]any{"cloudaicompanionProject": "discovered-project", "paidTier": map[string]any{"id": "paid"}})
	case "/v1internal:fetchAvailableModels":
		_ = json.NewEncoder(w).Encode(map[string]any{"models": map[string]any{"model-a": map[string]any{"displayName": "Model A"}}})
	case "/v1internal:streamGenerateContent":
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, antigravityFixtureCompletion)
	default:
		http.NotFound(w, r)
	}
}

// setupAntigravityRuntimeTest configures the installed Runtime's antigravity
// instance against upstream, for Cloud Code Assist and Google's OAuth
// endpoints, stores the connection setupAntigravityConnectionTest describes
// for a new human owner and returns the owner's facade.
func setupAntigravityRuntimeTest(
	t *testing.T, upstream http.Handler, accessToken, refreshToken, projectID string, expiresAt int64,
) (*antigravityProvider, iam.Principal) {
	t.Helper()
	human := setupAntigravityConnectionTest(t, accessToken, refreshToken, projectID, expiresAt)
	configureAntigravityInstance(t, "")
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	setAntigravityOAuthTestConfig(t, server)
	setAntigravityEndpointForTests(t, antigravityEndpoint{BaseURL: server.URL, HTTPClient: server.Client()})
	return antigravityFacade(t, human), human
}

func antigravityFacade(t *testing.T, owner iam.Principal) *antigravityProvider {
	t.Helper()
	provider, err := Current().newAntigravityProvider("antigravity", core.Caller{ID: owner.ID, Kind: core.CallerHuman})
	if err != nil {
		t.Fatal(err)
	}
	return provider.(*antigravityProvider)
}

// configureAntigravityInstance configures the antigravity instance, with
// publicClientID as its public OAuth client, until the test ends.
func configureAntigravityInstance(t *testing.T, publicClientID string) {
	t.Helper()
	old := config.Get().Providers
	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{
			"antigravity": {Type: "google_antigravity", PublicOAuthClientID: publicClientID},
		}
	})
	ResetProviders()
	t.Cleanup(func() {
		config.Update(func(s *config.Settings) { s.Providers = old })
		ResetProviders()
	})
}

// setAntigravityEndpointForTests points the installed Runtime's Cloud Code
// Assist calls at endpoint until the test ends. The core Runtime builds its
// Antigravity once per settings generation, so both swaps publish the
// settings again, unchanged, to have it rebuild.
func setAntigravityEndpointForTests(t *testing.T, endpoint antigravityEndpoint) {
	t.Helper()
	runtime := Current()
	previous := runtime.antigravityEndpoint.swap(endpoint)
	config.Update(func(*config.Settings) {})
	t.Cleanup(func() {
		runtime.antigravityEndpoint.swap(previous)
		config.Update(func(*config.Settings) {})
	})
}

// setupAntigravityConnectionTest stores a personal connection of a new human
// owner, granted to the runtime client, with the tokens, project and expiry
// given. An expiry of zero never expires.
func setupAntigravityConnectionTest(t *testing.T, accessToken, refreshToken, projectID string, expiresAt int64) iam.Principal {
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
	putAntigravityConnection(t, iam.OAuthConnectionCreate{
		PrincipalID: human.ID, AccessToken: accessToken, RefreshToken: refreshToken, ExpiresAt: expiresAt,
		ProjectID: projectID, OAuthProfile: antigravityOAuthProfileRuntimeSecret, OAuthClientID: "client-id",
	})
	return human
}

// putAntigravityConnection stores input as the owner's personal connection,
// as a sign-in does.
func putAntigravityConnection(t *testing.T, input iam.OAuthConnectionCreate) iam.ProviderConnection {
	t.Helper()
	input.ProviderID, input.Name, input.Kind, input.Status = "antigravity", "personal", "google_antigravity_oauth", "active"
	connection, err := iam.PutOAuthProviderConnection(input)
	if err != nil {
		t.Fatal(err)
	}
	return connection
}

func storedAntigravityConnection(t *testing.T, owner iam.Principal) (iam.OAuthTokenEnvelope, iam.ProviderConnection) {
	t.Helper()
	envelope, connection, ok, err := iam.OAuthProviderConnectionSecret(owner.ID, "antigravity", "personal")
	if err != nil || !ok {
		t.Fatalf("load connection: ok=%v err=%v", ok, err)
	}
	return envelope, connection
}

func setAntigravityOAuthTestConfig(t *testing.T, server *httptest.Server) {
	t.Helper()
	replaceAntigravityOAuthConfig(t, func(string) antigravityauth.Config {
		return antigravityauth.Config{
			ClientID: "client-id", ClientSecret: "client-secret", ClientAuthMode: antigravityauth.ClientAuthModeClientSecretPost, HTTPClient: server.Client(),
			Endpoints: antigravityauth.Endpoints{TokenURL: server.URL + "/token", LoadCodeAssistURL: server.URL + "/load"},
		}
	})
}

// replaceAntigravityOAuthConfig replaces the installed Runtime's settings
// OAuth client until the test ends. The core Runtime builds its refresh once
// per settings generation, so both swaps publish the settings again.
func replaceAntigravityOAuthConfig(t *testing.T, build func(string) antigravityauth.Config) {
	t.Helper()
	runtime := Current()
	previous := runtime.antigravityOAuth.swap(build)
	config.Update(func(*config.Settings) {})
	t.Cleanup(func() {
		runtime.antigravityOAuth.swap(previous)
		config.Update(func(*config.Settings) {})
	})
}

func antigravityBodyField(t *testing.T, body, field string) any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return decoded[field]
}

// antigravityImages answers the catalog with a chat model, an image model and
// an image roster naming a model the catalog lacks, and a generation with one
// image of data.
func antigravityImages(data string) func(http.ResponseWriter, *http.Request) bool {
	encoded := base64.StdEncoding.EncodeToString([]byte(data))
	return func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/v1internal:fetchAvailableModels":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models":                  map[string]any{"chat-model": map[string]any{}, "image-model": map[string]any{}},
				"imageGenerationModelIds": []string{"image-model", "orphan-image-model"},
			})
		case "/v1internal:streamGenerateContent":
			_, _ = fmt.Fprintf(w, "data: {\"response\":{\"candidates\":[{\"content\":{\"parts\":[{\"inlineData\":{\"mimeType\":\"image/png\",\"data\":%q}}]}}]}}\n\n", encoded)
		default:
			return false
		}
		return true
	}
}

func TestAntigravityChatIsNotWireNative(t *testing.T) {
	provider := &antigravityProvider{}
	if PreservesWireNativeSurface(provider, "model-a", core.ModelSurfaceChatCompletions) {
		t.Fatal("Antigravity Chat adaptation was certified as wire-native")
	}
}

// The gateway writes the OAuth profiles it signs in with, and core's refresh
// reads them.
func TestAntigravityOAuthProfilesAreCores(t *testing.T) {
	if antigravityOAuthProfileRuntimeSecret != coreproviders.AntigravityOAuthProfileRuntimeSecret ||
		antigravityOAuthProfilePublicPKCE != coreproviders.AntigravityOAuthProfilePublicPKCE ||
		antigravityOAuthProfileConsumerManual != coreproviders.AntigravityOAuthProfileConsumerManual {
		t.Fatal("the gateway's Antigravity OAuth profiles differ from core's")
	}
}

// The catalog discovers the project of a connection that names none rather
// than inventing one, stores it, and reports the connection at the revision
// that write left, so a probe can rebase onto it.
func TestAntigravityGatewayAdapterDiscoversCatalogWithoutFallbackProject(t *testing.T) {
	upstream := &antigravityUpstream{}
	provider, owner := setupAntigravityRuntimeTest(t, upstream, "access-token", "refresh-token", "", 0)
	models, observation, err := provider.ListModelsWithError()
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "model-a" || models[0].TypedCapabilities == nil || models[0].TypedCapabilities.Streaming != core.SupportUnsupported {
		t.Fatalf("models=%+v", models)
	}
	calls := upstream.recorded()
	if len(calls) != 2 || calls[0].path != "/v1internal:loadCodeAssist" || antigravityBodyField(t, calls[1].body, "project") != "discovered-project" {
		t.Fatalf("calls=%+v", calls)
	}
	current, found, err := iam.ActiveProviderAccountObservation(owner.ID, "antigravity")
	if err != nil || !found || observation == nil || *observation != *credentialObservation(&current) {
		t.Fatalf("observation=%+v current=%+v err=%v", observation, current, err)
	}
	if envelope, _ := storedAntigravityConnection(t, owner); envelope.ProjectID != "discovered-project" {
		t.Fatalf("project=%q", envelope.ProjectID)
	}
}

func TestAntigravityGatewayAdvertisesAndGeneratesImagesOnlyForRosterMembers(t *testing.T) {
	provider, _ := setupAntigravityRuntimeTest(t, &antigravityUpstream{handle: antigravityImages("image-bytes")}, "access-token", "refresh-token", "project", 0)
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
	if _, _, err := contextual.GenerateImagesContext(context.Background(), "orphan-image-model", "draw", 1); UpstreamStatus(err) != http.StatusBadRequest {
		t.Fatalf("a model outside the root roster generated: err=%v", err)
	}
}

func TestAntigravityGatewayImageFailurePreservesOnlySafeHTTPMetadata(t *testing.T) {
	upstream := &antigravityUpstream{handle: func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/v1internal:streamGenerateContent" {
			return antigravityImages("")(w, r)
		}
		w.Header().Set("Retry-After", "17")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"private quota prose"}}`))
		return true
	}}
	provider, _ := setupAntigravityRuntimeTest(t, upstream, "access-token", "refresh-token", "project", 0)
	_, _, err := provider.GenerateImagesContext(context.Background(), "image-model", "draw", 1)
	if err == nil || UpstreamStatus(err) != http.StatusTooManyRequests || InvocationRetryAfter(err) != "17" || !InvocationRetryable(err) {
		t.Fatalf("status=%d Retry-After=%q retryable=%v err=%v", UpstreamStatus(err), InvocationRetryAfter(err), InvocationRetryable(err), err)
	}
	if strings.Contains(err.Error(), "RESOURCE_EXHAUSTED") || strings.Contains(err.Error(), "private quota prose") {
		t.Fatalf("upstream body escaped redaction boundary: %v", err)
	}
}

// Image generation, which the core Runtime does not serve, recovers from a
// rejected token as the Runtime does: one refresh, then the same request.
// When that refresh fails, the failed recovery is reported without a status.
func TestAntigravityImageGenerationRefreshesARejectedToken(t *testing.T) {
	serve := antigravityImages("image-bytes")
	reject := rejectAntigravityTokensBut("refreshed-access")
	upstream := &antigravityUpstream{handle: func(w http.ResponseWriter, r *http.Request) bool {
		return reject(w, r) || serve(w, r)
	}}
	provider, owner := setupAntigravityRuntimeTest(t, upstream, "rejected-access", "refresh-token", "project", time.Now().Add(time.Hour).Unix())
	images, _, err := provider.GenerateImagesContext(context.Background(), "image-model", "draw", 1)
	if err != nil || len(images) != 1 || string(images[0].Data) != "image-bytes" {
		t.Fatalf("images=%+v err=%v", images, err)
	}
	want := []antigravityCall{
		{path: "/v1internal:fetchAvailableModels", authorization: "Bearer rejected-access"},
		{path: "/token"},
		{path: "/load", authorization: "Bearer refreshed-access"},
		{path: "/v1internal:fetchAvailableModels", authorization: "Bearer refreshed-access"},
		{path: "/v1internal:streamGenerateContent", authorization: "Bearer refreshed-access"},
	}
	if calls := antigravityCallsWithoutBodies(upstream.recorded()); !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls=%+v, want %+v", calls, want)
	}

	// A sign-in replaces the refreshed token, and its grant is refused.
	putAntigravityConnection(t, iam.OAuthConnectionCreate{
		PrincipalID: owner.ID, AccessToken: "rejected-access", RefreshToken: "refused-refresh", ProjectID: "project",
		OAuthProfile: antigravityOAuthProfileRuntimeSecret, OAuthClientID: "other-client",
	})
	if _, _, err := provider.GenerateImagesContext(context.Background(), "image-model", "draw", 1); !IsInvocation(err) || UpstreamStatus(err) != 0 {
		t.Fatalf("err=%v status=%d", err, UpstreamStatus(err))
	}
}

// An image request Antigravity cannot serve is refused with its 400 before
// any credential is resolved or refreshed, as the Antigravity path refused
// it, so a caller without a connection still learns what is wrong.
func TestAntigravityImageRequestIsRefusedBeforeItsCredential(t *testing.T) {
	upstream := &antigravityUpstream{handle: func(w http.ResponseWriter, r *http.Request) bool {
		t.Errorf("an image request Antigravity cannot serve reached %s", r.URL.Path)
		return false
	}}
	setupAntigravityRuntimeTest(t, upstream, "owner-access", "refresh-token", "project", 0)
	stranger, err := iam.CreatePrincipal("human", "fixture:antigravity-stranger", "", "Stranger")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = antigravityFacade(t, stranger).GenerateImagesContext(context.Background(), "image-model", "draw", 2)
	if !IsInvocation(err) || UpstreamStatus(err) != http.StatusBadRequest {
		t.Fatalf("err=%v status=%d", err, UpstreamStatus(err))
	}
}

func TestAntigravityCatalogAcceptsProjectPersistenceFromSameOperation(t *testing.T) {
	_, owner := setupAntigravityRuntimeTest(t, &antigravityUpstream{}, "access-token", "refresh-token", "", time.Now().Add(time.Hour).Unix())
	principal := core.Caller{ID: owner.ID, Kind: core.CallerHuman}
	models, _, err := RefreshCatalogForPrincipalWithError("antigravity", principal)
	if err != nil || len(models) != 1 || models[0].ID != "model-a" {
		t.Fatalf("models=%+v err=%v", models, err)
	}
	cached, refreshedAt := CatalogCachedForPrincipal("antigravity", principal)
	if len(cached) != 1 || cached[0].ID != "model-a" || refreshedAt.IsZero() {
		t.Fatalf("cached=%+v refreshedAt=%v", cached, refreshedAt)
	}
	if envelope, _ := storedAntigravityConnection(t, owner); envelope.ProjectID != "discovered-project" {
		t.Fatalf("project=%q", envelope.ProjectID)
	}
}

func TestAntigravityCatalogAcceptsCredentialRefreshFromSameOperation(t *testing.T) {
	upstream := &antigravityUpstream{handle: func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasPrefix(r.URL.Path, "/v1internal:") && r.Header.Get("Authorization") != "Bearer refreshed-access" {
			t.Errorf("authorization=%q", r.Header.Get("Authorization"))
		}
		return false
	}}
	_, owner := setupAntigravityRuntimeTest(t, upstream, "expired-access", "refresh-token", "", time.Now().Add(-time.Hour).Unix())
	principal := core.Caller{ID: owner.ID, Kind: core.CallerHuman}
	models, observation, err := RefreshCatalogForPrincipalWithError("antigravity", principal)
	if err != nil || len(models) != 1 || models[0].ID != "model-a" || observation == nil {
		t.Fatalf("models=%+v observation=%+v err=%v", models, observation, err)
	}
	cached, refreshedAt := CatalogCachedForPrincipal("antigravity", principal)
	if len(cached) != 1 || cached[0].ID != "model-a" || refreshedAt.IsZero() {
		t.Fatalf("cached=%+v refreshedAt=%v", cached, refreshedAt)
	}
	if upstream.count("/token") != 1 || upstream.count("/v1internal:loadCodeAssist") != 0 {
		t.Fatalf("calls=%+v", upstream.recorded())
	}
}

// One catalog operation refreshes the expired token, whose account discovery
// fails, and then stores the project it discovers itself: two writes to the
// connection. Neither turns the catalog it fetched into stale work.
func TestAntigravityCatalogAcceptsRefreshAndProjectFromSameOperation(t *testing.T) {
	upstream := &antigravityUpstream{handle: func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/load" {
			return false
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		return true
	}}
	_, owner := setupAntigravityRuntimeTest(t, upstream, "expired-access", "refresh-token", "", time.Now().Add(-time.Hour).Unix())
	principal := core.Caller{ID: owner.ID, Kind: core.CallerHuman}
	if models, _, err := RefreshCatalogForPrincipalWithError("antigravity", principal); err != nil || len(models) != 1 {
		t.Fatalf("models=%+v err=%v", models, err)
	}
	if cached, _ := CatalogCachedForPrincipal("antigravity", principal); len(cached) != 1 {
		t.Fatalf("cached=%+v", cached)
	}
	envelope, _ := storedAntigravityConnection(t, owner)
	if envelope.AccessToken != "refreshed-access" || envelope.ProjectID != "discovered-project" || upstream.count("/v1internal:loadCodeAssist") != 1 {
		t.Fatalf("project=%q calls=%+v", envelope.ProjectID, upstream.recorded())
	}
}
