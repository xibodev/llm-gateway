package providers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newVertexTestProvider builds a Vertex provider carrying an OAuth bearer
// token (the credential discovery requires), pointed at an httptest server
// via baseURL exactly as modelURL is exercised elsewhere in this package.
func newVertexTestProvider(t *testing.T, baseURL, location string) GoogleAIProvider {
	t.Helper()
	return NewVertexAIWithAccessToken(baseURL, "test-oauth-token", "test-project", location, 5)
}

// newVertexAPIKeyTestProvider builds a Vertex provider carrying only an API
// key — no OAuth bearer token — the credential shape Google refuses on
// ListPublisherModels.
func newVertexAPIKeyTestProvider(t *testing.T, baseURL, location string) GoogleAIProvider {
	t.Helper()
	return NewVertexAI(baseURL, "test-api-key", "test-project", location, 5)
}

// Discovery must use the v1beta1 COLLECTION route. The v1 collection route is
// not served upstream (it returns a generic HTML 404), and the project-scoped
// path has no list method — the project comes from the token.
func TestVertexDiscoveryUsesV1Beta1CollectionRoute(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"publisherModels": []map[string]any{
				{"name": "publishers/google/models/gemini-2.5-flash",
					"supportedActions": map[string]any{"openGenerationAiStudio": map[string]any{}}},
			},
		})
	}))
	defer server.Close()

	provider := newVertexTestProvider(t, server.URL, "global")
	models, err := provider.vertexPublisherModels("google")
	if err != nil {
		t.Fatalf("discovery failed: %v", err)
	}
	if !strings.Contains(gotPath, "/v1beta1/publishers/google/models") {
		t.Fatalf("requested %q, want the v1beta1 collection route", gotPath)
	}
	if strings.Contains(gotPath, "/projects/") {
		t.Fatalf("requested %q — the project-scoped path has no list method", gotPath)
	}
	if len(models) != 1 || models[0].ID != "gemini-2.5-flash" {
		t.Fatalf("models=%+v, want the bare model id", models)
	}
}

// A custom base_url carries the inference API root (/v1) per modelURL's
// contract, so discovery must replace that version rather than append its own
// to it. Without this the request went to <base>/v1/v1beta1/publishers/... and
// every proxied or custom Vertex instance failed discovery while inference on
// the same configuration worked.
func TestVertexDiscoveryDoesNotDoubleTheVersionSegmentOnACustomBase(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"publisherModels": []map[string]any{}})
	}))
	defer server.Close()

	// The same value modelURL would use for inference: an endpoint whose path
	// already ends in the inference API root.
	provider := newVertexTestProvider(t, server.URL+"/v1", "global")
	if _, err := provider.vertexPublisherModels("google"); err != nil {
		t.Fatalf("discovery failed: %v", err)
	}
	if gotPath != "/v1beta1/publishers/google/models" {
		t.Fatalf("requested %q, want exactly one version segment", gotPath)
	}
	versions := 0
	for _, segment := range strings.Split(strings.Trim(gotPath, "/"), "/") {
		if segment == "v1" || segment == "v1beta1" {
			versions++
		}
	}
	if versions != 1 {
		t.Fatalf("path %q carries %d version segments, want 1", gotPath, versions)
	}
}

// A base_url that does not end in the inference version is the caller's own
// path shape: discovery appends to it rather than stripping a segment it never
// put there.
func TestVertexDiscoveryKeepsABaseWithoutTheInferenceVersion(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"publisherModels": []map[string]any{}})
	}))
	defer server.Close()

	provider := newVertexTestProvider(t, server.URL+"/vertex-proxy", "global")
	if _, err := provider.vertexPublisherModels("google"); err != nil {
		t.Fatalf("discovery failed: %v", err)
	}
	if gotPath != "/vertex-proxy/v1beta1/publishers/google/models" {
		t.Fatalf("requested %q, want the configured prefix preserved", gotPath)
	}
}

// Only managed models belong in the catalog. Self-deploy Garden models require
// the operator to stand up their own endpoint first, and there are ~11,800 of
// them in a single region — including them buries the usable models.
func TestVertexDiscoveryKeepsOnlyManagedModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			// The ids carry a classifiable family so this test measures the
			// managed-model filter alone; an unclassifiable id is dropped by a
			// different rule (see TestVertexDiscoveryOmitsUnclassifiableModels).
			"publisherModels": []map[string]any{
				{"name": "publishers/google/models/gemini-managed-a",
					"supportedActions": map[string]any{"openGenerationAiStudio": map[string]any{}}},
				{"name": "publishers/google/models/gemini-gated-b",
					"supportedActions": map[string]any{"requestAccess": map[string]any{}}},
				{"name": "publishers/hf-someone/models/gemini-deployable-c",
					"supportedActions": map[string]any{"deploy": map[string]any{}, "deployGke": map[string]any{}}},
			},
		})
	}))
	defer server.Close()

	models, err := newVertexTestProvider(t, server.URL, "global").vertexPublisherModels("google")
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, m := range models {
		ids[m.ID] = true
	}
	if !ids["gemini-managed-a"] || !ids["gemini-gated-b"] {
		t.Fatalf("managed models missing: %+v", ids)
	}
	if ids["gemini-deployable-c"] {
		t.Fatalf("self-deploy Garden model must not enter the catalog: %+v", ids)
	}
}

// A model whose capability cannot be classified is omitted, not listed with an
// empty capability map. Listing it bare does not make it unroutable: the
// console's capabilitiesFor reads a row that declares no modality and no
// surface as a chat model, and every catalog row is offered as a route member —
// so an unknown family was selectable and 404ed on generateContent, which is
// the exact failure the classifier exists to prevent.
func TestVertexDiscoveryOmitsUnclassifiableModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"publisherModels": []map[string]any{
				{"name": "publishers/google/models/gemini-2.5-flash",
					"supportedActions": map[string]any{"openGenerationAiStudio": map[string]any{}}},
				{"name": "publishers/google/models/unknown-family-001",
					"supportedActions": map[string]any{"openGenerationAiStudio": map[string]any{}}},
			},
		})
	}))
	defer server.Close()

	models, err := newVertexTestProvider(t, server.URL, "global").vertexPublisherModels("google")
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range models {
		if model.ID == "unknown-family-001" {
			t.Fatalf("unclassifiable model entered the catalog: %+v", model)
		}
		if len(model.Capabilities) == 0 {
			t.Fatalf("%s listed with no capability at all: %+v", model.ID, model)
		}
	}
	if len(models) != 1 || models[0].ID != "gemini-2.5-flash" {
		t.Fatalf("models = %+v, want only the classifiable entry", models)
	}
}

// A Vertex instance holding only an API key cannot discover — Google refuses
// key auth on ListPublisherModels by design. It must report the catalog as
// undiscoverable rather than inventing one.
func TestVertexWithAPIKeyReportsCatalogUndiscoverable(t *testing.T) {
	provider := newVertexAPIKeyTestProvider(t, "https://example.invalid", "global")
	_, _, err := provider.catalog()
	if err == nil {
		t.Fatal("expected an undiscoverable catalog error")
	}
	if !strings.Contains(err.Error(), "not discoverable") {
		t.Fatalf("error %q does not report the catalog as undiscoverable", err)
	}
}

// The hardcoded list is gone: three of its nine ids exist in no region at all,
// and three more exist only in us-central1, so a global instance advertised six
// models that fail at call time. The property that keeps it gone is that the
// catalog is exactly what the upstream listed — so an upstream that lists
// nothing must yield nothing, with no floor of ids the gateway supplies itself.
func TestVertexCatalogIsOnlyWhatUpstreamListed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"publisherModels": []map[string]any{}})
	}))
	defer server.Close()

	provider := newVertexTestProvider(t, server.URL, "global")
	models, err := provider.vertexPublisherModels("google")
	if err != nil {
		t.Fatalf("discovery failed: %v", err)
	}
	if len(models) != 0 {
		t.Fatalf("empty upstream listing produced %d models: %+v", len(models), models)
	}
	// Same assertion through the public entry point, since that is what the
	// catalog cache stores and a reintroduced fallback would most likely sit
	// there rather than in the raw pagination loop.
	models, _, err = provider.ListModelsWithError()
	if err != nil {
		t.Fatalf("ListModelsWithError failed: %v", err)
	}
	if len(models) != 0 {
		t.Fatalf("ListModelsWithError supplied %d models upstream never listed: %+v", len(models), models)
	}
}

// ListPublisherModels carries no capability field. A naive id heuristic that
// defaults anything non-video/non-image to chat mis-tags embedding models —
// they pass the managed-model filter legitimately (openGenerationAiStudio),
// match neither "veo" nor "image", and would 404 upstream if routed to
// generateContent via /v1/chat/completions. This must not happen.
func TestVertexDiscoveryDoesNotAdvertiseEmbeddingModelsAsChat(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"publisherModels": []map[string]any{
				{"name": "publishers/google/models/text-embedding-005",
					"supportedActions": map[string]any{"openGenerationAiStudio": map[string]any{}}},
				{"name": "publishers/google/models/gemini-embedding-001",
					"supportedActions": map[string]any{"openGenerationAiStudio": map[string]any{}}},
			},
		})
	}))
	defer server.Close()

	models, err := newVertexTestProvider(t, server.URL, "global").vertexPublisherModels("google")
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 {
		t.Fatalf("models = %+v, want both embedding models still listed", models)
	}
	for _, model := range models {
		if _, tagged := model.Capabilities["chat"]; tagged {
			t.Fatalf("%s tagged chat-callable: %+v", model.ID, model)
		}
		for _, endpoint := range model.SupportedSurfaces {
			if endpoint == "/v1/chat/completions" || endpoint == "/v1/messages" {
				t.Fatalf("%s advertised on the chat surface: %+v", model.ID, model)
			}
		}
		if _, tagged := model.Capabilities["embedding"]; !tagged {
			t.Fatalf("%s not tagged embedding: %+v", model.ID, model)
		}
	}
}

// Discovery must page while nextPageToken is non-empty, and thread the token
// returned by one page into the next request rather than looping forever or
// stopping after the first page.
//
// The token deliberately carries '+' and '/': it is opaque, and appending it
// to the query unescaped survives only a URL-safe token. A '+' decodes back
// as a space, so the second page would be requested with a token upstream
// never issued.
func TestVertexDiscoveryFollowsPagination(t *testing.T) {
	const nextToken = "second+page/token=="
	var pageTokensSeen []string
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pageTokensSeen = append(pageTokensSeen, r.URL.Query().Get("pageToken"))
		requests++
		w.Header().Set("Content-Type", "application/json")
		if requests == 1 {
			json.NewEncoder(w).Encode(map[string]any{
				"publisherModels": []map[string]any{
					{"name": "publishers/google/models/gemini-page-one",
						"supportedActions": map[string]any{"openGenerationAiStudio": map[string]any{}}},
				},
				"nextPageToken": nextToken,
			})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"publisherModels": []map[string]any{
				{"name": "publishers/google/models/gemini-page-two",
					"supportedActions": map[string]any{"openGenerationAiStudio": map[string]any{}}},
			},
		})
	}))
	defer server.Close()

	models, err := newVertexTestProvider(t, server.URL, "global").vertexPublisherModels("google")
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2 (one per page)", requests)
	}
	if len(pageTokensSeen) != 2 || pageTokensSeen[0] != "" || pageTokensSeen[1] != nextToken {
		t.Fatalf("pageTokensSeen = %+v, want [\"\", %q]", pageTokensSeen, nextToken)
	}
	ids := map[string]bool{}
	for _, m := range models {
		ids[m.ID] = true
	}
	if !ids["gemini-page-one"] || !ids["gemini-page-two"] {
		t.Fatalf("models = %+v, want entries collected from both pages", ids)
	}
}
