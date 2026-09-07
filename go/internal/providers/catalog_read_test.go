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
	catMu.Lock()
	catData["catalog-read"] = catalogEntry{SchemaVersion: catalogSchemaVersion,
		Models: []ModelInfo{{ID: "old"}}, RefreshedAt: time.Now().Add(-2 * catalogTTL)}
	catMu.Unlock()
	for i := 0; i < 2; i++ {
		result := ReadCatalogForPrincipal("catalog-read", nil)
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
	owner := &config.Principal{PrincipalID: "fixture-owner", PrincipalKind: "human"}
	other := &config.Principal{PrincipalID: "fixture-other", PrincipalKind: "human"}
	refreshed := time.Now().Add(-2 * catalogTTL)
	catMu.Lock()
	catData[catalogCacheKey("catalog-read", owner)] = catalogEntry{
		SchemaVersion: catalogSchemaVersion, Models: []ModelInfo{{ID: "cached-model"}}, RefreshedAt: refreshed,
	}
	catMu.Unlock()
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
	for _, private := range []string{"fixture-secret", "fixture-token", owner.PrincipalID, other.PrincipalID} {
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
	first := &config.Principal{PrincipalID: "service", PrincipalKind: "service", ProjectID: "first"}
	second := &config.Principal{PrincipalID: "service", PrincipalKind: "service", ProjectID: "second"}
	for _, principal := range []*config.Principal{first, second, first} {
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
	catMu.Lock()
	catData["catalog-read"] = catalogEntry{SchemaVersion: catalogSchemaVersion,
		Models: []ModelInfo{{ID: "old"}}, RefreshedAt: time.Now().Add(-2 * catalogTTL)}
	catMu.Unlock()
	done := make(chan CatalogReadResult, 1)
	go func() { done <- ReadCatalogForPrincipal("catalog-read", nil) }()
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
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"data":[{"id":"fetched-model"}]}`))
			}))
			defer upstream.Close()
			setupCatalogReadTest(t, upstream.URL)
			principal := &config.Principal{PrincipalID: "fixture-owner", PrincipalKind: "human"}
			key := catalogCacheKey("catalog-read", principal)
			current := catalogEntry{SchemaVersion: catalogSchemaVersion,
				Models: []ModelInfo{{ID: "replacement-model"}}}
			if scenario == "empty" {
				current.Models = []ModelInfo{}
			}
			result := readCatalogForPrincipal("catalog-read", principal, func(providerID string, caller *config.Principal) ([]ModelInfo, *iam.ProviderAccountObservation, error) {
				models, observation, err := RefreshCatalogForPrincipalWithError(providerID, caller)
				if err != nil || len(models) != 1 || models[0].ID != "fetched-model" {
					t.Fatalf("refresh: %+v %v", models, err)
				}
				// Interleave a write or invalidation after the successful store,
				// before the read takes its final snapshot, without scheduler timing.
				if scenario == "invalidated" {
					generation := catalogGenerationFor(key)
					ForgetCatalogForPrincipal(providerID, caller.PrincipalID)
					if storeEntryIfGeneration(key, models, generation) {
						t.Fatal("invalidation accepted a stale write")
					}
					storeEntry(providerID, []ModelInfo{{ID: "other-scope-model"}})
				} else {
					catMu.Lock()
					current.RefreshedAt = catData[key].RefreshedAt.Add(time.Second)
					catData[key] = current
					catMu.Unlock()
				}
				return models, observation, err
			})
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
			if result.Err != nil || !result.RefreshedAt.Equal(current.RefreshedAt) || len(result.Models) != len(current.Models) {
				t.Fatalf("snapshot: %+v, want %+v", result, current)
			}
			if scenario == "empty" {
				if result.Diagnostics.Status != "empty" {
					t.Fatalf("empty snapshot: %+v", result)
				}
			} else if result.Diagnostics.Status != "synced" || result.Models[0].ID != current.Models[0].ID {
				t.Fatalf("mixed snapshot: %+v", result)
			}
		})
	}
}

type catalogReadErrorProvider struct{ Provider }

func (catalogReadErrorProvider) ListModelsWithError() ([]ModelInfo, *iam.ProviderAccountObservation, error) {
	return nil, nil, &CatalogError{Code: "catalog_http_error", Status: 502,
		Detail: `Authorization: Bearer fixture-private-token api_key=fixture-private-key`}
}

func TestCatalogReadUsesDiagnosticSanitizer(t *testing.T) {
	setupCatalogReadTest(t, "https://example.invalid")
	cacheMu.Lock()
	cache["catalog-read"] = catalogReadErrorProvider{}
	cacheMu.Unlock()
	result := ReadCatalogForPrincipal("catalog-read", nil)
	if result.Err == nil || result.Diagnostics.FailureCode != "catalog_http_error" {
		t.Fatalf("result: %+v", result)
	}
	if strings.Contains(result.Diagnostics.Detail, "fixture-private") {
		t.Fatalf("unsanitized detail: %s", result.Diagnostics.Detail)
	}
}
