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

	providerauth "github.com/xibodev/llm-provider-auth"
	translate "github.com/xibodev/llm-translate"
	core "github.com/xibodev/llmgw-core"
	coreproviders "github.com/xibodev/llmgw-core/providers"
)

func TestTransparentDecisionContractRejectsRoutesAndNonNativeSurfaces(t *testing.T) {
	config.Update(func(settings *config.Settings) {
		settings.Providers = map[string]*config.ProviderConfig{
			"fixture": {Type: "echo"},
		}
	})
	principal := &config.Principal{}

	if _, err := exactNativeTransparentTarget("route", "/v1/chat/completions", router.Resolution{
		Category: "route", Targets: []router.Target{{Provider: "fixture", Model: "model"}},
	}, principal); err == nil || !strings.Contains(err.Error(), "exact provider/model") {
		t.Fatalf("route error = %v", err)
	}
	if _, err := exactNativeTransparentTarget("fixture/model", "/v1/responses", router.Resolution{
		Targets: []router.Target{{Provider: "fixture", Model: "model"}},
	}, principal); err == nil || !strings.Contains(err.Error(), "catalog-confirmed") {
		t.Fatalf("catalog error = %v", err)
	}
}

func TestTransportModeHeaderValidationDoesNotUseUserAgent(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	request.Header.Set("User-Agent", "OpenCode/fixture")
	if mode, err := requestedTransportMode(request); err != nil || mode != "" {
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

func TestPlanTargetTransportUsesFreshCatalogEvidence(t *testing.T) {
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
	plan := planTargetTransport(
		model, now.Add(-time.Minute), []core.TransportInterface{{Surface: core.ModelSurfaceResponses, Native: core.SupportSupported}}, "/responses", true,
		core.TransportRequirementTransparent, false, translate.Report{}, now,
	)
	if plan.Disposition != core.TransportNative || plan.Confidence != core.TransportConfidenceHigh {
		t.Fatalf("fresh transparent plan = %+v", plan)
	}
	plan = planTargetTransport(
		model, now, []core.TransportInterface{{Surface: core.ModelSurfaceResponses, Native: core.SupportSupported}}, "/v1/messages", true,
		core.TransportRequirementTransparent, false, translate.Report{}, now,
	)
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
			plan := planTargetTransport(
				model, test.refreshedAt, []core.TransportInterface{{Surface: core.ModelSurfaceResponses, Native: core.SupportSupported}}, "/v1/responses", true,
				core.TransportRequirementTransparent, false, translate.Report{}, now,
			)
			if plan.Disposition != core.TransportReject || plan.Reason != core.TransportRejectNativeUnconfirmed {
				t.Fatalf("plan = %+v", plan)
			}
		})
	}
	plan = planTargetTransport(
		model, now.Add(-transportCatalogFreshness), []core.TransportInterface{{Surface: core.ModelSurfaceResponses, Native: core.SupportSupported}}, "/v1/responses", true,
		core.TransportRequirementAny, false, translate.Report{}, now,
	)
	if plan.Disposition != core.TransportNative || plan.Confidence != core.TransportConfidenceLow {
		t.Fatalf("permissive stale plan = %+v", plan)
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
				inner, err := coreproviders.NewCodexProvider(coreproviders.CodexProviderConfig{
					SessionSource: coreproviders.NewCodexTokenSessionSource(
						providerauth.NewStaticTokenSource(&providerauth.Token{AccessToken: "fixture"}), "",
					),
					Instructions: "fixture", ModelsURL: server.URL, Client: server.Client(),
					Now: func() time.Time { return now },
				})
				if err != nil {
					t.Fatal(err)
				}
				rows, err := inner.ListModels(context.Background(), nil)
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
			surface := modelSurface(test.surface)
			plan := planTargetTransport(
				rows[0], now, []core.TransportInterface{{Surface: surface, Native: core.SupportSupported}},
				test.surface, true, core.TransportRequirementTransparent, false, translate.Report{}, now,
			)
			if plan.Disposition != core.TransportNative || plan.Confidence != core.TransportConfidenceHigh {
				t.Fatalf("plan=%+v capabilities=%+v", plan, rows[0].TypedCapabilities)
			}
		})
	}
}

func TestPlanTargetTransportFreshnessDoesNotUpgradeCapabilityEvidence(t *testing.T) {
	now := time.Date(2026, time.September, 21, 12, 0, 0, 0, time.UTC)
	capabilities := providers.AdaptModelCapabilities(
		map[string]any{"chat": true}, []string{"/v1/responses"}, time.Time{}, time.Time{},
	)
	model := providers.ModelInfo{
		ID: "model", SupportedSurfaces: []string{"/v1/responses"}, TypedCapabilities: capabilities,
	}
	plan := planTargetTransport(
		model, now, []core.TransportInterface{{Surface: core.ModelSurfaceResponses, Native: core.SupportSupported}},
		"/v1/responses", true, core.TransportRequirementTransparent, false, translate.Report{}, now,
	)
	if plan.Disposition != core.TransportReject || plan.Reason != core.TransportRejectNativeUnconfirmed {
		t.Fatalf("medium-confidence cached evidence certified transparent: %+v", plan)
	}
	if capabilities.Provenance.Source != core.ModelCapabilitySourceInferred ||
		capabilities.Provenance.Confidence != core.ModelCapabilityConfidenceMedium {
		t.Fatalf("input evidence was mutated: %+v", capabilities.Provenance)
	}
}

func TestPlanTargetTransportRejectsRoutesZenAndLossyAdaptation(t *testing.T) {
	now := time.Date(2026, time.September, 21, 12, 0, 0, 0, time.UTC)
	model := providers.ModelInfo{
		ID:                "model",
		Capabilities:      map[string]any{"chat": true},
		SupportedSurfaces: []string{"/v1/chat/completions"},
	}
	plan := planTargetTransport(
		model, now, []core.TransportInterface{{Surface: core.ModelSurfaceChatCompletions, Native: core.SupportSupported}}, "/v1/chat/completions", false,
		core.TransportRequirementTransparent, false, translate.Report{}, now,
	)
	if plan.Reason != core.TransportRejectExactTargetRequired {
		t.Fatalf("route plan = %+v", plan)
	}

	plan = planTargetTransport(
		model, now, []core.TransportInterface{{Surface: core.ModelSurfaceChatCompletions, Native: core.SupportUnsupported}}, "/v1/chat/completions", true,
		core.TransportRequirementTransparent, false, translate.Report{}, now,
	)
	if plan.Disposition != core.TransportReject || plan.Reason != core.TransportRejectNativeUnconfirmed {
		t.Fatalf("Zen plan = %+v", plan)
	}

	plan = planTargetTransport(
		model, now, []core.TransportInterface{{Surface: core.ModelSurfaceChatCompletions, Native: core.SupportSupported}}, "/v1/responses", true,
		core.TransportRequirementAny, false, translate.Report{}, now,
	)
	if plan.Disposition != core.TransportReject || plan.Reason != core.TransportRejectTranslationUnevaluated {
		t.Fatalf("unevaluated adaptation plan = %+v", plan)
	}

	loss := translate.NewReport(translate.Loss{
		Path: "input[0]", Class: translate.LossDropped, Severity: translate.LossMaterial,
	})
	plan = planTargetTransport(
		model, now, []core.TransportInterface{{Surface: core.ModelSurfaceChatCompletions, Native: core.SupportSupported}}, "/v1/responses", true,
		core.TransportRequirementAny, true, loss, now,
	)
	if plan.Disposition != core.TransportReject || plan.Reason != core.TransportRejectTranslationLoss {
		t.Fatalf("lossy adaptation plan = %+v", plan)
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
	NewServer().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "exact provider/model") {
		t.Fatalf("transparent route status=%d body=%s", response.Code, response.Body.String())
	}

	request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "OpenCode/fixture")
	response = httptest.NewRecorder()
	NewServer().ServeHTTP(response, request)
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
		principal  *config.Principal
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
			principal: &config.Principal{PrincipalID: "owner"},
			model:     providers.ModelInfo{ID: "gemini", TypedCapabilities: trusted(core.ModelSurfaceChatCompletions), SupportedSurfaces: []string{"/v1/chat/completions"}},
		},
		{
			name: "Codex Responses declaration", providerID: "codex",
			principal:  &config.Principal{PrincipalID: "owner"},
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
				test.providerID, test.principal, test.model, now,
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
		"openai", nil, model, now.Add(-transportCatalogFreshness),
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
	principal := &config.Principal{PrincipalID: "owner", PrincipalKind: "human"}
	if got := targetTransportMode(router.Target{Provider: "codex", Model: "fixture"}, principal, "/v1/responses"); got != "native" {
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
	if got := targetTransportMode(target, nil, "/v1/chat/completions"); got != "native" {
		t.Fatalf("chat mode=%q, want native", got)
	}
	if got := targetTransportMode(target, nil, "/v1/responses"); got != "translated" {
		t.Fatalf("responses mode=%q, want translated", got)
	}
}

func TestModelTransportSurfacesKeepsPlannerRejectsUnknown(t *testing.T) {
	now := time.Date(2026, time.September, 21, 12, 0, 0, 0, time.UTC)
	model := providers.ModelInfo{ID: "unknown", Capabilities: map[string]any{"chat": true}}
	native, emulated, unknown := modelTransportSurfaces(
		"missing", nil, model, time.Time{}, model.Capabilities, nil, now,
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
		"openai", nil, model, now, map[string]any{"chat": true}, []string{"/v1/chat/completions"}, now,
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
