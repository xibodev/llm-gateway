package providers

import (
	_ "embed"
	"fmt"
	"regexp"

	coreproviders "github.com/xibodev/llmgw-core/providers"
)

// registryOverlayJSON is the gateway's curation of the core provider registry.
// The reviewed manifest and its validator live in llmgw-core; the gateway
// layers presentation, priority and curation changes on it here, and adds or
// removes entries. registry_snapshot.json pins the effective registry, which
// the website and documentation checks read.
//
//go:embed registry_overlay.json
var registryOverlayJSON []byte

var providerRegistry = mustLoadProviderRegistry(registryOverlayJSON)

var registryIdentifierPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

func mustLoadProviderRegistry(overlayJSON []byte) *coreproviders.Registry {
	registry, err := loadProviderRegistry(overlayJSON)
	if err != nil {
		panic("provider registry: " + err.Error())
	}
	return registry
}

// loadProviderRegistry applies the gateway overlay to the core registry and
// validates the result against the runtime types this gateway can execute.
func loadProviderRegistry(overlayJSON []byte) (*coreproviders.Registry, error) {
	overlay, err := coreproviders.DecodeOverlay(overlayJSON)
	if err != nil {
		return nil, fmt.Errorf("overlay: %w", err)
	}
	return coreproviders.DefaultRegistry().WithOverlay(
		overlay, coreproviders.ValidationOptions{RuntimeTypes: ProviderTypes},
	)
}
