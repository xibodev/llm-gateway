package providers

import (
	"fmt"
	"net/http"
	"strings"

	"llmgw/internal/config"

	core "github.com/xibodev/llmgw-core"
	coreproviders "github.com/xibodev/llmgw-core/providers"
)

// bedrockCoreType is Amazon Bedrock's key in coreVerticals. The facade names
// it on each operation, so the core Runtime's credential store loads
// Bedrock's keys from Bedrock's store.
const bedrockCoreType = "bedrock"

// bedrockCoreVertical is Amazon Bedrock as the core Runtime serves it: core's
// Bedrock, an OpenAICompatible at Bedrock's OpenAI-compatible endpoint, over
// the caller's cached catalog, and the API key the provider factory resolves
// for a Bedrock instance, or none. A Bedrock API key is a bearer token that
// never refreshes, so nothing is signed and there is no refresh.
//
// Its store is a connectionStore of API keys, the one kind the factory
// accepts, and a resolved key is normalized as the transport normalized a
// bearer key.
func (rt *Runtime) bedrockCoreVertical() coreVertical {
	return coreVertical{
		serves: func(_ *config.Settings, _ string, cfg *config.ProviderConfig) bool {
			return bedrockInstance(cfg)
		},
		provider: func(settings *config.Settings, instance string) (core.Provider, error) {
			return rt.newOpenAICoreProvider(bedrockCore(instance, settings.Providers[instance]))
		},
		credentials: connectionStore{
			open:       func() (core.CredentialStore, error) { return rt.openCredentials(false) },
			tokenTypes: []string{core.TokenTypeAPIKey},
			refusal:    "bedrock: the resolved connection is not an API key",
		},
	}
}

// bedrockInstance reports whether a configured provider is Amazon Bedrock,
// as the provider factory reads its type.
func bedrockInstance(cfg *config.ProviderConfig) bool {
	return strings.EqualFold(strings.TrimSpace(cfg.Type), "bedrock")
}

// bedrockCore is how a Bedrock instance's core provider is built: at its
// base URL, such as a bedrock-mantle or VPC endpoint, or else at its
// region's endpoint, which core refuses to build unless the region is a
// region name; with its configured adaptation; and with requests that time
// out after the instance's timeout, or core's default of five minutes when
// it configures none, where the transport's never timed out.
//
// The transport read the registry entry of a Bedrock instance only to serve
// Responses for every model of the openai entry, so only that entry reaches
// core, which would also send Chat for the Pollinations entry elsewhere.
func bedrockCore(instance string, cfg *config.ProviderConfig) openAICoreSpec {
	registryID := ""
	if EffectiveRegistryID(instance, cfg.RegistryID, cfg.Type) == "openai" {
		registryID = "openai"
	}
	var client *http.Client
	if cfg.Timeout != nil {
		client = httpClient(*cfg.Timeout)
	}
	region, baseURL := cfg.Region, cfg.BaseURL
	return openAICoreSpec{
		instance: instance,
		config:   coreproviders.OpenAICompatibleConfig{RegistryID: registryID, ForceAPISupport: cfg.ForceApiSupport, Client: client},
		build: func(config coreproviders.OpenAICompatibleConfig) (*coreproviders.OpenAICompatible, error) {
			provider, err := coreproviders.NewBedrock(region, baseURL, config)
			if err != nil {
				return nil, &ConfigError{Msg: fmt.Sprintf("provider '%s': initialize Bedrock: %v", instance, err)}
			}
			return provider, nil
		},
	}
}
