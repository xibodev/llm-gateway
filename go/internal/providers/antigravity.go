package providers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"

	antigravityauth "github.com/xibodev/llm-provider-auth/antigravity"
	core "github.com/xibodev/llmgw-core"
	coreproviders "github.com/xibodev/llmgw-core/providers"
)

type antigravityProvider struct {
	inner       *coreproviders.ExperimentalAntigravityProvider
	principalID string
	providerID  string
}

func newAntigravityProvider(providerID string, caller core.Caller) (Provider, error) {
	principalID := callerPrincipalID(caller)
	if strings.TrimSpace(principalID) == "" {
		return nil, &ConfigError{Msg: "google_antigravity: a human principal private connection is required"}
	}
	tokenSource := func(ctx context.Context) (string, string, error) {
		envelope, connection, ok, err := iam.OAuthProviderConnectionSecret(principalID, providerID, "")
		if err != nil {
			return "", "", err
		}
		if !ok {
			return "", "", fmt.Errorf("no active private Antigravity connection")
		}
		if envelope.ExpiresAt > 0 && envelope.ExpiresAt <= time.Now().Add(time.Minute).Unix() {
			if err := refreshAntigravityConnection(ctx, principalID, providerID, connection.Name, envelope.AccessToken); err != nil {
				return "", "", err
			}
			envelope, _, ok, err = iam.OAuthProviderConnectionSecret(principalID, providerID, connection.Name)
			if err != nil || !ok {
				return "", "", fmt.Errorf("refreshed Antigravity connection is unavailable")
			}
		}
		return envelope.AccessToken, envelope.ProjectID, nil
	}
	inner := coreproviders.NewExperimentalAntigravityProvider(tokenSource, nil, "")
	inner.SetUnauthorizedHandler(func(ctx context.Context, rejectedAccessToken string) error {
		return refreshAntigravityConnection(ctx, principalID, providerID, "", rejectedAccessToken)
	})
	inner.SetProjectObserver(func(ctx context.Context, accessToken, projectID string) error {
		return persistAntigravityProject(principalID, providerID, "", accessToken, projectID)
	})
	return &antigravityProvider{
		inner:       inner,
		principalID: principalID, providerID: providerID,
	}, nil
}

func (p *antigravityProvider) IsStub() bool { return false }

func (p *antigravityProvider) Complete(model string, messages []Message, kw Kwargs) (map[string]any, error) {
	return p.CompleteContext(context.Background(), model, messages, kw)
}

func (p *antigravityProvider) CompleteContext(ctx context.Context, model string, messages []Message, kw Kwargs) (map[string]any, error) {
	messageValues := make([]any, len(messages))
	for index, message := range messages {
		messageValues[index] = map[string]any(message)
	}
	payload := map[string]any{"messages": messageValues}
	for _, key := range []string{"max_tokens", "temperature", "tools"} {
		if value := kw[key]; value != nil {
			payload[key] = value
		}
	}
	response, err := p.inner.Complete(ctx, model, payload, nil)
	return response, adaptAntigravityError("completion", err)
}

func (p *antigravityProvider) Stream(string, []Message, Kwargs) (StreamIter, error) {
	return nil, &ConfigError{Msg: "google_antigravity: streaming is not supported"}
}

func (p *antigravityProvider) GenerateImages(model, prompt string, count int) ([]GeneratedImage, map[string]any, error) {
	return p.GenerateImagesContext(context.Background(), model, prompt, count)
}

func (p *antigravityProvider) GenerateImagesContext(ctx context.Context, model, prompt string, count int) ([]GeneratedImage, map[string]any, error) {
	result, err := p.inner.GenerateImages(ctx, core.GenerateImagesRequest{Model: model, Prompt: prompt, Count: count}, nil)
	if err != nil {
		return nil, nil, adaptAntigravityError("image generation", err)
	}
	images := make([]GeneratedImage, len(result.Images))
	for index, image := range result.Images {
		images[index] = GeneratedImage{Data: image.Data, MimeType: image.MimeType}
	}
	return images, result.Usage, nil
}

func (p *antigravityProvider) ListModels() []ModelInfo {
	models, _, _ := p.ListModelsWithError()
	return models
}

func (p *antigravityProvider) ListModelsWithError() ([]ModelInfo, *CredentialObservation, error) {
	models, err := p.inner.ListModels(context.Background(), nil)
	if err != nil {
		return nil, nil, adaptAntigravityCatalogError(err)
	}
	rows := make([]ModelInfo, 0, len(models))
	for _, model := range models {
		capabilities := model.Capabilities
		surfaces := []string{"/v1/chat/completions"}
		if capabilities != nil {
			capabilities.Streaming = core.SupportUnsupported
			if capabilities.Operations.Image == core.SupportSupported {
				surfaces = append(surfaces, "/v1/images/generations")
			}
		}
		rows = append(rows, ModelInfo{
			ID: model.ID, Vendor: model.OwnedBy, Label: model.Description,
			TypedCapabilities: capabilities, SupportedSurfaces: surfaces,
		})
	}
	return rows, nil, nil
}

func adaptAntigravityError(operation string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, coreproviders.ErrExperimentalAntigravityStreamingUnsupported) {
		return &ConfigError{Msg: "google_antigravity: streaming is not supported"}
	}
	var upstream *core.ProviderOperationError
	if errors.As(err, &upstream) {
		return invocationStatusRetryAfter("google_antigravity: "+operation+" failed", upstream.Failure.StatusCode, upstream.Failure.RetryAfter)
	}
	return invocation("google_antigravity: " + operation + " failed")
}

func adaptAntigravityCatalogError(err error) error {
	if err == nil {
		return nil
	}
	var upstream *core.ProviderOperationError
	if errors.As(err, &upstream) {
		return catalogError("catalog_failed", "Antigravity model discovery failed.", upstream.Failure.StatusCode)
	}
	return catalogError("catalog_failed", "Antigravity model discovery failed.", 0)
}

func refreshAntigravityConnection(ctx context.Context, principalID, providerID, name, rejectedAccessToken string) error {
	initial, initialConnection, ok, err := iam.OAuthProviderConnectionSecret(principalID, providerID, name)
	if err != nil || !ok || initial.RefreshToken == "" {
		return fmt.Errorf("Antigravity refresh token is unavailable")
	}
	if rejectedAccessToken != "" && initial.AccessToken != rejectedAccessToken {
		return nil
	}
	unlock := Current().antigravityRefresh.lock(principalID + "|" + providerID + "|" + initialConnection.ID)
	defer unlock()
	current, connection, ok, err := iam.OAuthProviderConnectionSecret(principalID, providerID, name)
	if err != nil || !ok {
		return fmt.Errorf("Antigravity connection is unavailable")
	}
	if connection.ID != initialConnection.ID || current.AccessToken != initial.AccessToken || current.RefreshToken != initial.RefreshToken {
		return nil
	}
	oauth, err := antigravityOAuthConfigForEnvelope(providerID, current)
	if err != nil {
		return err
	}
	tokens, err := oauth.Refresh(ctx, current.RefreshToken)
	if err != nil {
		return err
	}
	projectID := current.ProjectID
	if account, discoveryErr := oauth.DiscoverAccount(ctx, tokens.AccessToken); discoveryErr == nil && account.ProjectID != "" {
		projectID = account.ProjectID
	}
	expiresAt := tokens.ExpiresAt.Unix()
	refreshToken := chooseString(tokens.RefreshToken, current.RefreshToken)
	_, err = iam.ReplaceOAuthProviderConnectionIfCurrent(connection, current, iam.OAuthConnectionCreate{
		PrincipalID: principalID, ProviderID: providerID, Name: connection.Name, Kind: connection.Kind,
		Source: connection.Source, MakeDefault: connection.IsDefault, AccessToken: tokens.AccessToken,
		RefreshToken: refreshToken, IDToken: chooseString(tokens.IDToken, current.IDToken),
		TokenType: chooseString(tokens.TokenType, current.TokenType), ExpiresAt: expiresAt,
		AccountID: current.AccountID, AccountLabel: current.AccountLabel, Status: "active",
		ProjectID:    projectID,
		OAuthProfile: current.OAuthProfile, OAuthClientID: current.OAuthClientID,
		OAuthClientMode: current.OAuthClientMode, OAuthRedirectURI: current.OAuthRedirectURI,
		OAuthClientSecret: current.OAuthClientSecret,
	})
	if errors.Is(err, iam.ErrOAuthProviderConnectionChanged) {
		return nil
	}
	if err == nil {
		ForgetProviderForPrincipal(providerID, principalID)
		forgetCatalogAfterProviderPersistence(providerID, principalID)
	}
	return err
}

func persistAntigravityProject(principalID, providerID, name, accessToken, projectID string) error {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return nil
	}
	current, connection, ok, err := iam.OAuthProviderConnectionSecret(principalID, providerID, name)
	if err != nil || !ok {
		return fmt.Errorf("Antigravity connection is unavailable")
	}
	if current.AccessToken != accessToken {
		return nil
	}
	if strings.TrimSpace(current.ProjectID) != "" {
		return nil
	}
	_, err = iam.ReplaceOAuthProviderConnectionIfCurrent(connection, current, iam.OAuthConnectionCreate{
		PrincipalID: principalID, ProviderID: providerID, Name: connection.Name, Kind: connection.Kind,
		Source: connection.Source, MakeDefault: connection.IsDefault, AccessToken: current.AccessToken,
		RefreshToken: current.RefreshToken, IDToken: current.IDToken, TokenType: current.TokenType,
		ExpiresAt: current.ExpiresAt, AccountID: current.AccountID, AccountLabel: current.AccountLabel,
		Status: current.Status, ProjectID: projectID,
		OAuthProfile: current.OAuthProfile, OAuthClientID: current.OAuthClientID,
		OAuthClientMode: current.OAuthClientMode, OAuthRedirectURI: current.OAuthRedirectURI,
		OAuthClientSecret: current.OAuthClientSecret,
	})
	if errors.Is(err, iam.ErrOAuthProviderConnectionChanged) {
		return nil
	}
	if err == nil {
		forgetCatalogAfterProviderPersistence(providerID, principalID)
	}
	return err
}

func antigravityOAuthConfigForEnvelope(providerID string, envelope iam.OAuthTokenEnvelope) (antigravityauth.Config, error) {
	switch envelope.OAuthProfile {
	case "":
		return antigravityauth.Config{}, fmt.Errorf("Antigravity OAuth client profile is unavailable; reauthorize this connection")
	case antigravityOAuthProfileRuntimeSecret:
		if strings.TrimSpace(envelope.OAuthClientID) == "" {
			return antigravityauth.Config{}, fmt.Errorf("Antigravity OAuth client profile is unavailable; reauthorize this connection")
		}
		oauth := Current().antigravityOAuthConfig("")
		if strings.TrimSpace(oauth.ClientID) != strings.TrimSpace(envelope.OAuthClientID) {
			return antigravityauth.Config{}, fmt.Errorf("Antigravity OAuth client profile is unavailable")
		}
		return oauth, nil
	case antigravityOAuthProfilePublicPKCE:
		provider := config.Get().Providers[providerID]
		if provider == nil || strings.TrimSpace(provider.PublicOAuthClientID) == "" || strings.TrimSpace(provider.PublicOAuthClientID) != strings.TrimSpace(envelope.OAuthClientID) {
			return antigravityauth.Config{}, fmt.Errorf("Antigravity OAuth client profile is unavailable")
		}
		oauth := Current().antigravityOAuthConfig("")
		oauth.ClientID = strings.TrimSpace(provider.PublicOAuthClientID)
		oauth.ClientSecret = ""
		oauth.ClientAuthMode = antigravityauth.ClientAuthModePublicPKCE
		return oauth, nil
	case antigravityOAuthProfileConsumerManual:
		input := ProviderAuthManualConfig{
			ClientID: envelope.OAuthClientID, ClientSecret: envelope.OAuthClientSecret,
			ClientMode: envelope.OAuthClientMode, RedirectURI: envelope.OAuthRedirectURI,
		}
		return antigravityManualConfig(input)
	default:
		return antigravityauth.Config{}, fmt.Errorf("Antigravity OAuth client profile is unsupported")
	}
}

func RefreshAntigravityOAuthConnection(ctx context.Context, principalID, providerID, name string) (iam.OAuthTokenEnvelope, iam.ProviderConnection, error) {
	envelope, _, ok, err := iam.OAuthProviderConnectionSecret(principalID, providerID, name)
	if err != nil || !ok {
		return iam.OAuthTokenEnvelope{}, iam.ProviderConnection{}, fmt.Errorf("Antigravity connection is unavailable")
	}
	if err := refreshAntigravityConnection(ctx, principalID, providerID, name, envelope.AccessToken); err != nil {
		return iam.OAuthTokenEnvelope{}, iam.ProviderConnection{}, err
	}
	envelope, connection, ok, err := iam.OAuthProviderConnectionSecret(principalID, providerID, name)
	if err != nil {
		return iam.OAuthTokenEnvelope{}, iam.ProviderConnection{}, err
	}
	if !ok {
		return iam.OAuthTokenEnvelope{}, iam.ProviderConnection{}, fmt.Errorf("Antigravity connection is no longer active")
	}
	return envelope, connection, nil
}

func chooseString(value, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return value
	}
	return fallback
}
