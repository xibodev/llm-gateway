package api

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/gcpauth"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
	"llmgw/internal/router"
)

func serviceAccountJSONFixture(t *testing.T) string {
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
		"token_uri":      "https://oauth2.googleapis.com/token",
	})
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return string(raw)
}

// TestAdminStoresServiceAccountWithItsOwnKind reproduces the operator flow that
// actually configures Vertex: the provider dialog posts one credential field.
// A service account key pasted there was stored as an API key, so the provider
// sent the whole JSON document as x-goog-api-key. Google answered 401, which
// surfaces as "credential rejected" -- a message about a bad key, when the real
// problem is that the service-account path was never taken.
//
// This matters most for API-key callers: a request authenticated with
// LLMGW_API_KEY carries no principal id, so only the system connection created
// here is consulted, and a personal connection cannot stand in for it.
func TestAdminStoresServiceAccountWithItsOwnKind(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	router.ResetSavingsState()
	router.ResetTelemetryState()
	t.Cleanup(func() {
		iam.ResetForTests()
		router.ResetSavingsState()
		router.ResetTelemetryState()
		providers.ResetProviders()
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	encryptionKey := make([]byte, 32)
	for index := range encryptionKey {
		encryptionKey[index] = byte(index + 3)
	}
	config.Update(func(s *config.Settings) {
		s.APIKey = "admin-secret"
		s.AllowUnauthenticatedAPI = false
		s.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(encryptionKey)
		s.Providers = map[string]*config.ProviderConfig{}
		s.Categories = map[string]*config.CategoryConfig{}
	})
	providers.ResetProviders()

	server := httptest.NewServer(NewServer())
	defer server.Close()

	status, created := jsonRequest(t, server.URL+"/admin/api/providers", http.MethodPost, "admin-secret", map[string]any{
		"registry_id": "vertex_ai",
		"api_key":     serviceAccountJSONFixture(t),
		"project":     "fixture-project",
		"location":    "global",
	})
	if status != http.StatusOK {
		t.Fatalf("create vertex provider: %d %+v", status, created)
	}

	_, connection, ok, err := iam.SystemProviderConnectionSecret("vertex_ai")
	if err != nil {
		t.Fatalf("read system connection: %v", err)
	}
	if !ok {
		t.Fatal("no system connection was stored")
	}
	if connection.Kind != gcpauth.CredentialKind {
		t.Fatalf("stored credential_kind=%q, want %q -- a service account key stored "+
			"as an API key is sent as x-goog-api-key and rejected by Google",
			connection.Kind, gcpauth.CredentialKind)
	}
}

// TestAdminRejectsServiceAccountForNonGoogleProvider keeps the detection honest:
// a service account key is only meaningful for Vertex, and silently storing one
// elsewhere would swap the credential kind under a provider that cannot use it.
func TestAdminRejectsServiceAccountForNonGoogleProvider(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	router.ResetSavingsState()
	router.ResetTelemetryState()
	t.Cleanup(func() {
		iam.ResetForTests()
		router.ResetSavingsState()
		router.ResetTelemetryState()
		providers.ResetProviders()
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	encryptionKey := make([]byte, 32)
	config.Update(func(s *config.Settings) {
		s.APIKey = "admin-secret"
		s.AllowUnauthenticatedAPI = false
		s.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(encryptionKey)
		s.Providers = map[string]*config.ProviderConfig{}
		s.Categories = map[string]*config.CategoryConfig{}
	})
	providers.ResetProviders()

	server := httptest.NewServer(NewServer())
	defer server.Close()

	status, body := jsonRequest(t, server.URL+"/admin/api/providers", http.MethodPost, "admin-secret", map[string]any{
		"registry_id": "gemini",
		"api_key":     serviceAccountJSONFixture(t),
	})
	if status == http.StatusOK {
		t.Fatalf("expected a service account key to be refused for a non-Vertex provider, got %d %+v", status, body)
	}
}
