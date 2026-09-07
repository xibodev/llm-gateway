package providers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGoogleCatalogPaginationLimits(t *testing.T) {
	for _, surface := range []struct {
		name, basePath, path, field, publisher, pageSize string
	}{
		{"studio-v1", "/v1", "/v1/models", "models", "", "1000"},
		{"studio-v1beta", "/v1beta", "/v1beta/models", "models", "", "1000"},
		{"vertex-google", "/v1", "/v1beta1/publishers/google/models", "publisherModels", "google", "200"},
		{"vertex-publisher-proxy", "/proxy/v1", "/proxy/v1beta1/publishers/fixture/models", "publisherModels", "fixture", "200"},
	} {
		t.Run(surface.name, func(t *testing.T) {
			for _, tc := range []struct {
				name                          string
				pages, rows, lastRows, count  int
				endless, duplicates, filtered bool
				bodySize                      int
				failure                       string
			}{
				{name: "unique-token-loop", pages: googleCatalogMaxPages, rows: 1, lastRows: 1, endless: true, failure: "page"},
				{name: "empty-unique-token-loop", pages: googleCatalogMaxPages, endless: true, failure: "page"},
				{name: "complete-at-page-limit", pages: googleCatalogMaxPages, rows: 1, lastRows: 1, count: googleCatalogMaxPages},
				{name: "complete-at-model-limit", pages: 2, rows: googleCatalogMaxModels / 2, lastRows: googleCatalogMaxModels / 2, count: googleCatalogMaxModels},
				{name: "accumulated-model-overflow", pages: 3, rows: googleCatalogMaxModels / 2, lastRows: 1, failure: "model"},
				{name: "duplicate-model-overflow", pages: 3, rows: googleCatalogMaxModels / 2, lastRows: 1, duplicates: true, failure: "model"},
				{name: "filtered-model-overflow", pages: 3, rows: googleCatalogMaxModels / 2, lastRows: 1, filtered: true, failure: "model"},
				{name: "single-page-model-overflow", pages: 1, lastRows: googleCatalogMaxModels + 1, failure: "model"},
				{name: "complete-with-duplicates", pages: 2, rows: 1, lastRows: 1, duplicates: true, count: 2},
				{name: "complete-at-body-limit", pages: 1, lastRows: 1, bodySize: catalogMaxResponseBytes, count: 1},
				{name: "oversized-later-body", pages: 2, rows: 1, lastRows: 1, bodySize: catalogMaxResponseBytes + 1, failure: "size"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					var calls atomic.Int32
					token := func(page int) string { return fmt.Sprintf("fixture-private-token +/&=%d", page) }
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						page := int(calls.Add(1))
						if r.URL.Path != surface.path || page > tc.pages {
							t.Errorf("unexpected request: path=%s page=%d", r.URL.Path, page)
							http.Error(w, "fixture-private-body", http.StatusInternalServerError)
							return
						}
						wantToken := ""
						if page > 1 {
							wantToken = token(page - 1)
						}
						if r.URL.Query().Get("pageToken") != wantToken || r.URL.Query().Get("pageSize") != surface.pageSize {
							t.Error("pagination query changed")
						}
						if surface.publisher == "" {
							if r.Header.Get("x-goog-api-key") != "fixture-private-key" || r.Header.Get("Authorization") != "" {
								t.Error("AI Studio authentication changed")
							}
						} else if r.Header.Get("Authorization") != "Bearer fixture-private-bearer" || r.Header.Get("x-goog-api-key") != "" {
							t.Error("Vertex authentication changed")
						}
						n := tc.rows
						if page == tc.pages {
							n = tc.lastRows
						}
						rows := make([]map[string]any, n)
						for i := range rows {
							id := fmt.Sprintf("gemini-fixture-%d-%d", page, i)
							if tc.duplicates {
								id = "gemini-fixture-duplicate"
							}
							row := map[string]any{"name": "models/" + id}
							if surface.publisher == "" {
								if !tc.filtered {
									row["supportedGenerationMethods"] = []string{"generateContent"}
								}
							} else {
								row["name"] = "publishers/" + surface.publisher + "/models/" + id
								if !tc.filtered {
									row["supportedActions"] = map[string]any{"requestAccess": map[string]any{}}
								}
							}
							rows[i] = row
						}
						body := map[string]any{surface.field: rows, "private": "fixture-private-body"}
						if page < tc.pages || tc.endless {
							body["nextPageToken"] = token(page)
						}
						raw, err := json.Marshal(body)
						if err != nil {
							t.Error(err)
							return
						}
						w.Header().Set("Content-Type", "application/json")
						_, _ = w.Write(raw)
						if page == tc.pages && tc.bodySize > len(raw) {
							_, _ = fmt.Fprint(w, strings.Repeat(" ", tc.bodySize-len(raw)))
						}
					}))
					defer server.Close()
					p := NewAIStudio(server.URL+surface.basePath, "fixture-private-key", 2)
					if surface.publisher != "" {
						p = NewVertexAIWithAccessToken(server.URL+surface.basePath, "fixture-private-bearer", "fixture-project", "global", 2)
					}
					var models []ModelInfo
					var err error
					if surface.publisher == "fixture" {
						models, err = p.vertexPublisherModels(surface.publisher)
					} else {
						got, obs, failure := p.ListModelsWithError()
						models, err = got, failure
						if obs != nil {
							t.Fatal("catalog unexpectedly returned account state")
						}
					}
					if int(calls.Load()) != tc.pages {
						t.Fatalf("requests=%d, want %d", calls.Load(), tc.pages)
					}
					if tc.failure == "" {
						if err != nil || len(models) != tc.count {
							t.Fatalf("complete catalog: models=%d err=%v, want %d", len(models), err, tc.count)
						}
						return
					}
					wantStatus := 0 // Local walk ceilings are not upstream HTTP failures.
					if tc.failure == "size" {
						wantStatus = http.StatusOK
					}
					var catalogErr *CatalogError
					code, detail, status := CatalogFailure(err)
					if models != nil || !errors.As(err, &catalogErr) || code != "catalog_not_discoverable" || status != wantStatus ||
						detail != "Provider catalog "+map[string]string{"page": "listing", "model": "listing", "size": "response"}[tc.failure]+" exceeded the "+tc.failure+" limit." {
						t.Fatalf("limit failure: models=%d error=%v code=%s status=%d", len(models), err, code, status)
					}
					if surface.publisher == "fixture" {
						return
					}
					// Retry through the cache reader: neither partial rows nor a fresh
					// timestamp may replace the last complete snapshot on any limit.
					setupCatalogReadTest(t, server.URL)
					cacheMu.Lock()
					cache["catalog-read"] = p
					cacheMu.Unlock()
					stale := time.Now().Add(-2 * catalogTTL)
					catMu.Lock()
					catData["catalog-read"] = catalogEntry{SchemaVersion: catalogSchemaVersion, Models: []ModelInfo{{ID: "old"}}, RefreshedAt: stale}
					catMu.Unlock()
					calls.Store(0)
					result := ReadCatalogForPrincipal("catalog-read", nil)
					cached, refreshed := CatalogCached("catalog-read")
					if result.Err == nil || result.Diagnostics.Status != "error" || !result.Diagnostics.Stale || !result.Diagnostics.FromCache ||
						result.Diagnostics.FailureCode != code || result.Diagnostics.Detail != detail || result.Diagnostics.UpstreamStatus != wantStatus ||
						len(result.Models) != 1 || result.Models[0].ID != "old" || !result.RefreshedAt.Equal(stale) ||
						len(cached) != 1 || cached[0].ID != "old" || !refreshed.Equal(stale) || int(calls.Load()) != tc.pages {
						t.Fatalf("limit failure replaced complete cache: %+v", result)
					}
					diagnostics, _ := json.Marshal(result.Diagnostics)
					if strings.Contains(string(diagnostics), "fixture-private") || strings.Contains(catalogErr.Detail, "fixture-private") {
						t.Fatal("limit failure disclosed upstream data")
					}
				})
			}
		})
	}
}
