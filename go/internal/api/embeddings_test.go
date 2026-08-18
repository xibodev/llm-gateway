package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
	"llmgw/internal/router"
)

// resetState follows the house idiom from `connections_api_test.go`: the IAM
// store, the savings ledger and the telemetry DB are package-level singletons
// keyed to LLMGW_STATE_DIR, so a test that does not reset them leaves an open
// SQLite handle on its own TempDir and the cleanup fails on Windows.
func resetState(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	router.ResetSavingsState()
	router.ResetTelemetryState()
	providers.ResetProviders()
	t.Cleanup(func() {
		iam.ResetForTests()
		router.ResetSavingsState()
		router.ResetTelemetryState()
		providers.ResetProviders()
	})
}

// The route was ABSENT before this change, and the way it was absent is the
// reason it went unnoticed for so long: `POST /v1/embeddings` fell through to
// Go's default mux handler and returned a plain-text `404 page not found`, while
// `POST /v1/chat/completions` returned a structured JSON error. A caller reading
// only status codes sees "404, model not available" and goes looking for the
// model. So the first thing worth asserting is simply that the route exists and
// answers in the gateway's own error vocabulary.
func TestEmbeddingsRouteIsRegistered(t *testing.T) {
	resetState(t)
	srv := NewServer()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(`{}`))
	srv.ServeHTTP(rec, req)

	if rec.Code == 404 && strings.Contains(rec.Body.String(), "page not found") {
		t.Fatalf("POST /v1/embeddings is not routed: %d %q", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "json") {
		t.Fatalf("expected a structured JSON error, got Content-Type %q body %q",
			ct, rec.Body.String())
	}
}

// The bare `/embeddings` alias matters for the same reason the other eight
// entries in `pathAliases` do: clients configured with a base URL that already
// ends in `/v1` post to `/embeddings`, and without the alias they get the same
// bare 404.
func TestEmbeddingsBarePathIsAliased(t *testing.T) {
	if pathAliases["/embeddings"] != "/v1/embeddings" {
		t.Fatalf("missing /embeddings alias: %v", pathAliases["/embeddings"])
	}
}

func TestEmbeddingsRejectsEmptyInputBeforeCallingUpstream(t *testing.T) {
	for name, body := range map[string]string{
		"absent": `{"model":"p/m"}`,
		"empty":  `{"model":"p/m","input":""}`,
		"blank":  `{"model":"p/m","input":"   "}`,
		"array":  `{"model":"p/m","input":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			var req embeddingsRequest
			if err := json.Unmarshal([]byte(body), &req); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if !inputEmpty(req.Input) {
				t.Fatalf("%s input should be rejected: %#v", name, req.Input)
			}
		})
	}
	var req embeddingsRequest
	_ = json.Unmarshal([]byte(`{"input":["a"]}`), &req)
	if inputEmpty(req.Input) {
		t.Fatal("a non-empty array must not be rejected")
	}
}

// An embeddings response has `prompt_tokens` and `total_tokens` and NO
// `completion_tokens`. The ledger reads prompt tokens into `input_tokens` and
// leaves `output_tokens` at its NOT NULL DEFAULT 0, so no migration is needed --
// but only if the reader actually finds the count.
func TestEmbeddingsPromptTokens(t *testing.T) {
	cases := map[string]struct {
		body string
		want int
	}{
		"openai shape":       {`{"usage":{"prompt_tokens":12,"total_tokens":12}}`, 12},
		"total only":         {`{"usage":{"total_tokens":7}}`, 7},
		"no usage (LocalAI)": {`{"data":[{"embedding":[0.1]}]}`, 0},
		"not json":           {`<html>`, 0},
	}
	for name, tc := range cases {
		if got := embeddingsPromptTokens([]byte(tc.body)); got != tc.want {
			t.Fatalf("%s: got %d want %d", name, got, tc.want)
		}
	}
}

// The catalogue half of the bug: LocalAI reports a bare `{"id": ...}` with no
// capabilities and no supported_endpoints, so before this change
// `localai/granite-embedding-107m-multilingual` appeared in GET /v1/models
// advertising nothing at all and reachable through no endpoint.
func TestEmbeddingModelsAdvertiseTheEndpoint(t *testing.T) {
	resetState(t)
	for _, id := range []string{
		"granite-embedding-107m-multilingual",
		"bge-m3",
		"qwen3-embedding-0.6b",
		"embeddinggemma-300m",
		"text-embedding-3-small",
		"nomic-embed-text-v1.5",
	} {
		caps, endpoints := modelPresentationMetadata("localai", providers.ModelInfo{ID: id})
		if enabled, _ := caps["embedding"].(bool); !enabled {
			t.Fatalf("%s: not marked as an embedding model (caps %v)", id, caps)
		}
		if !containsString(endpoints, "/v1/embeddings") {
			t.Fatalf("%s: does not advertise /v1/embeddings (%v)", id, endpoints)
		}
	}
}

// The inverse, which is the one that would break routing if it regressed: a chat
// model must NOT be classified as an embedding model, or it acquires an endpoint
// that cannot serve it and loses the chat metadata that failover reads.
func TestChatModelsAreNotClassifiedAsEmbeddings(t *testing.T) {
	resetState(t)
	for _, id := range []string{
		"gpt-5.6-luna", "claude-opus-4.8", "gemini-3.5-flash", "whisper-base",
		"voice-pt_BR-cadu-medium",
	} {
		caps, endpoints := modelPresentationMetadata("localai", providers.ModelInfo{ID: id})
		if enabled, _ := caps["embedding"].(bool); enabled {
			t.Fatalf("%s: wrongly marked as an embedding model", id)
		}
		if containsString(endpoints, "/v1/embeddings") {
			t.Fatalf("%s: wrongly advertises /v1/embeddings", id)
		}
	}
}

// A provider that DOES declare the capability keeps its own answer and only
// gains the endpoint -- googleai.go sets `capabilities["embedding"] = true` and
// has, until now, appended no endpoint at all.
func TestDeclaredEmbeddingCapabilityGainsTheEndpoint(t *testing.T) {
	resetState(t)
	caps, endpoints := modelPresentationMetadata("ai_studio", providers.ModelInfo{
		ID: "gemini-embedding-001", Capabilities: map[string]any{"embedding": true},
	})
	if enabled, _ := caps["embedding"].(bool); !enabled {
		t.Fatalf("declared capability lost: %v", caps)
	}
	if !containsString(endpoints, "/v1/embeddings") {
		t.Fatalf("declared embedding model does not advertise the endpoint: %v", endpoints)
	}
}

// End to end through the real handler against a stand-in upstream, asserting the
// three things a proxy must get right: the upstream path, the model rewrite from
// the gateway's namespaced id to the bare upstream id, and byte-passthrough of
// the vector payload.
func TestEmbeddingsProxiesToUpstream(t *testing.T) {
	resetState(t)

	var gotPath string
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(
				`{"object":"list","data":[{"object":"embedding","index":0,` +
					`"embedding":[0.25,0.5]}],"usage":{"prompt_tokens":9,"total_tokens":9}}`))
		}))
	defer upstream.Close()

	old := config.Get().Providers
	t.Cleanup(func() {
		config.Update(func(s *config.Settings) { s.Providers = old })
	})
	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{
			"localai": {Type: "openai_compatible", BaseURL: upstream.URL + "/v1"},
		}
		s.AllowUnauthenticatedAPI = true
	})

	body, _ := json.Marshal(map[string]any{
		"model": "localai/granite-embedding-107m-multilingual",
		"input": "posso ser mandado embora por faltar ao servico",
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/embeddings", bytes.NewReader(body))
	NewServer().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/embeddings" {
		t.Fatalf("upstream path %q, want /v1/embeddings", gotPath)
	}
	if gotBody["model"] != "granite-embedding-107m-multilingual" {
		t.Fatalf("model not rewritten to the bare upstream id: %v", gotBody["model"])
	}
	var out struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response not parseable: %v (%s)", err, rec.Body.String())
	}
	if len(out.Data) != 1 || len(out.Data[0].Embedding) != 2 {
		t.Fatalf("vector did not survive the proxy: %s", rec.Body.String())
	}
}

func containsString(hay []string, needle string) bool {
	for _, v := range hay {
		if v == needle {
			return true
		}
	}
	return false
}
