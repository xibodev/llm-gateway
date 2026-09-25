package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
	"llmgw/internal/router"

	core "github.com/xibodev/llmgw-core"
)

type responsesOnlyVerificationProvider struct {
	chatCalls      int
	responsesCalls int
	payload        map[string]any
}

func (p *responsesOnlyVerificationProvider) Complete(string, []providers.Message, providers.Kwargs) (map[string]any, error) {
	p.chatCalls++
	return nil, nil
}

func (*responsesOnlyVerificationProvider) Stream(string, []providers.Message, providers.Kwargs) (providers.StreamIter, error) {
	return nil, providers.ErrResponsesUnsupported
}

func (*responsesOnlyVerificationProvider) ListModels() []providers.ModelInfo { return nil }
func (*responsesOnlyVerificationProvider) IsStub() bool                      { return false }
func (*responsesOnlyVerificationProvider) PreservesWireNativeSurface(_ string, surface core.ModelSurface) bool {
	return surface == core.ModelSurfaceResponses
}

func (p *responsesOnlyVerificationProvider) CompleteResponsesContext(
	_ context.Context, _ string, payload map[string]any,
) (map[string]any, *providers.CredentialObservation, error) {
	p.responsesCalls++
	p.payload = payload
	return map[string]any{
		"object": "response", "status": "completed",
		"output": []any{map[string]any{
			"type": "message", "content": []any{map[string]any{"type": "output_text", "text": "ok"}},
		}},
	}, nil, nil
}

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

func TestProviderVerificationRejectsMalformedSuccess(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chat_1","choices":[{}]}`))
	}))
	defer upstream.Close()
	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{
			"fixture": {Type: "openai_compatible", BaseURL: upstream.URL, APIKey: "fixture"},
		}
		s.Policies.Defaults = config.ProviderPolicy{}
		s.Policies.Overrides = map[string]config.ProviderPolicy{}
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)
	result := runProviderVerify("fixture", "fixture-model", nil)
	if result["success"] != false || result["failure_code"] != "verification_failed" {
		t.Fatalf("verification=%+v", result)
	}
}

func TestProviderVerificationAcceptsTextPartAcknowledgement(t *testing.T) {
	if !verificationReplyOK(map[string]any{"choices": []any{map[string]any{
		"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}},
	}}}, true) {
		t.Fatal("text-part response was not accepted")
	}
	if verificationReplyOK(map[string]any{"choices": []any{map[string]any{"message": map[string]any{}}}}, false) {
		t.Fatal("empty message was accepted")
	}
	if verificationReplyOK(map[string]any{"choices": []any{map[string]any{
		"message": map[string]any{"content": "quota exceeded"},
	}}}, true) {
		t.Fatal("automatic verification accepted a soft-error message")
	}
}

func TestProviderVerificationUsesNativeResponsesForResponsesOnlyModel(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{"codex": {Type: "openai_compatible", RegistryID: "openai_codex"}}
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)
	capabilities := &core.ModelCapabilities{}
	capabilities.Surfaces.Responses = core.SupportSupported
	capabilities.Surfaces.ChatCompletions = core.SupportUnsupported
	provider := &responsesOnlyVerificationProvider{}
	response, _, err := runVerificationCompletionForCatalogModel(
		context.Background(), "codex", provider, "responses-model",
		[]providers.Message{{"role": "user", "content": "ok"}},
		providers.Kwargs{"max_tokens": 16},
		providers.ModelInfo{ID: "responses-model", TypedCapabilities: capabilities}, true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if provider.responsesCalls != 1 || provider.chatCalls != 0 {
		t.Fatalf("responses calls=%d chat calls=%d", provider.responsesCalls, provider.chatCalls)
	}
	if provider.payload["max_output_tokens"] != nil || !verificationReplyOK(response, true) {
		t.Fatalf("payload=%+v response=%+v", provider.payload, response)
	}
	if input, ok := provider.payload["input"].([]any); !ok || len(input) != 1 {
		t.Fatalf("verification input is not a Responses item list: %+v", provider.payload)
	}
}

func TestProviderVerificationKeepsOutputLimitForOtherResponsesProviders(t *testing.T) {
	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{"fixture": {Type: "openai_compatible"}}
	})
	capabilities := &core.ModelCapabilities{}
	capabilities.Surfaces.Responses = core.SupportSupported
	capabilities.Surfaces.ChatCompletions = core.SupportUnsupported
	provider := &responsesOnlyVerificationProvider{}
	_, _, err := runVerificationCompletionForCatalogModel(
		context.Background(), "fixture", provider, "responses-model",
		[]providers.Message{{"role": "user", "content": "ok"}},
		providers.Kwargs{"max_tokens": 16},
		providers.ModelInfo{ID: "responses-model", TypedCapabilities: capabilities}, true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if provider.payload["max_output_tokens"] != 16 {
		t.Fatalf("payload=%+v", provider.payload)
	}
}

func TestProviderVerificationFailureAuditIsNotSuccess(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/admin/api/providers/fixture/verify", nil)
	setAdminActor(request, adminActor{Source: "static-admin-key"})
	auditAdminResult(
		request, "provider.verify", "provider", "fixture", "failure",
		map[string]any{"model": "fixture-model"},
	)
	events, err := iam.ListAudit(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Action != "provider.verify" || events[0].Result != "failure" {
		t.Fatalf("audit events=%+v", events)
	}
}

func TestProviderEvidenceDoesNotPromoteCatalogDiscoveryToCompletion(t *testing.T) {
	for _, scenario := range []struct {
		name                        string
		models                      int
		attempted                   bool
		succeeded                   bool
		wantCatalog, wantCompletion string
	}{
		{name: "empty catalog", wantCatalog: "empty", wantCompletion: "not_probed"},
		{name: "catalog only", models: 2, wantCatalog: "discovered", wantCompletion: "not_probed"},
		{name: "completion failed", models: 2, attempted: true, wantCatalog: "discovered", wantCompletion: "failed"},
		{name: "completion verified", models: 2, attempted: true, succeeded: true, wantCatalog: "discovered", wantCompletion: "verified"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			evidence := providers.ClassifyProviderEvidence(nil, scenario.models, scenario.attempted, scenario.succeeded)
			if evidence.Authentication != "accepted" || evidence.Catalog != scenario.wantCatalog || evidence.Completion != scenario.wantCompletion {
				t.Fatalf("evidence=%+v", evidence)
			}
		})
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
		"codex", human.ID, generation, credentialObservation(&before), credentialObservation(after),
	)
	if refreshed <= generation {
		t.Fatalf("generation did not advance: before=%d after=%d", generation, refreshed)
	}
}

func TestAccountObservationConversionRoundTrips(t *testing.T) {
	if got := credentialObservation(nil); got != nil {
		t.Fatalf("nil converted to %+v", got)
	}
	// Revision 0 survives both ways: the IfCurrent guards refuse it, not the
	// conversion.
	for _, account := range []iam.ProviderAccountObservation{
		{ConnectionID: "conn-fixture"},
		{ConnectionID: "conn-fixture", CredentialRevision: 3},
	} {
		observed := credentialObservation(&account)
		if observed == nil || accountObservation(*observed) != account {
			t.Fatalf("round trip of %+v = %+v", account, observed)
		}
	}
	reported := providers.CredentialObservation{ConnectionID: "conn-fixture", CredentialRevision: 3}
	account := accountObservation(reported)
	if back := credentialObservation(&account); back == nil || *back != reported {
		t.Fatalf("round trip of %+v = %+v", reported, back)
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
	if empty["success"] != false || empty["failure_code"] != "catalog_empty" ||
		empty["authentication_state"] != "accepted" || empty["catalog_evidence"] != "empty" ||
		empty["completion_evidence"] != "not_probed" || empty["owner_scope"] != "human_owner" {
		t.Fatalf("empty probe=%+v", empty)
	}
	state, found, err = iam.ProviderAccountStateByConnection(connection.ID)
	if err != nil || !found || state.HealthStatus != "healthy" ||
		state.LastFailureCode != "" {
		t.Fatalf("empty catalog poisoned state=%+v found=%v err=%v", state, found, err)
	}

	catalogMode.Store(2)
	recovered := runProviderProbe("fixture", "repair", principal)
	if recovered["success"] != true || recovered["model_count"] != 1 ||
		recovered["authentication_state"] != "accepted" || recovered["catalog_evidence"] != "discovered" ||
		recovered["completion_evidence"] != "not_probed" {
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

func TestProviderEvidenceSnapshotKeepsCatalogAndCompletionIndependent(t *testing.T) {
	refreshed := time.Now()
	auth, catalog, completion := providerEvidenceSnapshot(
		true, 3, refreshed, "catalog_synced",
		map[string]any{"verification_state": "unknown"},
	)
	if auth != "accepted" || catalog != "discovered" || completion != "not_probed" {
		t.Fatalf("catalog evidence = %q %q %q", auth, catalog, completion)
	}

	auth, catalog, completion = providerEvidenceSnapshot(
		true, 0, time.Time{}, "check_failed",
		map[string]any{"verification_state": "failed"},
	)
	if auth != "unknown" || catalog != "failed" || completion != "failed" {
		t.Fatalf("failed evidence = %q %q %q", auth, catalog, completion)
	}
}

func TestVerifyProviderImmediateFailureDetailsAreSanitized(t *testing.T) {
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

	secret := "gsk_" + strings.Repeat("x", 24)
	email := "verification-owner@example.test"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"message": "failed for " + email + " with " + secret},
		})
	}))
	defer upstream.Close()

	config.Update(func(s *config.Settings) {
		s.APIKey = "admin-secret"
		s.AllowUnauthenticatedAPI = false
		s.Providers = map[string]*config.ProviderConfig{
			"redaction-fixture": {
				Type: "openai_compatible", BaseURL: upstream.URL, APIKey: "fixture",
			},
		}
		s.Endpoints = map[string]*config.EndpointConfig{}
		s.Policies.Defaults = config.ProviderPolicy{RetryMaxAttempts: 1}
		s.Policies.Overrides = map[string]config.ProviderPolicy{}
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)

	server := httptest.NewServer(NewServer())
	defer server.Close()
	status, result := jsonRequest(
		t,
		server.URL+"/admin/api/providers/redaction-fixture/verify?model=fixture-model",
		http.MethodPost,
		"admin-secret",
		map[string]any{},
	)
	if status != http.StatusOK || result["success"] != false {
		t.Fatalf("verification response = %d %+v", status, result)
	}
	detail, _ := result["details"].(string)
	if strings.Contains(detail, secret) || strings.Contains(detail, email) {
		t.Fatalf("verification detail exposed diagnostics: %q", detail)
	}
	if !strings.Contains(detail, "[redacted]") || !strings.Contains(detail, "429") {
		t.Fatalf("verification detail lost safe context: %q", detail)
	}
}

func TestWriteErrorSanitizesDiagnosticMessage(t *testing.T) {
	secret := "llmgw_" + strings.Repeat("C", 32)
	for name, testCase := range map[string]struct {
		status int
		write  func(http.ResponseWriter)
	}{
		"standard error": {
			status: http.StatusBadGateway,
			write: func(w http.ResponseWriter) {
				writeError(w, http.StatusBadGateway, "failed for api-owner@example.test using "+secret)
			},
		},
		"upstream error": {
			status: http.StatusTooManyRequests,
			write: func(w http.ResponseWriter) {
				writeUpstreamError(w, &router.AllTargetsFailed{
					Msg: "failed for api-owner@example.test using " + secret, Status: http.StatusTooManyRequests,
				})
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			testCase.write(recorder)
			if recorder.Code != testCase.status {
				t.Fatalf("status = %d, want %d", recorder.Code, testCase.status)
			}
			var response map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			envelope, _ := response["error"].(map[string]any)
			message, _ := envelope["message"].(string)
			if strings.Contains(message, secret) || strings.Contains(message, "api-owner@example.test") ||
				!strings.Contains(message, "[redacted]") {
				t.Fatalf("API error message was not sanitized: %q", message)
			}
		})
	}
}

func TestWriteUpstreamErrorPreservesDirectInvocationStatus(t *testing.T) {
	secret := "llmgw_" + strings.Repeat("D", 32)
	for _, status := range []int{
		http.StatusForbidden,
		http.StatusTooManyRequests,
		http.StatusServiceUnavailable,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			err := &providers.InvocationError{
				Msg:    "failed for direct-owner@example.test using " + secret,
				Status: status,
			}
			if detail := err.Error(); strings.Contains(detail, secret) ||
				strings.Contains(detail, "direct-owner@example.test") ||
				!strings.Contains(detail, "[redacted]") {
				t.Fatalf("typed provider detail was not sanitized: %q", detail)
			}

			recorder := httptest.NewRecorder()
			writeUpstreamError(recorder, err)
			if recorder.Code != status {
				t.Fatalf("status = %d, want %d", recorder.Code, status)
			}
			var response map[string]any
			if decodeErr := json.Unmarshal(recorder.Body.Bytes(), &response); decodeErr != nil {
				t.Fatal(decodeErr)
			}
			envelope, _ := response["error"].(map[string]any)
			message, _ := envelope["message"].(string)
			if strings.Contains(message, secret) || strings.Contains(message, "direct-owner@example.test") {
				t.Fatalf("upstream response exposed diagnostic detail: %q", message)
			}
		})
	}
}

func TestWriteUpstreamErrorSafelyPreservesRetryAfter(t *testing.T) {
	for _, test := range []struct {
		name, supplied, want string
	}{
		{name: "delta seconds", supplied: " 17 ", want: "17"},
		{name: "HTTP date", supplied: "Wed, 21 Oct 2015 07:28:00 GMT", want: "Wed, 21 Oct 2015 07:28:00 GMT"},
		{name: "invalid", supplied: "quota project=private", want: ""},
		{name: "negative", supplied: "-1", want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			writeUpstreamError(recorder, &providers.InvocationError{
				Msg: "upstream body must not be exposed", Status: http.StatusTooManyRequests, RetryAfter: test.supplied,
			})
			if recorder.Code != http.StatusTooManyRequests || recorder.Header().Get("Retry-After") != test.want {
				t.Fatalf("status=%d Retry-After=%q", recorder.Code, recorder.Header().Get("Retry-After"))
			}
			var response map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			envelope, _ := response["error"].(map[string]any)
			if envelope["code"] != "429" || envelope["message"] != "Upstream provider request failed." ||
				strings.Contains(recorder.Body.String(), "body must not be exposed") || strings.Contains(recorder.Body.String(), "private") {
				t.Fatalf("unsafe or unstructured response: %s", recorder.Body.String())
			}
		})
	}
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
		"project": "my-project", "location": "us-central1", "vertex_request_type": "dedicated",
	})
	if status != http.StatusOK {
		t.Fatalf("create: %d %+v", status, created)
	}
	cfg := config.Get().Providers["vertex"]
	if cfg == nil || cfg.Project != "my-project" || cfg.Location != "us-central1" || cfg.VertexRequestType != "dedicated" {
		t.Fatalf("project/location were dropped: %+v", cfg)
	}
	status, _ = jsonRequest(t, server.URL+"/admin/api/providers", http.MethodPost, "admin-secret", map[string]any{
		"registry_id": "vertex_ai", "id": "vertex", "project": "my-project", "location": "us-central1",
	})
	if status != http.StatusOK || config.Get().Providers["vertex"].VertexRequestType != "dedicated" {
		t.Fatalf("omitted request type was not preserved: status=%d config=%+v", status, config.Get().Providers["vertex"])
	}
	clear := ""
	status, _ = jsonRequest(t, server.URL+"/admin/api/providers", http.MethodPost, "admin-secret", map[string]any{
		"registry_id": "vertex_ai", "id": "vertex", "project": "my-project", "location": "us-central1",
		"vertex_request_type": clear,
	})
	if status != http.StatusOK || config.Get().Providers["vertex"].VertexRequestType != "" {
		t.Fatalf("explicit clear failed: status=%d config=%+v", status, config.Get().Providers["vertex"])
	}
	status, _ = jsonRequest(t, server.URL+"/admin/api/providers", http.MethodPost, "admin-secret", map[string]any{
		"registry_id": "vertex_ai", "id": "vertex", "project": "my-project", "location": "us-central1",
		"vertex_request_type": "invalid",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("invalid request type status=%d", status)
	}
}
