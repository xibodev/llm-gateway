package providers

import (
	"net/http"

	"llmgw/internal/config"
)

// ProviderHTTPTarget returns the resolved base URL + auth headers for an
// OpenAI-standard provider, so non-chat endpoints (audio transcription/speech)
// can be reverse-proxied to the provider using the same auth + base URL the chat
// transport uses. ok is false for non-OpenAI providers (anthropic, ollama) or
// when auth can't be prepared.
func ProviderHTTPTarget(
	providerID string, principal *config.Principal,
) (baseURL string, headers http.Header, ok bool) {
	cfg, exists := config.Get().Providers[providerID]
	if !exists {
		return "", nil, false
	}
	inst, err := instantiate(providerID, cfg, principal)
	if err != nil {
		return "", nil, false
	}
	op, isOpenAI := inst.(OpenAIProvider)
	if !isOpenAI {
		return "", nil, false
	}
	base, hdr, err := op.auth.Prepare()
	if err != nil {
		return "", nil, false
	}
	return base, hdr, true
}
