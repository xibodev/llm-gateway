// Package api assembles the HTTP server: OpenAI + Anthropic + models + health
// facades, the /admin control panel, bearer auth, and error envelopes.
package api

import (
	"encoding/json"
	"net/http"

	"llmgw/internal/providers"
)

// errorPayload matches the Python error envelope shape. OpenAI's clients
// pick the fields they know out of its error, so the request's ID can sit
// beside them.
func errorPayload(message, errorType, code, requestID string) map[string]any {
	body := map[string]any{"message": message, "type": errorType, "code": code}
	if requestID != "" {
		body["request_id"] = requestID
	}
	return map[string]any{"error": body}
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

// anthropicErrorType is the error type the Messages API documents for an
// HTTP status. Anthropic clients decide on it, as they retry an
// overloaded_error or a rate_limit_error but not an invalid_request_error.
func anthropicErrorType(status int) string {
	switch {
	case status == 401:
		return "authentication_error"
	case status == 403:
		return "permission_error"
	case status == 404:
		return "not_found_error"
	case status == 429:
		return "rate_limit_error"
	case status == 529:
		return "overloaded_error"
	case status >= 500:
		return "api_error"
	default:
		return "invalid_request_error"
	}
}

// anthropicErrorWriter carries the response to a request on a Messages
// route, whose clients read Anthropic's error envelope. Every error a
// handler writes goes through writeError, so marking the writer once, on
// the way in, answers the whole route as Anthropic does.
type anthropicErrorWriter struct{ http.ResponseWriter }

// Unwrap lets http.ResponseController reach the writer that flushes a
// stream.
func (w anthropicErrorWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// anthropicErrors marks the responses to the Messages routes; see
// anthropicErrorWriter. It runs after path aliases are resolved.
func anthropicErrors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/messages", "/v1/messages/count_tokens":
			w = anthropicErrorWriter{w}
		}
		next.ServeHTTP(w, r)
	})
}

// answersAnthropic reports a response marked by anthropicErrors, through
// any writer wrapped around it since.
func answersAnthropic(w http.ResponseWriter) bool {
	for {
		if _, ok := w.(anthropicErrorWriter); ok {
			return true
		}
		wrapper, ok := w.(interface{ Unwrap() http.ResponseWriter })
		if !ok {
			return false
		}
		w = wrapper.Unwrap()
	}
}

// writeJSON writes v as JSON with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes the error envelope the route's clients read: Anthropic's
// on the Messages routes, typed by status, and the standard one elsewhere.
// Either names the request by the ID assignRequestIDs put on the response,
// since a client's report of a failure is often only the body it read.
func writeError(w http.ResponseWriter, status int, message string) {
	message = providers.SanitizeDiagnosticTextLimit(message, 2048)
	requestID := w.Header().Get(requestIDHeader)
	if answersAnthropic(w) {
		// Anthropic's own envelope carries request_id beside its error.
		envelope := map[string]any{"type": "error", "error": map[string]any{
			"type": anthropicErrorType(status), "message": message,
		}}
		if requestID != "" {
			envelope["request_id"] = requestID
		}
		writeJSON(w, status, envelope)
		return
	}
	writeJSON(w, status, errorPayload(message, httpExceptionType(status), itoa(status), requestID))
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
