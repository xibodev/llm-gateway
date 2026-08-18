package api

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"llmgw/internal/config"
	"llmgw/internal/iam"
)

type principalKey struct{}

// validKeys returns all admin API keys the gateway accepts.
func validKeys() []string {
	s := config.Get()
	var keys []string
	if s.APIKey != "" {
		keys = append(keys, s.APIKey)
	}
	for _, k := range s.APIKeys {
		if k != "" {
			keys = append(keys, k)
		}
	}
	return keys
}

func extractAPIKey(r *http.Request) string {
	if x := r.Header.Get("x-api-key"); x != "" {
		return x
	}
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return ""
	}
	scheme, token, ok := strings.Cut(auth, " ")
	if !ok || !strings.EqualFold(scheme, "bearer") || token == "" {
		return ""
	}
	return token
}

func matchesAnyKey(token string, keys []string) bool {
	matched := false
	for _, k := range keys {
		if subtle.ConstantTimeCompare([]byte(token), []byte(k)) == 1 {
			matched = true
		}
	}
	return matched
}

// requireAPIKey authenticates a request and returns its principal (or an error
// status). Mirrors the Python require_api_key: open-local, admin key, or a
// minted project key.
func requireAPIKey(r *http.Request) (*config.Principal, int, string) {
	token := extractAPIKey(r)
	adminKeys := validKeys()
	s := config.Get()

	if s.AllowUnauthenticatedAPI {
		if token != "" {
			p, found, err := iam.ResolveAPIKey(token)
			if err != nil {
				return nil, http.StatusInternalServerError, "Identity store unavailable."
			}
			if found {
				return p, 0, ""
			}
		}
		return &config.Principal{Project: "local", Key: "local"}, 0, ""
	}

	if token != "" && len(adminKeys) > 0 && matchesAnyKey(token, adminKeys) {
		return &config.Principal{Project: "admin", Key: "admin"}, 0, ""
	}
	if token != "" {
		p, found, err := iam.ResolveAPIKey(token)
		if err != nil {
			return nil, http.StatusInternalServerError, "Identity store unavailable."
		}
		if found {
			return p, 0, ""
		}
	}
	hasKeys, err := iam.HasAPIKeys()
	if err != nil {
		return nil, http.StatusInternalServerError, "Identity store unavailable."
	}
	if len(adminKeys) == 0 && !hasKeys {
		return nil, http.StatusInternalServerError, "No API key configured. Set LLMGW_API_KEY, " +
			"mint a project key in /admin, or set LLMGW_ALLOW_UNAUTHENTICATED_API=1 for local use."
	}
	return nil, http.StatusUnauthorized, "Invalid API key"
}

// authed wraps a handler that needs a principal. On failure it writes the error
// envelope and returns false.
func authed(w http.ResponseWriter, r *http.Request) (*config.Principal, bool) {
	principal, status, msg := requireAPIKey(r)
	if status != 0 {
		if status == http.StatusUnauthorized {
			w.Header().Set("WWW-Authenticate", "Bearer")
		}
		writeError(w, status, msg)
		return nil, false
	}
	return principal, true
}
