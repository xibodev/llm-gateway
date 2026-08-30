package providers

import "regexp"

const (
	redactedDiagnostic     = "[redacted]"
	diagnosticErrorLimit   = 2048
	sensitiveDiagnosticKey = `(?:access[_-]?token|refresh[_-]?token|id[_-]?token|api[_-]?key|x[_-]?api[_-]?key|x[_-]?goog[_-]?api[_-]?key|client[_-]?secret|private[_-]?key|proxy[_-]?authorization|authorization|password|passwd|credential|cookie|session[_-]?id|session|secret|token|auth)`
)

// Redaction of secrets/PII that might appear in upstream or local diagnostic
// text. Underscores are intentionally absent from the generic long-value rule,
// so ordinary identifiers are not mistaken for credentials.
var (
	pemBlockRE            = regexp.MustCompile(`(?is)-----BEGIN [A-Z0-9][A-Z0-9 -]{0,63}-----.*?-----END [A-Z0-9][A-Z0-9 -]{0,63}-----`)
	sensitiveAssignmentRE = regexp.MustCompile(
		`(?i)(\b` + sensitiveDiagnosticKey + `\b["']?\s*[:=]\s*)` +
			`(?:"[^"\r\n]*"|'[^'\r\n]*'|Bearer[ \t]+(?:\[redacted\]|[^\s,;&}\]\r\n"']+)|\[redacted\]|[^\s,;&}\]\r\n]+)`,
	)
	keyAssignmentRE = regexp.MustCompile(
		`(?i)((?:\bkey\s*=|["']key["']\s*[:=])\s*)` +
			`(?:"[^"\r\n]*"|'[^'\r\n]*'|\[redacted\]|[^\s,;&}\]\r\n]+)`,
	)
	bearerRE       = regexp.MustCompile(`(?i)\bBearer[ \t]+(?:\[redacted\]|[^\s,;&}\]\r\n"']+)`)
	emailRE        = regexp.MustCompile(`(?i)\b[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}\b`)
	gatewayTokenRE = regexp.MustCompile(`\bllmgw_[A-Za-z0-9_-]{32}([^A-Za-z0-9_-]|$)`)
	tokenRE        = regexp.MustCompile(`(?i)\b(?:gh[oupsr]_[A-Za-z0-9_]{10,}|github_pat_[A-Za-z0-9_]{10,}|sk-[A-Za-z0-9_-]{10,}|gsk_[A-Za-z0-9_-]{10,})\b`)
	longValueRE    = regexp.MustCompile(`\b[A-Za-z0-9]{20,}\b`)
)

// SanitizeDiagnosticText removes credential-shaped and personally identifying
// values from text that may be returned to a caller or written to diagnostics.
// Applying it repeatedly produces the same result.
func SanitizeDiagnosticText(text string) string {
	text = pemBlockRE.ReplaceAllString(text, redactedDiagnostic)
	text = sensitiveAssignmentRE.ReplaceAllString(text, `${1}`+redactedDiagnostic)
	text = keyAssignmentRE.ReplaceAllString(text, `${1}`+redactedDiagnostic)
	text = bearerRE.ReplaceAllString(text, "Bearer "+redactedDiagnostic)
	text = emailRE.ReplaceAllString(text, redactedDiagnostic)
	text = gatewayTokenRE.ReplaceAllString(text, redactedDiagnostic+`${1}`)
	text = tokenRE.ReplaceAllString(text, redactedDiagnostic)
	return longValueRE.ReplaceAllString(text, redactedDiagnostic)
}

// SanitizeDiagnosticTextLimit sanitizes text before limiting it to maxChars.
// Sanitizing first prevents a truncated credential prefix from evading the
// complete credential patterns above.
func SanitizeDiagnosticTextLimit(text string, maxChars int) string {
	text = SanitizeDiagnosticText(text)
	if maxChars <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) > maxChars {
		return string(runes[:maxChars])
	}
	return text
}

func redact(text string) string { return SanitizeDiagnosticText(text) }
