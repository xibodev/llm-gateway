package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"llmgw/internal/codexauth"
	"llmgw/internal/config"
	"llmgw/internal/copilotauth"
	"llmgw/internal/iam"
)

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
	Status       string
	Error        string
	AccessToken  string
	RefreshToken string
	IDToken      string
	TokenType    string
	ExpiresAt    int64
	AccountID    string
	AccountLabel string
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
}

type ProviderAuthBrowserStart struct {
	AuthorizationURL string
	PrivateState     string
	ExpiresIn        int
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
		clientID := strings.TrimSpace(config.Get().OpenAICodexClientID)
		if clientID == "" {
			return nil, fmt.Errorf("openai_codex_client_id is required for the official Codex OAuth flow")
		}
		return openAICodexAuthAdapter{clientID: clientID}, nil
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
	_, imported := adapter.(ImportProviderAuthAdapter)
	_, refreshed := adapter.(RefreshableProviderAuthAdapter)
	_, guardedRefresh := adapter.(GuardedRefreshProviderAuthAdapter)
	_, revoked := adapter.(RevocableProviderAuthAdapter)
	if capabilities.DeviceCode != device ||
		capabilities.BrowserCallback != browser ||
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
	return ProviderAuthPoll{Status: result.Status, Error: result.Error, AccessToken: result.AccessToken}
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
	private, _ := json.Marshal(codexDevicePrivateState{UserCode: flow.UserCode})
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
	if json.Unmarshal([]byte(privateState), &state) != nil || state.UserCode == "" {
		return ProviderAuthPoll{
			Status: "error", Error: "Codex device authorization state is unavailable. Start again.",
		}
	}
	status, tokens, err := codexauth.PollAndExchange(codexauth.DeviceFlow{
		DeviceAuthID: deviceCode, UserCode: state.UserCode, ClientID: adapter.clientID,
	})
	if err != nil {
		return ProviderAuthPoll{Status: status, Error: err.Error()}
	}
	return ProviderAuthPoll{
		Status: status, AccessToken: tokens.AccessToken, RefreshToken: tokens.RefreshToken,
		IDToken: tokens.IDToken, TokenType: tokens.TokenType, ExpiresAt: tokens.ExpiresAt,
		AccountID: tokens.AccountID, AccountLabel: tokens.AccountLabel,
	}
}
func (adapter openAICodexAuthAdapter) RefreshConnection(
	_ context.Context, principalID, providerID, connectionName string,
) (iam.OAuthTokenEnvelope, iam.ProviderConnection, error) {
	return RefreshCodexOAuthConnection(
		principalID, providerID, connectionName, adapter.clientID,
	)
}
func (adapter openAICodexAuthAdapter) Revoke(
	_ context.Context, envelope iam.OAuthTokenEnvelope,
) error {
	return codexauth.Revoke(adapter.clientID, envelope.RefreshToken, envelope.AccessToken)
}
