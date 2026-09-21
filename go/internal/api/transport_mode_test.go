package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
	"llmgw/internal/router"
)

func TestTransparentDecisionContractRejectsRoutesAndNonNativeSurfaces(t *testing.T) {
	config.Update(func(settings *config.Settings) {
		settings.Providers = map[string]*config.ProviderConfig{
			"fixture": {Type: "echo"},
			"zen":     {Type: "openai_compatible", RegistryID: "opencode_zen"},
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
	if _, err := exactNativeTransparentTarget("zen/model", "/v1/chat/completions", router.Resolution{
		Targets: []router.Target{{Provider: "zen", Model: "model"}},
	}, principal); err == nil || !strings.Contains(err.Error(), "OpenCode Zen") {
		t.Fatalf("Zen error = %v", err)
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

func TestModelTransportSurfacesClassifiesAdaptationAndZen(t *testing.T) {
	config.Update(func(settings *config.Settings) {
		settings.Providers = map[string]*config.ProviderConfig{
			"fixture": {Type: "openai_compatible"},
			"zen":     {Type: "openai_compatible", RegistryID: "opencode_zen"},
		}
	})
	native, emulated := modelTransportSurfaces("fixture", nil, map[string]any{"chat": true}, []string{"/v1/chat/completions"})
	if len(native) != 1 || len(emulated) != 2 {
		t.Fatalf("native=%v emulated=%v", native, emulated)
	}
	native, emulated = modelTransportSurfaces("zen", nil, map[string]any{"chat": true}, []string{"/v1/chat/completions"})
	if len(native) != 0 || len(emulated) < 1 {
		t.Fatalf("Zen native=%v emulated=%v", native, emulated)
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
