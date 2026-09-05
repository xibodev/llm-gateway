package providers

import "llmgw/internal/diagnostics"

const (
	redactedDiagnostic   = "[redacted]"
	diagnosticErrorLimit = 2048
)

// SanitizeDiagnosticText removes credential-shaped and personally identifying
// values from text that may be returned to a caller or written to diagnostics.
// Applying it repeatedly produces the same result.
func SanitizeDiagnosticText(text string) string {
	return diagnostics.SanitizeText(text)
}

// SanitizeDiagnosticTextLimit sanitizes text before limiting it to maxChars.
// Sanitizing first prevents a truncated credential prefix from evading the
// complete credential patterns above.
func SanitizeDiagnosticTextLimit(text string, maxChars int) string {
	return diagnostics.SanitizeTextLimit(text, maxChars)
}

func redact(text string) string { return SanitizeDiagnosticText(text) }
