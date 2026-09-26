package providers

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"

	core "github.com/xibodev/llmgw-core"
)

func TestAdaptCoreProviderConnection(t *testing.T) {
	tests := []struct {
		name       string
		providerID string
		cfg        config.ProviderConfig
		connection *iam.ProviderConnection
		kind       core.ProviderConnectionKind
		auth       core.ProviderAuthKind
		owner      string
	}{
		{
			name: "codex personal oauth", providerID: "codex",
			cfg:        config.ProviderConfig{Type: "openai_compatible", RegistryID: "openai_codex"},
			connection: &iam.ProviderConnection{ID: "conn-codex", PrincipalID: "owner-1", PrincipalKind: "human", Kind: "openai_codex_oauth"},
			kind:       core.ProviderConnectionPersonalSubscription, auth: core.ProviderAuthOAuthDevice, owner: "owner-1",
		},
		{
			name: "personal api key", providerID: "openai", cfg: config.ProviderConfig{Type: "openai"},
			connection: &iam.ProviderConnection{ID: "conn-personal", PrincipalID: "owner-2", PrincipalKind: "human", Kind: "api_key"},
			kind:       core.ProviderConnectionPersonal, auth: core.ProviderAuthAPIKey, owner: "owner-2",
		},
		{
			name: "system api key", providerID: "openai", cfg: config.ProviderConfig{Type: "openai"},
			connection: &iam.ProviderConnection{ID: "conn-system", PrincipalID: "system", PrincipalKind: "system", Kind: "api_key"},
			kind:       core.ProviderConnectionSystem, auth: core.ProviderAuthAPIKey,
		},
		{
			name: "config api key", providerID: "custom", cfg: config.ProviderConfig{Type: "openai_compatible"},
			kind: core.ProviderConnectionSystem, auth: core.ProviderAuthAPIKey,
		},
		{
			name: "explicit anonymous", providerID: "llm7",
			cfg:  config.ProviderConfig{Type: "openai_compatible", RegistryID: "llm7", APIKey: "public"},
			kind: core.ProviderConnectionAnonymous, auth: core.ProviderAuthAnonymous,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := AdaptCoreProviderConnection(test.providerID, &test.cfg, test.connection)
			if err != nil {
				t.Fatalf("AdaptCoreProviderConnection: %v", err)
			}
			if got.Kind != test.kind || got.AuthKind != test.auth || got.OwnerID != test.owner {
				t.Fatalf("connection = %+v", got)
			}
			encoded, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), "anonymous") && test.auth != core.ProviderAuthAnonymous {
				t.Fatalf("unexpected anonymous mapping: %s", encoded)
			}
			if strings.Contains(string(encoded), "secret-value") || got.Credential != nil {
				t.Fatalf("credential escaped adapter: %s", encoded)
			}
		})
	}
}

func TestAdaptCoreProviderConnectionNeverMakesCodexAnonymous(t *testing.T) {
	cfg := config.ProviderConfig{
		Type: "openai_compatible", RegistryID: "openai_codex", APIKey: "public",
	}
	if _, err := AdaptCoreProviderConnection("codex", &cfg, nil); err == nil {
		t.Fatal("Codex without a personal connection should be rejected")
	}
	connection := iam.ProviderConnection{PrincipalID: "system", PrincipalKind: "system", Kind: "openai_codex_oauth"}
	if _, err := AdaptCoreProviderConnection("codex", &cfg, &connection); err == nil {
		t.Fatal("Codex with a system owner should be rejected")
	}
}

func TestAdaptCoreProviderEvidenceSeparatesCatalogAndCompletion(t *testing.T) {
	checks := []iam.ProviderCheck{
		{Operation: iam.CheckCatalogSync, Success: true, CheckedAt: 100, LatencyMS: 20},
		{Operation: iam.CheckVerify, Success: false, Model: "model-a", CheckedAt: 110, LatencyMS: 30, Detail: "sensitive failure detail"},
		{Operation: iam.CheckVerify, Success: true, Model: "model-b", CheckedAt: 120, LatencyMS: 40},
	}
	evidence := AdaptCoreProviderEvidence("provider-a", []ModelInfo{
		{ID: "model-a", Label: "Model A", Vendor: "vendor"},
		{ID: "model-b", Label: "Model B", Vendor: "vendor"},
	}, checks)

	if evidence.Catalog.Status != core.CatalogDiscovered || len(evidence.Catalog.Models) != 2 {
		t.Fatalf("catalog = %+v", evidence.Catalog)
	}
	if !evidence.Catalog.ObservedAt.Equal(time.Unix(100, 0)) {
		t.Fatalf("catalog observed_at = %v", evidence.Catalog.ObservedAt)
	}
	if len(evidence.Probes) != 2 || evidence.Probes[0].Status != core.CompletionFailed ||
		!evidence.Probes[1].InferenceVerified() {
		t.Fatalf("probes = %+v", evidence.Probes)
	}
	if evidence.Health.Status != core.ProviderHealthHealthy ||
		evidence.Health.ObservedAt != time.Unix(120, 0).UTC() {
		t.Fatalf("health = %+v", evidence.Health)
	}
	encoded, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "sensitive failure detail") {
		t.Fatalf("check detail escaped adapter: %s", encoded)
	}
}

func TestAdaptCoreProviderEvidenceCatalogFailureDoesNotPublishModels(t *testing.T) {
	evidence := AdaptCoreProviderEvidence("provider-a", []ModelInfo{{ID: "stale"}}, []iam.ProviderCheck{
		{Operation: iam.CheckCatalogSync, Success: false, CheckedAt: 100},
	})
	if evidence.Catalog.Status != core.CatalogFailed || len(evidence.Catalog.Models) != 0 {
		t.Fatalf("catalog = %+v", evidence.Catalog)
	}
	if evidence.Health.Status != core.ProviderHealthUnhealthy || evidence.Health.ErrorClass != core.ProviderErrorUpstream {
		t.Fatalf("health = %+v", evidence.Health)
	}
}
