package diagnostics

import (
	"reflect"
	"regexp"
)

const (
	Redacted              = "[redacted]"
	maxStructuredDepth    = 16
	maxStructuredElements = 1024
	sensitiveKeyPattern   = `(?:access[_-]?token|refresh[_-]?token|id[_-]?token|api[_-]?key|x[_-]?api[_-]?key|x[_-]?goog[_-]?api[_-]?key|client[_-]?secret|private[_-]?key|proxy[_-]?authorization|authorization|password|passwd|credential|cookie|session[_-]?id|session|secret|token|auth)`
)

// Redaction of secrets/PII that might appear in upstream or local diagnostic
// text. Underscores are intentionally absent from the generic long-value rule,
// so ordinary identifiers are not mistaken for credentials.
var (
	pemBlockRE            = regexp.MustCompile(`(?is)-----BEGIN [A-Z0-9][A-Z0-9 -]{0,63}-----.*?-----END [A-Z0-9][A-Z0-9 -]{0,63}-----`)
	sensitiveAssignmentRE = regexp.MustCompile(
		`(?i)(\b` + sensitiveKeyPattern + `\b["']?\s*[:=]\s*)` +
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
	sensitiveKeyRE = regexp.MustCompile(`(?i)^(?:` + sensitiveKeyPattern + `|key)$`)
)

// SanitizeText removes credential-shaped and personally identifying values
// from text that may be returned to a caller or written to diagnostics.
// Applying it repeatedly produces the same result.
func SanitizeText(text string) string {
	text = pemBlockRE.ReplaceAllString(text, Redacted)
	text = sensitiveAssignmentRE.ReplaceAllString(text, `${1}`+Redacted)
	text = keyAssignmentRE.ReplaceAllString(text, `${1}`+Redacted)
	text = bearerRE.ReplaceAllString(text, "Bearer "+Redacted)
	text = emailRE.ReplaceAllString(text, Redacted)
	text = gatewayTokenRE.ReplaceAllString(text, Redacted+`${1}`)
	text = tokenRE.ReplaceAllString(text, Redacted)
	return longValueRE.ReplaceAllString(text, Redacted)
}

// SanitizeTextLimit sanitizes text before limiting it to maxChars.
func SanitizeTextLimit(text string, maxChars int) string {
	text = SanitizeText(text)
	if maxChars <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) > maxChars {
		return string(runes[:maxChars])
	}
	return text
}

// SanitizeValue returns a sanitized copy of a JSON-like value. Map keys are
// retained verbatim; only string-keyed maps and slices are traversed.
func SanitizeValue(value any) any {
	remaining := maxStructuredElements
	return sanitizeValue(reflect.ValueOf(value), 0, false, &remaining, make(map[visit]bool))
}

type visit struct {
	typ reflect.Type
	ptr uintptr
}

func sanitizeValue(value reflect.Value, depth int, redactStrings bool, remaining *int, seen map[visit]bool) any {
	if !value.IsValid() {
		return nil
	}
	if value.Kind() == reflect.Interface {
		if value.IsNil() {
			return nil
		}
		return sanitizeValue(value.Elem(), depth, redactStrings, remaining, seen)
	}

	switch value.Kind() {
	case reflect.String:
		if redactStrings {
			return Redacted
		}
		return SanitizeText(value.String())
	case reflect.Bool:
		return value.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return value.Interface()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return value.Interface()
	case reflect.Float32, reflect.Float64:
		return value.Interface()
	case reflect.Map:
		if value.Type().Key().Kind() != reflect.String || value.IsNil() {
			return value.Interface()
		}
		if depth >= maxStructuredDepth || value.Len() > *remaining || isSeen(value, seen) {
			return Redacted
		}
		*remaining -= value.Len()
		defer unsee(value, seen)
		result := make(map[string]any, value.Len())
		iterator := value.MapRange()
		for iterator.Next() {
			key := iterator.Key().String()
			result[key] = sanitizeValue(iterator.Value(), depth+1, redactStrings || sensitiveKeyRE.MatchString(key), remaining, seen)
		}
		return result
	case reflect.Slice:
		if value.IsNil() {
			return nil
		}
		if depth >= maxStructuredDepth || value.Len() > *remaining || isSeen(value, seen) {
			return Redacted
		}
		*remaining -= value.Len()
		defer unsee(value, seen)
		result := make([]any, value.Len())
		for i := range value.Len() {
			result[i] = sanitizeValue(value.Index(i), depth+1, redactStrings, remaining, seen)
		}
		return result
	default:
		return value.Interface()
	}
}

func isSeen(value reflect.Value, seen map[visit]bool) bool {
	current := visit{typ: value.Type(), ptr: value.Pointer()}
	if seen[current] {
		return true
	}
	seen[current] = true
	return false
}

func unsee(value reflect.Value, seen map[visit]bool) {
	delete(seen, visit{typ: value.Type(), ptr: value.Pointer()})
}
