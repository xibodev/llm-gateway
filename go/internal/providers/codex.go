package providers

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"

	codexauth "github.com/xibodev/llm-provider-auth/codex"
	"github.com/xibodev/llm-provider-auth/tokenstore"
	core "github.com/xibodev/llmgw-core"
	coreproviders "github.com/xibodev/llmgw-core/providers"
)

// codexRefreshInvocationError reports a failed Codex refresh with the status
// and retry semantics the gateway's routing acts on.
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

func codexAccountMismatch(expected, actual string) bool {
	expected = strings.TrimSpace(expected)
	actual = strings.TrimSpace(actual)
	return expected != "" && actual != "" && expected != actual
}

// RefreshCodexOAuthConnection is the console's "refresh now". It runs the
// refresh inference runs, through a Coordinator over the credential store, so
// it takes the same lease, account pin and compare-and-swap, and it refreshes
// even a token that has not expired: the Coordinator's Rejected refreshes
// whenever the record it is handed is still current.
func RefreshCodexOAuthConnection(
	principalID, providerID, connectionName string,
) (iam.OAuthTokenEnvelope, iam.ProviderConnection, error) {
	if err := Current().refreshCodexConnection(context.Background(), principalID, providerID, connectionName); err != nil {
		return iam.OAuthTokenEnvelope{}, iam.ProviderConnection{}, err
	}
	envelope, connection, ok, err := iam.OAuthProviderConnectionSecret(principalID, providerID, connectionName)
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

func (rt *Runtime) refreshCodexConnection(ctx context.Context, principalID, providerID, connectionName string) error {
	envelope, connection, ok, err := iam.OAuthProviderConnectionSecret(principalID, providerID, connectionName)
	if err != nil {
		return invocation("openai_codex: load refresh token: " + err.Error())
	}
	if !ok || strings.TrimSpace(envelope.RefreshToken) == "" {
		return errNoCodexRefreshToken()
	}
	ctx, _ = withOAuthCall(ctx, codexVertical, principalID, providerID)
	coordinator, err := rt.codexCoordinator()
	var record tokenstore.Record
	if err == nil {
		record, err = rt.credentials.Load(ctx, connection.ID)
	}
	if err == nil {
		_, err = coordinator.Rejected(ctx, connection.ID, record)
	}
	return codexFailure(err)
}

// codexCoordinator refreshes Codex credentials outside the core Runtime, for
// the catalog, which stays on the gateway's path, and for the console. Its
// refreshes take the credential store's lease, so they serialize with the
// Runtime's own coordinators in this process and every other.
func (rt *Runtime) codexCoordinator() (*tokenstore.Coordinator, error) {
	return tokenstore.NewCoordinator(rt.credentials, rt.codexRefresh(EffectiveCodexClientID()))
}

// codexGrantClient returns the OAuth client a connection's grant belongs to,
// from the profile and client stored with it. A connection without them
// predates stored client profiles, and using the configured client instead
// could spend its grant against a client it was never issued to.
func codexGrantClient(profile, clientID string) (string, error) {
	profile = strings.TrimSpace(profile)
	if (profile != codexOAuthProfileDevice && profile != codexOAuthProfileBrowser) || strings.TrimSpace(clientID) == "" {
		return "", errors.New("OAuth client profile is unavailable; reauthorize this connection")
	}
	return strings.TrimSpace(clientID), nil
}

const codexInstructions = "Follow the caller's request."

// codexCatalogClientVersion is the catalog schema compatibility contract this
// gateway has verified, not an attempt to impersonate an installed Codex CLI.
const codexCatalogClientVersion = "0.155.1"

// CodexProvider is the gateway's Codex facade. Inference goes through the
// core Runtime, which resolves the caller's own connection, keeps its token
// fresh and replays once a request whose token the upstream rejected. The
// catalog stays on the gateway's path: it calls core's Codex transport itself
// with a credential from the same store, refreshed by the same rules.
type CodexProvider struct {
	runtime  *Runtime
	instance string
	caller   core.Caller
	// catalog is core's Codex, used only for the catalog. It sends
	// clientVersion, and its client performs the catalog requests.
	catalog *coreproviders.Codex
}

// codexInstance reports whether a configured provider is served by Codex.
func codexInstance(providerID string, cfg *config.ProviderConfig) bool {
	switch strings.ToLower(strings.TrimSpace(cfg.Type)) {
	case "openai_compatible", "openai", "litellm":
		return EffectiveRegistryID(providerID, cfg.RegistryID, cfg.Type) == "openai_codex"
	}
	return false
}

// newCodexProvider returns the facade of instance for caller. A nil client
// times out after timeout seconds, and an empty clientVersion is the one this
// gateway verified the catalog against.
func (rt *Runtime) newCodexProvider(
	instance string, caller core.Caller, timeout float64, client *http.Client, clientVersion string,
) (CodexProvider, error) {
	if client == nil {
		client = httpClient(timeout)
	}
	if strings.TrimSpace(clientVersion) == "" {
		clientVersion = codexCatalogClientVersion
	}
	catalog, err := rt.newCoreCodex(client, clientVersion)
	if err != nil {
		return CodexProvider{}, err
	}
	return CodexProvider{runtime: rt, instance: instance, caller: caller, catalog: catalog}, nil
}

// call returns ctx carrying the oauthCall of one operation of this facade.
func (p CodexProvider) call(ctx context.Context) (context.Context, *oauthCall) {
	return withOAuthCall(ctx, codexVertical, callerPrincipalID(p.caller), p.instance)
}

func (p CodexProvider) IsStub() bool { return false }
func (p CodexProvider) PreservesWireNativeSurface(_ string, surface core.ModelSurface) bool {
	return surface == core.ModelSurfaceResponses
}
func (p CodexProvider) Complete(model string, messages []Message, kw Kwargs) (map[string]any, error) {
	return p.CompleteContext(context.Background(), model, messages, kw)
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
	ctx, collector := collectCredentials(context.Background())
	models, err := p.listModels(ctx)
	if err != nil {
		return nil, collector.Observation(), err
	}
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
		return nil, collector.Observation(), catalogError(
			"catalog_no_usable_models",
			"Provider catalog returned no API-eligible visible models.",
			0,
		)
	}
	return rows, collector.Observation(), nil
}

// listModels fetches the catalog the way the core Runtime performs an
// operation: it resolves the caller's connection, takes a fresh token, and
// refreshes and retries once when the upstream rejects that token.
func (p CodexProvider) listModels(ctx context.Context) ([]core.ModelInfo, error) {
	ctx, call := p.call(ctx)
	coordinator, err := p.runtime.codexCoordinator()
	var key string
	if err == nil {
		key, err = p.runtime.credentials.Resolve(ctx, p.caller, p.instance)
	}
	var record tokenstore.Record
	if err == nil {
		record, err = coordinator.Token(ctx, key)
	}
	if err != nil {
		return nil, catalogError("catalog_authentication_failed", "Provider authentication failed before catalog access.", 0)
	}
	call.attempt()
	models, err := p.catalog.ListModels(ctx, core.CredentialFromRecord(key, record))
	var catalog *coreproviders.CatalogError
	if errors.As(err, &catalog) && catalog.Status == http.StatusUnauthorized {
		refreshed, refreshErr := coordinator.Rejected(ctx, key, record)
		switch {
		case errors.Is(refreshErr, tokenstore.ErrNoRefreshToken):
			// Nothing can refresh the credential, so the upstream's
			// rejection stands.
		case refreshErr != nil:
			return nil, catalogError("catalog_refresh_failed", "Provider credential refresh failed.", http.StatusUnauthorized)
		default:
			call.attempt()
			models, err = p.catalog.ListModels(ctx, core.CredentialFromRecord(key, refreshed))
		}
	}
	if err != nil {
		return nil, codexCatalogError(err)
	}
	return models, nil
}

var _ Provider = CodexProvider{}

func codexCatalogError(err error) error {
	var catalog *coreproviders.CatalogError
	if !errors.As(err, &catalog) {
		return catalogError("catalog_failed", "Provider catalog failed.", 0)
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
				s.err = codexFailure(err)
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

// failure returns the error the Codex path returned for err, which the core
// Runtime returned for this call. After an upstream 401 the Runtime refreshes
// once and, when the refresh fails, returns the 401; the Codex path returned
// the refresh failure, which the call recorded, so that is what it reports.
func (c *oauthCall) failure(err error) error {
	if rejection := c.rejected(); rejection != nil && core.ClassifyError(err).StatusCode == http.StatusUnauthorized {
		err = rejection
	}
	return codexFailure(err)
}

// codexFailure maps what the core Runtime and a Coordinator return for a
// Codex operation to the gateway error the Codex path returned, so status,
// retry, failover and circuit decisions stay as they were. A gateway error
// in the chain is the store's or the refresh's own report and wins; an
// upstream failure keeps core's classification, as before.
func codexFailure(err error) error {
	if err == nil || isContextError(err) {
		return err
	}
	var invocationErr *InvocationError
	if errors.As(err, &invocationErr) {
		return invocationErr
	}
	var configErr *ConfigError
	if errors.As(err, &configErr) {
		return configErr
	}
	switch {
	case errors.Is(err, tokenstore.ErrNoRefreshToken):
		return errNoCodexRefreshToken()
	case errors.Is(err, tokenstore.ErrIdentityChanged):
		return errCodexAccountChanged()
	}
	var upstream *coreproviders.InvocationError
	if errors.As(err, &upstream) {
		retryAfter := ""
		if upstream.RetryAfter > 0 {
			retryAfter = strconv.FormatInt(int64(upstream.RetryAfter/time.Second), 10)
		}
		return &InvocationError{
			Msg: "openai_codex: " + upstream.Error(), Status: upstream.Status,
			RetryAfter: retryAfter, Retryable: upstream.Retryable,
			FailoverEligible: upstream.FailoverEligible, CircuitFailure: upstream.CircuitFailure,
		}
	}
	// The Runtime reports a credential it could not obtain, a failed lease
	// or a refresh that lost to a stale write, as an authentication failure.
	var providerErr *core.ProviderError
	if errors.As(err, &providerErr) && providerErr.Class == core.ProviderErrorAuth {
		return invocation("openai_codex: refresh failed")
	}
	return invocation("openai_codex: shared transport failed")
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

// codexStructuralChatFields are the Chat fields that change the structure of
// the answer. It is constant after init: nothing writes it.
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
	payload, err := codexChatPayload(model, messages, kw)
	if err != nil {
		return nil, err
	}
	return p.invoke(ctx, core.ModelSurfaceChatCompletions, model, payload)
}

// CompleteContextWithObservation also reports the credential the request
// used; see credentialCollector.
func (p CodexProvider) CompleteContextWithObservation(ctx context.Context, model string, messages []Message, kw Kwargs) (map[string]any, *CredentialObservation, error) {
	ctx, collector := collectCredentials(ctx)
	response, err := p.CompleteContext(ctx, model, messages, kw)
	return response, collector.Observation(), err
}

func (p CodexProvider) StreamContext(ctx context.Context, model string, messages []Message, kw Kwargs) (StreamIter, error) {
	payload, err := codexChatPayload(model, messages, kw)
	if err != nil {
		return nil, err
	}
	return p.stream(ctx, core.ModelSurfaceChatCompletions, model, payload)
}

func (p CodexProvider) CompleteResponsesContext(ctx context.Context, model string, payload map[string]any) (map[string]any, *CredentialObservation, error) {
	ctx, collector := collectCredentials(ctx)
	response, err := p.invoke(ctx, core.ModelSurfaceResponses, model, normalizeCodexResponsesInput(payload))
	return response, collector.Observation(), err
}

func (p CodexProvider) StreamResponsesContext(ctx context.Context, model string, payload map[string]any) (StreamIter, *CredentialObservation, error) {
	ctx, collector := collectCredentials(ctx)
	stream, err := p.stream(ctx, core.ModelSurfaceResponses, model, normalizeCodexResponsesInput(payload))
	return stream, collector.Observation(), err
}

// invoke sends payload, the body the Codex path handed core's CodexProvider,
// through the core Runtime. Core's Codex shapes it for upstream exactly as
// that provider did, so the upstream request is unchanged.
func (p CodexProvider) invoke(ctx context.Context, surface core.ModelSurface, model string, payload map[string]any) (map[string]any, error) {
	request, err := codexRequest(surface, model, payload)
	if err != nil {
		return nil, err
	}
	ctx, call := p.call(ctx)
	response, err := p.runtime.core.Invoke(ctx, p.caller, p.instance, request)
	if err != nil {
		return nil, call.failure(err)
	}
	var result map[string]any
	if err := json.Unmarshal(response.Body, &result); err != nil {
		return nil, invocation("openai_codex: shared transport returned an invalid response")
	}
	return result, nil
}

func (p CodexProvider) stream(ctx context.Context, surface core.ModelSurface, model string, payload map[string]any) (StreamIter, error) {
	request, err := codexRequest(surface, model, payload)
	if err != nil {
		return nil, err
	}
	ctx, call := p.call(ctx)
	stream, err := p.runtime.core.Stream(ctx, p.caller, p.instance, request)
	if err != nil {
		return nil, call.failure(err)
	}
	return &codexCoreStream{inner: stream}, nil
}

func codexRequest(surface core.ModelSurface, model string, payload map[string]any) (core.Request, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return core.Request{}, invocation("openai_codex: Codex request encoding failed")
	}
	return core.Request{Surface: surface, Model: model, Body: body, ContentType: core.ContentTypeJSON}, nil
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
