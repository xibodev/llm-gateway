package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"llmgw/internal/config"
	"llmgw/internal/diagnostics"
	"llmgw/internal/iam"
	"llmgw/internal/providers"

	core "github.com/xibodev/llmgw-core"
	"github.com/xibodev/llmgw-core/oauthflow"
)

const maxOAuthDiagnosticChars = 300

func oauthErrorText(text string) string {
	return diagnostics.SanitizeTextLimit(text, maxOAuthDiagnosticChars)
}

func safeOAuthPollResponse(status, detail string) map[string]any {
	response := map[string]any{"status": status}
	if detail := oauthErrorText(detail); detail != "" {
		response["error"] = detail
	}
	return response
}

func oauthAdapterFor(providerRef string) (string, string, providers.ProviderAuthAdapter, error) {
	reference := strings.ToLower(strings.TrimSpace(providerRef))
	if reference == "" {
		return "", "", nil, fmt.Errorf("provider_id is required")
	}
	providerID := reference
	runtimeType := ""
	registryID := ""
	if providerConfig, ok := config.Get().Providers[reference]; ok {
		runtimeType = strings.ToLower(strings.TrimSpace(providerConfig.Type))
		registryID = providers.EffectiveRegistryID(
			reference, providerConfig.RegistryID, providerConfig.Type,
		)
	} else {
		registryID = providers.CanonicalRegistryID(reference)
		matches := make([]string, 0, 1)
		matchRegistryIDs := map[string]string{}
		settings := config.Get()
		for id, providerConfig := range settings.Providers {
			configuredRegistryID := providers.EffectiveRegistryID(
				id, providerConfig.RegistryID, providerConfig.Type,
			)
			if configuredRegistryID == registryID {
				matches = append(matches, id)
				matchRegistryIDs[id] = configuredRegistryID
			}
		}
		sort.Strings(matches)
		if len(matches) > 1 {
			return "", "", nil, fmt.Errorf(
				"multiple configured providers use registry %q; select a configured provider id",
				registryID,
			)
		}
		if len(matches) == 1 {
			providerID = matches[0]
			providerConfig := settings.Providers[providerID]
			runtimeType = strings.ToLower(strings.TrimSpace(providerConfig.Type))
			registryID = matchRegistryIDs[providerID]
		}
	}
	if runtimeType == "" && registryID == "github_copilot" {
		providerID, runtimeType, registryID = "copilot", "github_copilot", "github_copilot"
	}
	if runtimeType == "" && registryID == "openai_codex" {
		providerID, runtimeType, registryID = "codex", "openai_codex", "openai_codex"
	}
	if runtimeType == "" && registryID == "google_antigravity" {
		providerID, runtimeType, registryID = "google-antigravity", "google_antigravity", "google_antigravity"
	}
	if registryID == "" {
		switch runtimeType {
		case "github_copilot":
			registryID = "github_copilot"
		case "openai_codex":
			registryID = "openai_codex"
		case "google_antigravity":
			registryID = "google_antigravity"
		}
	}
	entry, ok := providers.RegistryProviderByID(registryID)
	if !ok || entry.AuthAdapter == "" {
		return "", "", nil, fmt.Errorf("provider %q does not expose a supported official OAuth flow", providerRef)
	}
	if runtimeType != "" && !providers.RegistryRuntimeMatches(entry, runtimeType) {
		return "", "", nil, fmt.Errorf(
			"provider %q runtime %q does not match registry integration %q",
			providerID, runtimeType, entry.ID,
		)
	}
	adapter, err := providers.NewProviderAuthAdapter(entry.AuthAdapter, providerID)
	if err != nil {
		return "", "", nil, err
	}
	return providerID, adapter.CredentialKind(), adapter, nil
}

func ensureOAuthProviderConfig(providerID, providerRef string) error {
	endpointMutationMu.Lock()
	defer endpointMutationMu.Unlock()
	if _, exists := config.Get().Providers[providerID]; exists {
		return nil
	}
	if err := providerEndpointCollision(providerID); err != nil {
		return err
	}
	reference := providers.CanonicalRegistryID(providerRef)
	if reference == "copilot" {
		reference = "github_copilot"
	}
	if reference == "codex" {
		reference = "openai_codex"
	}
	entry, found := providers.RegistryProvider(reference)
	if !found || entry.ClientOnly {
		return nil
	}
	_, err := config.AddProviderIfMissing(providerID, &config.ProviderConfig{
		Type: entry.RuntimeType, RegistryID: entry.ID, BaseURL: entry.DefaultBaseURL, Region: entry.DefaultRegion,
	})
	if err != nil {
		return fmt.Errorf("could not persist the OAuth provider configuration")
	}
	return nil
}

func oauthRegistryIDForRef(providerRef string) string {
	reference := strings.ToLower(strings.TrimSpace(providerRef))
	if providerConfig, ok := config.Get().Providers[reference]; ok {
		return providers.EffectiveRegistryID(
			reference, providerConfig.RegistryID, providerConfig.Type,
		)
	}
	return providers.CanonicalRegistryID(reference)
}

func configureOAuthClientID(providerRef, clientID string, allowUpdate bool) error {
	if oauthRegistryIDForRef(providerRef) != "openai_codex" {
		return nil
	}
	clientID = strings.TrimSpace(clientID)
	if clientID != "" {
		if !allowUpdate {
			return fmt.Errorf("Only an administrator can configure the Codex OAuth client ID.")
		}
		previous := config.Get().OpenAICodexClientID
		config.Update(func(settings *config.Settings) {
			settings.OpenAICodexClientID = clientID
		})
		if err := config.Save(); err != nil {
			config.Update(func(settings *config.Settings) {
				settings.OpenAICodexClientID = previous
			})
			return fmt.Errorf("Could not persist the Codex OAuth client ID.")
		}
	}
	if strings.TrimSpace(providers.EffectiveCodexClientID()) == "" {
		return fmt.Errorf("OpenAI Codex client ID is required. Enter it in the official sign-in dialog.")
	}
	return nil
}

func (s *server) startOAuthFlow(
	principal iam.Principal, providerRef, clientID string, allowClientIDUpdate bool,
	request *http.Request, connectionName, source, preferredFlow string,
) (response map[string]any, err error) {
	previousCodexClientID := config.Get().OpenAICodexClientID
	rollbackCodexClientID := oauthRegistryIDForRef(providerRef) == "openai_codex" &&
		allowClientIDUpdate && strings.TrimSpace(clientID) != "" &&
		strings.TrimSpace(clientID) != strings.TrimSpace(previousCodexClientID)
	flowStarted := false
	defer func() {
		if !rollbackCodexClientID || flowStarted {
			return
		}
		config.Update(func(settings *config.Settings) {
			settings.OpenAICodexClientID = previousCodexClientID
		})
		_ = config.Save()
	}()
	if err := configureOAuthClientID(providerRef, clientID, allowClientIDUpdate); err != nil {
		return nil, err
	}
	providerID, kind, adapter, err := oauthAdapterFor(providerRef)
	if err != nil {
		return nil, err
	}
	_, providerConfigured := config.Get().Providers[providerID]
	if !providerConfigured && !allowClientIDUpdate {
		return nil, fmt.Errorf("Only an administrator can configure this OAuth provider.")
	}
	_, browserSupported := adapter.(providers.BrowserProviderAuthAdapter)
	_, deviceSupported := adapter.(providers.DeviceProviderAuthAdapter)
	useBrowser := preferredFlow == "browser" || (!deviceSupported && browserSupported)
	if preferredFlow != "" && preferredFlow != "browser" && preferredFlow != "device_code" {
		return nil, fmt.Errorf("Unsupported OAuth flow.")
	}
	// The provider is configured only once its sign-in has started. A start
	// whose configuration then fails leaves its flow behind unreturned: its
	// ID never reaches the client, and the cap evicts it first.
	ctx, startedAt := context.Background(), s.now()
	if useBrowser && browserSupported {
		callbackID := oauthRegistryIDForRef(providerRef)
		redirectURI, err := oauthCallbackURL(request, callbackID)
		if err != nil {
			return nil, err
		}
		view, err := s.oauth.Start(ctx, oauthCaller(principal), providerID, oauthflow.MethodBrowser,
			oauthflow.WithRedirectURI(redirectURI), oauthflow.WithParams(oauthConnectionParams(connectionName, source, kind)))
		if err != nil {
			return nil, err
		}
		if !providerConfigured {
			if err := ensureOAuthProviderConfig(providerID, providerRef); err != nil {
				return nil, err
			}
		}
		flowStarted = true
		return map[string]any{
			"provider_id": providerID, "flow": "browser", "authorization_url": view.AuthorizationURL,
			"flow_id": view.ID, "expires_in": oauthExpiresIn(view, startedAt), "expires_at": view.ExpiresAt.Unix(),
		}, nil
	}
	if !deviceSupported {
		return nil, fmt.Errorf("provider %q does not support device authorization", providerRef)
	}
	// The poll that completes a device flow names its connection.
	view, err := s.oauth.Start(ctx, oauthCaller(principal), providerID, oauthflow.MethodDevice,
		oauthflow.WithParams(map[string]string{oauthParamKind: kind}))
	if err != nil {
		return nil, err
	}
	if !providerConfigured {
		if err := ensureOAuthProviderConfig(providerID, providerRef); err != nil {
			return nil, err
		}
	}
	flowStarted = true
	// device_code carries the flow ID: the provider's device code stays on
	// the server, and clients only echo the value back.
	return map[string]any{
		"provider_id": providerID, "flow": "device_code", "device_code": view.ID,
		"user_code": view.UserCode, "verification_uri": view.VerificationURI,
		"interval": view.Interval, "expires_in": oauthExpiresIn(view, startedAt), "expires_at": view.ExpiresAt.Unix(),
	}, nil
}

func antigravityManualConfig(providerID string, input providers.ProviderAuthManualConfig, persist bool) (providers.ProviderAuthManualConfig, error) {
	if oauthRegistryIDForRef(providerID) != "google_antigravity" {
		return providers.ProviderAuthManualConfig{}, fmt.Errorf("consumer_manual is supported only for Google Antigravity")
	}
	input.ClientID = strings.TrimSpace(input.ClientID)
	input.ClientMode = strings.ToLower(strings.TrimSpace(input.ClientMode))
	input.RedirectURI = strings.TrimSpace(input.RedirectURI)
	if input.ClientID != "" || input.ClientSecret != "" || input.ClientMode != "" || input.RedirectURI != "" {
		if !persist {
			return providers.ProviderAuthManualConfig{}, fmt.Errorf("Only an administrator can configure the consumer_manual OAuth profile.")
		}
		if input.ClientMode == "public" {
			input.ClientSecret = ""
		}
		return input, nil
	}
	settings := config.Get()
	if strings.EqualFold(strings.TrimSpace(settings.GoogleAntigravityOAuthProfile), antigravityManualProfile) {
		configured := providers.ProviderAuthManualConfig{
			ClientID: settings.GoogleAntigravityClientID, ClientSecret: settings.GoogleAntigravityClientSecret,
			ClientMode: settings.GoogleAntigravityClientMode, RedirectURI: settings.GoogleAntigravityRedirectURI,
		}
		if strings.EqualFold(strings.TrimSpace(configured.ClientMode), "public") {
			configured.ClientSecret = ""
		}
		return configured, nil
	}
	stored, ok, err := iam.OAuthClientProfileByName("google_antigravity", antigravityManualProfile)
	if err != nil {
		return providers.ProviderAuthManualConfig{}, fmt.Errorf("Could not load the encrypted consumer_manual OAuth profile.")
	}
	if !ok {
		return providers.ProviderAuthManualConfig{}, fmt.Errorf("The consumer_manual OAuth profile is not configured.")
	}
	return providers.ProviderAuthManualConfig{
		ClientID: stored.ClientID, ClientSecret: stored.ClientSecret,
		ClientMode: stored.ClientMode, RedirectURI: stored.RedirectURI,
	}, nil
}

const antigravityManualProfile = "consumer_manual"
const codexBrowserManualProfile = "browser_pkce"
const codexBrowserLoopbackRedirectURI = "http://localhost:1455/auth/callback"

// manualOAuthParams are a manual flow's start parameters: the connection it
// stores and the OAuth client the owner signs in with.
func manualOAuthParams(connectionName, source, kind string, client providers.ProviderAuthManualConfig, persistProfile bool) map[string]string {
	params := oauthConnectionParams(connectionName, source, kind)
	for key, value := range client.OAuthParams() {
		params[key] = value
	}
	if persistProfile {
		params[oauthParamPersistProfile] = "true"
	}
	return params
}

func (s *server) startManualOAuthFlow(
	principal iam.Principal, providerRef, connectionName, source string,
	input providers.ProviderAuthManualConfig, persistConfig bool,
) (map[string]any, error) {
	providerID, kind, adapter, err := oauthAdapterFor(providerRef)
	if err != nil {
		return nil, err
	}
	if _, ok := adapter.(providers.ManualProviderAuthAdapter); !ok {
		return nil, fmt.Errorf("provider %q does not support manual authorization", providerRef)
	}
	configured, err := antigravityManualConfig(providerID, input, persistConfig)
	if err != nil {
		return nil, err
	}
	startedAt := s.now()
	view, err := s.oauth.Start(context.Background(), oauthCaller(principal), providerID, oauthflow.MethodManual,
		oauthflow.WithParams(manualOAuthParams(connectionName, source, kind, configured, persistConfig)))
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"provider_id": providerID, "flow": "consumer_manual", "profile": antigravityManualProfile,
		"authorization_url": view.AuthorizationURL, "flow_id": view.ID,
		"expires_in": oauthExpiresIn(view, startedAt), "expires_at": view.ExpiresAt.Unix(),
	}, nil
}

func (s *server) startCodexBrowserFlow(principal iam.Principal, providerRef, clientID string, allowClientIDUpdate bool, connectionName, source string) (map[string]any, error) {
	providerID, _, _, adapterErr := oauthAdapterFor(providerRef)
	if adapterErr != nil {
		return nil, adapterErr
	}
	_, providerConfigured := config.Get().Providers[providerID]
	if !providerConfigured && !allowClientIDUpdate {
		return nil, fmt.Errorf("Only an administrator can configure this OAuth provider.")
	}
	previousClientID := config.Get().OpenAICodexClientID
	rollbackClientID := allowClientIDUpdate && strings.TrimSpace(clientID) != "" && strings.TrimSpace(clientID) != strings.TrimSpace(previousClientID)
	flowStarted := false
	defer func() {
		if rollbackClientID && !flowStarted {
			config.Update(func(settings *config.Settings) { settings.OpenAICodexClientID = previousClientID })
			_ = config.Save()
		}
	}()
	if err := configureOAuthClientID(providerRef, clientID, allowClientIDUpdate); err != nil {
		return nil, err
	}
	providerID, kind, adapter, err := oauthAdapterFor(providerRef)
	if err != nil {
		return nil, err
	}
	if _, ok := adapter.(providers.ManualProviderAuthAdapter); !ok {
		return nil, fmt.Errorf("provider %q does not support manual browser authorization", providerRef)
	}
	// The flow captures the client configured now: the owner's grant, and
	// the connection that keeps it, belong to that client.
	capturedConfig := providers.ProviderAuthManualConfig{ClientID: providers.EffectiveCodexClientID(), ClientMode: "public", RedirectURI: codexBrowserLoopbackRedirectURI}
	startedAt := s.now()
	view, err := s.oauth.Start(context.Background(), oauthCaller(principal), providerID, oauthflow.MethodManual,
		oauthflow.WithParams(manualOAuthParams(connectionName, source, kind, capturedConfig, false)))
	if err != nil {
		return nil, err
	}
	flowStarted = true
	return map[string]any{
		"provider_id": providerID, "flow": codexBrowserManualProfile, "profile": codexBrowserManualProfile,
		"authorization_url": view.AuthorizationURL, "flow_id": view.ID,
		"expires_in": oauthExpiresIn(view, startedAt), "expires_at": view.ExpiresAt.Unix(),
	}, nil
}

func (s *server) completeManualOAuthFlow(principal iam.Principal, providerRef, flowID, authorizationResponse string) map[string]any {
	providerID, _, adapter, err := oauthAdapterFor(providerRef)
	if err != nil {
		return safeOAuthPollResponse("error", err.Error())
	}
	if _, ok := adapter.(providers.ManualProviderAuthAdapter); !ok {
		return safeOAuthPollResponse("error", "Provider does not support manual authorization.")
	}
	flowID = strings.TrimSpace(flowID)
	ctx, caller := context.Background(), oauthCaller(principal)
	inactive := safeOAuthPollResponse("expired", "Manual authorization is no longer active. Start again.")
	view, err := s.oauth.Get(ctx, caller, flowID)
	if err != nil || view.Method != oauthflow.MethodManual || view.Status != oauthflow.StatusPending || view.Instance != providerID {
		return inactive
	}
	// A paste that is not a code, or whose state is another attempt's,
	// spends nothing: the owner may paste again.
	code, returnedState, fromURL, err := manualAuthorizationCode(authorizationResponse)
	if err != nil {
		return safeOAuthPollResponse("error", err.Error())
	}
	input := oauthflow.CompleteInput{Code: code}
	if fromURL {
		input.State = returnedState
	}
	completion := &oauthCompletion{providerRef: providerRef}
	_, err = s.oauth.Complete(withOAuthCompletion(ctx, completion), caller, flowID, input)
	switch {
	case err == nil && completion.connection != nil:
		return map[string]any{"status": "authorized", "connection": *completion.connection}
	case errors.Is(err, oauthflow.ErrStateMismatch):
		return safeOAuthPollResponse("error", "The returned OAuth state did not match this authorization flow.")
	case completion.failure != "":
		return safeOAuthPollResponse("error", completion.failure)
	case errors.Is(err, oauthflow.ErrFlowNotFound), errors.Is(err, oauthflow.ErrFlowExpired):
		return inactive
	}
	return safeOAuthPollResponse("error", "Manual authorization failed.")
}

func manualAuthorizationCode(value string) (code, state string, fromURL bool, err error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", "", false, fmt.Errorf("Paste the authorization code or full redirect URL.")
	}
	parsed, parseErr := url.Parse(value)
	if parseErr == nil && parsed.IsAbs() && parsed.Host != "" {
		code, state = strings.TrimSpace(parsed.Query().Get("code")), strings.TrimSpace(parsed.Query().Get("state"))
		if code == "" || state == "" {
			return "", "", true, fmt.Errorf("The redirect URL must include code and state.")
		}
		return code, state, true, nil
	}
	if strings.ContainsAny(value, "?#&=") {
		return "", "", false, fmt.Errorf("Paste a code or a complete absolute redirect URL.")
	}
	return value, "", false, nil
}

func oauthCallbackURL(r *http.Request, providerRef string) (string, error) {
	base := strings.TrimSpace(config.Get().OAuthPublicBaseURL)
	if base == "" {
		host := strings.ToLower(strings.TrimSpace(r.Host))
		if host != "localhost" && !strings.HasPrefix(host, "localhost:") && host != "127.0.0.1" && !strings.HasPrefix(host, "127.0.0.1:") && host != "[::1]" && !strings.HasPrefix(host, "[::1]:") {
			return "", fmt.Errorf("LLMGW_OAUTH_PUBLIC_BASE_URL is required for browser OAuth outside loopback")
		}
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		base = scheme + "://" + r.Host
	}
	parsed, err := url.Parse(base)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" || parsed.User != nil {
		return "", fmt.Errorf("LLMGW_OAUTH_PUBLIC_BASE_URL must be an absolute origin")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return "", fmt.Errorf("LLMGW_OAUTH_PUBLIC_BASE_URL must be an HTTP(S) origin without path, query, or fragment")
	}
	parsed.Path = "/oauth/callback/" + url.PathEscape(providers.CanonicalRegistryID(providerRef))
	parsed.RawQuery, parsed.Fragment = "", ""
	return parsed.String(), nil
}

// pollBrowserOAuthFlow answers a poll of a browser or manual flow, which the
// provider's redirect or the owner's paste finishes rather than the poll.
func (s *server) pollBrowserOAuthFlow(ctx context.Context, caller core.Caller, providerID string, view oauthflow.View) map[string]any {
	if view.Instance != providerID {
		return safeOAuthPollResponse("expired", "Browser authorization is no longer active. Start again.")
	}
	switch {
	case view.Status == oauthflow.StatusPending:
		return map[string]any{"status": "pending"}
	case view.Method != oauthflow.MethodBrowser:
		// The paste that finished a manual flow answered for it.
	case view.Status == oauthflow.StatusFailed:
		return safeOAuthPollResponse("error", "Browser authorization failed.")
	case view.Status == oauthflow.StatusComplete:
		if key, ok := s.takeOAuthOutcome(ctx, caller, view.ID); ok {
			principalID, _ := iam.CallerPrincipalID(caller)
			if connection, found := oauthConnection(principalID, providerID, key); found {
				return map[string]any{"status": "authorized", "connection": connection}
			}
			return safeOAuthPollResponse("error", "Could not load the private OAuth connection.")
		}
	}
	return safeOAuthPollResponse("expired", "Device authorization is no longer active. Start again.")
}

// handleOAuthBrowserCallback finishes a browser flow from the provider's
// redirect. The redirect carries no session: its state finds the flow and
// its owner, and a redirect that arrived anywhere but the flow's own
// callback spends nothing.
func (s *server) handleOAuthBrowserCallback(w http.ResponseWriter, r *http.Request) {
	redirectURI, err := oauthCallbackURL(r, r.PathValue("provider_id"))
	if err != nil {
		http.Error(w, "OAuth authorization is no longer active.", http.StatusBadRequest)
		return
	}
	query := r.URL.Query()
	completion := &oauthCompletion{providerRef: r.PathValue("provider_id")}
	_, err = s.oauth.Callback(withOAuthCompletion(r.Context(), completion), oauthflow.CompleteInput{
		Code: query.Get("code"), State: query.Get("state"), Error: query.Get("error"), RedirectURI: redirectURI,
	})
	switch {
	case err == nil && completion.connection != nil:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<!doctype html><title>Provider connected</title><p>Authorization received. You can close this window and return to the gateway.</p>"))
	case errors.Is(err, oauthflow.ErrFlowNotFound), errors.Is(err, oauthflow.ErrFlowExpired), errors.Is(err, oauthflow.ErrWrongMethod):
		http.Error(w, "OAuth authorization is no longer active.", http.StatusBadRequest)
	default:
		http.Error(w, "Browser authorization failed. Return to the gateway and try again.", http.StatusBadRequest)
	}
}

// pollOAuthFlow answers a poll of the flow flowID names: a device_code or a
// flow_id, which both carry the flow's ID. A device poll asks the provider,
// no more often than its interval, and stores the connection name and
// source the poll carries once the owner approved.
func (s *server) pollOAuthFlow(principal iam.Principal, providerRef, flowID, name, source string) map[string]any {
	providerID, _, adapter, err := oauthAdapterFor(providerRef)
	if err != nil {
		return safeOAuthPollResponse("error", err.Error())
	}
	flowID = strings.TrimSpace(flowID)
	if flowID == "" {
		return safeOAuthPollResponse("error", "device_code is required")
	}
	ctx, caller := context.Background(), oauthCaller(principal)
	inactive := safeOAuthPollResponse("expired", "Device authorization is no longer active. Start again.")
	view, err := s.oauth.Get(ctx, caller, flowID)
	if errors.Is(err, oauthflow.ErrFlowExpired) {
		// An expired flow no longer names its method. The console polls a
		// browser flow only for a provider without device authorization.
		if _, device := adapter.(providers.DeviceProviderAuthAdapter); device {
			return safeOAuthPollResponse("expired", "Device authorization expired. Start again.")
		}
		return safeOAuthPollResponse("expired", "Browser authorization expired. Start again.")
	}
	if err != nil {
		return inactive
	}
	if view.Method != oauthflow.MethodDevice {
		return s.pollBrowserOAuthFlow(ctx, caller, providerID, view)
	}
	if view.Instance != providerID {
		return inactive
	}
	ctx, note := providers.WithOAuthPollNote(ctx)
	completion := &oauthCompletion{providerRef: providerRef, name: name, source: source}
	_, err = s.oauth.Poll(withOAuthCompletion(ctx, completion), caller, flowID)
	switch {
	case err == nil && completion.connection != nil:
		response := safeOAuthPollResponse("authorized", note.Detail)
		response["connection"] = *completion.connection
		return response
	case completion.failure != "":
		return safeOAuthPollResponse("error", completion.failure)
	case errors.Is(err, oauthflow.ErrFlowNotFound), note.Status == "authorized":
		// Another request ended the flow first, or the cap evicted it.
		return inactive
	case note.Polled:
		// The provider answered; its answer is the owner's.
		return safeOAuthPollResponse(note.Status, note.Detail)
	case errors.Is(err, oauthflow.ErrSlowDown):
		return safeOAuthPollResponse("slow_down", "Wait for the provider polling interval before retrying.")
	case errors.Is(err, oauthflow.ErrFlowExpired):
		return safeOAuthPollResponse("expired", "Device authorization expired. Start again.")
	case err != nil:
		return safeOAuthPollResponse("error", err.Error())
	}
	// The flow ended in an earlier poll, which answered for it.
	return inactive
}

func refreshOAuthFlow(principal iam.Principal, providerRef, name string) map[string]any {
	providerID, _, adapter, err := oauthAdapterFor(providerRef)
	if err != nil {
		return safeOAuthPollResponse("error", err.Error())
	}
	if guardedAdapter, ok := adapter.(providers.GuardedRefreshProviderAuthAdapter); ok {
		envelope, connection, err := guardedAdapter.RefreshConnection(
			context.Background(), principal.ID, providerID, name,
		)
		if err != nil {
			return safeOAuthPollResponse("error", "OAuth refresh failed.")
		}
		providers.ForgetProviderForPrincipal(providerID, principal.ID)
		providers.ForgetCatalogForPrincipal(providerID, principal.ID)
		return map[string]any{
			"status": "refreshed", "expires_at": envelope.ExpiresAt,
			"connection": connection,
		}
	}
	envelope, connection, ok, err := iam.OAuthProviderConnectionSecret(principal.ID, providerID, name)
	if err != nil {
		return safeOAuthPollResponse("error", "Could not load the private OAuth connection.")
	}
	if !ok {
		return safeOAuthPollResponse("missing", "No active OAuth connection exists.")
	}
	refreshAdapter, ok := adapter.(providers.RefreshableProviderAuthAdapter)
	if !ok {
		return safeOAuthPollResponse("unsupported", "Provider does not support OAuth refresh.")
	}
	refreshed, err := refreshAdapter.Refresh(context.Background(), envelope)
	if err != nil {
		return safeOAuthPollResponse("error", "OAuth refresh failed.")
	}
	if refreshed.AccessToken != "" {
		refreshToken := refreshed.RefreshToken
		if refreshToken == "" {
			refreshToken = envelope.RefreshToken
		}
		idToken := refreshed.IDToken
		if idToken == "" {
			idToken = envelope.IDToken
		}
		accountID := refreshed.AccountID
		if accountID == "" {
			accountID = envelope.AccountID
		}
		accountLabel := refreshed.AccountLabel
		if accountLabel == "" {
			accountLabel = envelope.AccountLabel
		}
		projectID := refreshed.ProjectID
		if projectID == "" {
			projectID = envelope.ProjectID
		}
		tokenType := refreshed.TokenType
		if tokenType == "" {
			tokenType = envelope.TokenType
		}
		expiresAt := refreshed.ExpiresAt
		if expiresAt == 0 {
			expiresAt = envelope.ExpiresAt
		}
		if _, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
			PrincipalID: principal.ID, ProviderID: providerID, Name: connection.Name, Kind: connection.Kind,
			Source: connection.Source, MakeDefault: connection.IsDefault, AccessToken: refreshed.AccessToken,
			RefreshToken: refreshToken, IDToken: idToken, TokenType: tokenType, ExpiresAt: expiresAt,
			AccountID: accountID, AccountLabel: accountLabel, ProjectID: projectID, Status: "active",
			OAuthProfile: envelope.OAuthProfile, OAuthClientID: envelope.OAuthClientID,
		}); err != nil {
			return safeOAuthPollResponse("error", "OAuth refresh could not be stored.")
		}
	}
	providers.ForgetProviderForPrincipal(providerID, principal.ID)
	providers.ForgetCatalogForPrincipal(providerID, principal.ID)
	return map[string]any{"status": refreshed.Status, "expires_at": refreshed.ExpiresAt}
}

func revokeOAuthFlow(principal iam.Principal, providerRef, name string) map[string]any {
	providerID, _, adapter, err := oauthAdapterFor(providerRef)
	if err != nil {
		return safeOAuthPollResponse("error", err.Error())
	}
	envelope, connection, ok, err := iam.OAuthProviderConnectionSecret(principal.ID, providerID, name)
	if err != nil || !ok {
		return safeOAuthPollResponse("missing", "No active OAuth connection exists.")
	}
	upstream := "revoked"
	if revokeAdapter, ok := adapter.(providers.RevocableProviderAuthAdapter); ok {
		if err := revokeAdapter.Revoke(context.Background(), envelope); err != nil {
			upstream = "best_effort_failed"
		}
	} else {
		upstream = "not_supported"
	}
	revoked, err := iam.RevokeOAuthProviderConnectionIfCurrent(connection, envelope)
	if err != nil {
		return safeOAuthPollResponse("error", "Local OAuth revocation failed.")
	}
	if !revoked {
		return safeOAuthPollResponse("changed", "OAuth connection changed while revocation was in progress.")
	}
	providers.ForgetProviderForPrincipal(providerID, principal.ID)
	providers.ForgetCatalogForPrincipal(providerID, principal.ID)
	return map[string]any{"status": "revoked", "upstream_revocation": upstream}
}

func oauthPrincipalByID(principalID string) (iam.Principal, error) {
	principal, found, err := iam.PrincipalByID(strings.TrimSpace(principalID))
	if err != nil {
		return iam.Principal{}, err
	}
	if !found {
		return iam.Principal{}, fmt.Errorf("unknown principal")
	}
	if principal.Kind != "human" {
		return iam.Principal{}, fmt.Errorf("OAuth connections require a human principal")
	}
	return principal, nil
}

func oauthPollInput(r *http.Request) (string, string, bool) {
	var body struct {
		DeviceCode     string `json:"device_code"`
		FlowID         string `json:"flow_id"`
		ConnectionName string `json:"connection_name"`
	}
	if !decodeBody(r, &body) {
		return "", "", false
	}
	code := strings.TrimSpace(body.DeviceCode)
	if code == "" {
		code = strings.TrimSpace(body.FlowID)
	}
	return code, body.ConnectionName, code != ""
}

type oauthStartRequest struct {
	Flow           string `json:"flow"`
	Profile        string `json:"profile"`
	ClientID       string `json:"client_id"`
	ClientSecret   string `json:"client_secret"`
	ClientMode     string `json:"client_mode"`
	RedirectURI    string `json:"redirect_uri"`
	ConnectionName string `json:"connection_name"`
}

func oauthStartInput(r *http.Request) oauthStartRequest {
	var body oauthStartRequest
	if !decodeBody(r, &body) {
		return oauthStartRequest{}
	}
	body.Profile = strings.ToLower(strings.TrimSpace(body.Profile))
	body.Flow = strings.ToLower(strings.TrimSpace(body.Flow))
	body.ClientID = strings.TrimSpace(body.ClientID)
	body.ClientMode = strings.ToLower(strings.TrimSpace(body.ClientMode))
	body.RedirectURI = strings.TrimSpace(body.RedirectURI)
	body.ConnectionName = strings.TrimSpace(body.ConnectionName)
	return body
}

func oauthCompleteInput(r *http.Request) (string, string, bool) {
	var body struct {
		FlowID                string `json:"flow_id"`
		AuthorizationResponse string `json:"authorization_response"`
	}
	if !decodeBody(r, &body) {
		return "", "", false
	}
	body.FlowID = strings.TrimSpace(body.FlowID)
	body.AuthorizationResponse = strings.TrimSpace(body.AuthorizationResponse)
	return body.FlowID, body.AuthorizationResponse, body.FlowID != "" && body.AuthorizationResponse != ""
}

func (s *server) handleUserOAuthStart(w http.ResponseWriter, r *http.Request) {
	principal, ok := requireSSOUser(w, r)
	if !ok {
		return
	}
	input := oauthStartInput(r)
	var response map[string]any
	var err error
	if input.Flow == "browser" && oauthRegistryIDForRef(r.PathValue("provider_id")) == "openai_codex" {
		response, err = s.startCodexBrowserFlow(principal, r.PathValue("provider_id"), "", false, input.ConnectionName, iam.ConnectionSourceUser)
	} else if input.Profile == antigravityManualProfile {
		response, err = s.startManualOAuthFlow(principal, r.PathValue("provider_id"), input.ConnectionName, iam.ConnectionSourceUser, providers.ProviderAuthManualConfig{}, false)
	} else {
		response, err = s.startOAuthFlow(principal, r.PathValue("provider_id"), "", false, r, input.ConnectionName, iam.ConnectionSourceUser, input.Flow)
	}
	if err != nil {
		writeError(w, 400, oauthErrorText(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *server) handleUserOAuthComplete(w http.ResponseWriter, r *http.Request) {
	principal, ok := requireSSOUser(w, r)
	if !ok {
		return
	}
	flowID, authorizationResponse, valid := oauthCompleteInput(r)
	if !valid {
		writeError(w, 400, "flow_id and authorization_response required")
		return
	}
	response := s.completeManualOAuthFlow(principal, r.PathValue("provider_id"), flowID, authorizationResponse)
	if response["status"] == "authorized" {
		_ = iam.RecordAudit(iam.AuditEvent{ActorPrincipalID: principal.ID, Action: "oauth_connection.connect", TargetType: "principal", TargetID: principal.ID, Result: "success", Detail: map[string]any{"provider": r.PathValue("provider_id"), "source": "self-service", "profile": manualOAuthAuditProfile(r.PathValue("provider_id"))}})
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *server) handleUserOAuthPoll(w http.ResponseWriter, r *http.Request) {
	principal, ok := requireSSOUser(w, r)
	if !ok {
		return
	}
	deviceCode, name, valid := oauthPollInput(r)
	if !valid {
		writeError(w, 400, "device_code required")
		return
	}
	response := s.pollOAuthFlow(principal, r.PathValue("provider_id"), deviceCode, name, iam.ConnectionSourceUser)
	if response["status"] == "authorized" {
		_ = iam.RecordAudit(iam.AuditEvent{ActorPrincipalID: principal.ID, Action: "oauth_connection.connect", TargetType: "principal", TargetID: principal.ID, Result: "success", Detail: map[string]any{"provider": r.PathValue("provider_id"), "source": "self-service"}})
	}
	writeJSON(w, http.StatusOK, response)
}

func handleUserOAuthRefresh(w http.ResponseWriter, r *http.Request) {
	principal, ok := requireSSOUser(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, refreshOAuthFlow(principal, r.PathValue("provider_id"), r.URL.Query().Get("connection_name")))
}

func (s *server) handlePrincipalOAuthStart(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	principal, err := oauthPrincipalByID(r.PathValue("id"))
	if err != nil {
		writeError(w, 400, oauthErrorText(err.Error()))
		return
	}
	input := oauthStartInput(r)
	var response map[string]any
	if input.Flow == "browser" && oauthRegistryIDForRef(r.PathValue("provider_id")) == "openai_codex" {
		response, err = s.startCodexBrowserFlow(principal, r.PathValue("provider_id"), input.ClientID, true, input.ConnectionName, iam.ConnectionSourceAdmin)
	} else if input.Profile == antigravityManualProfile {
		response, err = s.startManualOAuthFlow(principal, r.PathValue("provider_id"), input.ConnectionName, iam.ConnectionSourceAdmin, providers.ProviderAuthManualConfig{
			ClientID: input.ClientID, ClientSecret: input.ClientSecret,
			ClientMode: input.ClientMode, RedirectURI: input.RedirectURI,
		}, input.ClientID != "" || input.ClientSecret != "" || input.ClientMode != "" || input.RedirectURI != "")
	} else {
		response, err = s.startOAuthFlow(principal, r.PathValue("provider_id"), input.ClientID, true, r, input.ConnectionName, iam.ConnectionSourceAdmin, input.Flow)
	}
	if err != nil {
		writeError(w, 400, oauthErrorText(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *server) handlePrincipalOAuthComplete(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	principal, err := oauthPrincipalByID(r.PathValue("id"))
	if err != nil {
		writeError(w, 400, oauthErrorText(err.Error()))
		return
	}
	flowID, authorizationResponse, valid := oauthCompleteInput(r)
	if !valid {
		writeError(w, 400, "flow_id and authorization_response required")
		return
	}
	response := s.completeManualOAuthFlow(principal, r.PathValue("provider_id"), flowID, authorizationResponse)
	if response["status"] == "authorized" {
		auditAdmin(r, "oauth_connection.connect", "principal", principal.ID, map[string]any{"provider": r.PathValue("provider_id"), "profile": manualOAuthAuditProfile(r.PathValue("provider_id"))})
	}
	writeJSON(w, http.StatusOK, response)
}

func manualOAuthAuditProfile(providerRef string) string {
	if oauthRegistryIDForRef(providerRef) == "openai_codex" {
		return codexBrowserManualProfile
	}
	return antigravityManualProfile
}

func (s *server) handlePrincipalOAuthPoll(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	principal, err := oauthPrincipalByID(r.PathValue("id"))
	if err != nil {
		writeError(w, 400, oauthErrorText(err.Error()))
		return
	}
	deviceCode, name, valid := oauthPollInput(r)
	if !valid {
		writeError(w, 400, "device_code required")
		return
	}
	response := s.pollOAuthFlow(principal, r.PathValue("provider_id"), deviceCode, name, iam.ConnectionSourceAdmin)
	if response["status"] == "authorized" {
		auditAdmin(r, "oauth_connection.connect", "principal", principal.ID, map[string]any{"provider": r.PathValue("provider_id")})
	}
	writeJSON(w, http.StatusOK, response)
}

func handleUserOAuthRevoke(w http.ResponseWriter, r *http.Request) {
	principal, ok := requireSSOUser(w, r)
	if !ok {
		return
	}
	response := revokeOAuthFlow(principal, r.PathValue("provider_id"), r.URL.Query().Get("connection_name"))
	if response["status"] == "revoked" {
		_ = iam.RecordAudit(iam.AuditEvent{ActorPrincipalID: principal.ID, Action: "oauth_connection.revoke", TargetType: "principal", TargetID: principal.ID, Result: "success", Detail: map[string]any{"provider": r.PathValue("provider_id"), "source": "self-service"}})
	}
	writeJSON(w, http.StatusOK, response)
}

func handlePrincipalOAuthRefresh(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	principal, err := oauthPrincipalByID(r.PathValue("id"))
	if err != nil {
		writeError(w, 400, oauthErrorText(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, refreshOAuthFlow(principal, r.PathValue("provider_id"), r.URL.Query().Get("connection_name")))
}

func handlePrincipalOAuthRevoke(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	principal, err := oauthPrincipalByID(r.PathValue("id"))
	if err != nil {
		writeError(w, 400, oauthErrorText(err.Error()))
		return
	}
	response := revokeOAuthFlow(principal, r.PathValue("provider_id"), r.URL.Query().Get("connection_name"))
	if response["status"] == "revoked" {
		auditAdmin(r, "oauth_connection.revoke", "principal", principal.ID, map[string]any{"provider": r.PathValue("provider_id")})
	}
	writeJSON(w, http.StatusOK, response)
}
