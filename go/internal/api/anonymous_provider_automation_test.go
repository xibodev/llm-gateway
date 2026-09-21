package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
)

func setupAnonymousAutomationAPITest(t *testing.T) {
	t.Helper()
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	t.Setenv("LLMGW_CONFIG", config.StateDir()+"/config.yaml")
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	old := *config.Get()
	t.Cleanup(func() { config.Update(func(s *config.Settings) { *s = old }); providers.ResetProviders() })
	config.Update(func(s *config.Settings) {
		s.APIKey = "admin"
		s.Providers = map[string]*config.ProviderConfig{}
		s.Endpoints = map[string]*config.EndpointConfig{}
		s.Policies = config.BackendPolicies{Defaults: config.ProviderPolicy{}, Overrides: map[string]config.ProviderPolicy{}, OverrideFields: map[string]map[string]any{}}
	})
	providers.ResetProviders()
}

func TestAnonymousProviderAutomationSettingPrecedence(t *testing.T) {
	setupAnonymousAutomationAPITest(t)
	t.Setenv(anonymousProviderAutomationEnv, "true")
	state, err := anonymousProviderAutomationState()
	if err != nil || !state.Effective || state.EffectiveSource != "environment" {
		t.Fatalf("environment state=%+v err=%v", state, err)
	}
	if err := iam.SetAnonymousProviderAutomationOverride("off"); err != nil {
		t.Fatal(err)
	}
	state, err = anonymousProviderAutomationState()
	if err != nil || state.Effective || state.EffectiveSource != "console_override" {
		t.Fatalf("override state=%+v err=%v", state, err)
	}
	if err := iam.SetAnonymousProviderAutomationOverride("inherit"); err != nil {
		t.Fatal(err)
	}
	t.Setenv(anonymousProviderAutomationEnv, "invalid")
	state, err = anonymousProviderAutomationState()
	if err != nil || state.Effective || state.EnvironmentValid {
		t.Fatalf("invalid environment state=%+v err=%v", state, err)
	}
}

func TestAnonymousProviderAutomationAdminAPI(t *testing.T) {
	setupAnonymousAutomationAPITest(t)
	server := NewServer()
	request := httptest.NewRequest(http.MethodPost, "/admin/api/settings/anonymous-provider-automation", jsonBody(map[string]any{"override": "on"}))
	request.Header.Set("Authorization", "Bearer admin")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil || payload["effective"] != true || payload["reconcile_requested"] != true {
		t.Fatalf("payload=%+v err=%v", payload, err)
	}

	request = httptest.NewRequest(http.MethodPost, "/admin/api/settings/anonymous-provider-automation", jsonBody(map[string]any{"override": "invalid"}))
	request.Header.Set("Authorization", "Bearer admin")
	response = httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("invalid status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestEnsureAnonymousProviderDoesNotOverwriteOrEnable(t *testing.T) {
	setupAnonymousAutomationAPITest(t)
	profile := providers.AnonymousProviderProfile{
		RegistryID: "llm7", ProviderID: "llm7", RuntimeType: "openai_compatible", BaseURL: "https://api.llm7.io/v1",
	}
	if id, status := ensureAnonymousProvider(profile); id != "llm7" || status != "managed" {
		t.Fatalf("first ensure id=%q status=%q", id, status)
	}
	config.Update(func(s *config.Settings) { s.Providers["llm7"].Disabled = true })
	if _, status := ensureAnonymousProvider(profile); status != "disabled" {
		t.Fatalf("disabled status=%q", status)
	}
	if !config.Get().Providers["llm7"].Disabled {
		t.Fatal("automation re-enabled a disabled provider")
	}
	config.Update(func(s *config.Settings) {
		s.Providers["collision"] = &config.ProviderConfig{Type: "openai_compatible", BaseURL: "https://custom.example/v1"}
	})
	profile.ProviderID = "collision"
	if _, status := ensureAnonymousProvider(profile); status != "collision" {
		t.Fatalf("collision status=%q", status)
	}
	if config.Get().Providers["collision"].BaseURL != "https://custom.example/v1" {
		t.Fatal("automation overwrote a colliding provider")
	}
	config.Update(func(s *config.Settings) {
		s.Providers["runtime-mismatch"] = &config.ProviderConfig{
			Type: "anthropic", RegistryID: "llm7", BaseURL: profile.BaseURL,
		}
	})
	profile.ProviderID = "runtime-mismatch"
	if _, status := ensureAnonymousProvider(profile); status != "collision" {
		t.Fatalf("runtime mismatch status=%q", status)
	}
}

func TestEnsureAnonymousProviderRefusesRetainedPersonalConnection(t *testing.T) {
	setupAnonymousAutomationAPITest(t)
	config.Update(func(s *config.Settings) {
		s.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32))
	})
	human, err := iam.CreatePrincipal("human", "fixture:retained", "", "Retained")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := iam.PutProviderConnection(iam.ProviderConnectionCreate{
		PrincipalID: human.ID, ProviderID: "llm7", Kind: "api_key", Secret: "retained-secret", MakeDefault: true,
	}); err != nil {
		t.Fatal(err)
	}
	profile := providers.AnonymousProviderProfile{RegistryID: "llm7", ProviderID: "llm7", RuntimeType: "openai_compatible", BaseURL: "https://api.llm7.io/v1"}
	if _, status := ensureAnonymousProvider(profile); status != "credential_collision" {
		t.Fatalf("status=%q", status)
	}
	if _, exists := config.Provider("llm7"); exists {
		t.Fatal("automation recreated a provider ID with a retained credential")
	}
}

func TestDisabledAutomationDoesNotChangeProviders(t *testing.T) {
	setupAnonymousAutomationAPITest(t)
	config.Update(func(s *config.Settings) {
		s.Providers["keep"] = &config.ProviderConfig{Type: "echo"}
	})
	if err := iam.SetAnonymousProviderAutomationOverride("off"); err != nil {
		t.Fatal(err)
	}
	if results := runAnonymousProviderAutomation(context.Background()); len(results) != 0 {
		t.Fatalf("results=%+v", results)
	}
	if config.Get().Providers["keep"] == nil || len(config.Get().Providers) != 1 {
		t.Fatalf("providers=%+v", config.Get().Providers)
	}
}

func TestAnonymousProviderAutomationRunsOncePerDay(t *testing.T) {
	setupAnonymousAutomationAPITest(t)
	t.Setenv(anonymousProviderAutomationEnv, "true")
	var catalogs, completions atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			catalogs.Add(1)
			_, _ = w.Write([]byte(`{"data":[{"id":"codestral-latest","tier":"turbo","usage_based_only":false,"model_type":"chat","schema_endpoints":["openai"]}]}`))
		case "/chat/completions":
			completions.Add(1)
			var request map[string]any
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request["max_tokens"] != float64(16) {
				t.Fatalf("shared orchestrator probe request=%+v", request)
			}
			_, _ = w.Write([]byte(`{"id":"chat_1","model":"codestral-latest","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	profiles := []providers.AnonymousProviderProfile{{
		RegistryID: "llm7", ProviderID: "daily-fixture",
		RuntimeType: "openai_compatible", BaseURL: upstream.URL,
	}}
	results := runAnonymousProviderAutomationProfiles(context.Background(), profiles)
	if len(results) != 1 || results[0]["success"] != true || catalogs.Load() != 1 || completions.Load() != 1 {
		t.Fatalf("results=%+v catalogs=%d completions=%d", results, catalogs.Load(), completions.Load())
	}
	configured, found := config.Provider("daily-fixture")
	if !found || configured.RegistryID != "llm7" {
		t.Fatalf("configured=%+v found=%v", configured, found)
	}
	checks, err := iam.LastProviderChecks("")
	if err != nil || len(checks["daily-fixture"]) != 2 {
		t.Fatalf("checks=%+v err=%v", checks, err)
	}
	results = runAnonymousProviderAutomationProfiles(context.Background(), profiles)
	if len(results) != 0 || catalogs.Load() != 1 || completions.Load() != 1 {
		t.Fatalf("daily claim repeated work: results=%+v catalogs=%d completions=%d", results, catalogs.Load(), completions.Load())
	}
}

func TestAnonymousProviderAutomationPreservesSharedFailureEvidence(t *testing.T) {
	setupAnonymousAutomationAPITest(t)
	t.Setenv(anonymousProviderAutomationEnv, "true")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"codestral-latest","tier":"turbo","usage_based_only":false,"model_type":"chat","schema_endpoints":["openai"]}]}`))
		case "/chat/completions":
			w.Header().Set("Retry-After", "17")
			http.Error(w, "limited", http.StatusTooManyRequests)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	profile := providers.AnonymousProviderProfile{
		RegistryID: "llm7", ProviderID: "shared-failure-fixture",
		RuntimeType: "openai_compatible", BaseURL: upstream.URL,
	}
	results := runAnonymousProviderAutomationProfiles(context.Background(), []providers.AnonymousProviderProfile{profile})
	if len(results) != 1 {
		t.Fatalf("results=%+v", results)
	}
	result := results[0]
	if result["status"] != "failed" || result["success"] != false ||
		result["authentication_state"] != "accepted" || result["catalog_evidence"] != "discovered" ||
		result["completion_evidence"] != "failed" || result["failure_code"] != "verification_failed" ||
		result["retryable"] != true || result["retry_after"] != "17" {
		t.Fatalf("result=%+v", result)
	}
	checks, err := iam.LastProviderChecks("")
	providerChecks := checks[profile.ProviderID]
	if err != nil || len(providerChecks) != 2 {
		t.Fatalf("checks=%+v err=%v", checks[profile.ProviderID], err)
	}
	for _, check := range providerChecks {
		if check.Operation == iam.CheckVerify && check.Success {
			t.Fatalf("verification check=%+v", check)
		}
	}
}

func TestAnonymousVerificationClassifiesPermanentAuthFailureWithoutRetry(t *testing.T) {
	setupAnonymousAutomationAPITest(t)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		http.Error(w, "denied", http.StatusUnauthorized)
	}))
	defer upstream.Close()
	config.Update(func(s *config.Settings) {
		s.Providers["auth-fixture"] = &config.ProviderConfig{
			Type: "openai_compatible", RegistryID: "llm7", BaseURL: upstream.URL,
		}
		s.Policies.Defaults.RetryMaxAttempts = 3
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)

	result := runProviderVerifyContext(context.Background(), "auth-fixture", "fixture-model", nil)
	if result["success"] != false || result["failure_code"] != "authentication_rejected" ||
		result["authentication_state"] != "rejected" || result["completion_evidence"] != "failed" ||
		result["retryable"] != false || result["upstream_status"] != http.StatusUnauthorized || calls.Load() != 1 {
		t.Fatalf("result=%+v calls=%d", result, calls.Load())
	}
}

func TestAnonymousVerificationPreservesRetryAfterClassification(t *testing.T) {
	setupAnonymousAutomationAPITest(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "17")
		http.Error(w, "slow down", http.StatusTooManyRequests)
	}))
	defer upstream.Close()
	config.Update(func(s *config.Settings) {
		s.Providers["throttle-fixture"] = &config.ProviderConfig{
			Type: "openai_compatible", RegistryID: "llm7", BaseURL: upstream.URL,
		}
		s.Policies.Defaults.RetryMaxAttempts = 1
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)

	result := runProviderVerifyContext(context.Background(), "throttle-fixture", "fixture-model", nil)
	if result["retryable"] != true || result["upstream_status"] != http.StatusTooManyRequests || result["retry_after"] != "17" {
		t.Fatalf("result=%+v", result)
	}
}

func TestAutoConnectFreeProvidersAPI(t *testing.T) {
	setupAnonymousAutomationAPITest(t)
	server := httptest.NewServer(NewServer())
	defer server.Close()

	// Anonymous request rejected
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/admin/api/providers/auto-connect-free", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 unauthorized, got %d", resp.StatusCode)
	}

	// Authed request runs and returns ok
	req, _ = http.NewRequest(http.MethodPost, server.URL+"/admin/api/providers/auto-connect-free", nil)
	req.Header.Set("Authorization", "Bearer admin")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 ok, got %d", resp.StatusCode)
	}

	var payload struct {
		OK      bool             `json:"ok"`
		Total   int              `json:"total"`
		Results []map[string]any `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if !payload.OK || payload.Total != 5 || len(payload.Results) != 5 {
		t.Fatalf("unexpected payload: %+v", payload)
	}
}

func jsonBody(value any) *bytes.Reader {
	raw, _ := json.Marshal(value)
	return bytes.NewReader(raw)
}
