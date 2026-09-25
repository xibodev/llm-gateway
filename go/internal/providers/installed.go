package providers

import (
	"net/http"
	"time"

	copilotauth "github.com/xibodev/llm-provider-auth/copilot"
	core "github.com/xibodev/llmgw-core"
)

// The functions in this file are the forms of Runtime methods that act on the
// installed Runtime, for callers that do not own one. They shrink as those
// callers take a Runtime explicitly.

func GetProvider(providerID string) (Provider, error) {
	return Current().GetProvider(providerID)
}

func GetProviderForPrincipal(providerID string, caller core.Caller) (Provider, error) {
	return Current().GetProviderForPrincipal(providerID, caller)
}

func SpeechSynthesizerForPrincipal(providerID string, caller core.Caller) (SpeechSynthesizer, bool) {
	return Current().SpeechSynthesizerForPrincipal(providerID, caller)
}

func ListProviderModels(providerID string) []ModelInfo {
	return Current().ListProviderModels(providerID)
}

func ListProviderModelsForPrincipal(providerID string, caller core.Caller) []ModelInfo {
	return Current().ListProviderModelsForPrincipal(providerID, caller)
}

func ListProviderModelsForPrincipalWithError(
	providerID string, caller core.Caller,
) ([]ModelInfo, *CredentialObservation, error) {
	return Current().ListProviderModelsForPrincipalWithError(providerID, caller)
}

func ProviderHTTPTarget(providerID string, caller core.Caller) (baseURL string, headers http.Header, ok bool) {
	return Current().ProviderHTTPTarget(providerID, caller)
}

func ResetProviders() { Current().ResetProviders() }

func ForgetProvider(providerID string) { Current().ForgetProvider(providerID) }

func ForgetProviderForPrincipal(providerID, principalID string) {
	Current().ForgetProviderForPrincipal(providerID, principalID)
}

func CatalogModels(providerID string) []ModelInfo { return Current().CatalogModels(providerID) }

func CatalogModelsForPrincipal(providerID string, caller core.Caller) []ModelInfo {
	return Current().CatalogModelsForPrincipal(providerID, caller)
}

func ReadCatalogForPrincipal(providerID string, caller core.Caller) CatalogReadResult {
	return Current().ReadCatalogForPrincipal(providerID, caller)
}

func ReadCachedCatalogForPrincipal(providerID string, caller core.Caller) CatalogReadResult {
	return Current().ReadCachedCatalogForPrincipal(providerID, caller)
}

func RefreshCatalog(providerID string) []ModelInfo { return Current().RefreshCatalog(providerID) }

func RefreshCatalogForPrincipal(providerID string, caller core.Caller) []ModelInfo {
	return Current().RefreshCatalogForPrincipal(providerID, caller)
}

func RefreshCatalogForPrincipalWithError(
	providerID string, caller core.Caller,
) ([]ModelInfo, *CredentialObservation, error) {
	return Current().RefreshCatalogForPrincipalWithError(providerID, caller)
}

func CatalogLookup(providerID, model string) (ModelInfo, bool) {
	return Current().CatalogLookup(providerID, model)
}

func CatalogLookupForPrincipal(providerID, model string, caller core.Caller) (ModelInfo, bool) {
	return Current().CatalogLookupForPrincipal(providerID, model, caller)
}

func CatalogCachedLookupForPrincipal(providerID, model string, caller core.Caller) (ModelInfo, bool) {
	return Current().CatalogCachedLookupForPrincipal(providerID, model, caller)
}

func CatalogRefreshedAt(providerID string) time.Time { return Current().CatalogRefreshedAt(providerID) }

func CatalogRefreshedAtForPrincipal(providerID string, caller core.Caller) time.Time {
	return Current().CatalogRefreshedAtForPrincipal(providerID, caller)
}

func CatalogCached(providerID string) ([]ModelInfo, time.Time) {
	return Current().CatalogCached(providerID)
}

func CatalogCachedForPrincipal(providerID string, caller core.Caller) ([]ModelInfo, time.Time) {
	return Current().CatalogCachedForPrincipal(providerID, caller)
}

func CatalogSnapshot(providerID string) ([]ModelInfo, time.Time) {
	return Current().CatalogSnapshot(providerID)
}

func ForgetCatalog(providerID string) { Current().ForgetCatalog(providerID) }

func ForgetCatalogForPrincipal(providerID, principalID string) {
	Current().ForgetCatalogForPrincipal(providerID, principalID)
}

func ForgetInheritedCatalogs(providerID string) { Current().ForgetInheritedCatalogs(providerID) }

func ForgetCatalogForProject(providerID, projectID string) {
	Current().ForgetCatalogForProject(providerID, projectID)
}

func NewGatewayProviderOrchestrator(profiles []AnonymousProviderProfile) (*core.ProviderOrchestrator, error) {
	return Current().NewGatewayProviderOrchestrator(profiles)
}

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
