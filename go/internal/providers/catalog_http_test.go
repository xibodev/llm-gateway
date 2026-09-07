package providers

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Generate padding indefinitely so the decoder, not the fixture, must stop reading.
type catalogPaddingReader struct {
	read   int
	closed bool
}

func (r *catalogPaddingReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = ' '
	}
	r.read += len(p)
	return len(p), nil
}

func (r *catalogPaddingReader) Close() error {
	r.closed = true
	return nil
}

func TestDecodeCatalogResponseBoundsReadAndPreservesStatus(t *testing.T) {
	for _, status := range []int{200, 401, 403, 429, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			body := &catalogPaddingReader{}
			models, err := decodeCatalogResponse(&http.Response{StatusCode: status, Body: body}, "data", "id")
			code, detail, upstream := CatalogFailure(err)
			wantCode, wantRead := "catalog_http_error", 0
			if status == 200 {
				wantCode, wantRead = "catalog_not_discoverable", catalogMaxResponseBytes+1
			} else if status == 401 || status == 403 {
				wantCode = "catalog_authentication_failed"
			}
			var catalogErr *CatalogError
			if models != nil || !errors.As(err, &catalogErr) || code != wantCode || upstream != status ||
				body.read != wantRead || !body.closed || detail == "" {
				t.Fatalf("response not safely bounded: models=%v err=%v read=%d closed=%v", models, err, body.read, body.closed)
			}
		})
	}
}

func TestCatalogResponseSizeBoundaryAndCache(t *testing.T) {
	for _, provider := range []struct {
		name, body string
		new        func(string) Provider
	}{
		{"anthropic", `{"data":[{"id":"claude-fixture"}],"private":"fixture-secret"}`, func(base string) Provider {
			return AnthropicNativeProvider{BaseURL: base, APIKey: "fixture-key", Timeout: 5}
		}},
		{"openai", `{"data":[{"id":"fixture-model"}],"private":"fixture-secret"}`, func(base string) Provider {
			return OpenAIProvider{auth: catalogFixtureAuth{base: base}, Timeout: 5}
		}},
		{"ollama", `{"models":[{"name":"fixture-model"}],"private":"fixture-secret"}`, func(base string) Provider {
			return OllamaProvider{BaseURL: base, Timeout: 5}
		}},
	} {
		for _, oversized := range []bool{false, true} {
			name := "exact-boundary"
			if oversized {
				name = "oversized"
			}
			t.Run(provider.name+"/"+name, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					_, _ = io.WriteString(w, provider.body)
					size := catalogMaxResponseBytes - len(provider.body)
					if oversized {
						size++
					}
					_, _ = io.CopyN(w, &catalogPaddingReader{}, int64(size))
				}))
				defer server.Close()
				setupCatalogReadTest(t, server.URL)
				cacheMu.Lock()
				cache["catalog-read"] = provider.new(server.URL)
				cacheMu.Unlock()
				stale := time.Now().Add(-2 * catalogTTL)
				catMu.Lock()
				catData["catalog-read"] = catalogEntry{SchemaVersion: catalogSchemaVersion, Models: []ModelInfo{{ID: "old"}}, RefreshedAt: stale}
				catMu.Unlock()
				result := ReadCatalogForPrincipal("catalog-read", nil)
				cached, refreshed := CatalogCached("catalog-read")
				if oversized {
					code, detail, status := CatalogFailure(result.Err)
					if code != "catalog_not_discoverable" || status != 200 || detail != "Provider catalog response exceeded the size limit." ||
						result.Diagnostics.Status != "error" || !result.Diagnostics.Stale || !result.Diagnostics.FromCache ||
						len(result.Models) != 1 || result.Models[0].ID != "old" || !result.RefreshedAt.Equal(stale) ||
						len(cached) != 1 || cached[0].ID != "old" || !refreshed.Equal(stale) {
						t.Fatalf("oversized response replaced stale cache: %+v cache=%+v", result, cached)
					}
					if strings.Contains(result.Err.Error(), "fixture-secret") {
						t.Fatal("size failure disclosed response data")
					}
				} else if result.Err != nil || len(result.Models) != 1 || len(cached) != 1 || cached[0].ID == "old" ||
					!refreshed.After(stale) || !result.RefreshedAt.Equal(refreshed) || result.Diagnostics.Stale {
					t.Fatalf("exact-boundary catalog rejected: %+v cache=%+v", result, cached)
				}
			})
		}
	}
}
