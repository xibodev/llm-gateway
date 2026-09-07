package providers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCatalogPayloadValidationAndCache(t *testing.T) {
	for _, provider := range []struct {
		name, field, path, row, identity, variant string
		new                                       func(string) Provider
	}{
		{"anthropic", "data", "/v1/models", `{"id":"claude-fixture","display_name":"Fixture"}`, "id", `{"id":"claude-fixture","future":{"nested":[null,17]}}`,
			func(base string) Provider {
				return AnthropicNativeProvider{BaseURL: base, APIKey: "fixture-key", Timeout: 2}
			}},
		{"ollama", "models", "/api/tags", `{"model":"fixture-model","details":{"family":"fixture"}}`, "model", `{"name":"fixture-model:latest","future":[17]}`,
			func(base string) Provider { return OllamaProvider{BaseURL: base, Timeout: 2} }},
		{"studio-v1", "models", "/v1/models", `{"name":"models/gemini-fixture","supportedGenerationMethods":["generateContent"]}`, "name", `{"name":"models/gemini-fixture","supportedGenerationMethods":["futureMethod","generateContent"],"future":{"nested":true}}`,
			func(base string) Provider { return NewAIStudio(base+"/v1", "fixture-key", 2) }},
		{"studio-v1beta", "models", "/v1beta/models", `{"name":"models/gemini-fixture","displayName":"Fixture","supportedGenerationMethods":["generateContent"],"inputTokenLimit":1024}`, "name", `{"name":"models/gemini-fixture","supportedGenerationMethods":["generateContent"],"version":"future","future":false}`,
			func(base string) Provider { return NewAIStudio(base+"/v1beta", "fixture-key", 2) }},
		{"vertex", "publisherModels", "/v1beta1/publishers/google/models", `{"name":"publishers/google/models/gemini-fixture","supportedActions":{"openGenerationAiStudio":{}}}`, "name", `{"name":"publishers/google/models/gemini-fixture","versionId":"future","supportedActions":{"requestAccess":{},"futureAction":{"value":17}},"future":[]}`,
			func(base string) Provider {
				return NewVertexAIWithAccessToken(base+"/v1", "fixture-token", "fixture-project", "global", 2)
			}},
		{"openai", "data", "/models", `{"id":"fixture-model","owned_by":"fixture"}`, "id", `{"name":"fixture-model","future":{"nested":[null,17]}}`,
			func(base string) Provider { return OpenAIProvider{auth: catalogFixtureAuth{base: base}, Timeout: 2} }},
	} {
		t.Run(provider.name, func(t *testing.T) {
			for _, tc := range []struct {
				name, body, code string
				status, count    int
			}{
				{"empty", fmt.Sprintf(`{%q:[]}`, provider.field), "", 200, 0},
				{"populated", fmt.Sprintf(`{%q:[%s]}`, provider.field, provider.row), "", 200, 1},
				{"variant", fmt.Sprintf(`{%q:[%s]}`, provider.field, provider.variant), "", 200, 1},
				{"valid-mixed-variants", fmt.Sprintf(`{%q:[%s,%s]}`, provider.field, provider.row, provider.variant), "", 200, 2},
				{"null-row", fmt.Sprintf(`{%q:[null]}`, provider.field), "catalog_invalid_shape", 200, 0},
				{"number-row", fmt.Sprintf(`{%q:[17]}`, provider.field), "catalog_invalid_shape", 200, 0},
				{"string-row", fmt.Sprintf(`{%q:["fixture-secret"]}`, provider.field), "catalog_invalid_shape", 200, 0},
				{"bool-row", fmt.Sprintf(`{%q:[false]}`, provider.field), "catalog_invalid_shape", 200, 0},
				{"array-row", fmt.Sprintf(`{%q:[[]]}`, provider.field), "catalog_invalid_shape", 200, 0},
				{"missing-identity", fmt.Sprintf(`{%q:[{"future":"fixture-secret"}]}`, provider.field), "catalog_invalid_shape", 200, 0},
				{"null-identity", fmt.Sprintf(`{%q:[{%q:null}]}`, provider.field, provider.identity), "catalog_invalid_shape", 200, 0},
				{"bool-identity", fmt.Sprintf(`{%q:[{%q:false}]}`, provider.field, provider.identity), "catalog_invalid_shape", 200, 0},
				{"number-identity", fmt.Sprintf(`{%q:[{%q:17}]}`, provider.field, provider.identity), "catalog_invalid_shape", 200, 0},
				{"object-identity", fmt.Sprintf(`{%q:[{%q:{}}]}`, provider.field, provider.identity), "catalog_invalid_shape", 200, 0},
				{"array-identity", fmt.Sprintf(`{%q:[{%q:[]}]}`, provider.field, provider.identity), "catalog_invalid_shape", 200, 0},
				{"empty-identity", fmt.Sprintf(`{%q:[{%q:""}]}`, provider.field, provider.identity), "catalog_invalid_shape", 200, 0},
				{"blank-identity", fmt.Sprintf(`{%q:[{%q:"  "}]}`, provider.field, provider.identity), "catalog_invalid_shape", 200, 0},
				{"mixed-valid-first", fmt.Sprintf(`{%q:[%s,null]}`, provider.field, provider.row), "catalog_invalid_shape", 200, 0},
				{"mixed-invalid-first", fmt.Sprintf(`{%q:[{%q:false},%s]}`, provider.field, provider.identity, provider.row), "catalog_invalid_shape", 200, 0},
				{"reproducer", fmt.Sprintf(`{%q:[null,17,"invalid",{"id":false}]}`, provider.field), "catalog_invalid_shape", 200, 0},
				{"malformed", `{"fixture-private":"fixture-secret",`, "catalog_invalid_json", 200, 0},
				{"trailing-json", fmt.Sprintf(`{%q:[]} {"extra":true}`, provider.field), "catalog_invalid_json", 200, 0},
				{"partial-decode", fmt.Sprintf(`{%q:[],"extra":`, provider.field), "catalog_invalid_json", 200, 0},
				{"missing", `{}`, "catalog_invalid_shape", 200, 0},
				{"null-body", `null`, "catalog_invalid_shape", 200, 0},
				{"array-body", `["fixture-secret"]`, "catalog_invalid_shape", 200, 0},
				{"string-body", `"fixture-secret"`, "catalog_invalid_shape", 200, 0},
				{"number-body", `17`, "catalog_invalid_shape", 200, 0},
				{"true-body", `true`, "catalog_invalid_shape", 200, 0},
				{"false-body", `false`, "catalog_invalid_shape", 200, 0},
				{"wrong-field", `{"unrelated":[]}`, "catalog_invalid_shape", 200, 0},
				{"null-array", fmt.Sprintf(`{%q:null}`, provider.field), "catalog_invalid_shape", 200, 0},
				{"object-array", fmt.Sprintf(`{%q:{}}`, provider.field), "catalog_invalid_shape", 200, 0},
				{"string-array", fmt.Sprintf(`{%q:"fixture-secret"}`, provider.field), "catalog_invalid_shape", 200, 0},
				{"unauthorized", `{"error":"fixture-secret"}`, "catalog_authentication_failed", 401, 0},
				{"forbidden", `{"error":"fixture-secret"}`, "catalog_authentication_failed", 403, 0},
				{"limited", `{"error":"fixture-secret"}`, "catalog_http_error", 429, 0},
				{"unavailable", `fixture-secret`, "catalog_http_error", 503, 0},
			} {
				t.Run(tc.name, func(t *testing.T) {
					if provider.name == "openai" && (tc.status == 401 || tc.status == 403) {
						tc.code = "catalog_http_error"
					}
					var calls atomic.Int32
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls.Add(1)
						if r.Method != http.MethodGet || r.URL.Path != provider.path {
							t.Errorf("unexpected discovery request: %s %s", r.Method, r.URL.Path)
						}
						w.WriteHeader(tc.status)
						_, _ = w.Write([]byte(tc.body))
					}))
					defer upstream.Close()
					setupCatalogReadTest(t, upstream.URL)
					p := provider.new(upstream.URL)
					cacheMu.Lock()
					cache["catalog-read"] = p
					cacheMu.Unlock()
					stale := time.Now().Add(-2 * catalogTTL)
					catMu.Lock()
					catData["catalog-read"] = catalogEntry{SchemaVersion: catalogSchemaVersion,
						Models: []ModelInfo{{ID: "old"}}, RefreshedAt: stale}
					catMu.Unlock()
					result := ReadCatalogForPrincipal("catalog-read", nil)
					cached, refreshed := CatalogCached("catalog-read")
					if tc.code != "" {
						code, detail, status := CatalogFailure(result.Err)
						if result.Err == nil || code != tc.code || status != tc.status ||
							result.Diagnostics.Status != "error" || !result.Diagnostics.Stale || !result.Diagnostics.FromCache ||
							len(result.Models) != 1 || result.Models[0].ID != "old" || !result.RefreshedAt.Equal(stale) ||
							len(cached) != 1 || cached[0].ID != "old" || !refreshed.Equal(stale) {
							t.Fatalf("failure lost diagnostics or stale cache: %+v, cache=%+v at %v", result, cached, refreshed)
						}
						if strings.HasSuffix(tc.name, "-body") && detail != "Provider catalog response was not a JSON object." {
							t.Fatalf("non-object response has misleading detail: %q", detail)
						}
						if tc.code == "catalog_invalid_json" && detail != "Provider catalog response was not valid JSON." {
							t.Fatalf("malformed JSON has misleading detail: %q", detail)
						}
						raw, _ := json.Marshal(result.Diagnostics)
						for _, text := range []string{detail, result.Err.Error(), string(raw)} {
							if strings.Contains(text, "fixture-secret") || strings.Contains(text, "fixture-key") || strings.Contains(text, "fixture-token") {
								t.Fatal("catalog error disclosed a fixture credential")
							}
						}
						return
					}
					wantStatus := "synced"
					if tc.count == 0 {
						wantStatus = "empty"
					}
					if result.Err != nil || result.Models == nil || len(result.Models) != tc.count ||
						result.Diagnostics.Status != wantStatus || result.Diagnostics.Stale || result.Diagnostics.FromCache ||
						!result.RefreshedAt.After(stale) || len(cached) != tc.count || !refreshed.Equal(result.RefreshedAt) ||
						(tc.count > 0 && cached[0].ID == "old") {
						t.Fatalf("success did not replace stale cache: %+v, cache=%+v", result, cached)
					}
					second := ReadCatalogForPrincipal("catalog-read", nil)
					if second.Err != nil || !second.Diagnostics.FromCache || second.Diagnostics.Status != wantStatus ||
						len(second.Models) != tc.count || calls.Load() != 1 {
						t.Fatalf("success was not cached: %+v, calls=%d", second, calls.Load())
					}
					if models := p.ListModels(); models == nil || len(models) != tc.count {
						t.Fatalf("legacy ListModels lost successful result: %+v", models)
					}
				})
			}
		})
	}
}

func TestCatalogIdentityVariantsAndFiltering(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		valid      bool
		count      int
	}{
		{"ollama-name", `{"models":[{"name":"fixture:latest"}]}`, true, 1},
		{"ollama-both", `{"models":[{"name":"fixture:latest","model":"fixture:latest","future":null}]}`, true, 1},
		{"ollama-empty-name", `{"models":[{"name":"","model":"fixture:latest"}]}`, true, 1},
		{"ollama-wrong-name", `{"models":[{"name":false,"model":"fixture:latest"}]}`, false, 0},
		{"ollama-wrong-model", `{"models":[{"name":"fixture:latest","model":17}]}`, false, 0},
		{"openai-empty-id", `{"data":[{"id":"","name":"fixture"}]}`, true, 1},
		{"openai-both", `{"data":[{"id":"fixture","name":"Fixture","future":{"nested":null}}]}`, true, 1},
		{"openai-wrong-id", `{"data":[{"id":false,"name":"fixture"}]}`, false, 0},
		{"openai-wrong-name", `{"data":[{"name":17}]}`, false, 0},
		{"studio-prefix-only", `{"models":[{"name":"models/"}]}`, false, 0},
		{"studio-blank-suffix", `{"models":[{"name":"models/  "}]}`, false, 0},
		{"studio-minimal", `{"models":[{"name":"models/future"}]}`, true, 0},
		{"studio-unknown-method", `{"models":[{"name":"models/future","supportedGenerationMethods":["futureMethod"]}]}`, true, 0},
		{"studio-empty-methods", `{"models":[{"name":"models/future","supportedGenerationMethods":[]}]}`, true, 0},
		{"studio-null-methods", `{"models":[{"name":"models/fixture","supportedGenerationMethods":null}]}`, false, 0},
		{"studio-wrong-methods", `{"models":[{"name":"models/fixture","supportedGenerationMethods":"generateContent"}]}`, false, 0},
		{"studio-mixed-methods", `{"models":[{"name":"models/fixture","supportedGenerationMethods":["generateContent",17]}]}`, false, 0},
		{"studio-null-method", `{"models":[{"name":"models/fixture","supportedGenerationMethods":[null]}]}`, false, 0},
		{"studio-blank-method", `{"models":[{"name":"models/fixture","supportedGenerationMethods":[""]}]}`, false, 0},
		{"vertex-prefix-only", `{"publisherModels":[{"name":"publishers/google/models/"}]}`, false, 0},
		{"vertex-minimal", `{"publisherModels":[{"name":"publishers/google/models/future"}]}`, true, 0},
		{"vertex-unknown-action", `{"publisherModels":[{"name":"publishers/google/models/gemini-fixture","supportedActions":{"futureAction":{}}}]}`, true, 0},
		{"vertex-deploy-only", `{"publisherModels":[{"name":"publishers/google/models/gemini-fixture","supportedActions":{"deploy":{}}}]}`, true, 0},
		{"vertex-null-actions", `{"publisherModels":[{"name":"publishers/google/models/gemini-fixture","supportedActions":null}]}`, false, 0},
		{"vertex-wrong-actions", `{"publisherModels":[{"name":"publishers/google/models/gemini-fixture","supportedActions":[]}]}`, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			var p Provider
			switch strings.Split(tc.name, "-")[0] {
			case "ollama":
				p = OllamaProvider{BaseURL: server.URL, Timeout: 2}
			case "openai":
				p = OpenAIProvider{auth: catalogFixtureAuth{base: server.URL}, Timeout: 2}
			case "studio":
				p = NewAIStudio(server.URL+"/v1beta", "fixture-key", 2)
			case "vertex":
				p = NewVertexAIWithAccessToken(server.URL+"/v1", "fixture-token", "fixture-project", "global", 2)
			}
			models, _, err := listModelsWithError(p)
			if tc.valid {
				if err != nil || models == nil || len(models) != tc.count {
					t.Fatalf("valid variant rejected: models=%+v err=%v", models, err)
				}
			} else if code, _, status := CatalogFailure(err); models != nil || code != "catalog_invalid_shape" || status != 200 {
				t.Fatalf("invalid variant accepted: models=%+v err=%v", models, err)
			}
		})
	}
}

func TestGoogleCatalogPagesRetainStaleCacheOnInvalidPayload(t *testing.T) {
	for _, version := range []string{"v1", "v1beta", "vertex"} {
		t.Run(version, func(t *testing.T) {
			field, row := "models", `{"name":"models/gemini-fixture","supportedGenerationMethods":["generateContent"]}`
			if version == "vertex" {
				field, row = "publisherModels", `{"name":"publishers/google/models/gemini-fixture","supportedActions":{"requestAccess":{}}}`
			}
			for _, tc := range []struct {
				name, rows, token string
				valid             bool
				count             int
			}{
				{"empty-page", "", `""`, true, 1},
				{"valid-page", row, `""`, true, 2},
				{"null-row", "null", `""`, false, 0},
				{"scalar-row", "17", `""`, false, 0},
				{"missing-name", `{}`, `""`, false, 0},
				{"wrong-name", `{"name":false}`, `""`, false, 0},
				{"mixed-page", row + ",null", `""`, false, 0},
				{"wrong-token", row, `17`, false, 0},
				{"null-token", row, `null`, false, 0},
				{"repeated-token", row, `"next +/&="`, false, 0},
			} {
				t.Run(tc.name, func(t *testing.T) {
					var calls atomic.Int32
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						call := calls.Add(1)
						if call == 1 {
							_, _ = fmt.Fprintf(w, `{%q:[%s],"nextPageToken":"next +/&="}`, field, row)
							return
						}
						if call > 2 {
							t.Error("pagination did not terminate")
							http.Error(w, "fixture", 500)
							return
						}
						if r.URL.Query().Get("pageToken") != "next +/&=" {
							t.Errorf("page token not preserved: %s", r.URL.RawQuery)
						}
						_, _ = fmt.Fprintf(w, `{%q:[%s],"nextPageToken":%s}`, field, tc.rows, tc.token)
					}))
					defer server.Close()
					setupCatalogReadTest(t, server.URL)
					p := NewAIStudio(server.URL+"/"+version, "fixture-key", 2)
					if version == "vertex" {
						p = NewVertexAIWithAccessToken(server.URL+"/v1", "fixture-token", "fixture-project", "global", 2)
					}
					cacheMu.Lock()
					cache["catalog-read"] = p
					cacheMu.Unlock()
					stale := time.Now().Add(-2 * catalogTTL)
					catMu.Lock()
					catData["catalog-read"] = catalogEntry{SchemaVersion: catalogSchemaVersion, Models: []ModelInfo{{ID: "old"}}, RefreshedAt: stale}
					catMu.Unlock()
					result := ReadCatalogForPrincipal("catalog-read", nil)
					cached, refreshed := CatalogCached("catalog-read")
					if calls.Load() != 2 {
						t.Fatalf("pages fetched=%d, want 2", calls.Load())
					}
					if tc.valid {
						if result.Err != nil || len(result.Models) != tc.count || len(cached) != tc.count || !refreshed.After(stale) {
							t.Fatalf("valid pages rejected: %+v, cache=%+v", result, cached)
						}
					} else if code, _, status := CatalogFailure(result.Err); code != "catalog_invalid_shape" || status != 200 ||
						!result.Diagnostics.Stale || !result.Diagnostics.FromCache || result.Diagnostics.Status != "error" ||
						len(result.Models) != 1 || result.Models[0].ID != "old" || !result.RefreshedAt.Equal(stale) ||
						len(cached) != 1 || cached[0].ID != "old" || !refreshed.Equal(stale) {
						t.Fatalf("invalid later page replaced stale cache: %+v, cache=%+v", result, cached)
					}
				})
			}
		})
	}
}

func TestNativeCatalogInvalidURLReturnsSafeError(t *testing.T) {
	for _, p := range []Provider{
		AnthropicNativeProvider{BaseURL: "://fixture-secret"},
		OllamaProvider{BaseURL: "://fixture-secret"},
	} {
		models, _, err := listModelsWithError(p)
		code, detail, status := CatalogFailure(err)
		if models != nil || err == nil || code != "catalog_transport_error" || status != 0 || strings.Contains(detail, "fixture-secret") {
			t.Fatalf("%T: models=%+v error=%v status=%d", p, models, err, status)
		}
	}
}

func TestVertexCatalogRejectsMalformedLaterPage(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Query().Get("pageToken") == "next" {
			_, _ = w.Write([]byte(`{"models":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"publisherModels":[{"name":"publishers/google/models/gemini-fixture","supportedActions":{"requestAccess":{}}}],"nextPageToken":"next"}`))
	}))
	defer upstream.Close()
	p := newVertexTestProvider(t, upstream.URL+"/v1", "global")
	models, _, err := p.ListModelsWithError()
	code, _, status := CatalogFailure(err)
	if models != nil || code != "catalog_invalid_shape" || status != 200 || calls.Load() != 2 {
		t.Fatalf("partial catalog accepted: models=%+v error=%v calls=%d", models, err, calls.Load())
	}
}

func TestVertexCatalogActionValuesAndCache(t *testing.T) {
	for _, action := range []string{"requestAccess", "openGenerationAiStudio", "deploy", "deployGke", "multiDeployVertex"} {
		for _, value := range []string{`false`, `17`, `"fixture-secret"`, `null`, `[]`, `{}`, `{"future":{"nested":[null,17]}}`} {
			t.Run(action+"/"+value, func(t *testing.T) {
				valid := strings.HasPrefix(value, "{")
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Query().Get("pageToken") == "next" {
						// Unknown extensions are not capability evidence and may evolve
						// independently of the native action messages we consume.
						_, _ = fmt.Fprintf(w, `{"publisherModels":[{"name":"publishers/google/models/gemini-fixture","supportedActions":{%q:%s,"futureAction":false}}]}`, action, value)
						return
					}
					_, _ = fmt.Fprint(w, `{"publisherModels":[{"name":"publishers/google/models/gemini-first","supportedActions":{"requestAccess":{}}}],"nextPageToken":"next"}`)
				}))
				defer server.Close()
				setupCatalogReadTest(t, server.URL)
				cacheMu.Lock()
				cache["catalog-read"] = NewVertexAIWithAccessToken(server.URL+"/v1", "fixture-token", "fixture-project", "global", 2)
				cacheMu.Unlock()
				stale := time.Now().Add(-2 * catalogTTL)
				catMu.Lock()
				catData["catalog-read"] = catalogEntry{SchemaVersion: catalogSchemaVersion, Models: []ModelInfo{{ID: "old"}}, RefreshedAt: stale}
				catMu.Unlock()
				result := ReadCatalogForPrincipal("catalog-read", nil)
				cached, refreshed := CatalogCached("catalog-read")
				if valid {
					count := 1
					if action == "requestAccess" || action == "openGenerationAiStudio" {
						count = 2
					}
					if result.Err != nil || len(result.Models) != count || len(cached) != count || !refreshed.After(stale) {
						t.Fatalf("legitimate action rejected: %+v cache=%+v", result, cached)
					}
				} else if code, detail, status := CatalogFailure(result.Err); code != "catalog_invalid_shape" || status != 200 ||
					strings.Contains(detail, "fixture-secret") || !result.Diagnostics.Stale || !result.Diagnostics.FromCache ||
					len(result.Models) != 1 || result.Models[0].ID != "old" || !result.RefreshedAt.Equal(stale) ||
					len(cached) != 1 || cached[0].ID != "old" || !refreshed.Equal(stale) {
					t.Fatalf("malformed action replaced stale cache: %+v cache=%+v", result, cached)
				}
			})
		}
	}
}
