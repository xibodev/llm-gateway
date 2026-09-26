package providers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"

	core "github.com/xibodev/llmgw-core"
)

func setupCatalogReadTest(t *testing.T, url string) {
	t.Helper()
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	old := *config.Get()
	config.Update(func(s *config.Settings) {
		*s = *config.Defaults()
		s.Providers = map[string]*config.ProviderConfig{
			"catalog-read": {Type: "openai_compatible", BaseURL: url, APIKey: "fake-key"},
		}
	})
	ResetProviders()
	ForgetCatalog("catalog-read")
	t.Cleanup(func() {
		ForgetCatalog("catalog-read")
		ResetProviders()
		iam.ResetForTests()
		config.Update(func(s *config.Settings) { *s = old })
	})
}

func TestCatalogReadEmptyReplacesStaleRowsAndCachesSuccess(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer upstream.Close()
	setupCatalogReadTest(t, upstream.URL)
	putCatalogEntry("catalog-read", catalogEntry{SchemaVersion: catalogSchemaVersion,
		Models: []ModelInfo{{ID: "old"}}, RefreshedAt: time.Now().Add(-2 * catalogTTL)})
	for i := 0; i < 2; i++ {
		result := ReadCatalogForPrincipal("catalog-read", gatewayCaller())
		if result.Err != nil || len(result.Models) != 0 || result.RefreshedAt.IsZero() ||
			result.Diagnostics.Status != "empty" || result.Diagnostics.Stale || result.Diagnostics.FromCache != (i == 1) {
			t.Fatalf("read %d: %+v", i, result)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("empty catalog fetched %d times", calls.Load())
	}
}

func TestCatalogReadStaleFailureIsScopedAndRedacted(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `api_key=fixture-secret Authorization: Bearer fixture-token`, http.StatusForbidden)
	}))
	defer upstream.Close()
	setupCatalogReadTest(t, upstream.URL)
	owner := core.Caller{ID: "fixture-owner", Kind: core.CallerHuman}
	other := core.Caller{ID: "fixture-other", Kind: core.CallerHuman}
	refreshed := time.Now().Add(-2 * catalogTTL)
	putCatalogEntry(catalogCacheKey("catalog-read", owner), catalogEntry{
		SchemaVersion: catalogSchemaVersion, Models: []ModelInfo{{ID: "cached-model"}}, RefreshedAt: refreshed,
	})
	result := ReadCatalogForPrincipal("catalog-read", owner)
	if result.Err == nil || len(result.Models) != 1 || !result.RefreshedAt.Equal(refreshed) ||
		!result.Diagnostics.Stale || !result.Diagnostics.FromCache || result.Diagnostics.UpstreamStatus != 403 {
		t.Fatalf("stale failure: %+v", result)
	}
	second := ReadCatalogForPrincipal("catalog-read", other)
	if second.Err == nil || len(second.Models) != 0 || second.Diagnostics.FromCache || !second.RefreshedAt.IsZero() {
		t.Fatalf("other scope borrowed cache: %+v", second)
	}
	raw, _ := json.Marshal(result.Diagnostics)
	for _, private := range []string{"fixture-secret", "fixture-token", owner.ID, other.ID} {
		if strings.Contains(string(raw), private) {
			t.Fatalf("diagnostics disclosed %q", private)
		}
	}
}

func TestCatalogReadServiceProjectCacheBoundary(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer upstream.Close()
	setupCatalogReadTest(t, upstream.URL)
	first := core.Caller{ID: "service", Kind: core.CallerService, ProjectID: "first"}
	second := core.Caller{ID: "service", Kind: core.CallerService, ProjectID: "second"}
	for _, principal := range []core.Caller{first, second, first} {
		result := ReadCatalogForPrincipal("catalog-read", principal)
		if result.Err != nil || result.Diagnostics.SourceScope != "service_project" {
			t.Fatalf("result: %+v", result)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("service/project cache fetches = %d, want 2", calls.Load())
	}
}

func TestCatalogReadFailedRefreshCannotRestoreInvalidatedRows(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		http.Error(w, "fixture failure", http.StatusBadGateway)
	}))
	defer upstream.Close()
	setupCatalogReadTest(t, upstream.URL)
	putCatalogEntry("catalog-read", catalogEntry{SchemaVersion: catalogSchemaVersion,
		Models: []ModelInfo{{ID: "old"}}, RefreshedAt: time.Now().Add(-2 * catalogTTL)})
	done := make(chan CatalogReadResult, 1)
	go func() { done <- ReadCatalogForPrincipal("catalog-read", gatewayCaller()) }()
	<-started
	ForgetCatalog("catalog-read")
	close(release)
	result := <-done
	if result.Err == nil || len(result.Models) != 0 || result.Diagnostics.FromCache || !result.RefreshedAt.IsZero() {
		t.Fatalf("invalidation restored stale evidence: %+v", result)
	}
}

func TestCatalogReadSuccessfulRefreshUsesAuthoritativeSnapshot(t *testing.T) {
	for _, scenario := range []string{"replaced", "empty", "invalidated"} {
		t.Run(scenario, func(t *testing.T) {
			principal := core.Caller{ID: "fixture-owner", Kind: core.CallerHuman}
			replacement := []ModelInfo{{ID: "replacement-model"}}
			if scenario == "empty" {
				replacement = []ModelInfo{}
			}
			var key string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Interleave a write or an invalidation with the discovery,
				// before the read takes its final snapshot, without scheduler
				// timing.
				if scenario == "invalidated" {
					ForgetCatalogForPrincipal("catalog-read", principal.ID)
					Current().catalogs.store("catalog-read", []ModelInfo{{ID: "other-scope-model"}})
				} else {
					Current().catalogs.store(key, replacement)
				}
				_, _ = w.Write([]byte(`{"data":[{"id":"fetched-model"}]}`))
			}))
			defer upstream.Close()
			setupCatalogReadTest(t, upstream.URL)
			key = catalogCacheKey("catalog-read", principal)
			result := ReadCatalogForPrincipal("catalog-read", principal)
			if result.Diagnostics.SourceScope != "principal" || result.Diagnostics.FromCache || result.Diagnostics.Stale {
				t.Fatalf("diagnostics: %+v", result)
			}
			if scenario == "invalidated" {
				if result.Err == nil || result.Diagnostics.Status != "error" || result.Diagnostics.FailureCode != "catalog_state_changed" ||
					len(result.Models) != 0 || !result.RefreshedAt.IsZero() {
					t.Fatalf("invalidation restored removed rows or borrowed another scope: %+v", result)
				}
				return
			}
			current, _ := Current().catalogs.entry(key)
			if result.Err != nil || !result.RefreshedAt.Equal(current.RefreshedAt) || len(result.Models) != len(replacement) {
				t.Fatalf("snapshot: %+v, want %+v", result, current)
			}
			if scenario == "empty" {
				if result.Diagnostics.Status != "empty" {
					t.Fatalf("empty snapshot: %+v", result)
				}
			} else if result.Diagnostics.Status != "synced" || result.Models[0].ID != replacement[0].ID {
				t.Fatalf("mixed snapshot: %+v", result)
			}
		})
	}
}

type catalogReadErrorProvider struct{ Provider }

func (catalogReadErrorProvider) ListModelsWithError() ([]ModelInfo, *CredentialObservation, error) {
	return nil, nil, &CatalogError{Code: "catalog_http_error", Status: 502,
		Detail: `Authorization: Bearer fixture-private-token api_key=fixture-private-key`}
}

func TestCatalogReadUsesDiagnosticSanitizer(t *testing.T) {
	setupCatalogReadTest(t, "https://example.invalid")
	putProvider("catalog-read", catalogReadErrorProvider{})
	result := ReadCatalogForPrincipal("catalog-read", gatewayCaller())
	if result.Err == nil || result.Diagnostics.FailureCode != "catalog_http_error" {
		t.Fatalf("result: %+v", result)
	}
	if strings.Contains(result.Diagnostics.Detail, "fixture-private") {
		t.Fatalf("unsanitized detail: %s", result.Diagnostics.Detail)
	}
}

func TestCachedCatalogReadNeverContactsUpstream(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"data":[{"id":"network-model"}]}`))
	}))
	defer upstream.Close()
	setupCatalogReadTest(t, upstream.URL)

	result := ReadCachedCatalogForPrincipal("catalog-read", gatewayCaller())
	if result.Err != nil || len(result.Models) != 0 || result.Diagnostics.Status != "not_synced" || !result.Diagnostics.FromCache {
		t.Fatalf("empty cached read: %+v", result)
	}
	if calls.Load() != 0 {
		t.Fatalf("cached read contacted upstream %d times", calls.Load())
	}

	if _, _, err := RefreshCatalogForPrincipalWithError("catalog-read", gatewayCaller()); err != nil {
		t.Fatal(err)
	}
	result = ReadCachedCatalogForPrincipal("catalog-read", gatewayCaller())
	if len(result.Models) != 1 || result.Models[0].ID != "network-model" || calls.Load() != 1 {
		t.Fatalf("synced cached read: calls=%d result=%+v", calls.Load(), result)
	}
}
