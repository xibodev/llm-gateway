package providers

import (
	"os"
	"strings"
	"testing"

	"llmgw/internal/gcpauth"
)

// TestLiveVertexServiceAccountCall drives the whole service-account path
// against real Vertex: parse the key, mint a token, and issue a generateContent
// request. Skipped unless LLMGW_LIVE_GCP_KEY names a key file.
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
	credential, err := gcpauth.Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	token, err := gcpauth.AccessToken(credential, gcpauth.CloudPlatformScope)
	if err != nil {
		t.Fatalf("AccessToken: %v", err)
	}

	location := strings.TrimSpace(os.Getenv("LLMGW_LIVE_GCP_LOCATION"))
	if location == "" {
		location = "global"
	}
	model := strings.TrimSpace(os.Getenv("LLMGW_LIVE_GCP_MODEL"))
	if model == "" {
		model = "gemini-2.5-flash"
	}

	provider := NewVertexAIWithAccessToken("", token, credential.ProjectID(), location, 60)
	url, err := provider.modelURL(model, "generateContent")
	if err != nil {
		t.Fatalf("modelURL: %v", err)
	}
	t.Logf("calling %s", url)

	body, status, err := provider.do("POST", url, map[string]any{
		"contents": []any{map[string]any{
			"role":  "user",
			"parts": []any{map[string]any{"text": "Reply with the single word: ok"}},
		}},
		"generationConfig": map[string]any{"maxOutputTokens": 16},
	})
	if err != nil && status == 0 {
		t.Fatalf("transport error: %v", err)
	}

	switch {
	case status == 200:
		t.Logf("HTTP 200: Vertex accepted the service-account token and answered")
	case status == 401:
		t.Fatalf("HTTP 401: Vertex rejected the minted token: %v", summarise(body, err))
	case status == 403 && mentionsAuthentication(body, err):
		t.Fatalf("HTTP 403 with an authentication reason: %v", summarise(body, err))
	default:
		// Authentication succeeded; the call stopped on IAM, billing, model
		// availability or region, none of which this code controls.
		t.Logf("HTTP %d: token was accepted, request stopped later: %v",
			status, summarise(body, err))
	}
}

func summarise(body map[string]any, err error) string {
	if err != nil {
		return truncate(err.Error())
	}
	if body == nil {
		return "no body"
	}
	if detail, ok := body["error"]; ok {
		if record, ok := detail.(map[string]any); ok {
			if message, ok := record["message"].(string); ok {
				return truncate(message)
			}
		}
	}
	return "no error detail"
}

func mentionsAuthentication(body map[string]any, err error) bool {
	text := strings.ToLower(summarise(body, err))
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
