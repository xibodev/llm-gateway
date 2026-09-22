package providers

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"

	providerauth "github.com/xibodev/llm-provider-auth"
	codexauth "github.com/xibodev/llm-provider-auth/codex"
	core "github.com/xibodev/llmgw-core"
	coreproviders "github.com/xibodev/llmgw-core/providers"
)

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
	payload := codexChatPayload("gpt-5.6-sol", []Message{{"role": "user", "content": "hello"}}, Kwargs{
		"prompt_cache_key": "fixture-cache",
		"temperature":      0.7,
		"top_p":            0.8,
		"max_tokens":       128,
		"stream_options":   map[string]any{"include_usage": true},
	})
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
	oldModels, oldToken := codexauth.ModelsURL, codexauth.OAuthTokenURL
	codexauth.ModelsURL = server.URL + "/backend-api/codex/models"
	codexauth.OAuthTokenURL = server.URL + "/oauth/token"
	t.Cleanup(func() {
		codexauth.ModelsURL, codexauth.OAuthTokenURL = oldModels, oldToken
	})
	principal := &config.Principal{PrincipalID: human.ID, PrincipalKind: human.Kind}
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
			inner, createErr := coreproviders.NewCodexProvider(coreproviders.CodexProviderConfig{
				SessionSource: coreproviders.NewCodexTokenSessionSource(
					providerauth.NewStaticTokenSource(&providerauth.Token{AccessToken: "fixture"}), "",
				),
				Instructions: codexInstructions, ResponsesURL: server.URL + "/responses",
				ModelsURL: server.URL, Client: server.Client(),
			})
			if createErr != nil {
				t.Fatal(createErr)
			}
			provider := CodexProvider{inner: inner}
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
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5-codex","owned_by":"openai","description":"GPT-5 Codex","supported_in_api":true,"visibility":"list","supported_endpoints":["/responses"]}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	oldResponses, oldModels, oldToken := codexauth.ResponsesBaseURL, codexauth.ModelsURL, codexauth.OAuthTokenURL
	codexauth.ResponsesBaseURL = server.URL + "/backend-api/codex"
	codexauth.ModelsURL = server.URL + "/backend-api/codex/models"
	codexauth.OAuthTokenURL = server.URL + "/oauth/token"
	t.Cleanup(func() {
		codexauth.ResponsesBaseURL, codexauth.ModelsURL, codexauth.OAuthTokenURL = oldResponses, oldModels, oldToken
	})

	instance, err := newCodexProvider(codexAuth{
		principalID: human.ID, providerID: "codex", clientID: "fixture-client",
	}, 120, server.Client(), "fixture-catalog-version")
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
	if err != nil || !found || observation == nil || *observation != currentObservation {
		t.Fatalf("observation=%+v current=%+v found=%v err=%v", observation, currentObservation, found, err)
	}
	refreshed, _, ok, err := iam.OAuthProviderConnectionSecret(human.ID, "codex", "")
	if err != nil || !ok || refreshed.AccessToken != "new-access" || refreshed.RefreshToken != "new-refresh" || refreshed.IDToken != "old-id" || refreshed.TokenType != "Bearer" || refreshed.AccountID != "account-42" || refreshed.AccountLabel != "Fixture workspace" || refreshed.ExpiresAt != 0 {
		t.Fatalf("refreshed=%+v ok=%v err=%v", refreshed, ok, err)
	}
	rows := codex.ListModels()
	if len(rows) != 1 || rows[0].ID != "gpt-5-codex" || rows[0].Label != "GPT-5 Codex" || len(rows[0].SupportedSurfaces) != 1 {
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
	inner, err := coreproviders.NewCodexProvider(coreproviders.CodexProviderConfig{
		SessionSource: coreproviders.NewCodexTokenSessionSource(
			providerauth.NewStaticTokenSource(&providerauth.Token{AccessToken: "fixture"}), "",
		),
		Instructions: codexInstructions, ResponsesURL: server.URL + "/responses",
		ModelsURL: server.URL, ClientVersion: "fixture-version", Client: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	models, _, err := (CodexProvider{inner: inner}).ListModelsWithError()
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
	inner, err := coreproviders.NewCodexProvider(coreproviders.CodexProviderConfig{
		SessionSource: coreproviders.NewCodexTokenSessionSource(
			providerauth.NewStaticTokenSource(&providerauth.Token{AccessToken: "fixture"}), "",
		),
		Instructions: codexInstructions, ResponsesURL: server.URL + "/responses",
		ModelsURL: server.URL, Client: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	models, _, err := (CodexProvider{inner: inner}).ListModelsWithError()
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
	inner, err := coreproviders.NewCodexProvider(coreproviders.CodexProviderConfig{
		SessionSource: coreproviders.NewCodexTokenSessionSource(
			providerauth.NewStaticTokenSource(&providerauth.Token{AccessToken: "fixture"}), "",
		),
		Instructions: codexInstructions, ResponsesURL: server.URL, ModelsURL: server.URL,
		Client: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	provider := CodexProvider{inner: inner}

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
	oldToken := codexauth.OAuthTokenURL
	codexauth.OAuthTokenURL = server.URL
	t.Cleanup(func() { codexauth.OAuthTokenURL = oldToken })
	auth := codexAuth{principalID: human.ID, providerID: "codex", clientID: "fixture-client"}
	if err := auth.Refresh(); err == nil {
		t.Fatal("invalid refresh unexpectedly succeeded")
	}
	if _, _, ok, err := iam.OAuthProviderConnectionSecret(human.ID, "codex", ""); err != nil || ok {
		t.Fatalf("invalid refresh left active connection: ok=%v err=%v", ok, err)
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
	oldToken := codexauth.OAuthTokenURL
	codexauth.OAuthTokenURL = server.URL
	t.Cleanup(func() { codexauth.OAuthTokenURL = oldToken })
	if err := (codexAuth{principalID: human.ID, providerID: "codex", clientID: "mutable-global-client"}).Refresh(); err != nil {
		t.Fatal(err)
	}
	current, _, ok, err := iam.OAuthProviderConnectionSecret(human.ID, "codex", "")
	if err != nil || !ok || current.OAuthClientID != "bound-client" {
		t.Fatalf("current=%+v ok=%v err=%v", current, ok, err)
	}
}

func TestCodexLegacyRefreshWithoutProfileFailsClosed(t *testing.T) {
	setupCodexProviderTest(t)
	human, err := iam.CreatePrincipal("human", "authentik:codex-legacy", "", "Codex Legacy")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
		PrincipalID: human.ID, ProviderID: "codex", Kind: "openai_codex_oauth",
		AccessToken: "old", RefreshToken: "refresh",
	}); err != nil {
		t.Fatal(err)
	}
	err = (codexAuth{principalID: human.ID, providerID: "codex", clientID: "global-client"}).Refresh()
	if err == nil || !strings.Contains(err.Error(), "reauthorize") {
		t.Fatalf("legacy refresh error=%v", err)
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
	oldToken := codexauth.OAuthTokenURL
	codexauth.OAuthTokenURL = server.URL
	t.Cleanup(func() { codexauth.OAuthTokenURL = oldToken })

	auth := codexAuth{principalID: human.ID, providerID: "codex", clientID: "fixture-client"}
	start := make(chan struct{})
	errs := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			errs <- auth.refreshConnection(initial, connection)
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
	initial, connection, ok, err := iam.OAuthProviderConnectionSecret(human.ID, "codex", "")
	if err != nil || !ok {
		t.Fatalf("initial connection ok=%v err=%v", ok, err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"new-access","refresh_token":"new-refresh","account_id":"account-b"}`))
	}))
	defer server.Close()
	oldToken := codexauth.OAuthTokenURL
	codexauth.OAuthTokenURL = server.URL
	t.Cleanup(func() { codexauth.OAuthTokenURL = oldToken })

	auth := codexAuth{principalID: human.ID, providerID: "codex", clientID: "fixture-client"}
	err = auth.refreshConnection(initial, connection)
	if err == nil || !strings.Contains(err.Error(), "account changed") {
		t.Fatalf("refresh err=%v", err)
	}
	current, _, ok, err := iam.OAuthProviderConnectionSecret(human.ID, "codex", "")
	if err != nil || !ok || current.AccessToken != "old-access" || current.AccountID != "account-a" {
		t.Fatalf("current=%+v ok=%v err=%v", current, ok, err)
	}
}

func TestCodexPrepareRejectsConcurrentAccountReplacement(t *testing.T) {
	setupCodexProviderTest(t)
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
		if _, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
			PrincipalID: human.ID, ProviderID: "codex", Name: connection.Name, Kind: connection.Kind, Source: connection.Source, MakeDefault: connection.IsDefault,
			AccessToken: "replacement-access", RefreshToken: "replacement-refresh", AccountID: "account-b", Status: "active",
			OAuthProfile: codexOAuthProfileDevice, OAuthClientID: "fixture-client",
		}); err != nil {
			t.Fatalf("replace connection: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"new-access","refresh_token":"new-refresh","account_id":"account-a"}`))
	}))
	defer server.Close()
	oldToken := codexauth.OAuthTokenURL
	codexauth.OAuthTokenURL = server.URL
	t.Cleanup(func() { codexauth.OAuthTokenURL = oldToken })

	auth := codexAuth{principalID: human.ID, providerID: "codex", clientID: "fixture-client"}
	if _, _, err = auth.Prepare(); err == nil || !strings.Contains(err.Error(), "account changed") {
		t.Fatalf("prepare err=%v", err)
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
	initial, connection, ok, err := iam.OAuthProviderConnectionSecret(human.ID, "codex", "")
	if err != nil || !ok {
		t.Fatalf("initial connection ok=%v err=%v", ok, err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
			PrincipalID: human.ID, ProviderID: "codex", Name: connection.Name, Kind: connection.Kind, Source: connection.Source, MakeDefault: connection.IsDefault,
			AccessToken: "rotated-access", RefreshToken: "rotated-refresh", Status: "active",
			OAuthProfile: codexOAuthProfileDevice, OAuthClientID: "fixture-client",
		}); err != nil {
			t.Fatalf("rotate connection: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"refresh_token_reused"}`))
	}))
	defer server.Close()
	oldToken := codexauth.OAuthTokenURL
	codexauth.OAuthTokenURL = server.URL
	t.Cleanup(func() { codexauth.OAuthTokenURL = oldToken })

	auth := codexAuth{principalID: human.ID, providerID: "codex", clientID: "fixture-client"}
	if err := auth.refreshConnection(initial, connection); err == nil {
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
	oldToken := codexauth.OAuthTokenURL
	codexauth.OAuthTokenURL = server.URL
	t.Cleanup(func() { codexauth.OAuthTokenURL = oldToken })

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
