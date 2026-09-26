package providers

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"llmgw/internal/config"

	gcpauth "github.com/xibodev/llm-provider-auth/gcp"
	"github.com/xibodev/llm-provider-auth/tokenstore"
	core "github.com/xibodev/llmgw-core"
	coreproviders "github.com/xibodev/llmgw-core/providers"
)

// TestLiveVertexServiceAccountCall drives the whole service-account path
// against real Vertex: the key reaches core's Google as the credential store
// hands it out, and core's Google parses it, mints a token and issues a
// generateContent request. Skipped unless LLMGW_LIVE_GCP_KEY names a key file.
//
//	LLMGW_LIVE_GCP_KEY=/path/to/key.json go test ./internal/providers -run LiveVertex -v
//
// It asserts on authentication only. A 403 or 404 means Google accepted the
// credential and then applied IAM or model availability, which is a property of
// the project rather than of this code; an authentication failure is a defect.
func TestLiveVertexServiceAccountCall(t *testing.T) {
	path := strings.TrimSpace(os.Getenv("LLMGW_LIVE_GCP_KEY"))
	if path == "" {
		t.Skip("set LLMGW_LIVE_GCP_KEY to a service account key file to run the live check")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read key file: %v", err)
	}
	model := strings.TrimSpace(os.Getenv("LLMGW_LIVE_GCP_MODEL"))
	if model == "" {
		model = "gemini-2.5-flash"
	}
	timeout := 60.0
	google, err := newCoreGoogle("live-vertex", &config.ProviderConfig{
		Type: "vertex_ai", Location: strings.TrimSpace(os.Getenv("LLMGW_LIVE_GCP_LOCATION")), Timeout: &timeout,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The record the credential store loads for a service-account connection.
	credential := core.CredentialFromRecord("live", tokenstore.Record{
		AccessToken: string(raw), TokenType: core.TokenTypeGCPServiceAccount,
	})
	_, err = google.Invoke(context.Background(), core.Request{
		Surface: core.ModelSurfaceChatCompletions, Model: model, ContentType: core.ContentTypeJSON, Credential: credential,
		Body: []byte(`{"messages":[{"role":"user","content":"Reply with the single word: ok"}],"max_tokens":16}`),
	})
	var exchange *gcpauth.TokenError
	status, detail := core.ClassifyError(err).StatusCode, summarise(err)
	switch {
	case err == nil:
		t.Logf("HTTP 200: Vertex accepted the service-account token and answered")
	case errors.As(err, &exchange):
		t.Fatalf("token exchange failed: %v", exchange)
	case status == 0:
		t.Fatalf("no answer from Vertex: %v", detail)
	case status == 401:
		t.Fatalf("HTTP 401: Vertex rejected the minted token: %v", detail)
	case status == 403 && mentionsAuthentication(detail):
		t.Fatalf("HTTP 403 with an authentication reason: %v", detail)
	default:
		// Authentication succeeded; the call stopped on IAM, billing, model
		// availability or region, none of which this code controls.
		t.Logf("HTTP %d: token was accepted, request stopped later: %v", status, detail)
	}
}

// summarise is the gateway's diagnostic for err, which quotes Google's
// message, bounded.
func summarise(err error) string {
	var upstream *coreproviders.InvocationError
	switch {
	case err == nil:
		return ""
	case errors.As(err, &upstream):
		return truncate(upstream.Msg)
	}
	return truncate(err.Error())
}

func mentionsAuthentication(detail string) bool {
	text := strings.ToLower(detail)
	for _, needle := range []string{"unauthenticated", "invalid credential", "invalid authentication", "expired"} {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

func truncate(text string) string {
	text = strings.TrimSpace(text)
	if len(text) > 300 {
		return text[:300] + "..."
	}
	return text
}
