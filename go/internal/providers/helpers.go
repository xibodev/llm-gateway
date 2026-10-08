package providers

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"llmgw/internal/config"

	core "github.com/xibodev/llmgw-core"
)

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func openAICompatibleBase(settings *config.Settings, cfg *config.ProviderConfig) string {
	if cfg != nil && cfg.BaseURL != "" {
		return cfg.BaseURL
	}
	if settings != nil && settings.OpenAICompatibleBaseURL != "" {
		return settings.OpenAICompatibleBaseURL
	}
	return "https://api.openai.com/v1"
}

func ensureProviderZenInvocation(ctx context.Context, _ Provider) (context.Context, error) {
	return ctx, nil
}

func ResetProviders() {
	Current().ResetProviders()
}

func RegisterQuotaAdapter(adapter QuotaAdapter) error {
	return Current().RegisterQuotaAdapter(adapter)
}

func QuotaAdapterByID(id string) (QuotaAdapter, bool) {
	return Current().QuotaAdapterByID(id)
}

func ResetCircuit(name string) {
	Current().ResetCircuit(name)
}

func OpenCircuits(name string, now time.Time) []OpenCircuit {
	return Current().OpenCircuits(name, now)
}

func ForgetProvider(pid string) {
	Current().ForgetProvider(pid)
}

func ForgetProviderForPrincipal(providerID, principalID string) {
	Current().ForgetProviderForPrincipal(providerID, principalID)
}

func ForgetCatalog(pid string) {
	Current().ForgetCatalog(pid)
}

func ForgetCatalogForPrincipal(providerID, principalID string) {
	Current().ForgetCatalogForPrincipal(providerID, principalID)
}

func ForgetCatalogForProject(providerID, projectID string) {
	Current().ForgetCatalogForProject(providerID, projectID)
}

func ForgetInheritedCatalogs(providerID string) {
	Current().ForgetInheritedCatalogs(providerID)
}

func CatalogCached(providerID string) ([]ModelInfo, time.Time) {
	return Current().CatalogCached(providerID)
}

func CatalogModels(providerID string) []ModelInfo {
	return Current().CatalogModels(providerID)
}

func CatalogModelsForPrincipal(providerID string, caller core.Caller) []ModelInfo {
	return Current().CatalogModelsForPrincipal(providerID, caller)
}

func CatalogCachedForPrincipal(providerID string, caller core.Caller) ([]ModelInfo, time.Time) {
	return Current().CatalogCachedForPrincipal(providerID, caller)
}

func CatalogCachedLookupForPrincipal(providerID, model string, caller core.Caller) (ModelInfo, bool) {
	return Current().CatalogCachedLookupForPrincipal(providerID, model, caller)
}

func CatalogLookup(providerID, model string) (ModelInfo, bool) {
	return Current().CatalogCachedLookupForPrincipal(providerID, model, gatewayCaller())
}

func CatalogSnapshot(providerID string) ([]ModelInfo, time.Time) {
	return Current().CatalogSnapshot(providerID)
}

func CatalogRefreshedAt(providerID string) time.Time {
	_, t := Current().CatalogCached(providerID)
	return t
}

func CatalogRefreshedAtForPrincipal(providerID string, caller core.Caller) time.Time {
	_, t := Current().CatalogCachedForPrincipal(providerID, caller)
	return t
}

func RefreshCatalog(providerID string) []ModelInfo {
	return Current().RefreshCatalog(providerID)
}

func RefreshCatalogForPrincipal(providerID string, caller core.Caller) []ModelInfo {
	return Current().RefreshCatalogForPrincipal(providerID, caller)
}

func RefreshCatalogForPrincipalWithError(providerID string, caller core.Caller) ([]ModelInfo, *CredentialObservation, error) {
	return Current().RefreshCatalogForPrincipalWithError(providerID, caller)
}

func ReadCachedCatalogForPrincipal(providerID string, caller core.Caller) CatalogReadResult {
	return Current().ReadCachedCatalogForPrincipal(providerID, caller)
}

func ReadCatalogForPrincipal(providerID string, caller core.Caller) CatalogReadResult {
	return Current().ReadCatalogForPrincipal(providerID, caller)
}

func CatalogLookupForPrincipal(providerID, model string, caller core.Caller) (ModelInfo, bool) {
	return Current().CatalogLookupForPrincipal(providerID, model, caller)
}

func GetProvider(providerID string) (Provider, error) {
	return Current().GetProvider(providerID)
}

func GetProviderForPrincipal(providerID string, caller core.Caller) (Provider, error) {
	return Current().GetProviderForPrincipal(providerID, caller)
}

func SpeechSynthesizerForPrincipal(providerID string, caller core.Caller) (SpeechSynthesizer, bool) {
	return Current().SpeechSynthesizerForPrincipal(providerID, caller)
}

func ProviderHTTPTarget(providerID string, caller core.Caller) (string, http.Header, bool) {
	return Current().ProviderHTTPTarget(providerID, caller)
}

func CoreServesSurfaceForPrincipal(providerID, model string, surface core.ModelSurface, caller core.Caller) bool {
	return Current().CoreServesSurfaceForPrincipal(providerID, model, surface, caller)
}

func InvokeCoreSurfaceForPrincipal(
	ctx context.Context, providerID string, caller core.Caller, request core.Request,
) (core.Response, bool, error) {
	return Current().InvokeCoreSurfaceForPrincipal(ctx, providerID, caller, request)
}

type ProviderAuthAdapter interface {
	ID() string
	AdapterID() string
	CredentialKind() string
}

type BrowserProviderAuthAdapter interface {
	ProviderAuthAdapter
	BrowserAuth(ctx context.Context, redirectURI string, extra any) (any, error)
}

type DeviceProviderAuthAdapter interface {
	ProviderAuthAdapter
	DeviceAuth(ctx context.Context) (any, error)
	PollDeviceToken(ctx context.Context, code string) (any, error)
}

type ManualProviderAuthAdapter interface {
	ProviderAuthAdapter
}

type OAuthTokens struct {
	Status       string
	AccessToken  string
	RefreshToken string
	IDToken      string
	AccountID    string
	AccountLabel string
	ProjectID    string
	TokenType    string
	ExpiresAt    int64
}

type GuardedRefreshProviderAuthAdapter interface {
	ProviderAuthAdapter
	RefreshConnection(ctx context.Context, principalID, providerID, name string) (OAuthTokens, any, error)
}

type RefreshableProviderAuthAdapter interface {
	ProviderAuthAdapter
	Refresh(ctx context.Context, envelope any) (OAuthTokens, error)
}

type RevocableProviderAuthAdapter interface {
	ProviderAuthAdapter
	Revoke(ctx context.Context, envelope any) error
}

type mockAuthAdapter struct {
	id string
}

func (m mockAuthAdapter) ID() string             { return m.id }
func (m mockAuthAdapter) AdapterID() string      { return m.id }
func (m mockAuthAdapter) CredentialKind() string { return "oauth" }
func (m mockAuthAdapter) BrowserAuth(context.Context, string, any) (any, error) {
	return nil, errors.New("OAuth is handled via oauthflow service or extension")
}
func (m mockAuthAdapter) DeviceAuth(context.Context) (any, error) {
	return nil, errors.New("OAuth is handled via oauthflow service or extension")
}
func (m mockAuthAdapter) PollDeviceToken(context.Context, string) (any, error) {
	return nil, errors.New("OAuth is handled via oauthflow service or extension")
}

func NewProviderAuthAdapter(adapterID, providerID string) (mockAuthAdapter, error) {
	return mockAuthAdapter{id: adapterID}, nil
}

// EffectiveCodexClientID returns the Codex OAuth client ID settings configure,
// which a Codex sign-in names to the companion daemon. Empty means none: the
// daemon then signs in with its own client. The caller passes its settings
// snapshot, so this layer adds no read of the published settings.
func EffectiveCodexClientID(settings *config.Settings) string {
	if settings == nil {
		return ""
	}
	return strings.TrimSpace(settings.OpenAICodexClientID)
}
