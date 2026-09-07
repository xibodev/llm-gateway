package api

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
)

type catalogDNSTransport struct{}

func (catalogDNSTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, &net.DNSError{Name: "private-fixture.invalid", Err: "fixture-secret", IsNotFound: true}
}

func setupCatalogDiagnosticAPI(t *testing.T, url string) {
	t.Helper()
	old := *config.Get()
	setupProviderCredentialAPI(t, t.TempDir())
	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{
			"diagnostic": {Type: "openai_compatible", BaseURL: url, APIKey: "fixture-key"},
		}
		s.Endpoints = map[string]*config.EndpointConfig{}
		s.Policies.Defaults = config.ProviderPolicy{}
		s.Policies.Overrides = map[string]config.ProviderPolicy{}
	})
	providers.ForgetCatalog("diagnostic")
	providers.ResetProviders()
	t.Cleanup(func() {
		providers.ForgetCatalog("diagnostic")
		providers.ResetProviders()
		config.Update(func(s *config.Settings) { *s = old })
	})
}

func TestAdminCatalogDiagnosticsEmptyHTTPAndDNS(t *testing.T) {
	for _, scenario := range []struct {
		name, want, code, body string
		status                 int
	}{
		{name: "empty", want: "empty", status: 200},
		{name: "auth", want: "error", code: "catalog_http_error", status: 403},
		{name: "http", want: "error", code: "catalog_http_error", status: 503},
		{name: "dns", want: "error", code: "catalog_transport_error"},
		{name: "invalid-rows", want: "error", code: "catalog_invalid_shape", status: 200, body: `{"data":[null,17,"fixture-secret",{"id":false}]}`},
		{name: "mixed-rows", want: "error", code: "catalog_invalid_shape", status: 200, body: `{"data":[{"id":"fixture-model"},{"id":false,"private":"fixture-token"}]}`},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != "/models" {
					t.Errorf("catalog GET ran non-discovery request %s", r.URL.Path)
				}
				w.WriteHeader(scenario.status)
				if scenario.body != "" {
					_, _ = w.Write([]byte(scenario.body))
				} else if scenario.status == 200 {
					_, _ = w.Write([]byte(`{"data":[]}`))
				} else {
					_, _ = w.Write([]byte(`{"error":"api_key=fixture-secret Authorization: Bearer fixture-token"}`))
				}
			}))
			defer upstream.Close()
			setupCatalogDiagnosticAPI(t, upstream.URL)
			principal, err := iam.CreatePrincipal("human", "fixture:diagnostic-reader", "", "Reader")
			if err != nil {
				t.Fatal(err)
			}
			if scenario.name == "dns" {
				original := http.DefaultTransport
				http.DefaultTransport = catalogDNSTransport{}
				t.Cleanup(func() { http.DefaultTransport = original })
			}
			for _, route := range []string{"catalog", "models"} {
				method := http.MethodGet
				if route == "models" {
					method = http.MethodPost
				}
				req := httptest.NewRequest(method, "/admin/api/providers/diagnostic/"+route+"?principal_id="+principal.ID, nil)
				req.Header.Set("Authorization", "Bearer admin-secret")
				rec := httptest.NewRecorder()
				NewServer().ServeHTTP(rec, req)
				var payload map[string]any
				if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
					t.Fatal(err)
				}
				if rec.Code != 200 || len(payload["models"].([]any)) != 0 {
					t.Fatalf("legacy contract: %d %+v", rec.Code, payload)
				}
				diagnostic := payload["catalog"].(map[string]any)
				if diagnostic["status"] != scenario.want || stringOf(diagnostic["failure_code"]) != scenario.code ||
					diagnostic["stale"] != false || diagnostic["source_scope"] != "principal" {
					t.Fatalf("diagnostics: %+v", diagnostic)
				}
				if scenario.code != "" && scenario.status != 0 && diagnostic["upstream_status"] != float64(scenario.status) {
					t.Fatalf("upstream status: %+v", diagnostic)
				}
				for _, private := range []string{"fixture-secret", "fixture-token", "private-fixture.invalid", principal.ID} {
					if strings.Contains(rec.Body.String(), private) {
						t.Fatalf("response disclosed %q", private)
					}
				}
			}
			if scenario.name == "empty" && calls.Load() != 1 {
				t.Fatalf("successful empty catalog was not cached: %d calls", calls.Load())
			}
			checks, err := iam.LastProviderChecks("")
			if err != nil || len(checks["diagnostic"]) != 0 {
				t.Fatalf("GET must not run or persist verification: %+v %v", checks, err)
			}
		})
	}
}

func TestCatalogReadinessDoesNotBorrowAnotherScopeVerification(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"listed-model"}]}`))
	}))
	defer upstream.Close()
	setupCatalogDiagnosticAPI(t, upstream.URL)
	owner, err := iam.CreatePrincipal("human", "fixture:catalog-owner", "", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	other, err := iam.CreatePrincipal("human", "fixture:catalog-other", "", "Other")
	if err != nil {
		t.Fatal(err)
	}
	if err := iam.RecordProviderCheck(iam.ProviderCheck{
		ProviderID: "diagnostic", Operation: iam.CheckVerify, ScopeKey: owner.ID,
		Success: true, Model: "private-model", CheckedAt: time.Now().Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{owner.ID, other.ID} {
		req := httptest.NewRequest(http.MethodGet, "/admin/api/providers/diagnostic/catalog?principal_id="+id, nil)
		req.Header.Set("Authorization", "Bearer admin-secret")
		rec := httptest.NewRecorder()
		NewServer().ServeHTTP(rec, req)
		var payload map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil || rec.Code != 200 {
			t.Fatalf("response: %d %s %v", rec.Code, rec.Body.String(), err)
		}
		readiness := payload["readiness"].(map[string]any)
		if readiness["model_verified"] != (id == owner.ID) || readiness["catalog_synced"] != true {
			t.Fatalf("readiness: %+v", readiness)
		}
		if id == other.ID && strings.Contains(rec.Body.String(), "private-model") {
			t.Fatal("another principal received the owner's verification model")
		}
		if strings.Contains(rec.Body.String(), owner.ID) || strings.Contains(rec.Body.String(), other.ID) {
			t.Fatal("scope identifiers appeared in diagnostics")
		}
	}
	rows, err := providerStatusSnapshots(config.Get(), nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		if row["id"] != "diagnostic" {
			continue
		}
		found = true
		if row["status"] != "catalog_synced" {
			t.Fatalf("aggregate borrowed principal verification: %+v", row)
		}
		readiness := row["readiness"].(map[string]any)
		if readiness["model_verified"] != false || readiness["verification_state"] != "scope_mismatch" {
			t.Fatalf("aggregate readiness: %+v", readiness)
		}
	}
	if !found {
		t.Fatal("provider status missing")
	}
	portalRows, err := providerStatusSnapshots(config.Get(), nil, nil, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(portalRows)
	if strings.Contains(string(raw), "private-model") || strings.Contains(string(raw), owner.ID) {
		t.Fatal("portal received another principal's verification evidence")
	}
}

func TestScopedReadinessStaleAndFailedVerification(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		age    time.Duration
		failed bool
		want   string
	}{
		{name: "fresh", want: "verified"},
		{name: "stale", age: 2 * time.Hour, want: "stale"},
		{name: "failed", failed: true, want: "failed"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			checks := []iam.ProviderCheck{{Operation: iam.CheckVerify, ScopeKey: "owner", Success: true,
				Model: "fixture-model", CheckedAt: time.Now().Add(-scenario.age).Unix()}}
			if scenario.failed {
				checks = append(checks, iam.ProviderCheck{Operation: iam.CheckCatalogSync,
					ScopeKey: "owner", CheckedAt: time.Now().Unix()})
			}
			result := scopedReadiness(checks, "owner", false)
			if result["verification_state"] != scenario.want || result["model_verified"] != (scenario.want == "verified") {
				t.Fatalf("readiness: %+v", result)
			}
		})
	}
}

func TestModelListRetainsBestEffortContractOnCatalogFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "fixture failure", http.StatusBadGateway)
	}))
	defer upstream.Close()
	setupCatalogDiagnosticAPI(t, upstream.URL)
	config.Update(func(s *config.Settings) { s.Providers["healthy"] = &config.ProviderConfig{Type: "echo"} })
	providers.ForgetCatalog("healthy")
	t.Cleanup(func() { providers.ForgetCatalog("healthy") })
	result, err := buildModelList(nil)
	if err != nil || result["object"] != "list" || len(result["data"].([]any)) == 0 {
		t.Fatalf("best-effort model list: %+v %v", result, err)
	}
	for _, raw := range result["data"].([]any) {
		if raw.(map[string]any)["owned_by"] == "diagnostic" {
			t.Fatalf("failed discovery invented models: %+v", raw)
		}
	}
}

func TestGoogleCatalogMethodsDoNotProveModelEntitlement(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if !strings.HasSuffix(r.URL.Path, "/models") {
			t.Errorf("unexpected inference: %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"models":[{"name":"models/gemini-fixture","supportedGenerationMethods":["generateContent"]}]}`))
	}))
	defer upstream.Close()
	setupCatalogDiagnosticAPI(t, upstream.URL)
	config.Update(func(s *config.Settings) { s.Providers["diagnostic"].Type = "ai_studio" })
	req := httptest.NewRequest(http.MethodGet, "/admin/api/providers/diagnostic/catalog", nil)
	req.Header.Set("Authorization", "Bearer admin-secret")
	rec := httptest.NewRecorder()
	NewServer().ServeHTTP(rec, req)
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil || rec.Code != 200 {
		t.Fatalf("catalog: %d %s %v", rec.Code, rec.Body.String(), err)
	}
	rows := payload["models"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["supported_surfaces"] == nil ||
		rows[0].(map[string]any)["supported_endpoints"] == nil {
		t.Fatalf("Google method metadata lost: %+v", rows)
	}
	readiness := payload["readiness"].(map[string]any)
	if readiness["catalog_synced"] != true || readiness["model_verified"] != false || calls.Load() != 1 {
		t.Fatalf("catalog methods became entitlement: %+v calls=%d", readiness, calls.Load())
	}
	service := &config.Principal{PrincipalID: "fixture-service", PrincipalKind: "service", ProjectID: "fixture-project"}
	if err := iam.RecordProviderCheck(iam.ProviderCheck{ProviderID: "diagnostic", Operation: iam.CheckVerify,
		Success: true, Model: "gemini-fixture"}); err != nil {
		t.Fatal(err)
	}
	serviceEvidence := catalogReadiness("diagnostic", service, providers.CatalogReadResult{})
	if serviceEvidence["model_verified"] != false || serviceEvidence["source_scope"] != "service_project" {
		t.Fatalf("gateway verification became service project evidence: %+v", serviceEvidence)
	}
}
