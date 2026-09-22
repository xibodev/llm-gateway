package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/diagnostics"
	"llmgw/internal/iam"
	"llmgw/internal/providers"

	browseroauth "github.com/xibodev/llm-provider-auth/browseroauth"
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

type oauthFlowState struct {
	PrincipalID  string
	ProviderID   string
	StartedAt    int64
	ExpiresAt    int64
	NextPollAt   int64
	Interval     int
	PrivateState string
	Generation   uint64
}

type browserOAuthFlowState struct {
	PrincipalID    string
	ProviderID     string
	Kind           string
	ConnectionName string
	Source         string
	ExpectedState  string
	PrivateState   string
	ExpiresAt      int64
	StartedAt      int64
	Generation     uint64
	CallbackID     string
	Result         map[string]any
	Completing     bool
	Manual         bool
	PersistConfig  bool
	ManualConfig   providers.ProviderAuthManualConfig
}

var browserOAuthFlows = struct {
	sync.Mutex
	values         map[string]browserOAuthFlowState
	nextGeneration uint64
}{values: map[string]browserOAuthFlowState{}}

const maxOAuthFlowsPerPrincipal = 5

var oauthFlows = struct {
	sync.Mutex
	values         map[string]oauthFlowState
	nextGeneration uint64
}{values: map[string]oauthFlowState{}}

var oauthFlowCleanupOnce sync.Once

func oauthFlowKey(principalID, providerID, deviceCode string) string {
	return principalID + "|" + providerID + "|" + deviceCode
}

func storeOAuthFlow(key string, flow oauthFlowState) {
	ensureOAuthFlowCleanup()
	now := time.Now().Unix()
	oauthFlows.Lock()
	pruneOAuthFlowsLocked(now)
	oauthFlows.nextGeneration++
	flow.Generation = oauthFlows.nextGeneration
	for {
		count := 0
		oldestKey := ""
		oldestAt := int64(0)
		oldestGeneration := uint64(0)
		for existingKey, existing := range oauthFlows.values {
			if existing.PrincipalID != flow.PrincipalID {
				continue
			}
			count++
			if oldestKey == "" || existing.StartedAt < oldestAt ||
				(existing.StartedAt == oldestAt && existing.Generation < oldestGeneration) {
				oldestKey = existingKey
				oldestAt = existing.StartedAt
				oldestGeneration = existing.Generation
			}
		}
		if count < maxOAuthFlowsPerPrincipal || oldestKey == "" {
			break
		}
		delete(oauthFlows.values, oldestKey)
	}
	oauthFlows.values[key] = flow
	oauthFlows.Unlock()
}

func ensureOAuthFlowCleanup() {
	oauthFlowCleanupOnce.Do(func() {
		go func() {
			ticker := time.NewTicker(time.Minute)
			defer ticker.Stop()
			for now := range ticker.C {
				oauthFlows.Lock()
				pruneOAuthFlowsLocked(now.Unix())
				oauthFlows.Unlock()
				browserOAuthFlows.Lock()
				pruneBrowserOAuthFlowsLocked(now.Unix())
				browserOAuthFlows.Unlock()
			}
		}()
	})
}

func pruneBrowserOAuthFlowsLocked(now int64) {
	for key, flow := range browserOAuthFlows.values {
		if flow.ExpiresAt > 0 && flow.ExpiresAt <= now {
			delete(browserOAuthFlows.values, key)
		}
	}
}

func storeBrowserOAuthFlow(flowID string, flow browserOAuthFlowState) {
	ensureOAuthFlowCleanup()
	now := time.Now().Unix()
	browserOAuthFlows.Lock()
	defer browserOAuthFlows.Unlock()
	pruneBrowserOAuthFlowsLocked(now)
	browserOAuthFlows.nextGeneration++
	flow.Generation = browserOAuthFlows.nextGeneration
	for {
		count := 0
		oldestKey := ""
		oldestAt := int64(0)
		oldestGeneration := uint64(0)
		for key, existing := range browserOAuthFlows.values {
			if existing.PrincipalID != flow.PrincipalID {
				continue
			}
			count++
			if existing.Completing {
				continue
			}
			if oldestKey == "" || existing.StartedAt < oldestAt || existing.StartedAt == oldestAt && existing.Generation < oldestGeneration {
				oldestKey, oldestAt, oldestGeneration = key, existing.StartedAt, existing.Generation
			}
		}
		if count < maxOAuthFlowsPerPrincipal || oldestKey == "" {
			break
		}
		delete(browserOAuthFlows.values, oldestKey)
	}
	browserOAuthFlows.values[flowID] = flow
}

func pruneOAuthFlowsLocked(now int64) {
	for key, flow := range oauthFlows.values {
		if flow.ExpiresAt > 0 && flow.ExpiresAt <= now {
			delete(oauthFlows.values, key)
		}
	}
}

func applyOAuthPollResult(
	key string, observed oauthFlowState, result providers.ProviderAuthPoll, now int64,
) bool {
	oauthFlows.Lock()
	defer oauthFlows.Unlock()
	current, ok := oauthFlows.values[key]
	if !ok || current.Generation != observed.Generation {
		return false
	}
	if current.ExpiresAt > 0 && current.ExpiresAt <= now {
		delete(oauthFlows.values, key)
		return false
	}
	switch result.Status {
	case "pending":
		current.NextPollAt = now + int64(current.Interval)
		oauthFlows.values[key] = current
	case "slow_down":
		current.Interval += 5
		current.NextPollAt = now + int64(current.Interval)
		oauthFlows.values[key] = current
	default:
		delete(oauthFlows.values, key)
	}
	return true
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

func startOAuthFlow(
	principal iam.Principal, providerRef, clientID string, allowClientIDUpdate bool,
	request *http.Request, connectionName, source string,
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
	providerID, _, adapter, err := oauthAdapterFor(providerRef)
	if err != nil {
		return nil, err
	}
	_, providerConfigured := config.Get().Providers[providerID]
	if !providerConfigured && !allowClientIDUpdate {
		return nil, fmt.Errorf("Only an administrator can configure this OAuth provider.")
	}
	if browserAdapter, ok := adapter.(providers.BrowserProviderAuthAdapter); ok {
		callbackID := oauthRegistryIDForRef(providerRef)
		redirectURI, err := oauthCallbackURL(request, callbackID)
		if err != nil {
			return nil, err
		}
		start, err := browserAdapter.StartBrowser(context.Background(), redirectURI)
		if err != nil {
			return nil, err
		}
		var private map[string]string
		if json.Unmarshal([]byte(start.PrivateState), &private) != nil || private["state"] == "" || private["code_verifier"] == "" {
			return nil, fmt.Errorf("official browser OAuth returned incomplete authorization data")
		}
		if !providerConfigured {
			if err := ensureOAuthProviderConfig(providerID, providerRef); err != nil {
				return nil, err
			}
		}
		expiresIn := start.ExpiresIn
		if expiresIn <= 0 {
			expiresIn = 600
		}
		private["redirect_uri"] = redirectURI
		storedPrivate, _ := json.Marshal(private)
		expiresAt := time.Now().Add(time.Duration(expiresIn) * time.Second).Unix()
		flowID := private["state"]
		storeBrowserOAuthFlow(flowID, browserOAuthFlowState{
			PrincipalID: principal.ID, ProviderID: providerID, Kind: adapter.CredentialKind(),
			ConnectionName: connectionName, Source: source, ExpectedState: private["state"],
			PrivateState: string(storedPrivate), ExpiresAt: expiresAt, StartedAt: time.Now().Unix(), CallbackID: callbackID,
		})
		flowStarted = true
		return map[string]any{
			"provider_id": providerID, "flow": "browser", "authorization_url": start.AuthorizationURL,
			"flow_id": flowID, "expires_in": expiresIn, "expires_at": expiresAt,
		}, nil
	}
	deviceAdapter, ok := adapter.(providers.DeviceProviderAuthAdapter)
	if !ok {
		return nil, fmt.Errorf("provider %q does not support device authorization", providerRef)
	}
	start, err := deviceAdapter.StartDevice(context.Background())
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(start.DeviceCode) == "" || strings.TrimSpace(start.UserCode) == "" {
		return nil, fmt.Errorf("official OAuth device flow returned incomplete authorization data")
	}
	if !providerConfigured {
		if err := ensureOAuthProviderConfig(providerID, providerRef); err != nil {
			return nil, err
		}
	}
	now := time.Now().Unix()
	expiresAt := now + int64(start.ExpiresIn)
	storeOAuthFlow(oauthFlowKey(principal.ID, providerID, start.DeviceCode), oauthFlowState{
		PrincipalID: principal.ID, ProviderID: providerID, StartedAt: now, ExpiresAt: expiresAt,
		NextPollAt: now + int64(start.Interval), Interval: start.Interval, PrivateState: start.PrivateState,
	})
	flowStarted = true
	return map[string]any{
		"provider_id": providerID, "flow": "device_code", "device_code": start.DeviceCode,
		"user_code": start.UserCode, "verification_uri": start.VerificationURI,
		"interval": start.Interval, "expires_in": start.ExpiresIn, "expires_at": expiresAt,
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

var startManualProviderOAuth = func(ctx context.Context, adapter providers.ManualProviderAuthAdapter, input providers.ProviderAuthManualConfig) (providers.ProviderAuthBrowserStart, error) {
	return adapter.StartManual(ctx, input)
}

var completeManualProviderOAuth = func(ctx context.Context, adapter providers.ManualProviderAuthAdapter, code, privateState string, input providers.ProviderAuthManualConfig) (providers.ProviderAuthPoll, error) {
	return adapter.CompleteManual(ctx, code, privateState, input)
}

func startManualOAuthFlow(
	principal iam.Principal, providerRef, connectionName, source string,
	input providers.ProviderAuthManualConfig, persistConfig bool,
) (map[string]any, error) {
	providerID, kind, adapter, err := oauthAdapterFor(providerRef)
	if err != nil {
		return nil, err
	}
	manual, ok := adapter.(providers.ManualProviderAuthAdapter)
	if !ok {
		return nil, fmt.Errorf("provider %q does not support manual authorization", providerRef)
	}
	configured, err := antigravityManualConfig(providerID, input, persistConfig)
	if err != nil {
		return nil, err
	}
	start, err := startManualProviderOAuth(context.Background(), manual, configured)
	if err != nil {
		return nil, err
	}
	var private map[string]string
	if json.Unmarshal([]byte(start.PrivateState), &private) != nil || private["state"] == "" || private["code_verifier"] == "" {
		return nil, fmt.Errorf("official manual OAuth returned incomplete authorization data")
	}
	expiresIn := start.ExpiresIn
	if expiresIn <= 0 {
		expiresIn = 600
	}
	now := time.Now()
	flowID := private["state"]
	storeBrowserOAuthFlow(flowID, browserOAuthFlowState{
		PrincipalID: principal.ID, ProviderID: providerID, Kind: kind,
		ConnectionName: connectionName, Source: source, ExpectedState: flowID,
		PrivateState: start.PrivateState, ExpiresAt: now.Add(time.Duration(expiresIn) * time.Second).Unix(),
		StartedAt: now.Unix(), Manual: true, PersistConfig: persistConfig, ManualConfig: configured,
	})
	return map[string]any{
		"provider_id": providerID, "flow": "consumer_manual", "profile": antigravityManualProfile,
		"authorization_url": start.AuthorizationURL, "flow_id": flowID,
		"expires_in": expiresIn, "expires_at": now.Add(time.Duration(expiresIn) * time.Second).Unix(),
	}, nil
}

func completeManualOAuthFlow(principal iam.Principal, providerRef, flowID, authorizationResponse string) map[string]any {
	providerID, _, adapter, err := oauthAdapterFor(providerRef)
	if err != nil {
		return safeOAuthPollResponse("error", err.Error())
	}
	manual, ok := adapter.(providers.ManualProviderAuthAdapter)
	if !ok {
		return safeOAuthPollResponse("error", "Provider does not support manual authorization.")
	}
	flowID = strings.TrimSpace(flowID)
	browserOAuthFlows.Lock()
	flow, exists := browserOAuthFlows.values[flowID]
	valid := exists && flow.Manual && !flow.Completing && flow.Result == nil &&
		flow.PrincipalID == principal.ID && flow.ProviderID == providerID && flow.ExpiresAt > time.Now().Unix()
	if valid {
		flow.Completing = true
		browserOAuthFlows.values[flowID] = flow
	}
	browserOAuthFlows.Unlock()
	if !valid {
		return safeOAuthPollResponse("expired", "Manual authorization is no longer active. Start again.")
	}
	failed := func(detail string) map[string]any {
		browserOAuthFlows.Lock()
		if current, currentExists := browserOAuthFlows.values[flowID]; currentExists && current.Generation == flow.Generation {
			current.Completing = false
			browserOAuthFlows.values[flowID] = current
		}
		browserOAuthFlows.Unlock()
		return safeOAuthPollResponse("error", detail)
	}
	code, returnedState, fromURL, err := manualAuthorizationCode(authorizationResponse)
	if err != nil {
		return failed(err.Error())
	}
	if fromURL && browseroauth.ValidateState(flow.ExpectedState, returnedState) != nil {
		return failed("The returned OAuth state did not match this authorization flow.")
	}
	result, err := completeManualProviderOAuth(context.Background(), manual, code, flow.PrivateState, flow.ManualConfig)
	if err != nil || result.Status != "authorized" {
		return failed("Manual authorization failed.")
	}
	connection, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
		PrincipalID: principal.ID, ProviderID: providerID, Name: flow.ConnectionName,
		Kind: flow.Kind, Source: flow.Source, MakeDefault: true,
		AccessToken: result.AccessToken, RefreshToken: result.RefreshToken, IDToken: result.IDToken,
		TokenType: result.TokenType, ExpiresAt: result.ExpiresAt, AccountID: result.AccountID,
		AccountLabel: result.AccountLabel, ProjectID: result.ProjectID, Status: "active",
		OAuthProfile: antigravityManualProfile, OAuthClientID: flow.ManualConfig.ClientID,
		OAuthClientMode: flow.ManualConfig.ClientMode, OAuthRedirectURI: flow.ManualConfig.RedirectURI,
		OAuthClientSecret: flow.ManualConfig.ClientSecret,
	})
	if err != nil {
		return failed("Could not store the private OAuth connection.")
	}
	if flow.PersistConfig {
		if err := iam.PutOAuthClientProfile(iam.OAuthClientProfile{
			ProviderID: "google_antigravity", Profile: antigravityManualProfile,
			ClientID: flow.ManualConfig.ClientID, ClientSecret: flow.ManualConfig.ClientSecret,
			ClientMode: flow.ManualConfig.ClientMode, RedirectURI: flow.ManualConfig.RedirectURI,
		}); err != nil {
			envelope, stored, ok, loadErr := iam.OAuthProviderConnectionSecret(principal.ID, providerID, connection.Name)
			if loadErr == nil && ok {
				_, _ = iam.RevokeOAuthProviderConnectionIfCurrent(stored, envelope)
			}
			return failed("Could not store the encrypted consumer_manual OAuth profile.")
		}
	}
	if _, configured := config.Get().Providers[providerID]; !configured {
		if err := ensureOAuthProviderConfig(providerID, providerRef); err != nil {
			envelope, stored, ok, loadErr := iam.OAuthProviderConnectionSecret(principal.ID, providerID, connection.Name)
			if loadErr == nil && ok {
				_, _ = iam.RevokeOAuthProviderConnectionIfCurrent(stored, envelope)
			}
			return failed(err.Error())
		}
	}
	browserOAuthFlows.Lock()
	delete(browserOAuthFlows.values, flowID)
	browserOAuthFlows.Unlock()
	providers.ForgetProviderForPrincipal(providerID, principal.ID)
	providers.ForgetCatalogForPrincipal(providerID, principal.ID)
	return map[string]any{"status": "authorized", "connection": connection}
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

func pollBrowserOAuthFlow(principal iam.Principal, providerRef, flowID string) (map[string]any, bool) {
	providerID, _, adapter, err := oauthAdapterFor(providerRef)
	if err != nil {
		return safeOAuthPollResponse("error", err.Error()), true
	}
	if _, ok := adapter.(providers.BrowserProviderAuthAdapter); !ok {
		return nil, false
	}
	browserOAuthFlows.Lock()
	defer browserOAuthFlows.Unlock()
	flow, ok := browserOAuthFlows.values[strings.TrimSpace(flowID)]
	if !ok || flow.PrincipalID != principal.ID || flow.ProviderID != providerID {
		return safeOAuthPollResponse("expired", "Browser authorization is no longer active. Start again."), true
	}
	if flow.ExpiresAt <= time.Now().Unix() {
		delete(browserOAuthFlows.values, flowID)
		return safeOAuthPollResponse("expired", "Browser authorization expired. Start again."), true
	}
	if flow.Result == nil {
		return map[string]any{"status": "pending"}, true
	}
	result := flow.Result
	delete(browserOAuthFlows.values, flowID)
	return result, true
}

func handleOAuthBrowserCallback(w http.ResponseWriter, r *http.Request) {
	flowID := strings.TrimSpace(r.URL.Query().Get("state"))
	browserOAuthFlows.Lock()
	flow, ok := browserOAuthFlows.values[flowID]
	valid := ok && !flow.Completing && flow.Result == nil && flow.ExpiresAt > time.Now().Unix() && browseroauth.ValidateState(flow.ExpectedState, flowID) == nil && r.PathValue("provider_id") == flow.CallbackID
	if valid {
		flow.Completing = true
		browserOAuthFlows.values[flowID] = flow
	}
	browserOAuthFlows.Unlock()
	if !valid {
		http.Error(w, "OAuth authorization is no longer active.", http.StatusBadRequest)
		return
	}
	_, _, adapter, err := oauthAdapterFor(flow.ProviderID)
	if err != nil {
		finishBrowserOAuthFlow(flowID, flow, safeOAuthPollResponse("error", "OAuth provider is unavailable."))
		http.Error(w, "OAuth provider is unavailable.", http.StatusBadRequest)
		return
	}
	browserAdapter, ok := adapter.(providers.BrowserProviderAuthAdapter)
	if !ok {
		finishBrowserOAuthFlow(flowID, flow, safeOAuthPollResponse("error", "OAuth provider does not support browser authorization."))
		http.Error(w, "OAuth provider does not support browser authorization.", http.StatusBadRequest)
		return
	}
	result, err := browserAdapter.CompleteBrowser(r.Context(), r.URL.Query().Get("code"), flow.PrivateState)
	response := safeOAuthPollResponse("error", "Browser authorization failed.")
	if err == nil && result.Status == "authorized" {
		connection, storeErr := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
			PrincipalID: flow.PrincipalID, ProviderID: flow.ProviderID, Name: flow.ConnectionName,
			Kind: flow.Kind, Source: flow.Source, MakeDefault: true, AccessToken: result.AccessToken,
			RefreshToken: result.RefreshToken, IDToken: result.IDToken, TokenType: result.TokenType,
			ExpiresAt: result.ExpiresAt, AccountID: result.AccountID, AccountLabel: result.AccountLabel, ProjectID: result.ProjectID, Status: "active",
			OAuthProfile: result.OAuthProfile, OAuthClientID: result.OAuthClientID,
		})
		if storeErr == nil {
			response = map[string]any{"status": "authorized", "connection": connection}
			providers.ForgetProviderForPrincipal(flow.ProviderID, flow.PrincipalID)
			providers.ForgetCatalogForPrincipal(flow.ProviderID, flow.PrincipalID)
		}
	}
	finishBrowserOAuthFlow(flowID, flow, response)
	if response["status"] != "authorized" {
		http.Error(w, "Browser authorization failed. Return to the gateway and try again.", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte("<!doctype html><title>Provider connected</title><p>Authorization received. You can close this window and return to the gateway.</p>"))
}

func finishBrowserOAuthFlow(flowID string, flow browserOAuthFlowState, response map[string]any) {
	browserOAuthFlows.Lock()
	defer browserOAuthFlows.Unlock()
	if current, exists := browserOAuthFlows.values[flowID]; exists && current.ExpectedState == flow.ExpectedState {
		current.Result = response
		browserOAuthFlows.values[flowID] = current
	}
}

func pollOAuthFlow(principal iam.Principal, providerRef, deviceCode, name, source string) map[string]any {
	if result, handled := pollBrowserOAuthFlow(principal, providerRef, deviceCode); handled {
		return result
	}
	providerID, kind, adapter, err := oauthAdapterFor(providerRef)
	if err != nil {
		return safeOAuthPollResponse("error", err.Error())
	}
	deviceCode = strings.TrimSpace(deviceCode)
	if deviceCode == "" {
		return safeOAuthPollResponse("error", "device_code is required")
	}
	now := time.Now().Unix()
	key := oauthFlowKey(principal.ID, providerID, deviceCode)
	oauthFlows.Lock()
	flow, tracked := oauthFlows.values[key]
	if tracked && flow.ExpiresAt > 0 && now >= flow.ExpiresAt {
		delete(oauthFlows.values, key)
		oauthFlows.Unlock()
		return safeOAuthPollResponse("expired", "Device authorization expired. Start again.")
	}
	if tracked && flow.NextPollAt > now {
		oauthFlows.Unlock()
		return safeOAuthPollResponse("slow_down", "Wait for the provider polling interval before retrying.")
	}
	if !tracked {
		oauthFlows.Unlock()
		return safeOAuthPollResponse("expired", "Device authorization is no longer active. Start again.")
	}
	privateState := flow.PrivateState
	oauthFlows.Unlock()

	deviceAdapter, ok := adapter.(providers.DeviceProviderAuthAdapter)
	if !ok {
		return safeOAuthPollResponse("error", "Provider does not support device authorization.")
	}
	result := providers.SafeProviderAuthPoll(deviceAdapter.PollDevice(context.Background(), deviceCode, privateState))
	if !applyOAuthPollResult(key, flow, result, time.Now().Unix()) {
		return safeOAuthPollResponse("expired", "Device authorization is no longer active. Start again.")
	}

	out := safeOAuthPollResponse(result.Status, result.Error)
	if result.Status != "authorized" {
		return out
	}
	connection, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
		PrincipalID: principal.ID, ProviderID: providerID, Name: name, Kind: kind, Source: source,
		MakeDefault: true, AccessToken: result.AccessToken, RefreshToken: result.RefreshToken,
		IDToken: result.IDToken, TokenType: result.TokenType, ExpiresAt: result.ExpiresAt, AccountID: result.AccountID, AccountLabel: result.AccountLabel, ProjectID: result.ProjectID, Status: "active",
		OAuthProfile: result.OAuthProfile, OAuthClientID: result.OAuthClientID,
	})
	if err != nil {
		return safeOAuthPollResponse("error", "Could not store the private OAuth connection.")
	}
	providers.ForgetProviderForPrincipal(providerID, principal.ID)
	providers.ForgetCatalogForPrincipal(providerID, principal.ID)
	out["connection"] = connection
	return out
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

func handleUserOAuthStart(w http.ResponseWriter, r *http.Request) {
	principal, ok := requireSSOUser(w, r)
	if !ok {
		return
	}
	input := oauthStartInput(r)
	var response map[string]any
	var err error
	if input.Profile == antigravityManualProfile {
		response, err = startManualOAuthFlow(principal, r.PathValue("provider_id"), input.ConnectionName, iam.ConnectionSourceUser, providers.ProviderAuthManualConfig{}, false)
	} else {
		response, err = startOAuthFlow(principal, r.PathValue("provider_id"), "", false, r, input.ConnectionName, iam.ConnectionSourceUser)
	}
	if err != nil {
		writeError(w, 400, oauthErrorText(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func handleUserOAuthComplete(w http.ResponseWriter, r *http.Request) {
	principal, ok := requireSSOUser(w, r)
	if !ok {
		return
	}
	flowID, authorizationResponse, valid := oauthCompleteInput(r)
	if !valid {
		writeError(w, 400, "flow_id and authorization_response required")
		return
	}
	response := completeManualOAuthFlow(principal, r.PathValue("provider_id"), flowID, authorizationResponse)
	if response["status"] == "authorized" {
		_ = iam.RecordAudit(iam.AuditEvent{ActorPrincipalID: principal.ID, Action: "oauth_connection.connect", TargetType: "principal", TargetID: principal.ID, Result: "success", Detail: map[string]any{"provider": r.PathValue("provider_id"), "source": "self-service", "profile": antigravityManualProfile}})
	}
	writeJSON(w, http.StatusOK, response)
}

func handleUserOAuthPoll(w http.ResponseWriter, r *http.Request) {
	principal, ok := requireSSOUser(w, r)
	if !ok {
		return
	}
	deviceCode, name, valid := oauthPollInput(r)
	if !valid {
		writeError(w, 400, "device_code required")
		return
	}
	response := pollOAuthFlow(principal, r.PathValue("provider_id"), deviceCode, name, iam.ConnectionSourceUser)
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

func handlePrincipalOAuthStart(w http.ResponseWriter, r *http.Request) {
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
	if input.Profile == antigravityManualProfile {
		response, err = startManualOAuthFlow(principal, r.PathValue("provider_id"), input.ConnectionName, iam.ConnectionSourceAdmin, providers.ProviderAuthManualConfig{
			ClientID: input.ClientID, ClientSecret: input.ClientSecret,
			ClientMode: input.ClientMode, RedirectURI: input.RedirectURI,
		}, input.ClientID != "" || input.ClientSecret != "" || input.ClientMode != "" || input.RedirectURI != "")
	} else {
		response, err = startOAuthFlow(principal, r.PathValue("provider_id"), input.ClientID, true, r, input.ConnectionName, iam.ConnectionSourceAdmin)
	}
	if err != nil {
		writeError(w, 400, oauthErrorText(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, response)
}

func handlePrincipalOAuthComplete(w http.ResponseWriter, r *http.Request) {
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
	response := completeManualOAuthFlow(principal, r.PathValue("provider_id"), flowID, authorizationResponse)
	if response["status"] == "authorized" {
		auditAdmin(r, "oauth_connection.connect", "principal", principal.ID, map[string]any{"provider": r.PathValue("provider_id"), "profile": antigravityManualProfile})
	}
	writeJSON(w, http.StatusOK, response)
}

func handlePrincipalOAuthPoll(w http.ResponseWriter, r *http.Request) {
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
	response := pollOAuthFlow(principal, r.PathValue("provider_id"), deviceCode, name, iam.ConnectionSourceAdmin)
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
