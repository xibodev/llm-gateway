package api

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	llmgwproviders "llmgw/internal/providers"
	"llmgw/internal/router"
)

// credentialTestEnv is a running admin API server with one human principal,
// ready to accept a provider connection.
type credentialTestEnv struct {
	server      *httptest.Server
	principalID string
}

// newCredentialTestEnv wires up the same admin server the rest of this
// package tests against, plus a vertex_ai provider the upload tests attach a
// service-account key to. It also configures credential encryption, which
// PutProviderConnection requires before it will store any secret.
func newCredentialTestEnv(t *testing.T) credentialTestEnv {
	t.Helper()
	return newCredentialTestEnvWith(t, map[string]*config.ProviderConfig{
		"vertex-prod": {
			Type: "vertex_ai", RegistryID: "vertex_ai", Project: "fixture-project",
		},
	})
}

// newCredentialTestEnvWith is newCredentialTestEnv with a caller-supplied
// provider set, for tests that need a provider shape newCredentialTestEnv's
// fixed vertex-prod entry doesn't cover (e.g. a registry-less custom provider).
func newCredentialTestEnvWith(t *testing.T, providers map[string]*config.ProviderConfig) credentialTestEnv {
	t.Helper()
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
	encryptionKey := make([]byte, 32)
	config.Update(func(s *config.Settings) {
		s.APIKey = "admin-secret"
		s.AllowUnauthenticatedAPI = false
		s.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(encryptionKey)
		s.Providers = providers
		s.Endpoints = map[string]*config.EndpointConfig{}
	})
	llmgwproviders.ResetProviders()
	t.Cleanup(llmgwproviders.ResetProviders)

	principal, err := iam.CreatePrincipal("human", "authentik:credential-upload", "", "Credential Upload")
	if err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(NewServer())
	return credentialTestEnv{server: server, principalID: principal.ID}
}

// syntheticServiceAccountKey generates a throwaway RSA key in-process and
// wraps it in a service-account JSON document. This is never a real
// credential -- mirrors internal/gcpauth/gcpauth_test.go so the suite needs
// no fixture and stays safe to run in a public repository's CI.
func syntheticServiceAccountKey(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	doc := map[string]string{
		"type":           "service_account",
		"project_id":     "fixture-project",
		"private_key_id": "key-1",
		"private_key":    pemText,
		"client_email":   "svc@fixture-project.iam.gserviceaccount.com",
		"token_uri":      "https://oauth2.googleapis.com/token",
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return string(raw)
}

// multipartRequest builds a multipart body with the given fields, where a field
// named with a leading "@" is sent as a file part rather than a value part.
func multipartRequest(t *testing.T, fields map[string]string) (string, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for name, value := range fields {
		if len(name) > 0 && name[0] == '@' {
			fw, err := mw.CreateFormFile(name[1:], "credential.json")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := fw.Write([]byte(value)); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := mw.WriteField(name, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return mw.FormDataContentType(), &buf
}

func postMultipart(t *testing.T, url, token string, fields map[string]string) (int, map[string]any) {
	t.Helper()
	contentType, body := multipartRequest(t, fields)
	req, err := http.NewRequest(http.MethodPost, url, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return doJSONRequest(t, req)
}

// doJSONRequest performs a prebuilt request and decodes its JSON body.
func doJSONRequest(t *testing.T, req *http.Request) (int, map[string]any) {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// containsAll reports whether haystack contains every needle.
func containsAll(haystack string, needles ...string) bool {
	for _, needle := range needles {
		if !strings.Contains(haystack, needle) {
			return false
		}
	}
	return true
}

// stringifyAny renders any decoded JSON value (including nested maps) as text
// so a test can scan a whole response body for a value that must not appear.
func stringifyAny(v any) string {
	return fmt.Sprint(v)
}

// A service-account key uploaded as a file part must produce exactly the same
// stored connection as the same key sent as a JSON string. The whole point of
// this route is that a multi-line PEM-bearing document survives the trip.
func TestConnectionAcceptsServiceAccountAsFileUpload(t *testing.T) {
	env := newCredentialTestEnv(t)
	defer env.server.Close()

	key := syntheticServiceAccountKey(t)
	status, created := postMultipart(t, env.server.URL+"/admin/api/principals/"+env.principalID+"/connections",
		"admin-secret", map[string]string{
			"provider_id":     "vertex-prod",
			"credential_kind": "gcp_service_account",
			"connection_name": "uploaded",
			"@secret":         key,
		})
	if status != http.StatusCreated {
		t.Fatalf("multipart upload status=%d body=%+v", status, created)
	}
	connection, _ := created["connection"].(map[string]any)
	if connection["credential_kind"] != "gcp_service_account" {
		t.Fatalf("credential_kind=%v", connection["credential_kind"])
	}
}

// The JSON path must keep working untouched — multipart is additive.
func TestConnectionStillAcceptsServiceAccountAsJSON(t *testing.T) {
	env := newCredentialTestEnv(t)
	defer env.server.Close()

	key := syntheticServiceAccountKey(t)
	status, created := jsonRequest(t, env.server.URL+"/admin/api/principals/"+env.principalID+"/connections",
		http.MethodPost, "admin-secret", map[string]any{
			"provider_id": "vertex-prod", "credential_kind": "gcp_service_account",
			"connection_name": "pasted", "secret": key,
		})
	if status != http.StatusCreated {
		t.Fatalf("json status=%d body=%+v", status, created)
	}
}

// A mangled upload must still produce gcpauth.Parse's specific diagnosis, not a
// generic failure — the precise message is what tells an operator what broke.
func TestMangledUploadKeepsTheSpecificError(t *testing.T) {
	env := newCredentialTestEnv(t)
	defer env.server.Close()

	status, body := postMultipart(t, env.server.URL+"/admin/api/principals/"+env.principalID+"/connections",
		"admin-secret", map[string]string{
			"provider_id": "vertex-prod", "credential_kind": "gcp_service_account",
			"connection_name": "broken", "@secret": "{\"type\":\"service_account\"}",
		})
	if status != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", status)
	}
	msg, _ := body["error"].(map[string]any)["message"].(string)
	if msg == "" || !containsAll(msg, "missing required field") {
		t.Fatalf("error message %q does not name the missing fields", msg)
	}
}

// No credential value may appear in any error the API returns.
func TestUploadErrorsNeverEchoTheSecret(t *testing.T) {
	env := newCredentialTestEnv(t)
	defer env.server.Close()

	marker := "SUPER-SECRET-MARKER-VALUE"
	_, body := postMultipart(t, env.server.URL+"/admin/api/principals/"+env.principalID+"/connections",
		"admin-secret", map[string]string{
			"provider_id": "vertex-prod", "credential_kind": "gcp_service_account",
			"connection_name": "leaky", "@secret": "not json " + marker,
		})
	raw := stringifyAny(body)
	if containsAll(raw, marker) {
		t.Fatalf("response echoed the submitted credential: %s", raw)
	}
}

// handleUpsertProvider is the third route wired to decodeBodyOrForm, and the
// one the flexBool deviation on ForceApiSupport actually targets. A multipart
// checkbox field arrives as the string "true"/"false", never a JSON bool, so
// this must be proven directly rather than inferred from the connection tests.
func TestProviderUpsertAcceptsMultipartWithBooleanField(t *testing.T) {
	env := newCredentialTestEnv(t)
	defer env.server.Close()

	status, body := postMultipart(t, env.server.URL+"/admin/api/providers",
		"admin-secret", map[string]string{
			"id": "multipart-fixture", "type": "openai_compatible",
			"force_api_support": "true",
		})
	if status != http.StatusOK || body["ok"] != true {
		t.Fatalf("provider multipart upsert status=%d body=%+v", status, body)
	}
	cfg := config.Get().Providers["multipart-fixture"]
	if cfg == nil || !cfg.ForceApiSupport {
		t.Fatalf("force_api_support did not land as boolean true: %+v", cfg)
	}
}

// A file part larger than decodeBodyOrForm's size limit must be rejected with
// a clear 413, not silently accepted or spooled to an unbounded temp file.
// This is the regression test for the DoS vector: ParseMultipartForm's
// maxMemory argument only caps what stays resident in memory -- it does not
// cap request size -- so the guard has to be the http.MaxBytesReader wrapped
// around r.Body before parsing starts.
func TestUploadRejectsBodyOverSizeLimit(t *testing.T) {
	env := newCredentialTestEnv(t)
	defer env.server.Close()

	oversized := strings.Repeat("A", maxCredentialBodyBytes+64*1024)
	status, body := postMultipart(t, env.server.URL+"/admin/api/principals/"+env.principalID+"/connections",
		"admin-secret", map[string]string{
			"provider_id": "vertex-prod", "credential_kind": "gcp_service_account",
			"connection_name": "oversized", "@secret": oversized,
		})
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d, want %d", status, http.StatusRequestEntityTooLarge)
	}
	msg, _ := body["error"].(map[string]any)["message"].(string)
	if !containsAll(msg, "too large") {
		t.Fatalf("error message %q does not say the body was too large", msg)
	}
}

// A URL query parameter must never stand in for a submitted multipart form
// field -- net/http's r.Form (and so r.FormValue) is populated query-values-
// first, so reading through FormValue would let ?force_api_support=true win
// over a body that explicitly says false, with no error and no log.
func TestMultipartFormValueIsNotOverriddenByQueryParameter(t *testing.T) {
	env := newCredentialTestEnv(t)
	defer env.server.Close()

	status, body := postMultipart(t,
		env.server.URL+"/admin/api/providers?force_api_support=true",
		"admin-secret", map[string]string{
			"id": "query-override-fixture", "type": "openai_compatible",
			"force_api_support": "false",
		})
	if status != http.StatusOK || body["ok"] != true {
		t.Fatalf("provider multipart upsert status=%d body=%+v", status, body)
	}
	cfg := config.Get().Providers["query-override-fixture"]
	if cfg == nil || cfg.ForceApiSupport {
		t.Fatalf("query parameter overrode the submitted form value: %+v", cfg)
	}
}

// vertex_ai declares both api_key and gcp_service_account. A provider of that
// type must accept an API-key connection whether or not it came from the
// registry — provenance is not an auth method.
func TestCustomVertexAcceptsAPIKeyConnection(t *testing.T) {
	env := newCredentialTestEnvWith(t, map[string]*config.ProviderConfig{
		"vertex-custom": {Type: "vertex_ai", Project: "p-a", Location: "global"},
	})
	defer env.server.Close()

	status, created := jsonRequest(t, env.server.URL+"/admin/api/principals/"+env.principalID+"/connections",
		http.MethodPost, "admin-secret", map[string]any{
			"provider_id": "vertex-custom", "credential_kind": "api_key",
			"connection_name": "key-only", "secret": "vertex-api-key",
		})
	if status != http.StatusCreated {
		t.Fatalf("custom vertex api_key connection rejected: %d %+v", status, created)
	}
}

// RFC 9110 media types are case-insensitive, and "Multipart/Form-Data" is a
// legal spelling real clients send. Matching the raw header with a
// case-sensitive prefix test routed those bodies into the JSON decoder, which
// answered 400 on a valid request.
func TestUploadAcceptsMixedCaseMultipartContentType(t *testing.T) {
	env := newCredentialTestEnv(t)
	defer env.server.Close()

	contentType, body := multipartRequest(t, map[string]string{
		"id": "mixed-case-fixture", "type": "openai_compatible",
	})
	mangled := strings.Replace(contentType, "multipart/form-data", "Multipart/Form-Data", 1)
	if mangled == contentType {
		t.Fatalf("content type %q did not carry the expected media type", contentType)
	}
	req, err := http.NewRequest(http.MethodPost, env.server.URL+"/admin/api/providers", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mangled)
	req.Header.Set("Authorization", "Bearer admin-secret")
	status, decoded := doJSONRequest(t, req)
	if status != http.StatusOK || decoded["ok"] != true {
		t.Fatalf("mixed-case multipart status=%d body=%+v", status, decoded)
	}
	if config.Get().Providers["mixed-case-fixture"] == nil {
		t.Fatal("provider was not created from the mixed-case multipart body")
	}
}

// postRawJSON sends a byte-for-byte body with a JSON content type, so a test
// can submit something json.Marshal would never produce: trailing padding, or
// two values concatenated.
func postRawJSON(t *testing.T, url, token, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return doJSONRequest(t, req)
}

// The size limit must bound the JSON path too. json.Decoder.Decode stops at the
// end of the first complete value, so a small valid object followed by megabytes
// of whitespace used to decode successfully with MaxBytesReader never reaching
// its limit -- the credential routes accepted the request instead of answering
// 413. The multipart bound was tested; this one was not.
func TestJSONBodyOverSizeLimitIsRejected(t *testing.T) {
	env := newCredentialTestEnv(t)
	defer env.server.Close()

	padded := `{"id":"json-padded-fixture","type":"openai_compatible"}` +
		strings.Repeat(" ", maxCredentialBodyBytes+64*1024)
	status, body := postRawJSON(t, env.server.URL+"/admin/api/providers", "admin-secret", padded)
	if status != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d, want %d (body=%+v)", status, http.StatusRequestEntityTooLarge, body)
	}
	msg, _ := body["error"].(map[string]any)["message"].(string)
	if !containsAll(msg, "too large") {
		t.Fatalf("error message %q does not say the body was too large", msg)
	}
	if cfg := config.Get().Providers["json-padded-fixture"]; cfg != nil {
		t.Fatal("an over-limit JSON body was applied instead of rejected")
	}
}

// One request carries one payload. Two concatenated values must be refused
// rather than having everything past the first silently dropped.
func TestJSONBodyWithASecondValueIsRejected(t *testing.T) {
	env := newCredentialTestEnv(t)
	defer env.server.Close()

	doubled := `{"id":"json-first-fixture","type":"openai_compatible"}` +
		`{"id":"json-second-fixture","type":"openai_compatible"}`
	status, body := postRawJSON(t, env.server.URL+"/admin/api/providers", "admin-secret", doubled)
	if status != http.StatusBadRequest {
		t.Fatalf("status=%d, want %d (body=%+v)", status, http.StatusBadRequest, body)
	}
	for _, id := range []string{"json-first-fixture", "json-second-fixture"} {
		if cfg := config.Get().Providers[id]; cfg != nil {
			t.Fatalf("provider %q was created from a rejected body", id)
		}
	}
}

// The bound must not cost the ordinary case anything: a single valid object,
// with or without the trailing newline a shell pipeline appends, still applies.
func TestJSONBodyWithinTheLimitStillSucceeds(t *testing.T) {
	env := newCredentialTestEnv(t)
	defer env.server.Close()

	status, body := postRawJSON(t, env.server.URL+"/admin/api/providers", "admin-secret",
		`{"id":"json-plain-fixture","type":"openai_compatible"}`+"\n")
	if status != http.StatusOK || body["ok"] != true {
		t.Fatalf("status=%d body=%+v", status, body)
	}
	if cfg := config.Get().Providers["json-plain-fixture"]; cfg == nil {
		t.Fatal("a valid JSON body was not applied")
	}
}
