package providers

import (
	"strings"

	"llmgw/internal/config"
	"llmgw/internal/iam"
)

func AutomationManagedAnonymousProvider(providerID string) (bool, error) {
	cfg, found := config.Provider(providerID)
	if !found || cfg == nil {
		return false, nil
	}
	for _, profile := range AnonymousProviderProfiles() {
		if profile.ProviderID != providerID {
			continue
		}
		connectionExists, err := iam.ActiveProviderConnectionExists(providerID)
		if err != nil {
			return false, err
		}
		managed, err := iam.AnonymousProviderManaged(providerID)
		if err != nil {
			return false, err
		}
		return managed && !connectionExists &&
			EffectiveRegistryID(providerID, cfg.RegistryID, cfg.Type) == profile.RegistryID &&
			strings.EqualFold(strings.TrimSpace(cfg.Type), profile.RuntimeType) &&
			strings.TrimRight(cfg.BaseURL, "/") == strings.TrimRight(profile.BaseURL, "/") &&
			AnonymousAPIKey(config.ResolveProviderAPIKey(providerID, cfg)), nil
	}
	return false, nil
}

func AnonymousModelPublication(providerID, model string) (iam.ProviderModelEvidence, bool, error) {
	managed, err := AutomationManagedAnonymousProvider(providerID)
	if err != nil {
		return iam.ProviderModelEvidence{}, false, err
	}
	if !managed {
		return iam.ProviderModelEvidence{}, true, nil
	}
	evidence, err := iam.ProviderModelEvidenceFor(providerID, "")
	if err != nil {
		return iam.ProviderModelEvidence{}, false, err
	}
	current, found := evidence[model]
	return current, found && current.State == "verified", nil
}
