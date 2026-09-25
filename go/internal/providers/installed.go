package providers

import copilotauth "github.com/xibodev/llm-provider-auth/copilot"

// The functions in this file are the forms of Runtime methods that act on the
// installed Runtime, for callers that do not own one. They shrink as those
// callers take a Runtime explicitly.

// CopilotAuth returns the installed Runtime's Copilot authentication client.
func CopilotAuth() *copilotauth.Client { return Current().CopilotAuth() }

// ResetCircuit clears the installed Runtime's breaker state (test helper).
func ResetCircuit(name string) { Current().ResetCircuit(name) }

func RegisterProviderAuthAdapterFactory(id string, factory ProviderAuthAdapterFactory) error {
	return Current().RegisterProviderAuthAdapterFactory(id, factory)
}

func NewProviderAuthAdapter(adapterID, providerID string) (ProviderAuthAdapter, error) {
	return Current().NewProviderAuthAdapter(adapterID, providerID)
}

func RegisterQuotaAdapter(adapter QuotaAdapter) error {
	return Current().RegisterQuotaAdapter(adapter)
}

func QuotaAdapterByID(id string) (QuotaAdapter, bool) {
	return Current().QuotaAdapterByID(id)
}
