package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"llmgw/internal/config"
	"llmgw/internal/diagnostics"
	"llmgw/internal/iam"

	antigravityauth "github.com/xibodev/llm-provider-auth/antigravity"
	codexauth "github.com/xibodev/llm-provider-auth/codex"
	copilotauth "github.com/xibodev/llm-provider-auth/copilot"
)

const (
	maxProviderAuthDiagnosticChars = 300
	DefaultCodexClientID           = "app_EMoamEEZ73f0CkXaXp7hrann"
	codexOAuthProfileDevice        = "device_client_id"
)

func EffectiveCodexClientID() string {
	if clientID := strings.TrimSpace(config.Get().OpenAICodexClientID); clientID != "" {
		return clientID
	}
	return DefaultCodexClientID
}

type ProviderAuthCapabilities struct {
	DeviceCode      bool `json:"device_code"`
	BrowserCallback bool `json:"browser_callback"`
	TokenImport     bool `json:"token_import"`
	Refresh         bool `json:"refresh"`
	Revoke          bool `json:"revoke"`
}

type ProviderAuthStart struct {
	DeviceCode      string
	UserCode        string
	VerificationURI string
	Interval        int
	ExpiresIn       int
	PrivateState    string
}

type ProviderAuthPoll struct {
	Status        string
	Error         string
	AccessToken   string
	RefreshToken  string
	IDToken       string
	TokenType     string
	ExpiresAt     int64
	AccountID     string
	AccountLabel  string
	ProjectID     string
	OAuthProfile  string
	OAuthClientID string
}

// SafeProviderAuthPoll defends the API boundary from an adapter that returns
// unsanitized diagnostics while retaining the internal token result.
func SafeProviderAuthPoll(result ProviderAuthPoll) ProviderAuthPoll {
	result.Error = diagnostics.SanitizeTextLimit(result.Error, maxProviderAuthDiagnosticChars)
	return result
}

type ProviderAuthRefresh struct {
	Status       string
	AccessToken  string
	RefreshToken string
	IDToken      string
	TokenType    string
	ExpiresAt    int64
	AccountID    string
	AccountLabel string
	ProjectID    string
}

type ProviderAuthBrowserStart struct {
	AuthorizationURL string
	PrivateState     string
	ExpiresIn        int
}

type ProviderAuthManualConfig struct {
	ClientID     string
	ClientSecret string
	ClientMode   string
	RedirectURI  string
}

type ProviderAuthImport struct {
	Fields map[string]string
}

type ProviderAuthAdapter interface {
	ID() string
	CredentialKind() string
	Capabilities() ProviderAuthCapabilities
}

type DeviceProviderAuthAdapter interface {
	ProviderAuthAdapter
	StartDevice(context.Context) (ProviderAuthStart, error)
	PollDevice(context.Context, string, string) ProviderAuthPoll
}

type BrowserProviderAuthAdapter interface {
	ProviderAuthAdapter
	StartBrowser(context.Context, string) (ProviderAuthBrowserStart, error)
	CompleteBrowser(context.Context, string, string) (ProviderAuthPoll, error)
}

type ManualProviderAuthAdapter interface {
	ProviderAuthAdapter
	StartManual(context.Context, ProviderAuthManualConfig) (ProviderAuthBrowserStart, error)
	CompleteManual(context.Context, string, string, ProviderAuthManualConfig) (ProviderAuthPoll, error)
}

type ImportProviderAuthAdapter interface {
	ProviderAuthAdapter
	Import(context.Context, ProviderAuthImport) (ProviderAuthPoll, error)
}

type RefreshableProviderAuthAdapter interface {
	ProviderAuthAdapter
	Refresh(context.Context, iam.OAuthTokenEnvelope) (ProviderAuthRefresh, error)
}

type GuardedRefreshProviderAuthAdapter interface {
	ProviderAuthAdapter
	RefreshConnection(
		context.Context, string, string, string,
	) (iam.OAuthTokenEnvelope, iam.ProviderConnection, error)
}

type RevocableProviderAuthAdapter interface {
	ProviderAuthAdapter
	Revoke(context.Context, iam.OAuthTokenEnvelope) error
}

type ProviderAuthAdapterFactory func(providerID string) (ProviderAuthAdapter, error)

var providerAuthAdapters = struct {
	sync.RWMutex
	factories map[string]ProviderAuthAdapterFactory
}{factories: map[string]ProviderAuthAdapterFactory{
	"github_copilot": func(string) (ProviderAuthAdapter, error) {
		return githubCopilotAuthAdapter{}, nil
	},
	"openai_codex": func(string) (ProviderAuthAdapter, error) {
		clientID := EffectiveCodexClientID()
		return openAICodexAuthAdapter{clientID: clientID}, nil
	},
	"google_antigravity": func(providerID string) (ProviderAuthAdapter, error) {
		return googleAntigravityAuthAdapter{providerID: providerID}, nil
	},
}}

func RegisterProviderAuthAdapterFactory(id string, factory ProviderAuthAdapterFactory) error {
	id = strings.ToLower(strings.TrimSpace(id))
	if !registryIdentifierPattern.MatchString(id) {
		return fmt.Errorf("invalid provider auth adapter id %q", id)
	}
	if factory == nil {
		return fmt.Errorf("provider auth adapter factory is required")
	}
	providerAuthAdapters.Lock()
	defer providerAuthAdapters.Unlock()
	if _, exists := providerAuthAdapters.factories[id]; exists {
		return fmt.Errorf("provider auth adapter %q is already registered", id)
	}
	providerAuthAdapters.factories[id] = factory
	return nil
}

func NewProviderAuthAdapter(adapterID, providerID string) (ProviderAuthAdapter, error) {
	adapterID = strings.ToLower(strings.TrimSpace(adapterID))
	providerAuthAdapters.RLock()
	factory, ok := providerAuthAdapters.factories[adapterID]
	providerAuthAdapters.RUnlock()
	if !ok {
		return nil, fmt.Errorf("provider auth adapter %q is not registered", adapterID)
	}
	adapter, err := factory(strings.TrimSpace(providerID))
	if err != nil {
		return nil, err
	}
	if adapter == nil || strings.TrimSpace(adapter.ID()) != adapterID {
		return nil, fmt.Errorf("provider auth adapter %q returned an invalid implementation", adapterID)
	}
	if err := validateProviderAuthAdapterCapabilities(adapter); err != nil {
		return nil, err
	}
	return adapter, nil
}

func validateProviderAuthAdapterCapabilities(adapter ProviderAuthAdapter) error {
	capabilities := adapter.Capabilities()
	_, device := adapter.(DeviceProviderAuthAdapter)
	_, browser := adapter.(BrowserProviderAuthAdapter)
	_, manual := adapter.(ManualProviderAuthAdapter)
	_, imported := adapter.(ImportProviderAuthAdapter)
	_, refreshed := adapter.(RefreshableProviderAuthAdapter)
	_, guardedRefresh := adapter.(GuardedRefreshProviderAuthAdapter)
	_, revoked := adapter.(RevocableProviderAuthAdapter)
	if capabilities.DeviceCode != device ||
		capabilities.BrowserCallback != (browser || manual) ||
		capabilities.TokenImport != imported ||
		capabilities.Refresh != (refreshed || guardedRefresh) ||
		capabilities.Revoke != revoked {
		return fmt.Errorf("provider auth adapter %q capability contract does not match implemented interfaces", adapter.ID())
	}
	return nil
}

func unregisterProviderAuthAdapterFactoryForTests(id string) {
	providerAuthAdapters.Lock()
	delete(providerAuthAdapters.factories, strings.ToLower(strings.TrimSpace(id)))
	providerAuthAdapters.Unlock()
}

type githubCopilotAuthAdapter struct{}

func (githubCopilotAuthAdapter) ID() string             { return "github_copilot" }
func (githubCopilotAuthAdapter) CredentialKind() string { return "github_oauth" }
func (githubCopilotAuthAdapter) Capabilities() ProviderAuthCapabilities {
	return ProviderAuthCapabilities{DeviceCode: true, Refresh: true}
}
func (githubCopilotAuthAdapter) StartDevice(context.Context) (ProviderAuthStart, error) {
	device, err := copilotauth.StartDeviceFlow()
	if err != nil {
		return ProviderAuthStart{}, err
	}
	return ProviderAuthStart{
		DeviceCode: device.DeviceCode, UserCode: device.UserCode,
		VerificationURI: device.VerificationURI, Interval: device.Interval, ExpiresIn: device.ExpiresIn,
	}, nil
}
func (githubCopilotAuthAdapter) PollDevice(
	_ context.Context, deviceCode, _ string,
) ProviderAuthPoll {
	result := copilotauth.PollDeviceFlowTokenOnce(deviceCode)
	return SafeProviderAuthPoll(ProviderAuthPoll{
		Status: result.Status, Error: result.Error, AccessToken: result.AccessToken,
	})
}
func (githubCopilotAuthAdapter) Refresh(
	_ context.Context, envelope iam.OAuthTokenEnvelope,
) (ProviderAuthRefresh, error) {
	session, err := copilotauth.GetSessionForOAuth(envelope.AccessToken, true)
	if err != nil {
		return ProviderAuthRefresh{}, err
	}
	return ProviderAuthRefresh{Status: "refreshed", ExpiresAt: session.ExpiresAt}, nil
}

type openAICodexAuthAdapter struct{ clientID string }

type codexDevicePrivateState struct {
	UserCode string `json:"user_code"`
	ClientID string `json:"client_id"`
}

func (openAICodexAuthAdapter) ID() string             { return "openai_codex" }
func (openAICodexAuthAdapter) CredentialKind() string { return "openai_codex_oauth" }
func (openAICodexAuthAdapter) Capabilities() ProviderAuthCapabilities {
	return ProviderAuthCapabilities{DeviceCode: true, Refresh: true, Revoke: true}
}
func (adapter openAICodexAuthAdapter) StartDevice(context.Context) (ProviderAuthStart, error) {
	flow, err := codexauth.StartDeviceFlow(adapter.clientID)
	if err != nil {
		return ProviderAuthStart{}, err
	}
	private, _ := json.Marshal(codexDevicePrivateState{UserCode: flow.UserCode, ClientID: adapter.clientID})
	return ProviderAuthStart{
		DeviceCode: flow.DeviceAuthID, UserCode: flow.UserCode,
		VerificationURI: flow.VerificationURI, Interval: flow.Interval,
		ExpiresIn: flow.ExpiresIn, PrivateState: string(private),
	}, nil
}
func (adapter openAICodexAuthAdapter) PollDevice(
	_ context.Context, deviceCode, privateState string,
) ProviderAuthPoll {
	state := codexDevicePrivateState{}
	if json.Unmarshal([]byte(privateState), &state) != nil || state.UserCode == "" || strings.TrimSpace(state.ClientID) == "" {
		return ProviderAuthPoll{
			Status: "error", Error: "Codex device authorization state is unavailable. Start again.",
		}
	}
	status, tokens, err := codexauth.PollAndExchange(codexauth.DeviceFlow{
		DeviceAuthID: deviceCode, UserCode: state.UserCode, ClientID: state.ClientID,
	})
	if err != nil {
		return SafeProviderAuthPoll(ProviderAuthPoll{Status: status, Error: err.Error()})
	}
	return ProviderAuthPoll{
		Status: status, AccessToken: tokens.AccessToken, RefreshToken: tokens.RefreshToken,
		IDToken: tokens.IDToken, TokenType: tokens.TokenType, ExpiresAt: tokens.ExpiresAt,
		AccountID: tokens.AccountID, AccountLabel: tokens.AccountLabel,
		OAuthProfile: codexOAuthProfileDevice, OAuthClientID: state.ClientID,
	}
}
func (adapter openAICodexAuthAdapter) RefreshConnection(
	_ context.Context, principalID, providerID, connectionName string,
) (iam.OAuthTokenEnvelope, iam.ProviderConnection, error) {
	return RefreshCodexOAuthConnection(principalID, providerID, connectionName)
}
func (adapter openAICodexAuthAdapter) Revoke(
	_ context.Context, envelope iam.OAuthTokenEnvelope,
) error {
	clientID, err := codexOAuthClientIDForEnvelope(envelope)
	if err != nil {
		return err
	}
	return codexauth.Revoke(clientID, envelope.RefreshToken, envelope.AccessToken)
}

type googleAntigravityAuthAdapter struct {
	providerID string
	oauth      *antigravityauth.Config
}

const (
	antigravityOAuthProfileRuntimeSecret  = "runtime_client_secret_post"
	antigravityOAuthProfilePublicPKCE     = "public_pkce"
	antigravityOAuthProfileConsumerManual = "consumer_manual"
)

var newGoogleAntigravityOAuthConfig = func(redirectURI string) antigravityauth.Config {
	settings := config.Get()
	return antigravityauth.Config{
		ClientID: settings.GoogleAntigravityClientID, ClientSecret: settings.GoogleAntigravityClientSecret,
		ClientAuthMode: antigravityauth.ClientAuthModeClientSecretPost,
		RedirectURI:    redirectURI,
	}
}

func (googleAntigravityAuthAdapter) ID() string             { return "google_antigravity" }
func (googleAntigravityAuthAdapter) CredentialKind() string { return "google_antigravity_oauth" }
func (googleAntigravityAuthAdapter) Capabilities() ProviderAuthCapabilities {
	return ProviderAuthCapabilities{BrowserCallback: true, Refresh: true}
}
func (adapter googleAntigravityAuthAdapter) StartBrowser(_ context.Context, redirectURI string) (ProviderAuthBrowserStart, error) {
	oauth := adapter.config(redirectURI)
	authorization, err := oauth.AuthorizationURL()
	if err != nil {
		return ProviderAuthBrowserStart{}, err
	}
	private, err := json.Marshal(map[string]string{
		"state": authorization.State, "code_verifier": authorization.CodeVerifier,
		"oauth_profile": antigravityOAuthProfile(oauth.ClientAuthMode), "oauth_client_id": oauth.ClientID,
	})
	if err != nil {
		return ProviderAuthBrowserStart{}, err
	}
	return ProviderAuthBrowserStart{AuthorizationURL: authorization.URL, PrivateState: string(private), ExpiresIn: 600}, nil
}
func (adapter googleAntigravityAuthAdapter) CompleteBrowser(ctx context.Context, code, privateState string) (ProviderAuthPoll, error) {
	var state struct {
		CodeVerifier  string `json:"code_verifier"`
		RedirectURI   string `json:"redirect_uri"`
		OAuthProfile  string `json:"oauth_profile"`
		OAuthClientID string `json:"oauth_client_id"`
	}
	if json.Unmarshal([]byte(privateState), &state) != nil || state.CodeVerifier == "" || state.RedirectURI == "" {
		return ProviderAuthPoll{}, fmt.Errorf("browser OAuth state is unavailable")
	}
	oauth := adapter.config(state.RedirectURI)
	if state.OAuthProfile != "" && (antigravityOAuthProfile(oauth.ClientAuthMode) != state.OAuthProfile || strings.TrimSpace(oauth.ClientID) != strings.TrimSpace(state.OAuthClientID)) {
		return ProviderAuthPoll{}, fmt.Errorf("browser OAuth client profile is no longer available")
	}
	tokens, err := oauth.Exchange(ctx, code, state.CodeVerifier)
	if err != nil {
		return ProviderAuthPoll{}, err
	}
	account, _ := oauth.DiscoverAccount(ctx, tokens.AccessToken)
	return ProviderAuthPoll{
		Status: "authorized", AccessToken: tokens.AccessToken, RefreshToken: tokens.RefreshToken,
		IDToken: tokens.IDToken, TokenType: tokens.TokenType, ExpiresAt: tokens.ExpiresAt.Unix(),
		ProjectID: account.ProjectID, OAuthProfile: antigravityOAuthProfile(oauth.ClientAuthMode), OAuthClientID: oauth.ClientID,
	}, nil
}

func (adapter googleAntigravityAuthAdapter) StartManual(_ context.Context, input ProviderAuthManualConfig) (ProviderAuthBrowserStart, error) {
	oauth, err := antigravityManualConfig(input)
	if err != nil {
		return ProviderAuthBrowserStart{}, err
	}
	authorization, err := oauth.AuthorizationURL()
	if err != nil {
		return ProviderAuthBrowserStart{}, err
	}
	private, err := json.Marshal(map[string]string{
		"state": authorization.State, "code_verifier": authorization.CodeVerifier,
	})
	if err != nil {
		return ProviderAuthBrowserStart{}, err
	}
	return ProviderAuthBrowserStart{AuthorizationURL: authorization.URL, PrivateState: string(private), ExpiresIn: 600}, nil
}

func (adapter googleAntigravityAuthAdapter) CompleteManual(ctx context.Context, code, privateState string, input ProviderAuthManualConfig) (ProviderAuthPoll, error) {
	var state struct {
		CodeVerifier string `json:"code_verifier"`
	}
	if json.Unmarshal([]byte(privateState), &state) != nil || state.CodeVerifier == "" {
		return ProviderAuthPoll{}, fmt.Errorf("manual OAuth state is unavailable")
	}
	oauth, err := antigravityManualConfig(input)
	if err != nil {
		return ProviderAuthPoll{}, err
	}
	tokens, err := oauth.Exchange(ctx, code, state.CodeVerifier)
	if err != nil {
		return ProviderAuthPoll{}, err
	}
	account, _ := oauth.DiscoverAccount(ctx, tokens.AccessToken)
	return ProviderAuthPoll{
		Status: "authorized", AccessToken: tokens.AccessToken, RefreshToken: tokens.RefreshToken,
		IDToken: tokens.IDToken, TokenType: tokens.TokenType, ExpiresAt: tokens.ExpiresAt.Unix(),
		ProjectID: account.ProjectID, OAuthProfile: antigravityOAuthProfileConsumerManual,
		OAuthClientID: oauth.ClientID,
	}, nil
}

func antigravityManualConfig(input ProviderAuthManualConfig) (antigravityauth.Config, error) {
	mode := strings.ToLower(strings.TrimSpace(input.ClientMode))
	oauth := antigravityauth.Config{ClientID: strings.TrimSpace(input.ClientID), RedirectURI: strings.TrimSpace(input.RedirectURI)}
	switch mode {
	case "public":
		oauth.ClientAuthMode = antigravityauth.ClientAuthModePublicPKCE
	case "confidential":
		if strings.TrimSpace(input.ClientSecret) == "" {
			return antigravityauth.Config{}, fmt.Errorf("confidential OAuth clients require a client secret")
		}
		oauth.ClientSecret = input.ClientSecret
		oauth.ClientAuthMode = antigravityauth.ClientAuthModeClientSecretPost
	default:
		return antigravityauth.Config{}, fmt.Errorf("OAuth client mode must be public or confidential")
	}
	return oauth, nil
}
func (googleAntigravityAuthAdapter) RefreshConnection(ctx context.Context, principalID, providerID, connectionName string) (iam.OAuthTokenEnvelope, iam.ProviderConnection, error) {
	return RefreshAntigravityOAuthConnection(ctx, principalID, providerID, connectionName)
}

func (adapter googleAntigravityAuthAdapter) config(redirectURI string) antigravityauth.Config {
	if adapter.oauth != nil {
		configured := *adapter.oauth
		configured.RedirectURI = redirectURI
		return configured
	}
	if provider := config.Get().Providers[adapter.providerID]; provider != nil && strings.TrimSpace(provider.PublicOAuthClientID) != "" {
		return antigravityauth.Config{
			ClientID: strings.TrimSpace(provider.PublicOAuthClientID), ClientAuthMode: antigravityauth.ClientAuthModePublicPKCE,
			RedirectURI: redirectURI,
		}
	}
	return newGoogleAntigravityOAuthConfig(redirectURI)
}

func antigravityOAuthProfile(mode antigravityauth.ClientAuthMode) string {
	if mode == antigravityauth.ClientAuthModePublicPKCE {
		return antigravityOAuthProfilePublicPKCE
	}
	return antigravityOAuthProfileRuntimeSecret
}
