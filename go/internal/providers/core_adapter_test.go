package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"llmgw/internal/config"

	core "github.com/xibodev/llmgw-core"
)

func TestCoreAdapterProviderLifecycle(t *testing.T) {
	echo := EchoProvider{}
	adapter := NewCoreProviderAdapter(echo)

	// ListModels
	models, err := adapter.ListModels(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListModels error: %v", err)
	}
	if len(models) == 0 {
		t.Fatal("expected models from echo provider")
	}
	if models[0].Capabilities == nil || models[0].Capabilities.SchemaVersion != core.ModelCapabilitiesSchemaVersion ||
		models[0].Capabilities.Freshness.DiscoveredAt == nil || models[0].Capabilities.Freshness.VerifiedAt != nil {
		t.Fatalf("core model capabilities = %+v", models[0].Capabilities)
	}

	// Complete
	resp, err := adapter.Complete(context.Background(), "echo-model", map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "hello world"},
		},
	}, nil)
	if err != nil {
		t.Fatalf("Complete error: %v", err)
	}
	if resp == nil || resp["model"] != "echo-model" {
		t.Fatalf("unexpected completion response: %+v", resp)
	}
}

func TestCoreEngineIntegration(t *testing.T) {
	config.Update(func(s *config.Settings) {
		s.Providers["echo-p"] = &config.ProviderConfig{Type: "echo"}
		s.Endpoints["smart"] = &config.EndpointConfig{
			Failover: []config.EndpointMember{
				{Provider: "echo-p", Model: "echo-test"},
			},
		}
	})
	defer ResetProviders()

	engine := BuildCoreEngine()
	if engine == nil {
		t.Fatal("expected non-nil core engine")
	}

	// Test GET /v1/models against the core.Engine
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "smart") {
		t.Fatalf("expected smart endpoint in models list, got: %s", body)
	}

	// Test POST /v1/chat/completions against core.Engine
	chatPayload := `{"model":"smart","messages":[{"role":"user","content":"hi"}]}`
	chatReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatPayload))
	chatReq.Header.Set("Content-Type", "application/json")
	chatRec := httptest.NewRecorder()
	engine.ServeHTTP(chatRec, chatReq)

	if chatRec.Code != http.StatusOK {
		t.Fatalf("chat completion status = %d, want %d: %s", chatRec.Code, http.StatusOK, chatRec.Body.String())
	}
}

func TestCoreResolutionContract(t *testing.T) {
	res := core.Resolution{
		Targets: []core.Target{
			{Provider: "test", Model: "gpt"},
		},
		Category: "smart",
	}
	if len(res.Targets) != 1 || res.Category != "smart" {
		t.Fatalf("unexpected resolution: %+v", res)
	}
}
