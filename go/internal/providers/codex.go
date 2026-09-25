package providers

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"llmgw/internal/iam"

	providerauth "github.com/xibodev/llm-provider-auth"
	browseroauth "github.com/xibodev/llm-provider-auth/browseroauth"
	codexauth "github.com/xibodev/llm-provider-auth/codex"
	core "github.com/xibodev/llmgw-core"
	coreproviders "github.com/xibodev/llmgw-core/providers"
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

func codexRefreshInvocationError(err error) error {
	if err == nil || IsInvocation(err) || IsConfig(err) {
		return err
	}
	var refreshError *codexauth.RefreshError
	if errors.As(err, &refreshError) && refreshError.StatusCode != 0 {
		return failoverInvocationStatus("openai_codex: refresh failed", refreshError.StatusCode)
	}
	var authError *codexauth.AuthError
	if errors.As(err, &authError) {
		if authError.StatusCode != 0 {
			return failoverInvocationStatus("openai_codex: refresh failed", authError.StatusCode)
		}
		if authError.Code == "transport" {
			return retryableInvocation("openai_codex: refresh failed")
		}
	}
	return invocation("openai_codex: refresh failed")
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
	string, http.Header, *CredentialObservation, error,
) {
	envelope, connection, observation, ok, err := iam.OAuthProviderConnectionSecretWithObservation(
		a.principalID, a.providerID, a.connectionName,
	)
	if err != nil {
		return "", nil, credentialObservation(&observation), invocation("openai_codex: load private OAuth connection: " + err.Error())
	}
	if !ok {
		return "", nil, nil, &ConfigError{Msg: "openai_codex: this principal has no active private Codex connection"}
	}
	if envelope.ExpiresAt > 0 && envelope.ExpiresAt <= time.Now().Add(60*time.Second).Unix() && envelope.RefreshToken != "" {
		expectedAccountID := strings.TrimSpace(envelope.AccountID)
		if err := a.refreshConnection(envelope, connection); err != nil {
			return "", nil, credentialObservation(&observation), err
		}
		envelope, _, observation, ok, err = iam.OAuthProviderConnectionSecretWithObservation(
			a.principalID, a.providerID, a.connectionName,
		)
		if err != nil || !ok {
			return "", nil, credentialObservation(&observation), invocation("openai_codex: refresh did not yield an active connection")
		}
		if codexAccountMismatch(expectedAccountID, envelope.AccountID) {
			return "", nil, credentialObservation(&observation), invocation("openai_codex: account changed during refresh")
		}
	}
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+envelope.AccessToken)
	headers.Set("Content-Type", "application/json")
	headers.Set("Accept", "application/json")
	headers.Set("User-Agent", "llm-gateway/codex")
	headers.Set("originator", "codex_cli_rs")
	headers.Set("OpenAI-Beta", "responses=experimental")
	if strings.TrimSpace(envelope.AccountID) != "" {
		headers.Set("ChatGPT-Account-ID", envelope.AccountID)
	}
	return strings.TrimRight(codexEndpoints.withDefaults().ResponsesBaseURL, "/"), headers, credentialObservation(&observation), nil
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

	clientID, err := codexOAuthClientIDForEnvelope(envelope)
	if err != nil {
		return &ConfigError{Msg: "openai_codex: " + err.Error()}
	}
	var tokens codexauth.TokenSet
	if strings.TrimSpace(envelope.OAuthProfile) == codexOAuthProfileBrowser {
		var browserTokens browseroauth.TokenEnvelope
		browserTokens, err = codexBrowserConfig(clientID).Refresh(context.Background(), envelope.RefreshToken)
		if err == nil {
			accountID, accountLabel := codexIDTokenIdentity(browserTokens.IDToken)
			tokens = codexauth.TokenSet{
				AccessToken: browserTokens.AccessToken, RefreshToken: browserTokens.RefreshToken,
				IDToken: browserTokens.IDToken, TokenType: browserTokens.TokenType,
				ExpiresAt: browserTokens.ExpiresAt.Unix(), AccountID: accountID, AccountLabel: accountLabel,
			}
		} else {
			var endpoint *browseroauth.EndpointError
			if errors.As(err, &endpoint) {
				err = &codexauth.RefreshError{StatusCode: endpoint.StatusCode, Code: endpoint.Code, Description: endpoint.Description}
			}
		}
	} else {
		tokens, err = codexOAuth(clientID).Refresh(context.Background(), envelope.RefreshToken)
	}
	if err != nil {
		var refreshError *codexauth.RefreshError
		if errors.As(err, &refreshError) && shouldRevokeCodexRefresh(strings.ToLower(refreshError.Code)) {
			if revoked, revokeErr := iam.RevokeOAuthProviderConnectionIfCurrent(connection, envelope); revokeErr == nil && revoked {
				ForgetProviderForPrincipal(a.providerID, a.principalID)
				ForgetCatalogForPrincipal(a.providerID, a.principalID)
			}
		}
		return codexRefreshInvocationError(err)
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
			ProjectID: envelope.ProjectID, OAuthProfile: envelope.OAuthProfile,
			OAuthClientID: envelope.OAuthClientID, OAuthClientMode: envelope.OAuthClientMode,
			OAuthRedirectURI: envelope.OAuthRedirectURI, OAuthClientSecret: envelope.OAuthClientSecret,
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
	principalID, providerID, connectionName string,
) (iam.OAuthTokenEnvelope, iam.ProviderConnection, error) {
	auth := codexAuth{
		principalID: principalID, providerID: providerID,
		connectionName: connectionName,
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

func codexOAuthClientIDForEnvelope(envelope iam.OAuthTokenEnvelope) (string, error) {
	profile := strings.TrimSpace(envelope.OAuthProfile)
	if (profile != codexOAuthProfileDevice && profile != codexOAuthProfileBrowser) || strings.TrimSpace(envelope.OAuthClientID) == "" {
		return "", errors.New("OAuth client profile is unavailable; reauthorize this connection")
	}
	return strings.TrimSpace(envelope.OAuthClientID), nil
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

const codexInstructions = "Follow the caller's request."

// codexCatalogClientVersion is the catalog schema compatibility contract this
// gateway has verified, not an attempt to impersonate an installed Codex CLI.
const codexCatalogClientVersion = "0.155.1"

// CodexProvider keeps gateway IAM and legacy interfaces around the shared Codex
// catalog, request, and streaming transport.
type CodexProvider struct {
	inner *coreproviders.CodexProvider
	auth  codexAuth
}

func newCodexProvider(auth codexAuth, timeout float64, client *http.Client, clientVersion string) (CodexProvider, error) {
	transport := http.DefaultTransport
	if client != nil && client.Transport != nil {
		transport = client.Transport
	}
	if client == nil {
		client = httpClient(timeout)
	} else {
		client = &http.Client{Transport: client.Transport, Timeout: client.Timeout}
	}
	if strings.TrimSpace(clientVersion) == "" {
		clientVersion = codexCatalogClientVersion
	}
	client.Transport = codexRefreshTransport{auth: auth, inner: transport}
	endpoints := codexEndpoints.withDefaults()
	inner, err := coreproviders.NewCodexProvider(coreproviders.CodexProviderConfig{
		SessionSource: codexSessionSource{auth: auth},
		Instructions:  codexInstructions,
		ResponsesURL:  strings.TrimRight(endpoints.ResponsesBaseURL, "/") + "/responses",
		ModelsURL:     endpoints.ModelsURL,
		ClientVersion: clientVersion,
		Client:        client,
	})
	if err != nil {
		return CodexProvider{}, &ConfigError{Msg: "openai_codex: initialize shared transport: " + err.Error()}
	}
	return CodexProvider{inner: inner, auth: auth}, nil
}

type codexSessionSource struct{ auth codexAuth }

func (s codexSessionSource) Session(context.Context) (coreproviders.CodexSession, error) {
	_, headers, _, err := s.auth.PrepareObserved()
	if err != nil {
		return coreproviders.CodexSession{}, err
	}
	authorization := strings.TrimSpace(headers.Get("Authorization"))
	tokenType, accessToken, ok := strings.Cut(authorization, " ")
	if !ok || strings.TrimSpace(accessToken) == "" {
		return coreproviders.CodexSession{}, invocation("openai_codex: active connection has no access token")
	}
	return coreproviders.CodexSession{
		Token:     &providerauth.Token{AccessToken: strings.TrimSpace(accessToken), TokenType: strings.TrimSpace(tokenType)},
		AccountID: headers.Get("ChatGPT-Account-ID"),
	}, nil
}

// codexRefreshTransport supplies the one gateway-owned behavior intentionally
// outside the shared transport: rotate the private IAM session after a 401 and
// replay the request once with the CAS-protected replacement.
type codexRefreshTransport struct {
	auth  codexAuth
	inner http.RoundTripper
}

func (t codexRefreshTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method == http.MethodGet && request.Header.Get("OpenAI-Beta") != "" {
		request = request.Clone(request.Context())
		request.Header.Del("OpenAI-Beta")
	}
	response, err := t.inner.RoundTrip(request)
	if err != nil || response.StatusCode != http.StatusUnauthorized {
		return response, err
	}
	response.Body.Close()
	if err := t.auth.Refresh(); err != nil {
		return nil, err
	}
	_, headers, _, err := t.auth.PrepareObserved()
	if err != nil {
		return nil, err
	}
	retry := request.Clone(request.Context())
	if request.GetBody != nil {
		retry.Body, err = request.GetBody()
		if err != nil {
			return nil, err
		}
	}
	for _, name := range []string{"Authorization", "ChatGPT-Account-ID", "originator"} {
		if value := headers.Get(name); value != "" {
			retry.Header.Set(name, value)
		} else {
			retry.Header.Del(name)
		}
	}
	if request.Header.Get("OpenAI-Beta") != "" {
		retry.Header.Set("OpenAI-Beta", headers.Get("OpenAI-Beta"))
	} else {
		retry.Header.Del("OpenAI-Beta")
	}
	return t.inner.RoundTrip(retry)
}

func (p CodexProvider) IsStub() bool { return false }
func (p CodexProvider) PreservesWireNativeSurface(_ string, surface core.ModelSurface) bool {
	return surface == core.ModelSurfaceResponses
}
func (p CodexProvider) Complete(model string, messages []Message, kw Kwargs) (map[string]any, error) {
	response, _, err := p.CompleteWithObservation(model, messages, kw)
	return response, err
}
func (p CodexProvider) CompleteWithObservation(
	model string, messages []Message, kw Kwargs,
) (map[string]any, *CredentialObservation, error) {
	return p.CompleteContextWithObservation(context.Background(), model, messages, kw)
}
func (p CodexProvider) CompleteResponses(
	model string, payload map[string]any,
) (map[string]any, *CredentialObservation, error) {
	return p.CompleteResponsesContext(context.Background(), model, payload)
}
func (p CodexProvider) StreamResponses(
	model string, payload map[string]any,
) (StreamIter, *CredentialObservation, error) {
	return p.StreamResponsesContext(context.Background(), model, payload)
}
func (p CodexProvider) Stream(model string, messages []Message, kw Kwargs) (StreamIter, error) {
	return p.StreamContext(context.Background(), model, messages, kw)
}

func (p CodexProvider) ListModels() []ModelInfo {
	models, _, _ := p.ListModelsWithError()
	return models
}

func (p CodexProvider) ListModelsWithError() (
	[]ModelInfo, *CredentialObservation, error,
) {
	observation, err := p.observation()
	if err != nil {
		return nil, observation, catalogError(
			"catalog_authentication_failed",
			"Provider authentication failed before catalog access.",
			0,
		)
	}
	models, err := p.inner.ListModels(context.Background(), nil)
	if err != nil {
		return nil, observation, codexCatalogError(err)
	}
	observation = p.currentObservation(observation)
	rows := make([]ModelInfo, 0, len(models))
	for _, model := range models {
		if model.APIEligible == nil || !*model.APIEligible || model.APIVisibility != "list" {
			continue
		}
		surfaces := model.SupportedAPIs
		if len(surfaces) == 0 {
			surfaces = []string{"/responses"}
		}
		rows = append(rows, ModelInfo{
			ID: model.ID, Vendor: model.OwnedBy, Label: model.Description,
			TypedCapabilities: model.Capabilities, SupportedSurfaces: surfaces,
		})
	}
	if len(rows) == 0 {
		return nil, observation, catalogError(
			"catalog_no_usable_models",
			"Provider catalog returned no API-eligible visible models.",
			0,
		)
	}
	return rows, observation, nil
}

var _ OpenAIAuth = codexAuth{}
var _ Provider = CodexProvider{}

func (p CodexProvider) observation() (*CredentialObservation, error) {
	if p.auth.providerID == "" {
		return nil, nil
	}
	_, _, observation, err := p.auth.PrepareObserved()
	return observation, err
}

func (p CodexProvider) currentObservation(fallback *CredentialObservation) *CredentialObservation {
	if p.auth.providerID == "" {
		return fallback
	}
	_, _, observation, ok, err := iam.OAuthProviderConnectionSecretWithObservation(
		p.auth.principalID, p.auth.providerID, p.auth.connectionName,
	)
	if err == nil && ok {
		return credentialObservation(&observation)
	}
	return fallback
}

func codexCatalogError(err error) error {
	var catalog *coreproviders.CatalogError
	if !errors.As(err, &catalog) {
		return catalogError("catalog_failed", "Provider catalog failed.", 0)
	}
	var refreshErr *InvocationError
	if errors.As(catalog.Cause, &refreshErr) {
		return catalogError("catalog_refresh_failed", "Provider credential refresh failed.", http.StatusUnauthorized)
	}
	if catalog.Status == http.StatusUnauthorized || catalog.Status == http.StatusForbidden {
		return catalogError("catalog_authentication_failed", "Provider authentication failed during catalog access.", catalog.Status)
	}
	switch catalog.Code {
	case "authentication_failed":
		return catalogError("catalog_authentication_failed", "Provider authentication failed before catalog access.", catalog.Status)
	case "transport_error":
		return catalogError("catalog_transport_error", "Provider catalog request could not reach the upstream service.", catalog.Status)
	case "invalid_response":
		detail := "Provider catalog response was invalid."
		if catalog.Cause != nil && strings.Contains(catalog.Cause.Error(), "exceeds size limit") {
			detail = "Provider catalog response exceeded the size limit."
		}
		return catalogError("catalog_not_discoverable", detail, catalog.Status)
	default:
		return catalogError("catalog_http_error", "Provider catalog returned HTTP "+strconv.Itoa(catalog.Status)+".", catalog.Status)
	}
}

type codexCoreStream struct {
	inner core.StreamIter
	err   error
}

func (s *codexCoreStream) Next() (string, bool) {
	for {
		frame, err := s.inner.Next()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				s.err = codexInvocationError(err)
			}
			return "", false
		}
		data := strings.TrimSpace(string(frame))
		data = strings.TrimSpace(strings.TrimPrefix(data, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		return data, true
	}
}

func (s *codexCoreStream) Err() error   { return s.err }
func (s *codexCoreStream) Close() error { return s.inner.Close() }

func codexInvocationError(err error) error {
	if err == nil || IsInvocation(err) || IsConfig(err) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var invocationErr *coreproviders.InvocationError
	if !errors.As(err, &invocationErr) {
		return invocation("openai_codex: shared transport failed")
	}
	retryAfter := ""
	if invocationErr.RetryAfter > 0 {
		retryAfter = strconv.FormatInt(int64(invocationErr.RetryAfter/time.Second), 10)
	}
	return &InvocationError{
		Msg: "openai_codex: " + invocationErr.Error(), Status: invocationErr.Status,
		RetryAfter: retryAfter, Retryable: invocationErr.Retryable,
		FailoverEligible: invocationErr.FailoverEligible, CircuitFailure: invocationErr.CircuitFailure,
	}
}

// codexChatPayload builds the Chat facade request. The Codex Responses
// transport accepts only messages, tools and a prompt cache key. Length and
// sampling hints (max_tokens, temperature, top_p, stop) are advisory and
// ignored, as the official Codex client never sends them. Fields that change
// the structure of the answer are rejected rather than silently dropped.
func codexChatPayload(model string, messages []Message, kw Kwargs) (map[string]any, error) {
	for field, value := range kw {
		if value != nil && codexStructuralChatFields[field] && !codexChatFieldAllowed(field, value) {
			return nil, &ConfigError{Msg: "openai_codex: Chat field " + field + " is not supported by the Codex Responses transport"}
		}
	}
	payload := map[string]any{"model": model, "messages": messages}
	if tools := kw["tools"]; tools != nil {
		payload["tools"] = tools
	}
	if cacheKey, _ := kw["prompt_cache_key"].(string); cacheKey != "" {
		payload["prompt_cache_key"] = cacheKey
	}
	return payload, nil
}

var codexStructuralChatFields = map[string]bool{
	"response_format": true, "n": true, "logprobs": true, "top_logprobs": true, "audio": true,
	"modalities": true, "prediction": true, "tool_choice": true, "parallel_tool_calls": true,
}

// codexChatFieldAllowed accepts a structural field only at a value that
// matches what the Codex transport does anyway.
func codexChatFieldAllowed(field string, value any) bool {
	switch field {
	case "n":
		return intOf(value) == 1
	case "logprobs", "parallel_tool_calls":
		enabled, ok := value.(bool)
		return ok && enabled == (field == "parallel_tool_calls")
	case "tool_choice":
		return value == "auto"
	case "modalities":
		list, ok := value.([]any)
		return ok && len(list) == 1 && list[0] == "text"
	case "response_format":
		format, ok := value.(map[string]any)
		return ok && format["type"] == "text"
	default:
		return false
	}
}

func (p CodexProvider) CompleteContext(ctx context.Context, model string, messages []Message, kw Kwargs) (map[string]any, error) {
	response, _, err := p.CompleteContextWithObservation(ctx, model, messages, kw)
	return response, err
}

func (p CodexProvider) CompleteContextWithObservation(ctx context.Context, model string, messages []Message, kw Kwargs) (map[string]any, *CredentialObservation, error) {
	observation, err := p.observation()
	if err != nil {
		return nil, observation, err
	}
	payload, err := codexChatPayload(model, messages, kw)
	if err != nil {
		return nil, observation, err
	}
	response, err := p.inner.Complete(ctx, model, payload, nil)
	return response, p.currentObservation(observation), codexInvocationError(err)
}

func (p CodexProvider) StreamContext(ctx context.Context, model string, messages []Message, kw Kwargs) (StreamIter, error) {
	if _, err := p.observation(); err != nil {
		return nil, err
	}
	payload, err := codexChatPayload(model, messages, kw)
	if err != nil {
		return nil, err
	}
	stream, err := p.inner.Stream(ctx, model, payload, nil)
	if err != nil {
		return nil, codexInvocationError(err)
	}
	return &codexCoreStream{inner: stream}, nil
}

func (p CodexProvider) CompleteResponsesContext(ctx context.Context, model string, payload map[string]any) (map[string]any, *CredentialObservation, error) {
	observation, err := p.observation()
	if err != nil {
		return nil, observation, err
	}
	payload = normalizeCodexResponsesInput(payload)
	response, err := p.inner.CompleteResponses(ctx, model, payload, nil)
	return response, p.currentObservation(observation), codexInvocationError(err)
}

func (p CodexProvider) StreamResponsesContext(ctx context.Context, model string, payload map[string]any) (StreamIter, *CredentialObservation, error) {
	observation, err := p.observation()
	if err != nil {
		return nil, observation, err
	}
	payload = normalizeCodexResponsesInput(payload)
	stream, err := p.inner.StreamResponses(ctx, model, payload, nil)
	if err != nil {
		return nil, p.currentObservation(observation), codexInvocationError(err)
	}
	return &codexCoreStream{inner: stream}, p.currentObservation(observation), nil
}

func normalizeCodexResponsesInput(payload map[string]any) map[string]any {
	if text, ok := payload["input"].(string); ok {
		copy := make(map[string]any, len(payload))
		for key, value := range payload {
			copy[key] = value
		}
		copy["input"] = []any{map[string]any{"role": "user", "content": text}}
		return copy
	}
	return payload
}
