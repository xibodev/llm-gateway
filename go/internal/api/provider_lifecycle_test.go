package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
	"llmgw/internal/router"
)

func TestProviderStatusLadder(t *testing.T) {
	cases := []struct {
		name       string
		credential bool
		models     int
		checks     []iam.ProviderCheck
		want       string
	}{
		{name: "no credential", credential: false, want: "needs_credentials"},
		{name: "credential only", credential: true, want: "configured"},
		{name: "catalog synced", credential: true, models: 3, want: "catalog_synced"},
		{
			name: "verified", credential: true, models: 3,
			checks: []iam.ProviderCheck{{Operation: iam.CheckVerify, Success: true, CheckedAt: 100}},
			want:   "verified",
		},
		{
			name: "latest check failed wins over verify history", credential: true, models: 3,
			checks: []iam.ProviderCheck{
				{Operation: iam.CheckVerify, Success: true, CheckedAt: 100},
				{Operation: iam.CheckReachability, Success: false, CheckedAt: 200},
			},
			want: "check_failed",
		},
		{
			name: "recovered after failure", credential: true, models: 3,
			checks: []iam.ProviderCheck{
				{Operation: iam.CheckReachability, Success: false, CheckedAt: 100},
				{Operation: iam.CheckVerify, Success: true, CheckedAt: 200},
			},
			want: "verified",
		},
		{
			name: "credential present but never checked, catalog empty", credential: true,
			checks: []iam.ProviderCheck{{Operation: iam.CheckCatalogSync, Success: true, CheckedAt: 50}},
			want:   "configured",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, _, _ := providerStatus(testCase.credential, testCase.models, testCase.checks)
			if got != testCase.want {
				t.Fatalf("status = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestExplicitModelVerificationDoesNotRequireCatalog(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	var modelRequests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			modelRequests.Add(1)
			http.Error(w, "catalog unavailable", http.StatusServiceUnavailable)
		case "/chat/completions":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"chat_1","model":"fixture-model","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop","index":0}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{
			"fixture": {
				Type: "openai_compatible", BaseURL: upstream.URL, APIKey: "fixture",
			},
		}
		s.Policies.Defaults = config.ProviderPolicy{}
		s.Policies.Overrides = map[string]config.ProviderPolicy{}
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)
	result := runProviderVerify("fixture", "fixture-model", nil)
	if result["success"] != true {
		t.Fatalf("verification=%+v", result)
	}
	if got := modelRequests.Load(); got != 0 {
		t.Fatalf("explicit verification made %d catalog request(s)", got)
	}
}

func TestGoogleVerificationDoesNotStarveThinkingOutputBudget(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		generation, ok := payload["generationConfig"].(map[string]any)
		if !ok || generation["maxOutputTokens"] != float64(512) {
			t.Fatalf("verification output budget=%+v", generation)
		}
		if _, hasThinking := generation["thinkingConfig"]; hasThinking {
			t.Fatalf("verification imposed model-specific thinking config: %+v", generation)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`))
	}))
	defer upstream.Close()
	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{
			"ai_studio": {Type: "ai_studio", BaseURL: upstream.URL, APIKey: "fixture"},
		}
		s.Policies.Defaults = config.ProviderPolicy{}
		s.Policies.Overrides = map[string]config.ProviderPolicy{}
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)
	result := runProviderVerify("ai_studio", "thinking-model", nil)
	if result["success"] != true {
		t.Fatalf("verification=%+v", result)
	}
}

func TestRemovedProviderCannotPersistLifecycleResult(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{"removed": {Type: "echo"}}
	})
	if err := iam.DeleteProviderChecks("removed"); err != nil {
		t.Fatal(err)
	}
	config.Update(func(s *config.Settings) { delete(s.Providers, "removed") })
	result := runProviderProbe("removed", "test", nil)
	if result["success"] != false || result["failure_code"] != "provider_removed" {
		t.Fatalf("result=%+v", result)
	}
	checks, err := iam.LastProviderChecks("")
	if err != nil {
		t.Fatal(err)
	}
	if len(checks["removed"]) != 0 {
		t.Fatalf("removed provider retained checks: %+v", checks)
	}
}

func TestVerificationObservationAdvancesAcrossSameConnectionRefresh(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	key := make([]byte, 32)
	for index := range key {
		key[index] = byte(index + 1)
	}
	config.Update(func(s *config.Settings) {
		s.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(key)
	})
	human, err := iam.CreatePrincipal("human", "authentik:verify-refresh", "", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	connection, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
		PrincipalID: human.ID, ProviderID: "codex", Kind: "openai_codex_oauth",
		AccessToken: "first", RefreshToken: "refresh",
	})
	if err != nil {
		t.Fatal(err)
	}
	before, found, err := iam.ActiveProviderAccountObservation(human.ID, "codex")
	if err != nil || !found {
		t.Fatalf("before=%+v found=%v err=%v", before, found, err)
	}
	generation, err := iam.ProviderCheckGeneration("codex", human.ID)
	if err != nil {
		t.Fatal(err)
	}
	envelope, current, ok, err := iam.OAuthProviderConnectionSecret(human.ID, "codex", "")
	if err != nil || !ok {
		t.Fatalf("connection ok=%v err=%v", ok, err)
	}
	if _, err := iam.ReplaceOAuthProviderConnectionIfCurrent(
		current, envelope, iam.OAuthConnectionCreate{
			PrincipalID: human.ID, ProviderID: "codex", Name: current.Name,
			Kind: current.Kind, Source: current.Source, MakeDefault: current.IsDefault,
			AccessToken: "second", RefreshToken: "refresh",
		},
	); err != nil {
		t.Fatal(err)
	}
	principal := &config.Principal{PrincipalID: human.ID, PrincipalKind: human.Kind}
	_, after, ok, err := iam.ResolveProviderCredentialSecretWithObservation(
		principal, "codex",
	)
	if err != nil || !ok || after == nil || after.ConnectionID != connection.ID ||
		after.CredentialRevision <= before.CredentialRevision {
		t.Fatalf("after=%+v before=%+v", after, before)
	}
	refreshed := refreshedProviderCheckGeneration(
		"codex", human.ID, generation, &before, after,
	)
	if refreshed <= generation {
		t.Fatalf("generation did not advance: before=%d after=%d", generation, refreshed)
	}
}

func TestProviderProbeRecordsCatalogFailureAndAccountRecovery(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	key := make([]byte, 32)
	for index := range key {
		key[index] = byte(index + 1)
	}

	var catalogMode atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch catalogMode.Load() {
		case 0:
			http.Error(w, "catalog unavailable", http.StatusBadGateway)
			return
		case 1:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[]}`))
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"fixture-model"}]}`))
		}
	}))
	defer upstream.Close()

	config.Update(func(s *config.Settings) {
		s.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(key)
		s.Providers = map[string]*config.ProviderConfig{
			"fixture": {Type: "openai_compatible", BaseURL: upstream.URL},
		}
		s.Policies.Defaults = config.ProviderPolicy{}
		s.Policies.Overrides = map[string]config.ProviderPolicy{}
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)
	human, err := iam.CreatePrincipal("human", "authentik:probe-owner", "", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	connection, err := iam.PutProviderConnection(iam.ProviderConnectionCreate{
		PrincipalID: human.ID, ProviderID: "fixture", Name: "personal",
		Kind: "api_key", Secret: "fixture-key", Source: iam.ConnectionSourceUser,
		MakeDefault: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	principal := &config.Principal{PrincipalID: human.ID, PrincipalKind: human.Kind}

	failed := runProviderProbe("fixture", "test", principal)
	if failed["success"] != false || failed["failure_code"] != "catalog_http_error" {
		t.Fatalf("failed probe=%+v", failed)
	}
	state, found, err := iam.ProviderAccountStateByConnection(connection.ID)
	if err != nil || !found || state.HealthStatus != "error" ||
		state.LastFailureCode != "catalog_http_error" {
		t.Fatalf("failed state=%+v found=%v err=%v", state, found, err)
	}

	catalogMode.Store(1)
	empty := runProviderProbe("fixture", "repair", principal)
	if empty["success"] != false || empty["failure_code"] != "catalog_empty" {
		t.Fatalf("empty probe=%+v", empty)
	}
	state, found, err = iam.ProviderAccountStateByConnection(connection.ID)
	if err != nil || !found || state.HealthStatus != "healthy" ||
		state.LastFailureCode != "" {
		t.Fatalf("empty catalog poisoned state=%+v found=%v err=%v", state, found, err)
	}

	catalogMode.Store(2)
	recovered := runProviderProbe("fixture", "repair", principal)
	if recovered["success"] != true || recovered["model_count"] != 1 {
		t.Fatalf("recovered probe=%+v", recovered)
	}
	state, found, err = iam.ProviderAccountStateByConnection(connection.ID)
	if err != nil || !found || state.HealthStatus != "healthy" ||
		state.LastFailureCode != "" {
		t.Fatalf("recovered state=%+v found=%v err=%v", state, found, err)
	}
}

func TestVerifyProviderRunsRealCompletionAndPersistsCheck(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	router.ResetSavingsState()
	router.ResetTelemetryState()
	t.Cleanup(func() {
		iam.ResetForTests()
		router.ResetSavingsState()
		router.ResetTelemetryState()
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	config.Update(func(s *config.Settings) {
		s.APIKey = "admin-secret"
		s.AllowUnauthenticatedAPI = false
		s.Providers = map[string]*config.ProviderConfig{
			"echo-verify": {Type: "echo"},
		}
		s.Endpoints = map[string]*config.EndpointConfig{}
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)

	server := httptest.NewServer(NewServer())
	defer server.Close()

	status, result := jsonRequest(t, server.URL+"/admin/api/providers/echo-verify/verify", http.MethodPost, "admin-secret", map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("verify status: %d %+v", status, result)
	}
	if result["success"] != true || result["operation"] != "verify" {
		t.Fatalf("verify result: %+v", result)
	}
	if result["model"] == "" {
		t.Fatalf("verify did not resolve a model: %+v", result)
	}

	checks, err := iam.LastProviderChecks("")
	if err != nil {
		t.Fatal(err)
	}
	recorded := checks["echo-verify"]
	if len(recorded) != 1 || recorded[0].Operation != iam.CheckVerify || !recorded[0].Success {
		t.Fatalf("persisted checks: %+v", recorded)
	}

	// The state snapshot must now report the provider as verified.
	status, state := jsonRequest(t, server.URL+"/admin/api/state", http.MethodGet, "admin-secret", nil)
	if status != http.StatusOK {
		t.Fatalf("state: %d", status)
	}
	statuses, ok := state["provider_statuses"].([]any)
	if !ok {
		t.Fatalf("provider_statuses missing")
	}
	found := false
	for _, raw := range statuses {
		row, _ := raw.(map[string]any)
		if row["id"] == "echo-verify" {
			found = true
			if row["status"] != "verified" {
				t.Fatalf("snapshot status = %v, want verified (%+v)", row["status"], row)
			}
			if row["last_verified_at"] == nil || row["last_check_operation"] != "verify" {
				t.Fatalf("snapshot missing check metadata: %+v", row)
			}
		}
	}
	if !found {
		t.Fatalf("echo-verify snapshot not present")
	}
}

func TestVerifyProviderFailureIsRecordedAndSurfaced(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	router.ResetSavingsState()
	router.ResetTelemetryState()
	t.Cleanup(func() {
		iam.ResetForTests()
		router.ResetSavingsState()
		router.ResetTelemetryState()
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	config.Update(func(s *config.Settings) {
		s.APIKey = "admin-secret"
		s.AllowUnauthenticatedAPI = false
		s.Providers = map[string]*config.ProviderConfig{
			// Points at a closed port: instantiation succeeds, completion fails.
			"dead-upstream": {Type: "openai_compatible", BaseURL: "http://127.0.0.1:1", APIKey: "sk-dead"},
		}
		s.Endpoints = map[string]*config.EndpointConfig{}
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)

	server := httptest.NewServer(NewServer())
	defer server.Close()

	status, result := jsonRequest(t, server.URL+"/admin/api/providers/dead-upstream/verify?model=gpt-test", http.MethodPost, "admin-secret", map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("verify transport status: %d", status)
	}
	if result["success"] != false || result["status"] != "failed" {
		t.Fatalf("verify should fail against dead upstream: %+v", result)
	}

	status, state := jsonRequest(t, server.URL+"/admin/api/state", http.MethodGet, "admin-secret", nil)
	if status != http.StatusOK {
		t.Fatalf("state: %d", status)
	}
	statuses, _ := state["provider_statuses"].([]any)
	for _, raw := range statuses {
		row, _ := raw.(map[string]any)
		if row["id"] == "dead-upstream" {
			if row["status"] != "check_failed" {
				t.Fatalf("snapshot status = %v, want check_failed", row["status"])
			}
			if row["last_check_success"] != false {
				t.Fatalf("snapshot last_check_success = %v", row["last_check_success"])
			}
			return
		}
	}
	t.Fatalf("dead-upstream snapshot not present")
}

func TestSpeedToRateMapsOpenAISpeeds(t *testing.T) {
	cases := map[float64]string{1.0: "+0%", 1.25: "+25%", 0.5: "-50%", 2.0: "+100%", 0: "+0%"}
	for speed, want := range cases {
		if got := speedToRate(speed); got != want {
			t.Fatalf("speedToRate(%v) = %q, want %q", speed, got, want)
		}
	}
}

func TestProviderEnabledToggleTakesProviderOutOfService(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	router.ResetSavingsState()
	router.ResetTelemetryState()
	t.Cleanup(func() {
		iam.ResetForTests()
		router.ResetSavingsState()
		router.ResetTelemetryState()
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	config.Update(func(s *config.Settings) {
		s.APIKey = "admin-secret"
		s.AllowUnauthenticatedAPI = false
		s.Providers = map[string]*config.ProviderConfig{"echo-toggle": {Type: "echo"}}
		s.Endpoints = map[string]*config.EndpointConfig{}
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)

	server := httptest.NewServer(NewServer())
	defer server.Close()

	status, result := jsonRequest(t, server.URL+"/admin/api/providers/echo-toggle/enabled", http.MethodPost, "admin-secret", map[string]any{"enabled": false})
	if status != http.StatusOK || result["enabled"] != false {
		t.Fatalf("disable: %d %+v", status, result)
	}
	if !config.Get().Providers["echo-toggle"].Disabled {
		t.Fatalf("config not marked disabled")
	}

	// Disabled providers refuse instantiation and verify records the failure.
	status, verify := jsonRequest(t, server.URL+"/admin/api/providers/echo-toggle/verify", http.MethodPost, "admin-secret", map[string]any{})
	if status != http.StatusOK || verify["success"] != false {
		t.Fatalf("verify against disabled provider should fail: %d %+v", status, verify)
	}

	// Snapshot reports the disabled status and id.
	status, state := jsonRequest(t, server.URL+"/admin/api/state", http.MethodGet, "admin-secret", nil)
	if status != http.StatusOK {
		t.Fatalf("state: %d", status)
	}
	statuses, _ := state["provider_statuses"].([]any)
	found := false
	for _, raw := range statuses {
		row, _ := raw.(map[string]any)
		if row["id"] == "echo-toggle" {
			found = true
			if row["status"] != "disabled" {
				t.Fatalf("status = %v, want disabled", row["status"])
			}
			ids, _ := row["disabled_provider_ids"].([]any)
			if len(ids) != 1 || ids[0] != "echo-toggle" {
				t.Fatalf("disabled_provider_ids = %+v", row["disabled_provider_ids"])
			}
		}
	}
	if !found {
		t.Fatalf("echo-toggle snapshot missing")
	}

	// Re-enable and verify recovery works end to end.
	status, result = jsonRequest(t, server.URL+"/admin/api/providers/echo-toggle/enabled", http.MethodPost, "admin-secret", map[string]any{"enabled": true})
	if status != http.StatusOK || result["enabled"] != true {
		t.Fatalf("enable: %d %+v", status, result)
	}
	status, verify = jsonRequest(t, server.URL+"/admin/api/providers/echo-toggle/verify", http.MethodPost, "admin-secret", map[string]any{})
	if status != http.StatusOK || verify["success"] != true {
		t.Fatalf("verify after re-enable: %d %+v", status, verify)
	}
}

func TestEdgeTTSNeedsNoCredentials(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	router.ResetSavingsState()
	router.ResetTelemetryState()
	t.Cleanup(func() {
		iam.ResetForTests()
		router.ResetSavingsState()
		router.ResetTelemetryState()
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	config.Update(func(s *config.Settings) {
		s.APIKey = "admin-secret"
		s.AllowUnauthenticatedAPI = false
		s.Providers = map[string]*config.ProviderConfig{"edge_tts": {Type: "edge_tts"}}
		s.Endpoints = map[string]*config.EndpointConfig{}
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)

	server := httptest.NewServer(NewServer())
	defer server.Close()
	status, state := jsonRequest(t, server.URL+"/admin/api/state", http.MethodGet, "admin-secret", nil)
	if status != http.StatusOK {
		t.Fatalf("state: %d", status)
	}
	statuses, _ := state["provider_statuses"].([]any)
	for _, raw := range statuses {
		row, _ := raw.(map[string]any)
		if row["id"] == "edge_tts" {
			if row["status"] == "needs_credentials" {
				t.Fatalf("edge_tts must not require credentials: %+v", row)
			}
			return
		}
	}
	t.Fatalf("edge_tts snapshot missing")
}

// A provider body that silently drops fields is worse than rejecting them: the
// API returns 200 and the provider then fails at request time with a vague
// configuration error. Vertex needs project and location to build any URL.
func TestUpsertProviderPersistsProjectAndLocation(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	router.ResetSavingsState()
	router.ResetTelemetryState()
	t.Cleanup(func() {
		iam.ResetForTests()
		router.ResetSavingsState()
		router.ResetTelemetryState()
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	config.Update(func(s *config.Settings) {
		s.APIKey = "admin-secret"
		s.AllowUnauthenticatedAPI = false
		s.Providers = map[string]*config.ProviderConfig{}
		s.Endpoints = map[string]*config.EndpointConfig{}
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)

	server := httptest.NewServer(NewServer())
	defer server.Close()
	status, created := jsonRequest(t, server.URL+"/admin/api/providers", http.MethodPost, "admin-secret", map[string]any{
		"registry_id": "vertex_ai", "id": "vertex", "api_key": "test-key",
		"project": "my-project", "location": "us-central1",
	})
	if status != http.StatusOK {
		t.Fatalf("create: %d %+v", status, created)
	}
	cfg := config.Get().Providers["vertex"]
	if cfg == nil || cfg.Project != "my-project" || cfg.Location != "us-central1" {
		t.Fatalf("project/location were dropped: %+v", cfg)
	}
}
