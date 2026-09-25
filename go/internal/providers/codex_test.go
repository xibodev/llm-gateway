package providers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"

	browseroauth "github.com/xibodev/llm-provider-auth/browseroauth"
	codexauth "github.com/xibodev/llm-provider-auth/codex"
	"github.com/xibodev/llm-provider-auth/tokenstore"
	core "github.com/xibodev/llmgw-core"
)

// codexFixture returns the Codex facade of instance for the gateway caller.
// Its Runtime is not installed, sends Codex calls to endpoints, and resolves
// every caller of instance to one static access token held in core's
// in-memory credential store.
func codexFixture(t *testing.T, instance string, endpoints CodexEndpoints, clientVersion string) CodexProvider {
	t.Helper()
	store := core.NewMemoryCredentialStore()
	if _, err := store.Save(context.Background(), "fixture-connection", tokenstore.Record{AccessToken: "fixture", TokenType: "Bearer"}); err != nil {
		t.Fatal(err)
	}
	store.BindShared(instance, "fixture-connection")
	runtime := newRuntime(func() (core.CredentialStore, error) { return store, nil })
	runtime.codexEndpoints.swap(endpoints)
	provider, err := runtime.newCodexProvider(instance, gatewayCaller(), 5, nil, clientVersion)
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

// configureCodexInstance configures instance as a Codex provider until the
// test ends, as the core Runtime needs to serve inference for it.
func configureCodexInstance(t *testing.T, instance string) {
	t.Helper()
	old := config.Get().Providers
	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{instance: {Type: "openai_compatible", RegistryID: "openai_codex"}}
	})
	t.Cleanup(func() { config.Update(func(s *config.Settings) { s.Providers = old }) })
}

func TestCodexResponsesIsWireNative(t *testing.T) {
	provider := CodexProvider{}
	if !PreservesWireNativeSurface(provider, "gpt-codex", core.ModelSurfaceResponses) {
		t.Fatal("Codex Responses transport was not certified as wire-native")
	}
	if PreservesWireNativeSurface(provider, "gpt-codex", core.ModelSurfaceChatCompletions) {
		t.Fatal("Codex Chat adaptation was certified as wire-native")
	}
}

func TestCodexChatPayloadPassesOnlyProvenOptionalFields(t *testing.T) {
	payload, err := codexChatPayload("gpt-5.6-sol", []Message{{"role": "user", "content": "hello"}}, Kwargs{
		"prompt_cache_key": "fixture-cache",
		"temperature":      0.7,
		"top_p":            0.8,
		"max_tokens":       128,
		"stream_options":   map[string]any{"include_usage": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if payload["model"] != "gpt-5.6-sol" || payload["prompt_cache_key"] != "fixture-cache" {
		t.Fatalf("Codex Chat payload lost exact model/cache key: %+v", payload)
	}
	if payload["stream"] != nil || payload["stream_options"] != nil {
		t.Fatalf("gateway-only streaming fields reached shared Codex transport: %+v", payload)
	}
	for _, unsupported := range []string{"temperature", "top_p", "max_tokens", "max_completion_tokens"} {
		if payload[unsupported] != nil {
			t.Fatalf("gateway passed unproven Codex field %q: %+v", unsupported, payload)
		}
	}
}

func TestCodexCatalogRefreshStoresModelsAfterCredentialRotation(t *testing.T) {
	setupCodexProviderTest(t)
	human, err := iam.CreatePrincipal("human", "authentik:codex-catalog-refresh", "", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	oldClientID := config.Get().OpenAICodexClientID
	oldProviders := config.Get().Providers
	config.Update(func(s *config.Settings) {
		s.OpenAICodexClientID = "fixture-client"
		s.Providers = map[string]*config.ProviderConfig{
			"codex": {Type: "openai_compatible", RegistryID: "openai_codex"},
		}
	})
	t.Cleanup(func() {
		config.Update(func(s *config.Settings) {
			s.OpenAICodexClientID, s.Providers = oldClientID, oldProviders
		})
	})
	if _, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
		PrincipalID: human.ID, ProviderID: "codex", Kind: "openai_codex_oauth",
		AccessToken: "old-access", RefreshToken: "refresh-token",
		ExpiresAt:    time.Now().Add(-time.Minute).Unix(),
		OAuthProfile: codexOAuthProfileDevice, OAuthClientID: "fixture-client",
	}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"new-access","refresh_token":"new-refresh"}`))
		case "/backend-api/codex/models":
			if r.Header.Get("Authorization") != "Bearer new-access" {
				t.Fatalf("model auth=%q", r.Header.Get("Authorization"))
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-codex","supported_in_api":true,"visibility":"list","supported_endpoints":["/responses"]}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	SetCodexEndpointsForTests(t, CodexEndpoints{
		OAuth:     codexauth.Endpoints{OAuthTokenURL: server.URL + "/oauth/token"},
		ModelsURL: server.URL + "/backend-api/codex/models",
	})
	principal := core.Caller{ID: human.ID, Kind: core.CallerHuman}
	models, observation, err := RefreshCatalogForPrincipalWithError("codex", principal)
	if err != nil || len(models) != 1 || observation == nil {
		t.Fatalf("models=%+v observation=%+v err=%v", models, observation, err)
	}
	cached, refreshed := CatalogCachedForPrincipal("codex", principal)
	if len(cached) != 1 || refreshed.IsZero() {
		t.Fatalf("cached=%+v refreshed=%v", cached, refreshed)
	}
}

func TestCodexCatalogRejectsInexactIdentity(t *testing.T) {
	for _, body := range []string{
		`{"data":[{"id":" ","name":" valid-model "}]}`,
		`{"data":[{"id":" valid-model ","name":"other-model"}]}`,
		`{"data":[{"name":" valid-model "}]}`,
	} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			defer server.Close()
			models, _, err := listModelsWithError(catalogFixtureCodex(t, server.URL))
			if err == nil || len(models) != 0 {
				t.Fatalf("Codex accepted inexact identity: models=%+v err=%v", models, err)
			}
		})
	}
}

func TestCodexCatalogEvidenceSeparatesOAuthFromCatalogContents(t *testing.T) {
	for _, scenario := range []struct {
		name, body  string
		status      int
		wantAuth    string
		wantCatalog string
	}{
		{name: "authenticated empty catalog", status: http.StatusOK, body: `{"data":[]}`, wantAuth: "unknown", wantCatalog: "failed"},
		{name: "authenticated catalog", status: http.StatusOK, body: `{"data":[{"id":"gpt-codex","supported_in_api":true,"visibility":"list"}]}`, wantAuth: "accepted", wantCatalog: "discovered"},
		{name: "OAuth rejected", status: http.StatusUnauthorized, body: `{"error":"invalid token"}`, wantAuth: "rejected", wantCatalog: "failed"},
		{name: "catalog unavailable", status: http.StatusServiceUnavailable, body: `{"error":"unavailable"}`, wantAuth: "unknown", wantCatalog: "failed"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer fixture" {
					t.Fatalf("Codex catalog omitted personal OAuth bearer: %v", r.Header)
				}
				w.WriteHeader(scenario.status)
				_, _ = w.Write([]byte(scenario.body))
			}))
			defer server.Close()
			provider := codexFixture(t, "codex", CodexEndpoints{ResponsesBaseURL: server.URL, ModelsURL: server.URL}, codexCatalogClientVersion)
			models, _, err := provider.ListModelsWithError()
			evidence := ClassifyProviderEvidence(err, len(models), false, false)
			if evidence.Authentication != scenario.wantAuth || evidence.Catalog != scenario.wantCatalog || evidence.Completion != "not_probed" {
				t.Fatalf("evidence=%+v err=%v models=%+v", evidence, err, models)
			}
		})
	}
}

func TestCodexRequiresHumanOwnedOAuth(t *testing.T) {
	oldProviders := config.Get().Providers
	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{"codex": {Type: "openai_compatible", RegistryID: "openai_codex"}}
	})
	t.Cleanup(func() { config.Update(func(s *config.Settings) { s.Providers = oldProviders }); ResetProviders() })
	ResetProviders()
	if _, err := GetProvider("codex"); err == nil || !strings.Contains(err.Error(), "human principal private connection is required") {
		t.Fatalf("Codex became anonymous/free: %v", err)
	}
}

func TestCodexProviderUsesResponsesRefreshesOnceAndCatalogsWithClientVersion(t *testing.T) {
	setupCodexProviderTest(t)
	human, err := iam.CreatePrincipal("human", "authentik:codex-owner", "", "Codex Owner")
	if err != nil {
		t.Fatal(err)
	}
	oldClientID := config.Get().OpenAICodexClientID
	oldProviders := config.Get().Providers
	t.Cleanup(func() {
		config.Update(func(s *config.Settings) { s.OpenAICodexClientID, s.Providers = oldClientID, oldProviders })
	})
	config.Update(func(s *config.Settings) {
		s.OpenAICodexClientID = "fixture-client"
		s.Providers = map[string]*config.ProviderConfig{"codex": {Type: "openai_compatible", RegistryID: "openai_codex"}}
	})
	originalExpiresAt := time.Now().Add(time.Hour).Unix()
	if _, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
		PrincipalID: human.ID, ProviderID: "codex", Kind: "openai_codex_oauth", Source: iam.ConnectionSourceUser,
		AccessToken: "old-access", RefreshToken: "refresh-token", IDToken: "old-id", TokenType: "Bearer",
		AccountID: "account-42", AccountLabel: "Fixture workspace", ExpiresAt: originalExpiresAt,
		OAuthProfile: codexOAuthProfileDevice, OAuthClientID: "fixture-client",
	}); err != nil {
		t.Fatal(err)
	}

	responses := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/backend-api/codex/responses":
			responses++
			if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" || r.Header.Get("ChatGPT-Account-ID") != "account-42" {
				t.Fatalf("responses request method=%s content=%q account=%q", r.Method, r.Header.Get("Content-Type"), r.Header.Get("ChatGPT-Account-ID"))
			}
			if responses == 1 {
				if r.Header.Get("Authorization") != "Bearer old-access" {
					t.Fatalf("first auth=%q", r.Header.Get("Authorization"))
				}
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if r.Header.Get("Authorization") != "Bearer new-access" {
				t.Fatalf("retry auth=%q", r.Header.Get("Authorization"))
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"status\":\"in_progress\",\"output\":[]}}\n\n" +
				"data: {\"type\":\"response.output_text.delta\",\"delta\":\"Codex response\"}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"gpt-5-codex\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"Codex response\"}]}],\"usage\":{\"input_tokens\":2,\"output_tokens\":3,\"total_tokens\":5}}}\n\n"))
		case "/oauth/token":
			body := map[string]string{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if r.Header.Get("Content-Type") != "application/json" || body["grant_type"] != "refresh_token" || body["client_id"] != "fixture-client" || body["refresh_token"] != "refresh-token" || len(body) != 3 {
				t.Fatalf("refresh body=%v", body)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"new-access","refresh_token":"new-refresh"}`))
		case "/backend-api/codex/models":
			if r.URL.Query().Get("client_version") != "fixture-catalog-version" {
				t.Fatalf("model query=%s", r.URL.RawQuery)
			}
			if r.Header.Get("Authorization") != "Bearer new-access" || r.Header.Get("ChatGPT-Account-ID") != "account-42" {
				t.Fatalf("model headers auth=%q account=%q", r.Header.Get("Authorization"), r.Header.Get("ChatGPT-Account-ID"))
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5-codex","owned_by":"openai","description":"GPT-5 Codex","supported_in_api":true,"visibility":"list"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	SetCodexEndpointsForTests(t, CodexEndpoints{
		OAuth:            codexauth.Endpoints{OAuthTokenURL: server.URL + "/oauth/token"},
		ResponsesBaseURL: server.URL + "/backend-api/codex",
		ModelsURL:        server.URL + "/backend-api/codex/models",
	})

	instance, err := Current().newCodexProvider(
		"codex", core.Caller{ID: human.ID, Kind: core.CallerHuman}, 120, server.Client(), "fixture-catalog-version",
	)
	if err != nil {
		t.Fatal(err)
	}
	codex := instance
	response, observation, err := codex.CompleteWithObservation(
		"gpt-5-codex", []Message{{"role": "user", "content": "hello"}}, Kwargs{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if responses != 2 || response["model"] != "gpt-5-codex" {
		t.Fatalf("responses=%d response=%+v", responses, response)
	}
	currentObservation, found, err := iam.ActiveProviderAccountObservation(human.ID, "codex")
	if err != nil || !found || observation == nil || *observation != *credentialObservation(&currentObservation) {
		t.Fatalf("observation=%+v current=%+v found=%v err=%v", observation, currentObservation, found, err)
	}
	refreshed, _, ok, err := iam.OAuthProviderConnectionSecret(human.ID, "codex", "")
	if err != nil || !ok || refreshed.AccessToken != "new-access" || refreshed.RefreshToken != "new-refresh" || refreshed.IDToken != "old-id" || refreshed.TokenType != "Bearer" || refreshed.AccountID != "account-42" || refreshed.AccountLabel != "Fixture workspace" || refreshed.ExpiresAt != 0 {
		t.Fatalf("refreshed=%+v ok=%v err=%v", refreshed, ok, err)
	}
	rows := codex.ListModels()
	if len(rows) != 1 || rows[0].ID != "gpt-5-codex" || rows[0].Label != "GPT-5 Codex" ||
		len(rows[0].SupportedSurfaces) != 1 || rows[0].SupportedSurfaces[0] != "/responses" {
		t.Fatalf("Codex models=%+v", rows)
	}
	capabilities := rows[0].TypedCapabilities
	if capabilities == nil || capabilities.Surfaces.Responses != core.SupportSupported ||
		capabilities.Surfaces.ChatCompletions != core.SupportUnsupported ||
		capabilities.Streaming != core.SupportSupported ||
		capabilities.Provenance.Source != core.ModelCapabilitySourceUpstreamReported ||
		capabilities.Provenance.Confidence != core.ModelCapabilityConfidenceHigh {
		t.Fatalf("Codex capability evidence=%+v", capabilities)
	}
}

func TestCodexCatalogRefreshRetryDoesNotGainInferenceBetaHeader(t *testing.T) {
	setupCodexProviderTest(t)
	human, err := iam.CreatePrincipal("human", "authentik:codex-catalog-header", "", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	oldProviders := config.Get().Providers
	t.Cleanup(func() { config.Update(func(s *config.Settings) { s.Providers = oldProviders }) })
	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{"codex": {Type: "openai_compatible", RegistryID: "openai_codex"}}
	})
	if _, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
		PrincipalID: human.ID, ProviderID: "codex", Kind: "openai_codex_oauth",
		AccessToken: "old-access", RefreshToken: "refresh-token", AccountID: "account-42",
		OAuthProfile: codexOAuthProfileDevice, OAuthClientID: "fixture-client",
	}); err != nil {
		t.Fatal(err)
	}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			requests++
			if r.Header.Get("OpenAI-Beta") != "" {
				t.Fatalf("catalog request %d carried beta header %q", requests, r.Header.Get("OpenAI-Beta"))
			}
			if requests == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-5.6-sol","visibility":"list","supported_in_api":true}]}`))
		case "/oauth/token":
			_, _ = w.Write([]byte(`{"access_token":"new-access","refresh_token":"new-refresh"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	SetCodexEndpointsForTests(t, CodexEndpoints{
		OAuth: codexauth.Endpoints{OAuthTokenURL: server.URL + "/oauth/token"}, ModelsURL: server.URL + "/models",
	})
	provider, err := Current().newCodexProvider("codex", core.Caller{ID: human.ID, Kind: core.CallerHuman}, 30, server.Client(), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if models := provider.ListModels(); len(models) != 1 || requests != 2 {
		t.Fatalf("models=%+v requests=%d", models, requests)
	}
}

func TestCodexCatalogFiltersEligibilityAndVisibility(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("client_version") != "fixture-version" {
			t.Fatalf("client_version=%q", r.URL.Query().Get("client_version"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[
			{"id":"usable","supported_in_api":true,"visibility":"list","supported_endpoints":["/responses"]},
			{"id":"hidden","supported_in_api":true,"visibility":"hide","supported_endpoints":["/responses"]},
			{"id":"not-api","supported_in_api":false,"visibility":"list","supported_endpoints":["/responses"]},
			{"id":"unspecified","visibility":"list","supported_endpoints":["/responses"]}
		]}`))
	}))
	defer server.Close()
	provider := codexFixture(t, "codex", CodexEndpoints{ResponsesBaseURL: server.URL, ModelsURL: server.URL}, "fixture-version")
	models, _, err := provider.ListModelsWithError()
	if err != nil || len(models) != 1 || models[0].ID != "usable" {
		t.Fatalf("models=%+v err=%v", models, err)
	}
}

func TestCodexCatalogFailsWhenNoUsableModelsRemain(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"hidden","supported_in_api":true,"visibility":"hide"}]}`))
	}))
	defer server.Close()
	provider := codexFixture(t, "codex", CodexEndpoints{ResponsesBaseURL: server.URL, ModelsURL: server.URL}, codexCatalogClientVersion)
	models, _, err := provider.ListModelsWithError()
	code, detail, _ := CatalogFailure(err)
	if models != nil || code != "catalog_no_usable_models" || detail != "Provider catalog returned no API-eligible visible models." {
		t.Fatalf("models=%+v code=%q detail=%q err=%v", models, code, detail, err)
	}
}

func TestCodexSharedTransportPreservesStreamingAndResponsesSurface(t *testing.T) {
	var requests int
	var nativeRequests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request["stream"] != true || request["model"] != "gpt-codex" || request["instructions"] != codexInstructions {
			t.Fatalf("shared request=%+v", request)
		}
		nativeRequests = append(nativeRequests, request)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.reasoning_summary_text.delta\",\"sequence_number\":1,\"delta\":\"thinking\"}\n\n" +
			"data: {\"type\":\"response.output_text.delta\",\"sequence_number\":2,\"delta\":\"hello\"}\n\n" +
			"data: {\"type\":\"response.completed\",\"sequence_number\":3,\"response\":{\"id\":\"resp_shared\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"gpt-codex\",\"conversation\":{\"id\":\"conv_shared\"},\"output\":[{\"type\":\"reasoning\",\"encrypted_content\":\"opaque\",\"summary\":[{\"type\":\"summary_text\",\"text\":\"thinking\"}]},{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"))
	}))
	defer server.Close()
	configureCodexInstance(t, "codex")
	provider := codexFixture(t, "codex", CodexEndpoints{ResponsesBaseURL: server.URL, ModelsURL: server.URL}, codexCatalogClientVersion)

	stream, err := provider.Stream("gpt-codex", []Message{{"role": "user", "content": "hello"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var chunks []string
	for {
		chunk, ok := stream.Next()
		if !ok {
			break
		}
		chunks = append(chunks, chunk)
	}
	if stream.Err() != nil || len(chunks) < 2 || !strings.Contains(strings.Join(chunks, "\n"), `"content":"hello"`) {
		t.Fatalf("chunks=%q err=%v", chunks, stream.Err())
	}

	response, _, err := provider.CompleteResponses("gpt-codex", map[string]any{
		"input": "hello", "previous_response_id": "resp_previous", "conversation": "conv_shared",
		"include": []any{"reasoning.encrypted_content"}, "reasoning": map[string]any{"effort": "high", "summary": "auto"},
	})
	if err != nil {
		t.Fatal(err)
	}
	output := response["output"].([]any)
	if requests != 2 || response["object"] != "response" || response["model"] != "gpt-codex" || len(output) != 2 || output[0].(map[string]any)["encrypted_content"] != "opaque" {
		t.Fatalf("requests=%d response=%+v", requests, response)
	}
	request := nativeRequests[1]
	if request["previous_response_id"] != "resp_previous" || request["conversation"] != "conv_shared" || request["stream"] != true {
		t.Fatalf("native request state=%+v", request)
	}
	input, ok := request["input"].([]any)
	if !ok || len(input) != 1 || input[0].(map[string]any)["role"] != "user" || input[0].(map[string]any)["content"] != "hello" {
		t.Fatalf("native shorthand input was not normalized: %+v", request["input"])
	}

	nativeStream, _, err := provider.StreamResponses("gpt-codex", map[string]any{
		"input": "hello", "include": []any{"reasoning.encrypted_content"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer nativeStream.Close()
	var events []string
	for {
		event, ok := nativeStream.Next()
		if !ok {
			break
		}
		events = append(events, event)
	}
	joined := strings.Join(events, "\n")
	if nativeStream.Err() != nil || !strings.Contains(joined, `"type":"response.reasoning_summary_text.delta"`) || !strings.Contains(joined, `"sequence_number":1`) || !strings.Contains(joined, `"encrypted_content":"opaque"`) {
		t.Fatalf("native events=%q err=%v", events, nativeStream.Err())
	}
}

func TestCodexBrowserRefreshEndpointErrorUsesTypedRevocationContract(t *testing.T) {
	err := &browseroauth.EndpointError{StatusCode: http.StatusBadRequest, Code: "invalid_grant", Description: "expired"}
	var endpoint *browseroauth.EndpointError
	if !errors.As(err, &endpoint) {
		t.Fatal("fixture endpoint error did not match")
	}
	translated := &codexauth.RefreshError{StatusCode: endpoint.StatusCode, Code: endpoint.Code, Description: endpoint.Description}
	if !tokenstore.IsTerminal(translated) || codexRefreshInvocationError(translated) == nil {
		t.Fatalf("translated refresh error=%+v", translated)
	}
}

func TestCodexInvalidRefreshRevokesPrivateConnection(t *testing.T) {
	setupCodexProviderTest(t)
	human, err := iam.CreatePrincipal("human", "authentik:codex-invalid", "", "Codex Invalid")
	if err != nil {
		t.Fatal(err)
	}
	oldClientID := config.Get().OpenAICodexClientID
	t.Cleanup(func() { config.Update(func(s *config.Settings) { s.OpenAICodexClientID = oldClientID }) })
	config.Update(func(s *config.Settings) { s.OpenAICodexClientID = "fixture-client" })
	if _, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
		PrincipalID: human.ID, ProviderID: "codex", Kind: "openai_codex_oauth", AccessToken: "old", RefreshToken: "stale", OAuthProfile: codexOAuthProfileDevice, OAuthClientID: "fixture-client",
	}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer server.Close()
	SetCodexEndpointsForTests(t, CodexEndpoints{OAuth: codexauth.Endpoints{OAuthTokenURL: server.URL}})
	cached := "codex@" + human.ID
	putProvider(cached, EchoProvider{})
	_, _, err = RefreshCodexOAuthConnection(human.ID, "codex", "")
	if err == nil {
		t.Fatal("invalid refresh unexpectedly succeeded")
	}
	if status := UpstreamStatus(err); status != http.StatusBadRequest || !InvocationFailoverEligible(err) {
		t.Fatalf("refresh err=%v status=%d, want the token endpoint's failover-eligible 400", err, status)
	}
	if _, _, ok, err := iam.OAuthProviderConnectionSecret(human.ID, "codex", ""); err != nil || ok {
		t.Fatalf("invalid refresh left active connection: ok=%v err=%v", ok, err)
	}
	if _, found := Current().instances.instances[cached]; found {
		t.Fatal("the revocation left the owner's provider cached")
	}
}

func TestCodexRefreshUsesConnectionBoundClientID(t *testing.T) {
	setupCodexProviderTest(t)
	human, err := iam.CreatePrincipal("human", "authentik:codex-bound-client", "", "Codex Bound Client")
	if err != nil {
		t.Fatal(err)
	}
	config.Update(func(s *config.Settings) { s.OpenAICodexClientID = "mutable-global-client" })
	if _, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
		PrincipalID: human.ID, ProviderID: "codex", Kind: "openai_codex_oauth",
		AccessToken: "old", RefreshToken: "refresh", OAuthProfile: codexOAuthProfileDevice,
		OAuthClientID: "bound-client",
	}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["client_id"] != "bound-client" {
			t.Fatalf("refresh client_id=%q", body["client_id"])
		}
		_, _ = w.Write([]byte(`{"access_token":"new"}`))
	}))
	defer server.Close()
	SetCodexEndpointsForTests(t, CodexEndpoints{OAuth: codexauth.Endpoints{OAuthTokenURL: server.URL}})
	if _, _, err := RefreshCodexOAuthConnection(human.ID, "codex", ""); err != nil {
		t.Fatal(err)
	}
	current, _, ok, err := iam.OAuthProviderConnectionSecret(human.ID, "codex", "")
	if err != nil || !ok || current.OAuthClientID != "bound-client" {
		t.Fatalf("current=%+v ok=%v err=%v", current, ok, err)
	}
}

// A connection without a stored client profile predates them. Core would
// refresh it with the configured client; the gateway refuses, on the
// console's refresh and on inference alike, before anything is sent.
func TestCodexLegacyRefreshWithoutProfileFailsClosed(t *testing.T) {
	setupCodexProviderTest(t)
	configureCodexInstance(t, "codex")
	human, err := iam.CreatePrincipal("human", "authentik:codex-legacy", "", "Codex Legacy")
	if err != nil {
		t.Fatal(err)
	}
	oldClientID := config.Get().OpenAICodexClientID
	t.Cleanup(func() { config.Update(func(s *config.Settings) { s.OpenAICodexClientID = oldClientID }) })
	config.Update(func(s *config.Settings) { s.OpenAICodexClientID = "global-client" })
	if _, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
		PrincipalID: human.ID, ProviderID: "codex", Kind: "openai_codex_oauth",
		AccessToken: "old", RefreshToken: "refresh", ExpiresAt: time.Now().Add(-time.Minute).Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	SetCodexEndpointsForTests(t, CodexEndpoints{
		OAuth: codexauth.Endpoints{OAuthTokenURL: server.URL + "/oauth/token"}, ResponsesBaseURL: server.URL,
	})
	_, _, err = RefreshCodexOAuthConnection(human.ID, "codex", "")
	if err == nil || !IsConfig(err) || !strings.Contains(err.Error(), "reauthorize") {
		t.Fatalf("legacy refresh error=%v", err)
	}
	provider, err := Current().newCodexProvider("codex", core.Caller{ID: human.ID, Kind: core.CallerHuman}, 30, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = provider.CompleteResponses("gpt-codex", map[string]any{"input": "hello"})
	if err == nil || !IsConfig(err) || !strings.Contains(err.Error(), "reauthorize") {
		t.Fatalf("legacy inference error=%v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("a legacy grant reached upstream: calls=%d", calls.Load())
	}
}

func TestCodexRefreshSerializesConcurrentRotation(t *testing.T) {
	setupCodexProviderTest(t)
	human, err := iam.CreatePrincipal("human", "authentik:codex-concurrent", "", "Codex Concurrent")
	if err != nil {
		t.Fatal(err)
	}
	oldClientID := config.Get().OpenAICodexClientID
	t.Cleanup(func() { config.Update(func(s *config.Settings) { s.OpenAICodexClientID = oldClientID }) })
	config.Update(func(s *config.Settings) { s.OpenAICodexClientID = "fixture-client" })
	if _, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
		PrincipalID: human.ID, ProviderID: "codex", Kind: "openai_codex_oauth", AccessToken: "old-access", RefreshToken: "old-refresh", OAuthProfile: codexOAuthProfileDevice, OAuthClientID: "fixture-client",
	}); err != nil {
		t.Fatal(err)
	}
	initial, connection, ok, err := iam.OAuthProviderConnectionSecret(human.ID, "codex", "")
	if err != nil || !ok {
		t.Fatalf("initial connection ok=%v err=%v", ok, err)
	}
	var refreshCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCalls.Add(1)
		body := map[string]string{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["refresh_token"] != "old-refresh" {
			t.Fatalf("refresh body=%+v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"new-access","refresh_token":"new-refresh"}`))
	}))
	defer server.Close()
	SetCodexEndpointsForTests(t, CodexEndpoints{OAuth: codexauth.Endpoints{OAuthTokenURL: server.URL}})

	// Two coordinators stand for two processes; the credential store's
	// lease is all they share.
	record, err := Current().credentials.Load(context.Background(), connection.ID)
	if err != nil || record.AccessToken != initial.AccessToken {
		t.Fatalf("record=%v err=%v", record, err)
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	for range 2 {
		coordinator, err := Current().codexCoordinator()
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			<-start
			_, err := coordinator.Rejected(context.Background(), connection.ID, record)
			errs <- err
		}()
	}
	close(start)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if refreshCalls.Load() != 1 {
		t.Fatalf("refresh calls=%d want 1", refreshCalls.Load())
	}
	current, _, ok, err := iam.OAuthProviderConnectionSecret(human.ID, "codex", "")
	if err != nil || !ok || current.AccessToken != "new-access" || current.RefreshToken != "new-refresh" {
		t.Fatalf("current=%+v ok=%v err=%v", current, ok, err)
	}
}

func TestCodexRefreshRejectsChangedAccount(t *testing.T) {
	setupCodexProviderTest(t)
	human, err := iam.CreatePrincipal("human", "authentik:codex-account-change", "", "Codex Account Change")
	if err != nil {
		t.Fatal(err)
	}
	oldClientID := config.Get().OpenAICodexClientID
	t.Cleanup(func() { config.Update(func(s *config.Settings) { s.OpenAICodexClientID = oldClientID }) })
	config.Update(func(s *config.Settings) { s.OpenAICodexClientID = "fixture-client" })
	if _, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
		PrincipalID: human.ID, ProviderID: "codex", Kind: "openai_codex_oauth", AccessToken: "old-access", RefreshToken: "old-refresh", AccountID: "account-a", OAuthProfile: codexOAuthProfileDevice, OAuthClientID: "fixture-client",
	}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"new-access","refresh_token":"new-refresh","account_id":"account-b"}`))
	}))
	defer server.Close()
	SetCodexEndpointsForTests(t, CodexEndpoints{OAuth: codexauth.Endpoints{OAuthTokenURL: server.URL}})

	_, _, err = RefreshCodexOAuthConnection(human.ID, "codex", "")
	if err == nil || !strings.Contains(err.Error(), "account changed") {
		t.Fatalf("refresh err=%v", err)
	}
	current, _, ok, err := iam.OAuthProviderConnectionSecret(human.ID, "codex", "")
	if err != nil || !ok || current.AccessToken != "old-access" || current.AccountID != "account-a" {
		t.Fatalf("current=%+v ok=%v err=%v", current, ok, err)
	}
}

// The connection is signed in again to another account while a request
// refreshes it. The Coordinator's compare-and-swap loses and it would serve
// the new sign-in; the request was bound to the first account and must fail
// instead, before anything reaches Codex. See oauthCall.pin.
func TestCodexPrepareRejectsConcurrentAccountReplacement(t *testing.T) {
	setupCodexProviderTest(t)
	configureCodexInstance(t, "codex")
	human, err := iam.CreatePrincipal("human", "authentik:codex-account-replacement", "", "Codex Account Replacement")
	if err != nil {
		t.Fatal(err)
	}
	oldClientID := config.Get().OpenAICodexClientID
	t.Cleanup(func() { config.Update(func(s *config.Settings) { s.OpenAICodexClientID = oldClientID }) })
	config.Update(func(s *config.Settings) { s.OpenAICodexClientID = "fixture-client" })
	if _, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
		PrincipalID: human.ID, ProviderID: "codex", Kind: "openai_codex_oauth", AccessToken: "old-access", RefreshToken: "old-refresh", AccountID: "account-a", ExpiresAt: time.Now().Add(-time.Minute).Unix(), OAuthProfile: codexOAuthProfileDevice, OAuthClientID: "fixture-client",
	}); err != nil {
		t.Fatal(err)
	}
	_, connection, ok, err := iam.OAuthProviderConnectionSecret(human.ID, "codex", "")
	if err != nil || !ok {
		t.Fatalf("initial connection ok=%v err=%v", ok, err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			t.Errorf("a request reached Codex at %s", r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if _, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
			PrincipalID: human.ID, ProviderID: "codex", Name: connection.Name, Kind: connection.Kind, Source: connection.Source, MakeDefault: connection.IsDefault,
			AccessToken: "replacement-access", RefreshToken: "replacement-refresh", AccountID: "account-b", Status: "active",
			OAuthProfile: codexOAuthProfileDevice, OAuthClientID: "fixture-client",
		}); err != nil {
			t.Errorf("replace connection: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"new-access","refresh_token":"new-refresh","account_id":"account-a"}`))
	}))
	defer server.Close()
	SetCodexEndpointsForTests(t, CodexEndpoints{
		OAuth: codexauth.Endpoints{OAuthTokenURL: server.URL + "/oauth/token"}, ResponsesBaseURL: server.URL,
	})

	provider, err := Current().newCodexProvider("codex", core.Caller{ID: human.ID, Kind: core.CallerHuman}, 30, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	_, observation, err := provider.CompleteResponses("gpt-codex", map[string]any{"input": "hello"})
	if err == nil || !strings.Contains(err.Error(), "account changed") {
		t.Fatalf("request err=%v", err)
	}
	if observation == nil || observation.ConnectionID != connection.ID {
		t.Fatalf("observation=%+v, want the connection the refresh started from", observation)
	}
	current, _, ok, err := iam.OAuthProviderConnectionSecret(human.ID, "codex", "")
	if err != nil || !ok || current.AccessToken != "replacement-access" || current.AccountID != "account-b" {
		t.Fatalf("current=%+v ok=%v err=%v", current, ok, err)
	}
}

func TestCodexReusedRefreshDoesNotRevokeRotatedConnection(t *testing.T) {
	setupCodexProviderTest(t)
	human, err := iam.CreatePrincipal("human", "authentik:codex-reused", "", "Codex Reused")
	if err != nil {
		t.Fatal(err)
	}
	oldClientID := config.Get().OpenAICodexClientID
	t.Cleanup(func() { config.Update(func(s *config.Settings) { s.OpenAICodexClientID = oldClientID }) })
	config.Update(func(s *config.Settings) { s.OpenAICodexClientID = "fixture-client" })
	if _, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
		PrincipalID: human.ID, ProviderID: "codex", Kind: "openai_codex_oauth", AccessToken: "old-access", RefreshToken: "old-refresh", OAuthProfile: codexOAuthProfileDevice, OAuthClientID: "fixture-client",
	}); err != nil {
		t.Fatal(err)
	}
	_, connection, ok, err := iam.OAuthProviderConnectionSecret(human.ID, "codex", "")
	if err != nil || !ok {
		t.Fatalf("initial connection ok=%v err=%v", ok, err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
			PrincipalID: human.ID, ProviderID: "codex", Name: connection.Name, Kind: connection.Kind, Source: connection.Source, MakeDefault: connection.IsDefault,
			AccessToken: "rotated-access", RefreshToken: "rotated-refresh", Status: "active",
			OAuthProfile: codexOAuthProfileDevice, OAuthClientID: "fixture-client",
		}); err != nil {
			t.Errorf("rotate connection: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"refresh_token_reused"}`))
	}))
	defer server.Close()
	SetCodexEndpointsForTests(t, CodexEndpoints{OAuth: codexauth.Endpoints{OAuthTokenURL: server.URL}})

	// The grant is rejected for good, but the revocation is fenced by the
	// revision the refresh started from, which the rotation replaced.
	if _, _, err := RefreshCodexOAuthConnection(human.ID, "codex", ""); err == nil {
		t.Fatal("reused refresh unexpectedly succeeded")
	}
	current, _, ok, err := iam.OAuthProviderConnectionSecret(human.ID, "codex", "")
	if err != nil || !ok || current.AccessToken != "rotated-access" || current.RefreshToken != "rotated-refresh" {
		t.Fatalf("rotated connection was revoked: current=%+v ok=%v err=%v", current, ok, err)
	}
}

func TestExplicitCodexRefreshDoesNotRecreateRevokedConnection(t *testing.T) {
	setupCodexProviderTest(t)
	human, err := iam.CreatePrincipal("human", "authentik:codex-explicit-revoke", "", "Codex Explicit Revoke")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
		PrincipalID: human.ID, ProviderID: "codex", Kind: "openai_codex_oauth",
		AccessToken: "old-access", RefreshToken: "old-refresh", AccountID: "account-a",
		OAuthProfile: codexOAuthProfileDevice, OAuthClientID: "fixture-client",
	}); err != nil {
		t.Fatal(err)
	}
	_, connection, ok, err := iam.OAuthProviderConnectionSecret(human.ID, "codex", "")
	if err != nil || !ok {
		t.Fatalf("initial connection ok=%v err=%v", ok, err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"new-access","refresh_token":"new-refresh","account_id":"account-a"}`))
	}))
	defer server.Close()
	SetCodexEndpointsForTests(t, CodexEndpoints{OAuth: codexauth.Endpoints{OAuthTokenURL: server.URL}})

	errs := make(chan error, 1)
	go func() {
		_, _, refreshErr := RefreshCodexOAuthConnection(human.ID, "codex", "")
		errs <- refreshErr
	}()
	<-started
	if err := iam.RevokeProviderConnection(human.ID, connection.ID); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-errs; err == nil {
		t.Fatal("explicit refresh unexpectedly recreated a revoked connection")
	}
	if _, _, ok, err := iam.OAuthProviderConnectionSecret(human.ID, "codex", ""); err != nil || ok {
		t.Fatalf("revoked connection became active: ok=%v err=%v", ok, err)
	}
}

func TestCodexRefreshInvocationErrorPreservesRetryClassification(t *testing.T) {
	transport := codexRefreshInvocationError(&codexauth.AuthError{Operation: "OAuth token", Code: "transport"})
	if !InvocationRetryable(transport) {
		t.Fatal("Codex transport error should be retryable")
	}
	rejected := codexRefreshInvocationError(&codexauth.RefreshError{StatusCode: http.StatusUnauthorized, Code: "invalid_grant"})
	if InvocationRetryable(rejected) || InvocationFailoverEligible(rejected) || UpstreamStatus(rejected) != http.StatusUnauthorized {
		t.Fatalf("Codex credential rejection retry=%v failover=%v status=%d", InvocationRetryable(rejected), InvocationFailoverEligible(rejected), UpstreamStatus(rejected))
	}
}

func setupCodexProviderTest(t *testing.T) {
	t.Helper()
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	ResetProviders()
	oldKey := config.Get().CredentialEncryptionKey
	oldProviders := config.Get().Providers
	oldPolicies := config.Get().Policies
	t.Cleanup(func() {
		iam.ResetForTests()
		ResetProviders()
		config.Update(func(s *config.Settings) {
			s.CredentialEncryptionKey = oldKey
			s.Providers = oldProviders
			s.Policies = oldPolicies
		})
	})
	key := make([]byte, 32)
	config.Update(func(s *config.Settings) {
		s.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(key)
		s.Providers = map[string]*config.ProviderConfig{}
		s.Policies.Defaults = config.ProviderPolicy{}
		s.Policies.Overrides = map[string]config.ProviderPolicy{}
	})
}

func TestCodexChatFacadeRejectsStructuralFields(t *testing.T) {
	messages := []Message{{"role": "user", "content": "hi"}}
	payload, err := codexChatPayload("gpt-codex", messages, Kwargs{
		"max_tokens": json.Number("64"), "temperature": 0.2, "top_p": 0.9, "stop": []any{"END"},
		"n": json.Number("1"), "tool_choice": "auto", "parallel_tool_calls": true, "logprobs": false,
		"response_format": map[string]any{"type": "text"}, "modalities": []any{"text"},
		"prompt_cache_key": "fixture-cache", "tools": []any{map[string]any{"type": "function"}},
	})
	if err != nil {
		t.Fatalf("advisory and default-valued fields were rejected: %v", err)
	}
	if len(payload) != 4 || payload["prompt_cache_key"] != "fixture-cache" || payload["tools"] == nil {
		t.Fatalf("payload=%v", payload)
	}
	for field, value := range map[string]any{
		"response_format":     map[string]any{"type": "json_schema"},
		"n":                   json.Number("2"),
		"tool_choice":         "required",
		"parallel_tool_calls": false,
		"logprobs":            true,
		"top_logprobs":        json.Number("3"),
		"modalities":          []any{"text", "audio"},
		"audio":               map[string]any{"voice": "fixture"},
		"prediction":          map[string]any{"type": "content"},
	} {
		if _, err := codexChatPayload("gpt-codex", messages, Kwargs{field: value}); err == nil || !IsConfig(err) || !strings.Contains(err.Error(), field) {
			t.Errorf("%s=%v: err=%v, want a configuration error naming the field", field, value, err)
		}
	}
}

// A client replays a Gemini history, thought signatures included, when it
// fails over to Codex. Responses cannot carry a signature, so the facade must
// serve the history without it and leave the caller's messages intact for the
// next target.
func TestCodexChatFacadeServesThoughtSignatureHistory(t *testing.T) {
	var input []any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		input, _ = request["input"].([]any)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_fixture\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"gpt-codex\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}]}}\n\n"))
	}))
	defer server.Close()
	configureCodexInstance(t, "codex")
	provider := codexFixture(t, "codex", CodexEndpoints{ResponsesBaseURL: server.URL, ModelsURL: server.URL}, codexCatalogClientVersion)
	function := map[string]any{"name": "lookup", "arguments": "{}", "thought_signature": "fixture-signature"}
	google := map[string]any{"thought_signature": "fixture-signature"}
	call := map[string]any{
		"id": "call_fixture", "type": "function", "function": function,
		"thought_signature": "fixture-signature", "extra_content": map[string]any{"google": google},
	}
	history := []Message{
		{"role": "user", "content": "Look it up"},
		{"role": "assistant", "content": nil, "tool_calls": []any{call}},
		{"role": "tool", "tool_call_id": "call_fixture", "content": "found"},
	}
	response, err := provider.Complete("gpt-codex", history, nil)
	if err != nil {
		t.Fatalf("history with thought signatures was rejected: %v", err)
	}
	if len(input) != 3 || input[1].(map[string]any)["call_id"] != "call_fixture" {
		t.Fatalf("input=%+v", input)
	}
	if encoded, _ := json.Marshal(input); strings.Contains(string(encoded), "thought_signature") {
		t.Fatalf("a signature reached Codex: %s", encoded)
	}
	if message := response["choices"].([]any)[0].(map[string]any)["message"].(map[string]any); message["content"] != "ok" {
		t.Fatalf("response=%+v", response)
	}
	if function["thought_signature"] == nil || google["thought_signature"] == nil || call["thought_signature"] == nil {
		t.Fatal("the caller's history lost a signature that the next target may need")
	}
}

func TestAdaptsChatToNativeResponses(t *testing.T) {
	for name, tc := range map[string]struct {
		cfg  *config.ProviderConfig
		want bool
	}{
		"codex registry":   {&config.ProviderConfig{Type: "openai_compatible", RegistryID: "openai_codex"}, true},
		"zen registry":     {&config.ProviderConfig{Type: "openai_compatible", RegistryID: "opencode_zen"}, true},
		"zen base URL":     {&config.ProviderConfig{Type: "openai_compatible", BaseURL: "https://opencode.ai/zen/v1"}, true},
		"plain compatible": {&config.ProviderConfig{Type: "openai_compatible", BaseURL: "https://api.example.test/v1"}, false},
		"missing config":   {nil, false},
	} {
		if got := AdaptsChatToNativeResponses("fixture", tc.cfg); got != tc.want {
			t.Errorf("%s: got %v, want %v", name, got, tc.want)
		}
	}
}
