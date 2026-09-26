// Package api assembles the HTTP server: OpenAI + Anthropic + models + health
// facades, the /admin control panel, bearer auth, and error envelopes.
package api

import (
	"encoding/json"
	"net/http"

	"llmgw/internal/providers"
)

// errorPayload matches the Python error envelope shape.
func errorPayload(message, errorType, code string) map[string]any {
	return map[string]any{"error": map[string]any{"message": message, "type": errorType, "code": code}}
}

func httpExceptionType(status int) string {
	switch {
	case status == 401:
		return "unauthorized_error"
	case status >= 400 && status < 500:
		return "invalid_request_error"
	default:
		return "api_error"
	}
}

// writeJSON writes v as JSON with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes the standard error envelope.
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, errorPayload(
		providers.SanitizeDiagnosticTextLimit(message, 2048),
		httpExceptionType(status),
		itoa(status),
	))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
