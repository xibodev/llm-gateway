package providers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// captureAuth runs one Vertex call against a stub and returns the auth headers
// the provider sent.
func captureAuth(t *testing.T, provider GoogleAIProvider) (string, string) {
	t.Helper()
	var authorization, apiKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		apiKey = r.Header.Get("x-goog-api-key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[]}`))
	}))
	defer server.Close()

	_, _, _ = provider.do(http.MethodPost, server.URL, map[string]any{"contents": []any{}})
	return authorization, apiKey
}

// TestServiceAccountProviderSendsBearerToken pins the header swap: a minted
// access token must travel as a Bearer credential, never as x-goog-api-key.
func TestServiceAccountProviderSendsBearerToken(t *testing.T) {
	provider := NewVertexAIWithAccessToken("", "ya29.minted-token", "proj", "global", 5)
	authorization, apiKey := captureAuth(t, provider)

	if authorization != "Bearer ya29.minted-token" {
		t.Fatalf("Authorization = %q, want the minted bearer token", authorization)
	}
	if apiKey != "" {
		t.Fatalf("x-goog-api-key = %q, want empty (Vertex rejects both)", apiKey)
	}
}

// TestAPIKeyProviderKeepsLegacyHeader guards the existing path: adding
// service-account support must not change how API-key providers authenticate.
func TestAPIKeyProviderKeepsLegacyHeader(t *testing.T) {
	provider := NewVertexAI("", "an-api-key", "proj", "global", 5)
	authorization, apiKey := captureAuth(t, provider)

	if apiKey != "an-api-key" {
		t.Fatalf("x-goog-api-key = %q, want the configured key", apiKey)
	}
	if authorization != "" {
		t.Fatalf("Authorization = %q, want empty for an API-key provider", authorization)
	}
}

// TestAccessTokenIsTrimmed guards against a pasted token with stray whitespace
// producing a malformed header.
func TestAccessTokenIsTrimmed(t *testing.T) {
	provider := NewVertexAIWithAccessToken("", "  ya29.padded  ", "proj", "global", 5)
	authorization, _ := captureAuth(t, provider)

	if strings.Contains(authorization, "  ") {
		t.Fatalf("Authorization = %q, want the token trimmed", authorization)
	}
	if authorization != "Bearer ya29.padded" {
		t.Fatalf("Authorization = %q", authorization)
	}
}
