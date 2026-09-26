package providers

import (
	"fmt"
	"strings"

	"llmgw/internal/config"

	core "github.com/xibodev/llmgw-core"
	coreproviders "github.com/xibodev/llmgw-core/providers"
)

// ollamaCoreType is Ollama's key in coreVerticals. The facade names it on
// each operation, so the core Runtime's credential store reaches Ollama's.
const ollamaCoreType = "ollama"

// ollamaCoreVertical is Ollama as the core Runtime serves it: core's Ollama
// at the instance's daemon root, with the instance's timeout. Ollama is
// keyless, so its store is empty: no caller resolves a credential, and the
// Runtime sends every request without one. Nothing refreshes.
func (rt *Runtime) ollamaCoreVertical() coreVertical {
	return coreVertical{
		serves: func(_ *config.Settings, _ string, cfg *config.ProviderConfig) bool {
			return ollamaInstance(cfg)
		},
		provider: func(settings *config.Settings, instance string) (core.Provider, error) {
			cfg := settings.Providers[instance]
			ollama, err := newCoreOllama(instance, ollamaBase(settings, cfg), cfg.TimeoutOr(settings.OllamaTimeoutSeconds))
			if err != nil {
				return nil, err
			}
			return ollama, nil
		},
		credentials: core.NewMemoryCredentialStore(),
	}
}

// ollamaInstance reports whether a configured provider is an Ollama daemon,
// as the provider factory reads its type.
func ollamaInstance(cfg *config.ProviderConfig) bool {
	return strings.EqualFold(strings.TrimSpace(cfg.Type), "ollama")
}

// ollamaBase is the daemon root of an Ollama instance: its own, or the
// configured default when it names none.
func ollamaBase(settings *config.Settings, cfg *config.ProviderConfig) string {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return settings.OllamaBaseURL
	}
	return cfg.BaseURL
}

// newCoreOllama builds core's Ollama at base. Its requests time out after
// timeout seconds and its catalog after at most ten, as the gateway's
// transport timed them. A base that is not a daemon root is a configuration
// error, whose message is the issue core found and never the base.
func newCoreOllama(instance, base string, timeout float64) (*coreproviders.Ollama, error) {
	ollama, err := coreproviders.NewOllama(coreproviders.OllamaConfig{
		BaseURL: base, Client: httpClient(timeout), CatalogClient: httpClient(min(timeout, 10)),
	})
	if err != nil {
		return nil, &ConfigError{Msg: fmt.Sprintf("provider '%s': %v", instance, err)}
	}
	return ollama, nil
}
