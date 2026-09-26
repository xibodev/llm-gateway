package providers

import (
	"fmt"
	"strings"

	"llmgw/internal/config"

	core "github.com/xibodev/llmgw-core"
	coreproviders "github.com/xibodev/llmgw-core/providers"
)

// azureCoreType is Azure OpenAI's key in coreVerticals. The facade names it
// on each operation, so the core Runtime's credential store loads Azure's
// keys from Azure's store.
const azureCoreType = "azure_openai"

// azureCoreVertical is Azure OpenAI as the core Runtime serves it: core's
// AzureOpenAI on the Chat Completions surface of one resource, and the API
// key the provider factory resolves for an Azure instance. API keys never
// refresh, so there is no refresh.
//
// Its store is a connectionStore of API keys, the one kind the factory
// accepts. Core's AzureOpenAI sends nothing without a key, so an instance
// without one is refused before a request is made, where the transport sent
// an empty api-key and read the resource's 401.
func (rt *Runtime) azureCoreVertical() coreVertical {
	return coreVertical{
		serves: func(_ *config.Settings, _ string, cfg *config.ProviderConfig) bool {
			return strings.ToLower(strings.TrimSpace(cfg.Type)) == "azure_openai"
		},
		provider: func(settings *config.Settings, instance string) (core.Provider, error) {
			cfg := settings.Providers[instance]
			timeout := settings.OpenAICompatibleTimeoutSeconds
			if cfg.Timeout != nil {
				timeout = *cfg.Timeout
			}
			return newCoreAzure(instance, cfg.BaseURL, timeout)
		},
		credentials: connectionStore{
			open:       func() (core.CredentialStore, error) { return rt.openCredentials(false) },
			tokenTypes: []string{core.TokenTypeAPIKey},
			refusal:    "azure_openai: the resolved connection is not an API key",
		},
	}
}

// newCoreAzure builds core's AzureOpenAI for the resource at baseURL, which
// core normalizes as azureInferenceBaseURL does. Its requests time out as the
// transport's did, after timeout, and never when it is zero. Core's catalog
// client is never used, since the catalog stays on the gateway's path.
func newCoreAzure(instance, baseURL string, timeout float64) (*coreproviders.AzureOpenAI, error) {
	azure, err := coreproviders.NewAzureOpenAI(coreproviders.AzureOpenAIConfig{
		BaseURL: baseURL, Client: httpClient(timeout),
	})
	if err != nil {
		return nil, &ConfigError{Msg: fmt.Sprintf("provider '%s': initialize Azure OpenAI: %v", instance, err)}
	}
	return azure, nil
}
