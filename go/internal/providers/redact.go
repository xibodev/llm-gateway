package providers

import "regexp"

// Redaction of secrets/PII that might appear in an upstream error body before we
// surface it. Shared by every OpenAI-standard provider.
var (
	emailRE = regexp.MustCompile(`(?i)\b[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}\b`)
	tokenRE = regexp.MustCompile(`\b(?:gh[oupsr]_[A-Za-z0-9_]{10,}|sk-[A-Za-z0-9_-]{10,}|[A-Fa-f0-9]{20,}|[A-Za-z0-9]{20,})\b`)
)

func redact(text string) string {
	text = emailRE.ReplaceAllString(text, "[redacted]")
	return tokenRE.ReplaceAllString(text, "[redacted]")
}
