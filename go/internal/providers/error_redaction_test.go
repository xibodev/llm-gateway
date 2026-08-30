package providers

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"llmgw/internal/diagnostics"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

func syntheticGatewayToken() string {
	return "llmgw_" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0xab}, 24))
}

func TestSanitizeDiagnosticText(t *testing.T) {
	gatewayToken := syntheticGatewayToken()
	pem := "-----BEGIN PRIVATE KEY-----\nZmFrZSBrZXkgbWF0ZXJpYWw=\n-----END PRIVATE KEY-----"
	cases := []struct {
		name   string
		secret string
		text   string
	}{
		{name: "email", secret: "owner@example.test", text: "account owner@example.test failed"},
		{name: "GitHub token", secret: "gho_" + strings.Repeat("a", 16), text: "token gho_" + strings.Repeat("a", 16)},
		{name: "OpenAI token", secret: "sk-" + strings.Repeat("b", 24), text: "credential sk-" + strings.Repeat("b", 24)},
		{name: "gateway token", secret: gatewayToken, text: "gateway rejected " + gatewayToken},
		{name: "gsk token", secret: "gsk_" + strings.Repeat("c", 12), text: "provider rejected gsk_" + strings.Repeat("c", 12)},
		{name: "Bearer value", secret: "small!bearer/value", text: "Authorization: Bearer small!bearer/value"},
		{name: "query assignment", secret: "short-value", text: "https://example.test/callback?api_key=short-value&mode=test"},
		{name: "plain key query assignment", secret: "small-key", text: "https://example.test/callback?key=small-key&mode=test"},
		{name: "quoted key assignment", secret: "small-json-key", text: `{"key": "small-json-key"}`},
		{name: "key value assignment", secret: "small-pass", text: `password: "small-pass"`},
		{name: "long hex", secret: strings.Repeat("deadbeef", 5), text: "digest=" + strings.Repeat("deadbeef", 5)},
		{name: "long alphanumeric", secret: "AbCdEf0123456789GhIjKlMn", text: "opaque AbCdEf0123456789GhIjKlMn"},
		{name: "PEM block", secret: pem, text: "parse failed:\n" + pem},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := SanitizeDiagnosticText(testCase.text)
			if strings.Contains(got, testCase.secret) {
				t.Fatalf("sensitive value survived sanitization: %q", got)
			}
			if !strings.Contains(got, redactedDiagnostic) {
				t.Fatalf("sanitized text has no redaction marker: %q", got)
			}
			if twice := SanitizeDiagnosticText(got); twice != got {
				t.Fatalf("sanitization is not idempotent: once=%q twice=%q", got, twice)
			}
		})
	}

	ordinary := "request_id model_name abcdefghijklmnopqrstuvwx_identifier ordinary_snake_case service account key: missing fields"
	if got := SanitizeDiagnosticText(ordinary); got != ordinary {
		t.Fatalf("ordinary underscore identifiers changed: %q", got)
	}
}

func TestSanitizeDiagnosticTextLimitSanitizesBeforeTruncating(t *testing.T) {
	secret := syntheticGatewayToken()
	text := strings.Repeat("safe ", 35) + secret
	got := SanitizeDiagnosticTextLimit(text, 200)
	if utf8.RuneCountInString(got) > 200 {
		t.Fatalf("bounded text has %d characters", utf8.RuneCountInString(got))
	}
	if strings.Contains(got, secret[:24]) {
		t.Fatalf("truncated credential prefix survived: %q", got)
	}
	if !strings.Contains(got, redactedDiagnostic) {
		t.Fatalf("credential was not sanitized before truncation: %q", got)
	}
}

func TestDiagnosticSanitizerWrappersMatchSharedImplementation(t *testing.T) {
	cases := []string{
		"ordinary diagnostic",
		"owner@example.test",
		"Authorization: Bearer small!bearer/value",
		`{"api_key":"short-value"}`,
		"llmgw_" + strings.Repeat("a", 32),
		strings.Repeat("界", 12),
	}
	for _, text := range cases {
		if got, want := SanitizeDiagnosticText(text), diagnostics.SanitizeText(text); got != want {
			t.Errorf("text wrapper mismatch for %q: got %q want %q", text, got, want)
		}
		for _, limit := range []int{-1, 0, 1, 12, 2048} {
			if got, want := SanitizeDiagnosticTextLimit(text, limit), diagnostics.SanitizeTextLimit(text, limit); got != want {
				t.Errorf("limit wrapper mismatch for %q at %d: got %q want %q", text, limit, got, want)
			}
		}
	}
}

func TestDiagnosticErrorTextIsSanitizedAndBounded(t *testing.T) {
	raw := strings.Repeat("safe ", 500) + "owner@example.test " + syntheticGatewayToken()
	errors := map[string]error{
		"invocation": &InvocationError{Msg: raw, Status: http.StatusBadGateway},
		"config":     &ConfigError{Msg: raw},
		"catalog":    &CatalogError{Code: "catalog_http_error", Detail: raw, Status: http.StatusBadGateway},
	}
	for name, err := range errors {
		t.Run(name, func(t *testing.T) {
			got := err.Error()
			if utf8.RuneCountInString(got) > diagnosticErrorLimit {
				t.Fatalf("error has %d characters", utf8.RuneCountInString(got))
			}
			if strings.Contains(got, "owner@example.test") || strings.Contains(got, "llmgw_") {
				t.Fatalf("error exposed diagnostic data: %q", got)
			}
		})
	}

	catalog := errors["catalog"]
	_, detail, status := CatalogFailure(catalog)
	if status != http.StatusBadGateway || detail != catalog.Error() {
		t.Fatalf("catalog failure detail/status = (%q, %d)", detail, status)
	}
}

func TestHTTPInvocationErrorExtractsSanitizesAndKeepsStatus(t *testing.T) {
	token := syntheticGatewayToken()
	err := HTTPInvocationError("embeddings", http.StatusServiceUnavailable, []byte(
		`{"error":{"message":"token `+token+` belongs to owner@example.test"}}`,
	))
	if status := UpstreamStatus(err); status != http.StatusServiceUnavailable {
		t.Fatalf("status=%d", status)
	}
	if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "owner@example.test") {
		t.Fatalf("HTTP invocation error exposed diagnostic data: %q", err)
	}
	if !strings.Contains(err.Error(), "embeddings: upstream returned 503") ||
		!strings.Contains(err.Error(), redactedDiagnostic) {
		t.Fatalf("HTTP invocation error lost safe context: %q", err)
	}
}

func TestProviderNon2xxErrorsAreSanitizedAndKeepStatus(t *testing.T) {
	const status = http.StatusTooManyRequests
	gatewayToken := syntheticGatewayToken()
	gskToken := "gsk_" + strings.Repeat("z", 24)
	email := "provider-owner@example.test"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `{"error":{"status":"FAILED_PRECONDITION","message":"account %s used %s with Bearer %s"}}`, email, gatewayToken, gskToken)
	}))
	defer server.Close()

	messages := []Message{{"role": "user", "content": "hi"}}
	cases := map[string]func() error{
		"OpenAI-compatible completion": func() error {
			_, err := (OpenAIProvider{auth: bearerAuth{base: server.URL}, Timeout: 2}).Complete("model", messages, nil)
			return err
		},
		"Azure completion": func() error {
			_, err := (AzureOpenAIProvider{BaseURL: server.URL, Timeout: 2}).Complete("model", messages, nil)
			return err
		},
		"Anthropic completion": func() error {
			_, err := (AnthropicNativeProvider{BaseURL: server.URL, Timeout: 2}).Complete("model", messages, nil)
			return err
		},
		"Anthropic stream setup": func() error {
			_, err := (AnthropicNativeProvider{BaseURL: server.URL, Timeout: 2}).Stream("model", messages, nil)
			return err
		},
		"Ollama completion": func() error {
			_, err := (OllamaProvider{BaseURL: server.URL, Timeout: 2}).Complete("model", messages, nil)
			return err
		},
		"Ollama stream setup": func() error {
			_, err := (OllamaProvider{BaseURL: server.URL, Timeout: 2}).Stream("model", messages, nil)
			return err
		},
		"Google completion": func() error {
			_, err := NewAIStudio(server.URL, "fixture", 2).Complete("model", messages, nil)
			return err
		},
	}

	for name, invoke := range cases {
		t.Run(name, func(t *testing.T) {
			err := invoke()
			if err == nil {
				t.Fatal("non-2xx response returned no error")
			}
			if got := UpstreamStatus(err); got != status {
				t.Fatalf("upstream status = %d, want %d (error %q)", got, status, err)
			}
			message := err.Error()
			for _, sensitive := range []string{gatewayToken, gskToken, email} {
				if strings.Contains(message, sensitive) {
					t.Fatalf("error exposed %q: %q", sensitive, message)
				}
			}
			if !strings.Contains(message, redactedDiagnostic) {
				t.Fatalf("error has no redaction marker: %q", message)
			}
		})
	}
}

func TestProviderErrorSanitizesCredentialAcrossExtractionBoundary(t *testing.T) {
	secret := syntheticGatewayToken()
	prefix := `{"detail":"`
	body := prefix + strings.Repeat(" ", 500-len(prefix)) + secret +
		strings.Repeat(" safe", 600) + `"}`
	secretStart := strings.Index(body, secret)
	if secretStart >= 512 || secretStart+len(secret) <= 512 {
		t.Fatalf("fixture secret range %d..%d does not cross byte 512", secretStart, secretStart+len(secret))
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	_, err := (OpenAIProvider{auth: bearerAuth{base: server.URL}, Timeout: 2}).Complete(
		"model", []Message{{"role": "user", "content": "hi"}}, nil,
	)
	if err == nil {
		t.Fatal("non-2xx response returned no error")
	}
	if got := UpstreamStatus(err); got != http.StatusServiceUnavailable {
		t.Fatalf("upstream status = %d", got)
	}
	message := err.Error()
	if utf8.RuneCountInString(message) > diagnosticErrorLimit {
		t.Fatalf("provider error has %d characters", utf8.RuneCountInString(message))
	}
	if strings.Contains(message, secret) || strings.Contains(message, secret[:12]) {
		t.Fatalf("provider error retained a full or partial credential: %q", message)
	}
	if !strings.Contains(message, redactedDiagnostic) {
		t.Fatalf("provider error has no redaction marker: %q", message)
	}
}
