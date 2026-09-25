package providers

import (
	"net/http"
	"strings"
)

// OpenAIAuth decouples authentication from the OpenAI wire transport. It
// resolves the base URL + request headers for a call. This is why one
// transport (OpenAIProvider) serves every OpenAI-compatible backend —
// openai_compatible, bedrock, litellm — which differ ONLY in how a request is
// authenticated + where it points. GitHub Copilot, whose session a 401 could
// replace, is served by llmgw-core's Copilot vertical, and its catalog reaches
// the transport with a session already exchanged (see copilotTarget).
type OpenAIAuth interface {
	// Prepare resolves the base URL and headers for a request.
	Prepare() (baseURL string, headers http.Header, err error)
}

type observedOpenAIAuth interface {
	PrepareObserved() (
		baseURL string,
		headers http.Header,
		observation *CredentialObservation,
		err error,
	)
}

func prepareOpenAIAuth(
	auth OpenAIAuth,
) (string, http.Header, *CredentialObservation, error) {
	if observed, ok := auth.(observedOpenAIAuth); ok {
		return observed.PrepareObserved()
	}
	baseURL, headers, err := auth.Prepare()
	return baseURL, headers, nil, err
}

// bearerAuth is a static base URL + optional Bearer key: openai_compatible,
// bedrock (token), litellm, localai, and any keyless local server.
type bearerAuth struct {
	base        string
	apiKey      string
	observation *CredentialObservation
}

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

func AnonymousAPIKey(value string) bool {
	key := normalizeBearerKey(value)
	return key == "" || strings.EqualFold(key, "public")
}

func (a bearerAuth) Prepare() (string, http.Header, error) {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	if key := normalizeBearerKey(a.apiKey); key != "" {
		h.Set("Authorization", "Bearer "+key)
	}
	return a.base, h, nil
}

func (a bearerAuth) PrepareObserved() (
	string, http.Header, *CredentialObservation, error,
) {
	baseURL, headers, err := a.Prepare()
	return baseURL, headers, a.observation, err
}
