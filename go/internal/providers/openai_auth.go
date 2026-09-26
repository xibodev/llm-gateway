package providers

import (
	"net/http"
	"strings"
)

// normalizeBearerKey reads an OpenAI-wire key as the gateway reads one:
// trimmed, without a "Bearer " prefix, and none for "free" and "none", the
// sentinels of anonymous access. Core's OpenAI-compatible providers read a
// key the same way; the gateway still reads one for the requests it sends
// on its own path.
func normalizeBearerKey(value string) string {
	key := strings.TrimSpace(value)
	if strings.HasPrefix(strings.ToLower(key), "bearer ") {
		key = strings.TrimSpace(key[7:])
	}
	if strings.EqualFold(key, "free") || strings.EqualFold(key, "none") {
		key = ""
	}
	return key
}

// AnonymousAPIKey reports a key that asks for anonymous access: none, or the
// "public" bearer an anonymous registry entry is sent.
func AnonymousAPIKey(value string) bool {
	key := normalizeBearerKey(value)
	return key == "" || strings.EqualFold(key, "public")
}

// openAIHeader is what the gateway sends an OpenAI-compatible upstream it
// calls on its own path, its catalog and the endpoints it proxies: JSON,
// and the key as the bearer unless it normalizes to none.
func openAIHeader(apiKey string) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	if key := normalizeBearerKey(apiKey); key != "" {
		h.Set("Authorization", "Bearer "+key)
	}
	return h
}
