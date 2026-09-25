package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
	"llmgw/internal/router"

	core "github.com/xibodev/llmgw-core"
	coreproviders "github.com/xibodev/llmgw-core/providers"
)

func TestTransparentDecisionContractRejectsRoutesAndNonNativeSurfaces(t *testing.T) {
	config.Update(func(settings *config.Settings) {
		settings.Providers = map[string]*config.ProviderConfig{
			"fixture": {Type: "echo"},
		}
	})
	caller := core.Caller{Kind: core.CallerAnonymous}

	if _, err := exactNativeTransparentTarget("route", "/v1/chat/completions", router.Resolution{
		Category: "route", Targets: []router.Target{{Provider: "fixture", Model: "model"}},
	}, caller); err == nil || !strings.Contains(err.Error(), "exact provider/model") {
		t.Fatalf("route error = %v", err)
	}
	if _, err := exactNativeTransparentTarget("fixture/model", "/v1/responses", router.Resolution{
		Targets: []router.Target{{Provider: "fixture", Model: "model"}},
	}, caller); err == nil || !strings.Contains(err.Error(), "catalog-confirmed") {
		t.Fatalf("catalog error = %v", err)
	}
}

func TestTransportModeHeaderValidationDoesNotUseUserAgent(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	request.Header.Set("User-Agent", "OpenCode/fixture")
	if mode, err := requestedTransportMode(request); err != nil || mode != core.TransportRequirementAny {
		t.Fatalf("User-Agent selected mode=%q err=%v", mode, err)
	}
	request.Header.Set(transportModeHeader, "privileged")
	if _, err := requestedTransportMode(request); err == nil {
		t.Fatal("unknown transport mode was accepted")
	}
}

func TestTransparentStreamingRemainsUnsupported(t *testing.T) {
	response := httptest.NewRecorder()
	if !rejectTransparentStream(response, true) {
		t.Fatal("transparent stream was accepted")
	}
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "not supported") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if rejectTransparentStream(httptest.NewRecorder(), false) {
		t.Fatal("non-streaming transparent request was rejected")
	}
}

func TestPlanTransparentUsesFreshCatalogEvidence(t *testing.T) {
	now := time.Date(2026, time.September, 21, 12, 0, 0, 0, time.UTC)
	capabilities := providers.AdaptModelCapabilities(
		map[string]any{"chat": true}, []string{"/v1/responses"}, time.Time{}, time.Time{},
	)
	capabilities.Provenance = core.ModelCapabilityProvenance{
		Source: core.ModelCapabilitySourceUpstreamReported, Confidence: core.ModelCapabilityConfidenceHigh,
	}
	model := providers.ModelInfo{
		ID:                "model",
		Capabilities:      map[string]any{"chat": true},
		SupportedSurfaces: []string{"/v1/responses"},
		TypedCapabilities: capabilities,
	}
	nativeResponses := []core.TransportInterface{{Surface: core.ModelSurfaceResponses, Native: core.SupportSupported}}
	plan := planTransparent(model, now.Add(-time.Minute), nativeResponses, core.ParseSurfacePath("/responses"), now)
	if plan.Disposition != core.TransportNative || plan.Confidence != core.TransportConfidenceHigh {
		t.Fatalf("fresh transparent plan = %+v", plan)
	}
	plan = planTransparent(model, now, nativeResponses, core.ParseSurfacePath("/v1/messages"), now)
	if plan.Disposition != core.TransportReject {
		t.Fatalf("unsupported surface plan = %+v", plan)
	}

	for _, test := range []struct {
		name        string
		refreshedAt time.Time
	}{
		{name: "stale", refreshedAt: now.Add(-transportCatalogFreshness)},
		{name: "unknown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan := planTransparent(model, test.refreshedAt, nativeResponses, core.ModelSurfaceResponses, now)
			if plan.Disposition != core.TransportReject || plan.Reason != core.TransportRejectNativeUnconfirmed {
				t.Fatalf("plan = %+v", plan)
			}
		})
	}
}

func TestTransparentPlansUseNativeProviderCatalogEvidence(t *testing.T) {
	now := time.Date(2026, time.September, 21, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name    string
		surface string
		models  func(*testing.T, *httptest.Server) []providers.ModelInfo
	}{
		{
			name: "Codex Responses", surface: "/v1/responses",
			models: func(t *testing.T, server *httptest.Server) []providers.ModelInfo {
				inner, err := coreproviders.NewCodex(coreproviders.CodexConfig{
					Instructions: "fixture", ModelsURL: server.URL, ClientVersion: "fixture-version",
					Client: server.Client(), Now: func() time.Time { return now },
				})
				if err != nil {
					t.Fatal(err)
				}
				rows, err := inner.ListModels(context.Background(), &core.Credential{Token: "fixture"})
				if err != nil {
					t.Fatal(err)
				}
				return []providers.ModelInfo{{
					ID: rows[0].ID, SupportedSurfaces: rows[0].SupportedAPIs,
					TypedCapabilities: rows[0].Capabilities,
				}}
			},
		},
		{
			name: "Anthropic Messages", surface: "/v1/messages",
			models: func(t *testing.T, server *httptest.Server) []providers.ModelInfo {
				rows, _, err := (providers.AnthropicNativeProvider{
					BaseURL: server.URL, Now: func() time.Time { return now },
				}).ListModelsWithError()
				if err != nil {
					t.Fatal(err)
				}
				return rows
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"data":[{"id":"native-model","supported_in_api":true,"visibility":"list","supported_endpoints":["/responses"]}]}`))
			}))
			defer server.Close()
			rows := test.models(t, server)
			if len(rows) != 1 {
				t.Fatalf("catalog rows=%+v", rows)
			}
			surface := core.ParseSurfacePath(test.surface)
			plan := planTransparent(
				rows[0], now, []core.TransportInterface{{Surface: surface, Native: core.SupportSupported}}, surface, now,
			)
			if plan.Disposition != core.TransportNative || plan.Confidence != core.TransportConfidenceHigh {
				t.Fatalf("plan=%+v capabilities=%+v", plan, rows[0].TypedCapabilities)
			}
		})
	}
}

func TestTransparentHeaderRejectsEndpointWhileUserAgentKeepsNormalRouting(t *testing.T) {
	config.Update(func(settings *config.Settings) {
		settings.AllowUnauthenticatedAPI = true
		settings.Providers = map[string]*config.ProviderConfig{"echo": {Type: "echo"}}
		settings.Endpoints = map[string]*config.EndpointConfig{
			"smart": {Failover: []config.EndpointMember{{Provider: "echo", Model: "echo-default"}}},
		}
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)

	body := []byte(`{"model":"smart","messages":[{"role":"user","content":"hello"}]}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(transportModeHeader, "transparent")
	response := httptest.NewRecorder()
	NewServer(Runtime{}).ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "exact provider/model") {
		t.Fatalf("transparent route status=%d body=%s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "OpenCode/fixture")
	response = httptest.NewRecorder()
	NewServer(Runtime{}).ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("User-Agent changed normal routing: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestModelTransportSurfacesUsesConcreteDeclarationAndTrustedFreshPlan(t *testing.T) {
	config.Update(func(settings *config.Settings) {
		settings.OpenAICodexClientID = "fixture-client"
		settings.Providers = map[string]*config.ProviderConfig{
			"openai":      {Type: "openai_compatible", APIKey: "fixture"},
			"ai-studio":   {Type: "ai_studio", APIKey: "fixture"},
			"vertex":      {Type: "vertex_ai", APIKey: "fixture", Project: "fixture"},
			"antigravity": {Type: "google_antigravity"},
			"codex":       {Type: "openai_compatible", RegistryID: "openai_codex"},
			"anthropic":   {Type: "anthropic", APIKey: "fixture"},
		}
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)
	now := time.Date(2026, time.September, 21, 12, 0, 0, 0, time.UTC)
	trusted := func(surface core.ModelSurface) *core.ModelCapabilities {
		capabilities := providers.AdaptModelCapabilities(
			map[string]any{"chat": true}, []string{string(surface)}, time.Time{}, time.Time{},
		)
		capabilities.Provenance = core.ModelCapabilityProvenance{
			Source: core.ModelCapabilitySourceUpstreamReported, Confidence: core.ModelCapabilityConfidenceHigh,
		}
		return capabilities
	}

	for _, test := range []struct {
		name       string
		providerID string
		caller     core.Caller
		model      providers.ModelInfo
		wantNative []string
	}{
		{
			name: "OpenAI Chat declaration", providerID: "openai",
			model:      providers.ModelInfo{ID: "chat", TypedCapabilities: trusted(core.ModelSurfaceChatCompletions), SupportedSurfaces: []string{"/v1/chat/completions"}},
			wantNative: []string{"/v1/chat/completions"},
		},
		{
			name: "AI Studio Chat adaptation", providerID: "ai-studio",
			model: providers.ModelInfo{ID: "gemini", TypedCapabilities: trusted(core.ModelSurfaceChatCompletions), SupportedSurfaces: []string{"/v1/chat/completions"}},
		},
		{
			name: "Vertex Chat adaptation", providerID: "vertex",
			model: providers.ModelInfo{ID: "gemini", TypedCapabilities: trusted(core.ModelSurfaceChatCompletions), SupportedSurfaces: []string{"/v1/chat/completions"}},
		},
		{
			name: "Antigravity Chat adaptation", providerID: "antigravity",
			caller: core.Caller{ID: "owner", Kind: core.CallerHuman},
			model:  providers.ModelInfo{ID: "gemini", TypedCapabilities: trusted(core.ModelSurfaceChatCompletions), SupportedSurfaces: []string{"/v1/chat/completions"}},
		},
		{
			name: "Codex Responses declaration", providerID: "codex",
			caller:     core.Caller{ID: "owner", Kind: core.CallerHuman},
			model:      providers.ModelInfo{ID: "codex", TypedCapabilities: trusted(core.ModelSurfaceResponses), SupportedSurfaces: []string{"/v1/responses"}},
			wantNative: []string{"/v1/responses"},
		},
		{
			name: "Anthropic Messages declaration", providerID: "anthropic",
			model:      providers.ModelInfo{ID: "claude", TypedCapabilities: trusted(core.ModelSurfaceMessages), SupportedSurfaces: []string{"/v1/messages"}},
			wantNative: []string{"/v1/messages"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			native, emulated, unknown := modelTransportSurfaces(
				test.providerID, test.caller, test.model, now,
				map[string]any{"chat": true}, test.model.SupportedSurfaces, now,
			)
			if strings.Join(native, ",") != strings.Join(test.wantNative, ",") {
				t.Fatalf("native=%v, want %v", native, test.wantNative)
			}
			if len(native)+len(emulated)+len(unknown) != 3 {
				t.Fatalf("native=%v emulated=%v unknown=%v", native, emulated, unknown)
			}
		})
	}

	model := providers.ModelInfo{
		ID: "chat", TypedCapabilities: trusted(core.ModelSurfaceChatCompletions),
		SupportedSurfaces: []string{"/v1/chat/completions"},
	}
	native, _, _ := modelTransportSurfaces(
		"openai", callerOf(nil), model, now.Add(-transportCatalogFreshness),
		map[string]any{"chat": true}, model.SupportedSurfaces, now,
	)
	if len(native) != 1 || native[0] != "/v1/chat/completions" {
		t.Fatalf("stale projection lost concrete native declaration: %v", native)
	}
}

func TestTargetTransportModeReportsExecutedProviderPathDespiteStaleCatalog(t *testing.T) {
	config.Update(func(settings *config.Settings) {
		settings.OpenAICodexClientID = "fixture-client"
		settings.Providers = map[string]*config.ProviderConfig{
			"codex": {Type: "openai_compatible", RegistryID: "openai_codex"},
		}
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)
	caller := core.Caller{ID: "owner", Kind: core.CallerHuman}
	if got := targetTransportMode(router.Target{Provider: "codex", Model: "fixture"}, caller, "/v1/responses"); got != "native" {
		t.Fatalf("mode=%q, want native", got)
	}
}

func TestTargetTransportModeUsesModelScopedAdaptation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"chat-only","supported_endpoints":["/v1/chat/completions"]}]}`))
	}))
	defer server.Close()
	config.Update(func(settings *config.Settings) {
		settings.Providers = map[string]*config.ProviderConfig{
			"fixture": {Type: "openai_compatible", BaseURL: server.URL, APIKey: "none", ForceApiSupport: true},
		}
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)
	if rows := providers.RefreshCatalog("fixture"); len(rows) != 1 {
		t.Fatalf("catalog rows=%+v", rows)
	}
	target := router.Target{Provider: "fixture", Model: "chat-only"}
	if got := targetTransportMode(target, callerOf(nil), "/v1/chat/completions"); got != "native" {
		t.Fatalf("chat mode=%q, want native", got)
	}
	if got := targetTransportMode(target, callerOf(nil), "/v1/responses"); got != "translated" {
		t.Fatalf("responses mode=%q, want translated", got)
	}
}

func TestModelTransportSurfacesKeepsPlannerRejectsUnknown(t *testing.T) {
	now := time.Date(2026, time.September, 21, 12, 0, 0, 0, time.UTC)
	model := providers.ModelInfo{ID: "unknown", Capabilities: map[string]any{"chat": true}}
	native, emulated, unknown := modelTransportSurfaces(
		"missing", callerOf(nil), model, time.Time{}, model.Capabilities, nil, now,
	)
	if len(native) != 0 || len(emulated) != 0 || len(unknown) != 3 {
		t.Fatalf("native=%v emulated=%v unknown=%v", native, emulated, unknown)
	}
}

func TestModelTransportSurfacesUsesInferredSurfaceArgument(t *testing.T) {
	now := time.Date(2026, time.September, 21, 12, 0, 0, 0, time.UTC)
	config.Update(func(settings *config.Settings) {
		settings.Providers = map[string]*config.ProviderConfig{"openai": {Type: "openai_compatible", APIKey: "none"}}
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)
	capabilities := &core.ModelCapabilities{SchemaVersion: core.ModelCapabilitiesSchemaVersion}
	capabilities.Operations.Chat = core.SupportSupported
	capabilities.Surfaces.ChatCompletions = core.SupportSupported
	capabilities.Provenance = core.ModelCapabilityProvenance{Source: core.ModelCapabilitySourceUpstreamReported, Confidence: core.ModelCapabilityConfidenceHigh}
	model := providers.ModelInfo{ID: "chat", TypedCapabilities: capabilities}
	native, _, _ := modelTransportSurfaces(
		"openai", callerOf(nil), model, now, map[string]any{"chat": true}, []string{"/v1/chat/completions"}, now,
	)
	if len(native) != 1 || native[0] != "/v1/chat/completions" {
		t.Fatalf("inferred surface was not used for interface planning: %v", native)
	}
}

func TestModelVerifiedAtRequiresSuccessfulMatchingProbe(t *testing.T) {
	checks := []iam.ProviderCheck{
		{Operation: iam.CheckVerify, Success: true, Model: "other", CheckedAt: 10},
		{Operation: iam.CheckVerify, Success: false, Model: "model", CheckedAt: 20},
		{Operation: iam.CheckVerify, Success: true, Model: "model", CheckedAt: 30},
	}
	if got := modelVerifiedAt("model", checks); got.Unix() != 30 {
		t.Fatalf("verified at = %v", got)
	}
}
