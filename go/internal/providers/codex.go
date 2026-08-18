package providers

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"llmgw/internal/codexauth"
	"llmgw/internal/iam"
)

// codexAuth supplies owner-private official Codex credentials to the common
// OpenAI Responses transport. Account identity is sent only when the official
// upstream requires it.
type codexAuth struct {
	principalID    string
	providerID     string
	connectionName string
	clientID       string
}

type codexRefreshLock struct {
	mu   sync.Mutex
	refs int
}

var codexRefreshLocks = struct {
	sync.Mutex
	entries map[string]*codexRefreshLock
}{entries: map[string]*codexRefreshLock{}}

func lockCodexRefresh(key string) func() {
	codexRefreshLocks.Lock()
	entry := codexRefreshLocks.entries[key]
	if entry == nil {
		entry = &codexRefreshLock{}
		codexRefreshLocks.entries[key] = entry
	}
	entry.refs++
	codexRefreshLocks.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		codexRefreshLocks.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(codexRefreshLocks.entries, key)
		}
		codexRefreshLocks.Unlock()
	}
}

func (a codexAuth) Prepare() (string, http.Header, error) {
	baseURL, headers, _, err := a.PrepareObserved()
	return baseURL, headers, err
}

func (a codexAuth) PrepareObserved() (
	string, http.Header, *iam.ProviderAccountObservation, error,
) {
	envelope, connection, observation, ok, err := iam.OAuthProviderConnectionSecretWithObservation(
		a.principalID, a.providerID, a.connectionName,
	)
	if err != nil {
		return "", nil, &observation, invocation("openai_codex: load private OAuth connection: " + err.Error())
	}
	if !ok {
		return "", nil, nil, &ConfigError{Msg: "openai_codex: this principal has no active private Codex connection"}
	}
	if envelope.ExpiresAt > 0 && envelope.ExpiresAt <= time.Now().Add(60*time.Second).Unix() && envelope.RefreshToken != "" {
		expectedAccountID := strings.TrimSpace(envelope.AccountID)
		if err := a.refreshConnection(envelope, connection); err != nil {
			return "", nil, &observation, err
		}
		envelope, _, observation, ok, err = iam.OAuthProviderConnectionSecretWithObservation(
			a.principalID, a.providerID, a.connectionName,
		)
		if err != nil || !ok {
			return "", nil, &observation, invocation("openai_codex: refresh did not yield an active connection")
		}
		if codexAccountMismatch(expectedAccountID, envelope.AccountID) {
			return "", nil, &observation, invocation("openai_codex: account changed during refresh")
		}
	}
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+envelope.AccessToken)
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "application/json")
	headers.Set("User-Agent", "llm-gateway/codex")
	if strings.TrimSpace(envelope.AccountID) != "" {
		headers.Set("ChatGPT-Account-ID", envelope.AccountID)
	}
	return strings.TrimRight(codexauth.ResponsesBaseURL, "/"), headers, &observation, nil
}

func (codexAuth) CanRefresh() bool { return true }

func (a codexAuth) Refresh() error {
	envelope, connection, ok, err := iam.OAuthProviderConnectionSecret(a.principalID, a.providerID, a.connectionName)
	if err != nil {
		return invocation("openai_codex: load refresh token: " + err.Error())
	}
	if !ok || strings.TrimSpace(envelope.RefreshToken) == "" {
		return &ConfigError{Msg: "openai_codex: no refresh token is available"}
	}
	return a.refreshConnection(envelope, connection)
}

func (a codexAuth) refreshConnection(initial iam.OAuthTokenEnvelope, initialConnection iam.ProviderConnection) error {
	expectedAccountID := strings.TrimSpace(initial.AccountID)
	unlock := lockCodexRefresh(a.principalID + "|" + a.providerID + "|" + initialConnection.ID)
	defer unlock()

	envelope, connection, ok, err := iam.OAuthProviderConnectionSecret(a.principalID, a.providerID, a.connectionName)
	if err != nil {
		return invocation("openai_codex: reload refresh token: " + err.Error())
	}
	if !ok || strings.TrimSpace(envelope.RefreshToken) == "" {
		return &ConfigError{Msg: "openai_codex: no refresh token is available"}
	}
	if codexAccountMismatch(expectedAccountID, envelope.AccountID) {
		return invocation("openai_codex: account changed during refresh")
	}
	if connection.ID != initialConnection.ID || !sameCodexRefreshState(initial, envelope) {
		return nil
	}

	tokens, err := codexauth.Refresh(a.clientID, envelope.RefreshToken)
	if err != nil {
		var refreshError *codexauth.RefreshError
		if errors.As(err, &refreshError) && shouldRevokeCodexRefresh(strings.ToLower(refreshError.Code)) {
			if revoked, revokeErr := iam.RevokeOAuthProviderConnectionIfCurrent(connection, envelope); revokeErr == nil && revoked {
				ForgetProviderForPrincipal(a.providerID, a.principalID)
				ForgetCatalogForPrincipal(a.providerID, a.principalID)
			}
		}
		return invocation("openai_codex: refresh failed")
	}
	if codexAccountMismatch(expectedAccountID, tokens.AccountID) {
		return invocation("openai_codex: account changed during refresh")
	}
	refreshToken := tokens.RefreshToken
	if refreshToken == "" {
		refreshToken = envelope.RefreshToken
	}
	idToken := tokens.IDToken
	if idToken == "" {
		idToken = envelope.IDToken
	}
	accountID := tokens.AccountID
	if accountID == "" {
		accountID = envelope.AccountID
	}
	accountLabel := tokens.AccountLabel
	if accountLabel == "" {
		accountLabel = envelope.AccountLabel
	}
	tokenType := tokens.TokenType
	if tokenType == "" {
		tokenType = envelope.TokenType
	}
	expiresAt := tokens.ExpiresAt
	_, err = iam.ReplaceOAuthProviderConnectionIfCurrent(
		connection, envelope, iam.OAuthConnectionCreate{
			PrincipalID: a.principalID, ProviderID: a.providerID, Name: connection.Name, Kind: connection.Kind,
			Source: connection.Source, MakeDefault: connection.IsDefault, AccessToken: tokens.AccessToken,
			RefreshToken: refreshToken, IDToken: idToken, TokenType: tokenType, ExpiresAt: expiresAt,
			AccountID: accountID, AccountLabel: accountLabel, Status: "active",
		},
	)
	if errors.Is(err, iam.ErrOAuthProviderConnectionChanged) {
		current, _, ok, loadErr := iam.OAuthProviderConnectionSecret(a.principalID, a.providerID, a.connectionName)
		if loadErr != nil {
			return invocation("openai_codex: reload changed connection: " + loadErr.Error())
		}
		if !ok {
			return invocation("openai_codex: connection changed during refresh")
		}
		if codexAccountMismatch(expectedAccountID, current.AccountID) {
			return invocation("openai_codex: account changed during refresh")
		}
		return nil
	}
	if err != nil {
		return invocation("openai_codex: store refreshed connection: " + err.Error())
	}
	ForgetProviderForPrincipal(a.providerID, a.principalID)
	ForgetCatalogForPrincipal(a.providerID, a.principalID)
	return nil
}

func codexAccountMismatch(expected, actual string) bool {
	expected = strings.TrimSpace(expected)
	actual = strings.TrimSpace(actual)
	return expected != "" && actual != "" && expected != actual
}

// RefreshCodexOAuthConnection runs the guarded refresh path used by inference.
// Explicit console refreshes must not bypass its lock, account pin, or
// compare-before-write checks.
func RefreshCodexOAuthConnection(
	principalID, providerID, connectionName, clientID string,
) (iam.OAuthTokenEnvelope, iam.ProviderConnection, error) {
	auth := codexAuth{
		principalID: principalID, providerID: providerID,
		connectionName: connectionName, clientID: clientID,
	}
	if err := auth.Refresh(); err != nil {
		return iam.OAuthTokenEnvelope{}, iam.ProviderConnection{}, err
	}
	envelope, connection, ok, err := iam.OAuthProviderConnectionSecret(
		principalID, providerID, connectionName,
	)
	if err != nil {
		return iam.OAuthTokenEnvelope{}, iam.ProviderConnection{}, err
	}
	if !ok {
		return iam.OAuthTokenEnvelope{}, iam.ProviderConnection{}, &ConfigError{
			Msg: "openai_codex: refreshed connection is no longer active",
		}
	}
	return envelope, connection, nil
}

func sameCodexRefreshState(left, right iam.OAuthTokenEnvelope) bool {
	return left.AccessToken == right.AccessToken && left.RefreshToken == right.RefreshToken
}

func shouldRevokeCodexRefresh(code string) bool {
	switch code {
	case "invalid_grant", "invalid_token", "token_reused", "refresh_token_reused", "refresh_token_invalidated", "expired_token", "refresh_token_expired":
		return true
	default:
		return false
	}
}

// CodexProvider always uses the verified Responses path rather than the chat
// completions path. The shared OpenAI provider implementation supplies exactly
// one refresh and bounded retry after a 401 through OpenAIAuth.
type CodexProvider struct {
	inner OpenAIProvider
}

func (p CodexProvider) IsStub() bool { return false }
func (p CodexProvider) Complete(model string, messages []Message, kw Kwargs) (map[string]any, error) {
	response, _, err := p.CompleteWithObservation(model, messages, kw)
	return response, err
}
func (p CodexProvider) CompleteWithObservation(
	model string, messages []Message, kw Kwargs,
) (map[string]any, *iam.ProviderAccountObservation, error) {
	return p.inner.completeViaResponsesWithObservation(model, messages, kw)
}
func (p CodexProvider) CompleteResponses(
	model string, payload map[string]any,
) (map[string]any, *iam.ProviderAccountObservation, error) {
	request := cloneMap(payload)
	request["model"] = model
	request["stream"] = false
	delete(request, "force_api_support")
	return p.inner.callResponsesPayloadWithObservation(request)
}
func (p CodexProvider) StreamResponses(
	model string, payload map[string]any,
) (StreamIter, *iam.ProviderAccountObservation, error) {
	request := cloneMap(payload)
	request["model"] = model
	request["stream"] = true
	delete(request, "force_api_support")
	return p.inner.streamResponsesPayload(request)
}
func (p CodexProvider) Stream(model string, messages []Message, kw Kwargs) (StreamIter, error) {
	return p.inner.streamViaResponses(model, messages, kw)
}

func (p CodexProvider) ListModels() []ModelInfo {
	models, _, _ := p.ListModelsWithError()
	return models
}

func (p CodexProvider) ListModelsWithError() (
	[]ModelInfo, *iam.ProviderAccountObservation, error,
) {
	_, headers, observation, err := prepareOpenAIAuth(p.inner.auth)
	if err != nil {
		return nil, observation, catalogError(
			"catalog_authentication_failed",
			"Provider authentication failed before catalog access.",
			0,
		)
	}
	modelsURL, err := url.Parse(codexauth.ModelsURL)
	if err != nil {
		return nil, observation, catalogError(
			"catalog_provider_unavailable",
			"Provider catalog endpoint is invalid.",
			0,
		)
	}
	query := modelsURL.Query()
	query.Set("client_version", codexauth.ClientVersion)
	modelsURL.RawQuery = query.Encode()
	get := func(h http.Header) (*http.Response, error) {
		request, _ := http.NewRequest(http.MethodGet, modelsURL.String(), nil)
		request.Header = h
		return httpClient(p.inner.Timeout).Do(request)
	}
	response, err := get(headers)
	if err != nil {
		return nil, observation, catalogError(
			"catalog_transport_error",
			"Provider catalog request could not reach the upstream service.",
			0,
		)
	}
	if response.StatusCode == http.StatusUnauthorized && p.inner.auth.CanRefresh() {
		response.Body.Close()
		if p.inner.auth.Refresh() != nil {
			return nil, observation, catalogError(
				"catalog_refresh_failed",
				"Provider credential refresh failed.",
				http.StatusUnauthorized,
			)
		}
		_, headers, observation, err = prepareOpenAIAuth(p.inner.auth)
		if err != nil {
			return nil, observation, catalogError(
				"catalog_authentication_failed",
				"Provider authentication failed after credential refresh.",
				http.StatusUnauthorized,
			)
		}
		response, err = get(headers)
		if err != nil {
			return nil, observation, catalogError(
				"catalog_transport_error",
				"Provider catalog retry could not reach the upstream service.",
				0,
			)
		}
	}
	defer response.Body.Close()
	if response.StatusCode >= 400 {
		return nil, observation, catalogError(
			"catalog_http_error",
			fmt.Sprintf("Provider catalog returned HTTP %d.", response.StatusCode),
			response.StatusCode,
		)
	}
	raw, _ := io.ReadAll(response.Body)
	payload := map[string]any{}
	if json.Unmarshal(raw, &payload) != nil {
		return nil, observation, catalogError(
			"catalog_invalid_json",
			"Provider catalog response was not valid JSON.",
			response.StatusCode,
		)
	}
	entries, ok := payload["data"].([]any)
	if !ok {
		return nil, observation, catalogError(
			"catalog_invalid_shape",
			"Provider catalog response did not contain a data array.",
			response.StatusCode,
		)
	}
	rows := make([]ModelInfo, 0, len(entries))
	for _, value := range entries {
		entry, ok := value.(map[string]any)
		if !ok {
			continue
		}
		id, _ := entry["id"].(string)
		if id == "" {
			id, _ = entry["name"].(string)
		}
		if id == "" {
			continue
		}
		vendor, _ := entry["vendor"].(string)
		if vendor == "" {
			vendor, _ = entry["owned_by"].(string)
		}
		row := ModelInfo{ID: id, Vendor: vendor}
		if label, _ := entry["display_name"].(string); label != "" && label != id {
			row.Label = label
		} else if label, _ := entry["name"].(string); label != "" && label != id {
			row.Label = label
		}
		if caps := extractCapabilities(entry["capabilities"]); len(caps) > 0 {
			row.Capabilities = caps
		}
		if endpoints := stringList(entry["supported_endpoints"]); len(endpoints) > 0 {
			row.SupportedEndpoints = endpoints
		}
		rows = append(rows, row)
	}
	return rows, observation, nil
}

var _ OpenAIAuth = codexAuth{}
var _ Provider = CodexProvider{}
