package api

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"llmgw/internal/codexauth"
	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
	"llmgw/internal/router"
)

func TestProviderQuotaAdvisoriesAreHonestUnknownWithoutNumericLimits(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	router.ResetSavingsState()
	router.ResetTelemetryState()
	t.Cleanup(func() { iam.ResetForTests(); router.ResetSavingsState(); router.ResetTelemetryState() })
	oldProviders, oldKey, oldAllow := config.Get().Providers, config.Get().APIKey, config.Get().AllowUnauthenticatedAPI
	t.Cleanup(func() {
		config.Update(func(s *config.Settings) {
			s.Providers, s.APIKey, s.AllowUnauthenticatedAPI = oldProviders, oldKey, oldAllow
		})
	})
	config.Update(func(s *config.Settings) {
		s.APIKey, s.AllowUnauthenticatedAPI = "admin-secret", false
		s.Providers = map[string]*config.ProviderConfig{"echo": {Type: "echo"}}
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(NewServer())
	defer server.Close()
	from := time.Now().Add(-time.Hour).Unix()
	to := time.Now().Unix() + 1
	status, payload := jsonRequest(t, server.URL+"/admin/api/usage?from="+strconv.FormatInt(from, 10)+"&to="+strconv.FormatInt(to, 10)+"&bucket=hour", http.MethodGet, "admin-secret", nil)
	if status != http.StatusOK {
		t.Fatalf("usage status=%d payload=%+v", status, payload)
	}
	advisories := payload["quota_advisories"].([]any)
	if len(advisories) != 1 {
		t.Fatalf("advisories=%+v", advisories)
	}
	advisory := advisories[0].(map[string]any)
	if advisory["provider_id"] != "echo" || advisory["status"] != "unknown" || advisory["source"] != "no_verified_quota_adapter" {
		t.Fatalf("advisory=%+v", advisory)
	}
	for _, forbidden := range []string{"remaining", "limit", "percent", "used"} {
		if _, exists := advisory[forbidden]; exists {
			t.Fatalf("unknown quota fabricated %s: %+v", forbidden, advisory)
		}
	}
	status, _ = jsonRequest(t, server.URL+"/admin/api/usage?bucket=minute", http.MethodGet, "admin-secret", nil)
	if status != http.StatusBadRequest {
		t.Fatalf("invalid bucket status=%d", status)
	}
}

func TestProviderQuotaAdvisoriesUseFreshPrincipalScopedSnapshots(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	providers.ResetProviders()
	t.Cleanup(func() { iam.ResetForTests(); providers.ResetProviders() })
	key := make([]byte, 32)
	oldProviders, oldCredentialKey := config.Get().Providers, config.Get().CredentialEncryptionKey
	t.Cleanup(func() {
		config.Update(func(s *config.Settings) {
			s.Providers, s.CredentialEncryptionKey = oldProviders, oldCredentialKey
		})
	})
	config.Update(func(s *config.Settings) {
		s.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(key)
		s.Providers = map[string]*config.ProviderConfig{"echo": {Type: "echo"}}
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	owner, err := iam.CreatePrincipal("human", "authentik:quota-owner", "", "Quota Owner")
	if err != nil {
		t.Fatal(err)
	}
	other, err := iam.CreatePrincipal("human", "authentik:quota-other", "", "Quota Other")
	if err != nil {
		t.Fatal(err)
	}
	ownerConnection, err := iam.PutProviderConnection(iam.ProviderConnectionCreate{
		PrincipalID: owner.ID, ProviderID: "echo", Kind: "api_key",
		Secret: "owner-secret", Source: iam.ConnectionSourceUser,
	})
	if err != nil {
		t.Fatal(err)
	}
	otherConnection, err := iam.PutProviderConnection(iam.ProviderConnectionCreate{
		PrincipalID: other.ID, ProviderID: "echo", Kind: "api_key",
		Secret: "other-secret", Source: iam.ConnectionSourceUser,
	})
	if err != nil {
		t.Fatal(err)
	}
	ownerRemaining := 80.0
	otherRemaining := 20.0
	now := time.Now().Unix()
	ownerState, found, err := iam.ProviderAccountStateByConnection(ownerConnection.ID)
	if err != nil || !found {
		t.Fatalf("owner account state=%+v found=%v err=%v", ownerState, found, err)
	}
	otherState, found, err := iam.ProviderAccountStateByConnection(otherConnection.ID)
	if err != nil || !found {
		t.Fatalf("other account state=%+v found=%v err=%v", otherState, found, err)
	}
	revisions := map[string]int64{
		ownerConnection.ID: ownerState.CredentialRevision,
		otherConnection.ID: otherState.CredentialRevision,
	}
	for connectionID, remaining := range map[string]*float64{
		ownerConnection.ID: &ownerRemaining,
		otherConnection.ID: &otherRemaining,
	} {
		if err := iam.ReplaceProviderQuotaSnapshots(connectionID, revisions[connectionID], []iam.ProviderQuotaSnapshot{{
			Metric: "requests", Unit: "percent", Window: "day",
			RemainingValue: remaining, Source: "fixture", Confidence: "verified",
			RefreshedAt: now, ExpiresAt: now + 3600,
		}}); err != nil {
			t.Fatal(err)
		}
	}

	scoped, err := providerQuotaAdvisories(owner.ID)
	if err != nil || len(scoped) != 1 {
		t.Fatalf("scoped advisories=%+v err=%v", scoped, err)
	}
	if scoped[0]["status"] != "available" || scoped[0]["account_count"] != 1 ||
		scoped[0]["source"] != "fixture" {
		t.Fatalf("scoped advisory=%+v", scoped[0])
	}
	dimensions := scoped[0]["dimensions"].([]map[string]any)
	if len(dimensions) != 1 || dimensions[0]["remaining"] != ownerRemaining {
		t.Fatalf("scoped dimensions=%+v", dimensions)
	}

	admin, err := providerQuotaAdvisories("")
	if err != nil || len(admin) != 1 || admin[0]["account_count"] != 2 {
		t.Fatalf("admin advisories=%+v err=%v", admin, err)
	}

	if err := iam.ReplaceProviderQuotaSnapshots(ownerConnection.ID, ownerState.CredentialRevision, []iam.ProviderQuotaSnapshot{{
		Metric: "requests", Unit: "percent", Window: "day",
		Source: "fixture", Confidence: "unknown", RefreshedAt: now, ExpiresAt: now + 3600,
	}}); err != nil {
		t.Fatal(err)
	}
	scoped, err = providerQuotaAdvisories(owner.ID)
	if err != nil || scoped[0]["status"] != "unknown" ||
		scoped[0]["source"] != "adapter_reported_unknown" {
		t.Fatalf("unknown advisory=%+v err=%v", scoped, err)
	}

	if err := iam.ReplaceProviderQuotaSnapshots(ownerConnection.ID, ownerState.CredentialRevision, []iam.ProviderQuotaSnapshot{{
		Metric: "requests", Unit: "percent", Window: "day",
		RemainingValue: &ownerRemaining, Source: "fixture", Confidence: "verified",
		RefreshedAt: now - int64(2*time.Hour/time.Second),
	}}); err != nil {
		t.Fatal(err)
	}
	scoped, err = providerQuotaAdvisories(owner.ID)
	if err != nil || scoped[0]["status"] != "stale" ||
		scoped[0]["source"] != "stale_quota_snapshot" {
		t.Fatalf("stale advisory=%+v err=%v", scoped, err)
	}
}

func TestProviderProbeUsesSelectedCodexOwnerCatalog(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	providers.ResetProviders()
	t.Cleanup(func() { iam.ResetForTests(); providers.ResetProviders() })
	key := make([]byte, 32)
	oldProviders, oldAPIKey, oldAllow := config.Get().Providers, config.Get().APIKey, config.Get().AllowUnauthenticatedAPI
	oldCredentialKey, oldClientID := config.Get().CredentialEncryptionKey, config.Get().OpenAICodexClientID
	t.Cleanup(func() {
		config.Update(func(s *config.Settings) {
			s.Providers, s.APIKey, s.AllowUnauthenticatedAPI = oldProviders, oldAPIKey, oldAllow
			s.CredentialEncryptionKey, s.OpenAICodexClientID = oldCredentialKey, oldClientID
		})
	})
	config.Update(func(s *config.Settings) {
		s.APIKey, s.AllowUnauthenticatedAPI = "admin-secret", false
		s.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(key)
		s.OpenAICodexClientID = "fixture-client"
		s.Providers = map[string]*config.ProviderConfig{"codex": {Type: "openai_compatible", RegistryID: "openai_codex"}}
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	owner, err := iam.CreatePrincipal("human", "authentik:probe-owner", "", "Probe Owner")
	if err != nil {
		t.Fatal(err)
	}
	other, err := iam.CreatePrincipal("human", "authentik:probe-other", "", "Probe Other")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
		PrincipalID: owner.ID, ProviderID: "codex", Kind: "openai_codex_oauth", AccessToken: "owner-access", RefreshToken: "owner-refresh",
	}); err != nil {
		t.Fatal(err)
	}
	modelCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5-codex"}]}`))
	}))
	defer upstream.Close()
	oldModels := codexauth.ModelsURL
	codexauth.ModelsURL = upstream.URL
	t.Cleanup(func() { codexauth.ModelsURL = oldModels })
	server := httptest.NewServer(NewServer())
	defer server.Close()

	status, denied := jsonRequest(t, server.URL+"/admin/api/providers/codex/test", http.MethodPost, "admin-secret", map[string]any{})
	if status != http.StatusBadRequest || denied["error"] == nil || modelCalls != 0 {
		t.Fatalf("unscoped probe status=%d payload=%+v calls=%d", status, denied, modelCalls)
	}
	for _, operation := range []string{"refresh", "test", "repair"} {
		status, result := jsonRequest(t, server.URL+"/admin/api/providers/codex/"+operation+"?principal_id="+owner.ID, http.MethodPost, "admin-secret", map[string]any{})
		if status != http.StatusOK || result["success"] != true || result["model_count"] != float64(1) {
			t.Fatalf("owner %s status=%d result=%+v", operation, status, result)
		}
	}
	status, result := jsonRequest(t, server.URL+"/admin/api/providers/codex/test?principal_id="+other.ID, http.MethodPost, "admin-secret", map[string]any{})
	if status != http.StatusOK || result["success"] != false || modelCalls != 3 {
		t.Fatalf("other probe status=%d result=%+v calls=%d", status, result, modelCalls)
	}
}
