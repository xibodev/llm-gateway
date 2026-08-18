package providers

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/gcpauth"
	"llmgw/internal/iam"
)

// serviceAccountFixture builds a service-account key whose token endpoint is a
// local stub, so the whole stored-credential path can run without a real key.
func serviceAccountFixture(t *testing.T, tokenURI string) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	raw, err := json.Marshal(map[string]string{
		"type":           "service_account",
		"project_id":     "fixture-project",
		"private_key_id": "key-1",
		"private_key":    string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"client_email":   "svc@fixture-project.iam.gserviceaccount.com",
		"token_uri":      tokenURI,
	})
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return string(raw)
}

func stubTokenEndpoint(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"ya29.stored-path","expires_in":3600}`))
	}))
	t.Cleanup(server.Close)
	return server
}

func setupVertexIAM(t *testing.T) {
	t.Helper()
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	ResetProviders()
	gcpauth.ResetCache()
	t.Cleanup(func() {
		iam.ResetForTests()
		ResetProviders()
		gcpauth.ResetCache()
	})
	key := make([]byte, 32)
	for index := range key {
		key[index] = byte(index + 7)
	}
	config.Update(func(s *config.Settings) {
		s.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(key)
		s.Providers = map[string]*config.ProviderConfig{
			"vertex_ai": {Type: "vertex_ai", Location: "global"},
		}
		s.Policies.Defaults = config.ProviderPolicy{}
		s.Policies.Overrides = map[string]config.ProviderPolicy{}
	})
}

// TestVertexUsesStoredServiceAccountConnection covers the seam between storage
// and the provider: a key that was uploaded, encrypted and read back must end
// up authenticating as a Bearer token. Unit tests exercise minting and the
// header separately; only this test proves they meet.
func TestVertexUsesStoredServiceAccountConnection(t *testing.T) {
	setupVertexIAM(t)
	server := stubTokenEndpoint(t)

	human, err := iam.CreatePrincipal("human", "authentik:vertex-owner", "", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := iam.PutProviderConnection(iam.ProviderConnectionCreate{
		PrincipalID: human.ID, ProviderID: "vertex_ai", Name: "personal",
		Kind: gcpauth.CredentialKind, Secret: serviceAccountFixture(t, server.URL),
		Source: iam.ConnectionSourceUser, MakeDefault: true,
	}); err != nil {
		t.Fatalf("store service account connection: %v", err)
	}

	provider, err := GetProviderForPrincipal(
		"vertex_ai", &config.Principal{PrincipalID: human.ID, PrincipalKind: human.Kind},
	)
	if err != nil {
		t.Fatalf("build provider from stored connection: %v", err)
	}
	vertex, ok := provider.(GoogleAIProvider)
	if !ok {
		t.Fatalf("provider type=%T", provider)
	}
	if vertex.bearerToken != "ya29.stored-path" {
		t.Fatalf("bearerToken=%q, want the token minted from the stored key", vertex.bearerToken)
	}
	if vertex.apiKey != "" {
		t.Fatalf("apiKey=%q, want empty so no x-goog-api-key is sent", vertex.apiKey)
	}
	// The project must come from the key when none is configured.
	if vertex.project != "fixture-project" {
		t.Fatalf("project=%q, want it taken from the key", vertex.project)
	}
}

// TestVertexWithoutAnyCredentialFailsClearly is the regression test for the
// confusing failure this reproduces: with a project and location configured but
// no credential stored, the gateway used to build an unauthenticated provider
// and call Vertex anyway. Google answered 401, which surfaced as
// "credential rejected" -- language that points at a bad key rather than at a
// missing one, and sends you looking in the wrong place.
func TestVertexWithoutAnyCredentialFailsClearly(t *testing.T) {
	setupVertexIAM(t)
	config.Update(func(s *config.Settings) {
		s.Providers["vertex_ai"].Project = "fixture-project"
	})

	human, err := iam.CreatePrincipal("human", "authentik:vertex-none", "", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	_, err = GetProviderForPrincipal(
		"vertex_ai", &config.Principal{PrincipalID: human.ID, PrincipalKind: human.Kind},
	)
	if err == nil {
		t.Fatal("expected an error when no credential is configured")
	}
	for _, want := range []string{"no credential", "vertex_ai"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q should mention %q", err.Error(), want)
		}
	}
}
