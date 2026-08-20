package providers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"llmgw/internal/config"
)

// newAzureTestProvider builds a provider pointed at an httptest server via
// baseURL, carrying a synthetic api-key exactly as azureopenai.go expects it
// (the base_url the resource's "Endpoint" value already carries /openai/v1).
func newAzureTestProvider(t *testing.T, baseURL string) AzureOpenAIProvider {
	t.Helper()
	return AzureOpenAIProvider{BaseURL: baseURL, APIKey: "test-api-key", Timeout: 5}
}

// azureChatForTest issues a minimal chat completion against the deployment
// name given as model, matching how Complete is invoked elsewhere.
func azureChatForTest(t *testing.T, provider AzureOpenAIProvider, model string) (map[string]any, error) {
	t.Helper()
	return provider.Complete(model, []Message{{"role": "user", "content": "hi"}}, nil)
}

// The catalog comes from deployments. /models returns Azure's entire vendor
// catalogue — 410 entries against 1 real deployment on the probed resource —
// and every entry reports status "succeeded", so nothing in that response can
// be filtered to recover the truth.
func TestAzureCatalogUsesDeploymentsNeverModels(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/deployments") {
			json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{{"id": "gpt-5.6-sol", "status": "succeeded"}},
			})
			return
		}
		// If the provider ever calls this, the test must fail loudly.
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"id": "vendor-catalogue-entry", "status": "succeeded"}},
		})
	}))
	defer server.Close()

	models, _, err := newAzureTestProvider(t, server.URL+"/openai/v1").catalog()
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	for _, path := range paths {
		if strings.HasSuffix(path, "/models") {
			t.Fatalf("provider requested %q — /models is the vendor catalogue, never the catalog source", path)
		}
	}
	if len(models) != 1 || models[0].ID != "gpt-5.6-sol" {
		t.Fatalf("models=%+v, want the single deployment", models)
	}
}

// When deployments cannot be listed the catalog is undiscoverable. Falling back
// to /models would silently report hundreds of models the resource cannot serve.
func TestAzureNeverFallsBackToModelsOnDeploymentFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/deployments") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		t.Errorf("provider fell back to %q after a deployments failure", r.URL.Path)
	}))
	defer server.Close()

	models, _, err := newAzureTestProvider(t, server.URL+"/openai/v1").catalog()
	if err == nil {
		t.Fatalf("expected an error, got models=%+v", models)
	}
	if len(models) != 0 {
		t.Fatalf("models must be empty when undiscoverable, got %+v", models)
	}
}

// A 401 or 403 from the deployments route means the credential was rejected,
// not that the catalog cannot be discovered by anyone. The distinction is
// load-bearing beyond the wording: account health only reacts to the codes in
// api.catalogFailureAffectsAccount, which lists the authentication code and
// deliberately does not list the discovery one, so a rejection reported as
// undiscoverable is an auth failure nothing ever attributes to the credential.
func TestAzureRejectedCredentialReportsAnAuthenticationFailure(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		}))
		_, _, err := newAzureTestProvider(t, server.URL+"/openai/v1").catalog()
		server.Close()
		if err == nil {
			t.Fatalf("HTTP %d: expected a catalog error", status)
		}
		code, _, gotStatus := CatalogFailure(err)
		if code != "catalog_authentication_failed" {
			t.Fatalf("HTTP %d reported %q, want catalog_authentication_failed", status, code)
		}
		if gotStatus != status {
			t.Fatalf("HTTP %d reported status %d, want the upstream status", status, gotStatus)
		}
	}
}

// The other side of the same boundary: a route that is missing or broken is
// genuinely undiscoverable, and must keep saying so.
func TestAzureNonAuthDeploymentsFailureStaysUndiscoverable(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusInternalServerError} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		}))
		_, _, err := newAzureTestProvider(t, server.URL+"/openai/v1").catalog()
		server.Close()
		if err == nil {
			t.Fatalf("HTTP %d: expected a catalog error", status)
		}
		if code, _, _ := CatalogFailure(err); code != "catalog_not_discoverable" {
			t.Fatalf("HTTP %d reported %q, want catalog_not_discoverable", status, code)
		}
	}
}

// An Azure OpenAI resource holds embedding, image and audio deployments next
// to chat ones, and all of them report status "succeeded". This provider
// implements /chat/completions only, so listing the rest advertises models it
// cannot serve — and a row that declares no capability is read as a chat model
// downstream, so they must not merely be left unannotated.
func TestAzureCatalogListsOnlyChatCallableDeployments(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Shape as observed: "id" is the operator-chosen deployment name and
		// "model" is the base model it was created from.
		json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data": []map[string]any{
				{"id": "chat-main", "model": "gpt-4o", "status": "succeeded", "object": "deployment"},
				{"id": "vectors", "model": "text-embedding-3-large", "status": "succeeded", "object": "deployment"},
				{"id": "pictures", "model": "dall-e-3", "status": "succeeded", "object": "deployment"},
				{"id": "listen", "model": "whisper", "status": "succeeded", "object": "deployment"},
				{"id": "speak", "model": "gpt-4o-mini-tts", "status": "succeeded", "object": "deployment"},
				{"id": "legacy", "model": "gpt-35-turbo-instruct", "status": "succeeded", "object": "deployment"},
			},
		})
	}))
	defer server.Close()

	models, _, err := newAzureTestProvider(t, server.URL+"/openai/v1").catalog()
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	if len(models) != 1 || models[0].ID != "chat-main" {
		t.Fatalf("models=%+v, want only the chat deployment", models)
	}
	// The id stays the deployment name; the base model is only a label.
	if models[0].Label != "gpt-4o" {
		t.Fatalf("label=%q, want the base model as the label", models[0].Label)
	}
	if chat, _ := models[0].Capabilities["chat"].(bool); !chat {
		t.Fatalf("chat deployment does not declare the chat capability: %+v", models[0])
	}
	if len(models[0].SupportedSurfaces) == 0 {
		t.Fatalf("chat deployment declares no surface: %+v", models[0])
	}
}

// A deployment whose base model this provider does not recognise is excluded
// rather than listed without metadata: an unannotated row is presented as a
// chat model, so silence is not available as a way to say "unknown". Nothing
// is lost at call time — `<provider>/<deployment>` resolves without the
// catalog.
func TestAzureCatalogExcludesUnrecognisedDeployments(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": "mystery", "model": "some-unreleased-family-1", "status": "succeeded"},
			},
		})
	}))
	defer server.Close()

	models, _, err := newAzureTestProvider(t, server.URL+"/openai/v1").catalog()
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	if len(models) != 0 {
		t.Fatalf("models=%+v, want an unrecognised deployment excluded", models)
	}
}

// The endpoint the Azure portal shows is a bare origin. The catalog derived
// its deployments route from scheme+host and so listed deployments correctly
// from that value, while inference appended "/chat/completions" to it and hit
// a route Azure does not serve — setup succeeded, the catalog populated, and
// every completion returned 404. base_url must therefore carry the inference
// suffix by the time a provider is built.
func TestAzureBaseURLNormalisesAPortalOriginToTheInferenceEndpoint(t *testing.T) {
	for _, configured := range []string{
		"https://resource.endpoint.invalid",
		"https://resource.endpoint.invalid/",
		"https://resource.endpoint.invalid/openai",
		"https://resource.endpoint.invalid/openai/v1",
		"https://resource.endpoint.invalid/openai/v1/",
	} {
		got, err := azureInferenceBaseURL(configured)
		if err != nil {
			t.Fatalf("azureInferenceBaseURL(%q) failed: %v", configured, err)
		}
		if got != "https://resource.endpoint.invalid/openai/v1" {
			t.Fatalf("azureInferenceBaseURL(%q) = %q, want the /openai/v1 endpoint", configured, got)
		}
	}
}

// A path that is a different endpoint rather than a missing suffix is
// rejected at config time, not silently repaired into something else.
func TestAzureBaseURLRejectsANonResourceEndpoint(t *testing.T) {
	for _, configured := range []string{
		"",
		"resource.endpoint.invalid/openai/v1",
		"https://resource.endpoint.invalid/openai/deployments/some-deployment",
		"https://resource.endpoint.invalid/openai/v1?api-version=2024-01-01",
	} {
		if got, err := azureInferenceBaseURL(configured); err == nil {
			t.Fatalf("azureInferenceBaseURL(%q) = %q, want an error", configured, got)
		}
	}
}

// A scheme this gateway cannot speak is rejected where every other URL is
// held to http(s) (validateRegistryURL). Accepted, it cleared setup and then
// failed every catalog and completion request with an unsupported-protocol
// error that points at nothing the operator can act on.
func TestAzureBaseURLRejectsANonHTTPScheme(t *testing.T) {
	for _, configured := range []string{
		"ftp://resource.endpoint.invalid",
		"ftp://resource.endpoint.invalid/openai/v1",
		"file://resource.endpoint.invalid/openai/v1",
		"ws://resource.endpoint.invalid/openai/v1",
	} {
		if got, err := azureInferenceBaseURL(configured); err == nil {
			t.Fatalf("azureInferenceBaseURL(%q) = %q, want an error", configured, got)
		}
	}
}

// And the rejection must land at configuration time, on the provider row,
// rather than only when a request is made through it.
func TestAzureNonHTTPSchemeIsAConfigurationIssue(t *testing.T) {
	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{
			"azure-fixture": {Type: "azure_openai", RegistryID: "azure_openai",
				BaseURL: "ftp://resource.endpoint.invalid/openai/v1", APIKey: "test-api-key"},
		}
	})
	ResetProviders()
	t.Cleanup(ResetProviders)

	if issue := ProviderConfigurationIssue("azure-fixture"); issue == "" {
		t.Fatal("a non-http(s) base_url reported no configuration issue")
	}
	if _, err := GetProvider("azure-fixture"); err == nil {
		t.Fatal("a non-http(s) base_url built a provider instead of failing setup")
	}
}

// The same normalisation must reach the built provider, since that is the
// value Complete appends "/chat/completions" to.
func TestAzureCompletionsPostToTheOpenAIV1Route(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": "chatcmpl-1", "choices": []any{}})
	}))
	defer server.Close()

	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{
			// The portal value: an origin with no path at all.
			"azure-fixture": {Type: "azure_openai", RegistryID: "azure_openai",
				BaseURL: server.URL, APIKey: "test-api-key"},
		}
	})
	ResetProviders()
	t.Cleanup(ResetProviders)

	provider, err := GetProvider("azure-fixture")
	if err != nil {
		t.Fatalf("instantiate: %v", err)
	}
	if _, err := provider.Complete("gpt-fixture", []Message{{"role": "user", "content": "hi"}}, nil); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if gotPath != "/openai/v1/chat/completions" {
		t.Fatalf("completions posted to %q, want \"/openai/v1/chat/completions\"", gotPath)
	}
}

// Auth is the api-key header, not Authorization: Bearer. This is the main
// divergence from every other OpenAI-compatible provider.
func TestAzureAuthenticatesWithApiKeyHeader(t *testing.T) {
	var sawAPIKey, sawBearer bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAPIKey = r.Header.Get("api-key") != ""
		sawBearer = strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{}})
	}))
	defer server.Close()

	_, _, _ = newAzureTestProvider(t, server.URL+"/openai/v1").catalog()
	if !sawAPIKey {
		t.Fatal("request carried no api-key header")
	}
	if sawBearer {
		t.Fatal("request used Authorization: Bearer; Azure expects api-key")
	}
}

// The response model id is version-stamped and is NOT a valid request model id.
// Anything that echoes it back into routing or catalog state silently mismatches.
func TestAzureDoesNotRoundTripTheResponseModelID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-1", "object": "chat.completion",
			"model":                 "gpt-5.6-sol-2026-07-09",
			"prompt_filter_results": []any{}, "service_tier": "default",
			"choices": []map[string]any{{"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "hi"}}},
			"usage": map[string]any{"total_tokens": 2,
				"latency_checkpoint": map[string]any{"ttft_ms": 1}},
		})
	}))
	defer server.Close()

	provider := newAzureTestProvider(t, server.URL+"/openai/v1")
	out, err := azureChatForTest(t, provider, "gpt-5.6-sol")
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	// Unknown Azure-only fields must not break decoding.
	if out == nil {
		t.Fatal("no response decoded")
	}
	if got, _ := out["model"].(string); got == "gpt-5.6-sol" {
		t.Fatal("provider rewrote the response model to the request id; report what upstream said")
	}
}

// Pagination on this route is defensive, not exercised: a real resource,
// queried directly at the pinned api-version, returned {"data": [...],
// "object": "list"} with no pagination field of any kind (see the
// ListModelsWithError doc comment for the full measurement). If a future
// api-version sets has_more, the cursor convention the OpenAI list envelope's
// own family uses is followed — never an OData nextLink, which this route has
// never been observed to return.
func TestAzureCatalogFollowsHasMoreCursorPagination(t *testing.T) {
	var requestedQueries []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedQueries = append(requestedQueries, r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("after") == "gpt-page-one" {
			json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{{"id": "gpt-page-two", "status": "succeeded", "model": "gpt-4o"}},
			})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"data":     []map[string]any{{"id": "gpt-page-one", "status": "succeeded", "model": "gpt-4o"}},
			"has_more": true,
		})
	}))
	defer server.Close()

	models, _, err := newAzureTestProvider(t, server.URL+"/openai/v1").catalog()
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	if len(requestedQueries) != 2 {
		t.Fatalf("requestedQueries = %+v, want one request per page", requestedQueries)
	}
	ids := map[string]bool{}
	for _, model := range models {
		ids[model.ID] = true
	}
	if !ids["gpt-page-one"] || !ids["gpt-page-two"] {
		t.Fatalf("models = %+v, want deployments from both pages", ids)
	}
}

// A prior review's premise for this handling was that the route pages with
// nextLink. Tested directly against a real resource, it never does — so even
// if a response happens to carry that field, the provider must not act on
// it.
func TestAzureCatalogIgnoresANextLinkField(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"data":     []map[string]any{{"id": "gpt-page-one", "status": "succeeded", "model": "gpt-4o"}},
			"nextLink": r.URL.String() + "&page=2",
		})
	}))
	defer server.Close()

	models, _, err := newAzureTestProvider(t, server.URL+"/openai/v1").catalog()
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	if requests != 1 {
		t.Fatalf("provider made %d request(s); a nextLink field must not be followed", requests)
	}
	if len(models) != 1 || models[0].ID != "gpt-page-one" {
		t.Fatalf("models=%+v, want only the one page's deployment", models)
	}
}

// has_more:true with no usable cursor cannot be followed safely. Returning
// what was gathered so far would be the same silent truncation this handling
// exists to avoid, so it must fail the listing instead.
func TestAzureCatalogFailsWhenHasMoreHasNoCursor(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"data":     []map[string]any{},
			"has_more": true,
		})
	}))
	defer server.Close()

	models, _, err := newAzureTestProvider(t, server.URL+"/openai/v1").catalog()
	if err == nil {
		t.Fatalf("catalog succeeded with models=%+v; has_more with no cursor must fail the listing", models)
	}
	if code, _, _ := CatalogFailure(err); code != "catalog_not_discoverable" {
		t.Fatalf("catalog failure code = %q, want \"catalog_not_discoverable\"", code)
	}
}

// The cursor only ever travels as a query value on a URL built from the
// already-validated resource origin — it is never parsed as a URL itself —
// so a resource cannot use it to redirect the api-key header to another
// host, even by shaping the cursor to look like one.
func TestAzureCatalogCursorCannotRedirectToAnotherHost(t *testing.T) {
	foreignRequests := 0
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignRequests++
		w.WriteHeader(http.StatusOK)
	}))
	defer foreign.Close()

	resource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("after") != "" {
			json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{{"id": "gpt-page-two", "status": "succeeded", "model": "gpt-4o"}},
			})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"id": "gpt-page-one", "status": "succeeded", "model": "gpt-4o"}},
			// A cursor value shaped like an attempt to smuggle a foreign URL.
			"last_id":  foreign.URL + "/openai/deployments?api-version=" + azureDeploymentsAPIVersion,
			"has_more": true,
		})
	}))
	defer resource.Close()

	models, _, err := newAzureTestProvider(t, resource.URL+"/openai/v1").catalog()
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	if foreignRequests != 0 {
		t.Fatalf("provider made %d request(s) to a host outside the configured resource", foreignRequests)
	}
	ids := map[string]bool{}
	for _, model := range models {
		ids[model.ID] = true
	}
	if !ids["gpt-page-one"] || !ids["gpt-page-two"] {
		t.Fatalf("models = %+v, want deployments from both pages", ids)
	}
}

// A proxy-prefixed path is rejected at configuration time. It reads like a
// valid endpoint — the suffix is right there — but catalog discovery derives
// its route from scheme+host alone (azureResourceOrigin) and requests
// /openai/deployments from the host root, which a proxy mounting the API under
// a prefix does not serve. Accepting it meant inference worked through the
// prefix while catalog sync silently went somewhere else.
func TestAzureBaseURLRejectsAProxyPrefixedPath(t *testing.T) {
	for _, configured := range []string{
		"https://proxy.endpoint.invalid/proxy/openai/v1",
		"https://proxy.endpoint.invalid/proxy/openai",
		"https://proxy.endpoint.invalid/azure/resource/openai/v1/",
	} {
		if got, err := azureInferenceBaseURL(configured); err == nil {
			t.Fatalf("azureInferenceBaseURL(%q) = %q, want an error", configured, got)
		}
	}
}
