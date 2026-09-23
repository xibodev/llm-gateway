package providers

import (
	"testing"
	"time"

	"llmgw/internal/iam"

	core "github.com/xibodev/llmgw-core"
)

func TestAdaptModelCapabilitiesMapsLegacyMetadataWithoutInventingSupport(t *testing.T) {
	discovered := time.Date(2026, time.September, 20, 10, 0, 0, 0, time.UTC)
	verified := discovered.Add(time.Hour)
	capabilities := AdaptModelCapabilities(map[string]any{
		"embedding": false, "vision": true, "tool_calls": true,
		"reasoning_effort": []string{"low", "high"}, "structured_outputs": true,
		"context_window": 128000, "max_output_tokens": float64(16384),
	}, []string{"/v1/chat/completions", "/responses", "/v1/audio/speech"}, discovered, verified)

	if capabilities.Operations.Chat != core.SupportSupported ||
		capabilities.Operations.Embeddings != core.SupportUnsupported ||
		capabilities.Operations.AudioOut != core.SupportSupported ||
		capabilities.Operations.Image != core.SupportUnknown {
		t.Fatalf("operations = %+v", capabilities.Operations)
	}
	if capabilities.Surfaces.ChatCompletions != core.SupportSupported ||
		capabilities.Surfaces.Responses != core.SupportSupported ||
		capabilities.Surfaces.Messages != core.SupportUnknown {
		t.Fatalf("surfaces = %+v", capabilities.Surfaces)
	}
	if capabilities.Inputs.Text != core.SupportSupported || capabilities.Inputs.Image != core.SupportSupported ||
		capabilities.Tools != core.SupportSupported || capabilities.Reasoning != core.SupportSupported ||
		capabilities.StructuredOutput != core.SupportSupported {
		t.Fatalf("feature mapping = %+v", capabilities)
	}
	if capabilities.Limits.ContextTokens == nil || *capabilities.Limits.ContextTokens != 128000 ||
		capabilities.Limits.MaxOutputTokens == nil || *capabilities.Limits.MaxOutputTokens != 16384 {
		t.Fatalf("limits = %+v", capabilities.Limits)
	}
	if capabilities.Provenance.Source != core.ModelCapabilitySourceInferred ||
		capabilities.Freshness.DiscoveredAt == nil || !capabilities.Freshness.DiscoveredAt.Equal(discovered) ||
		capabilities.Freshness.VerifiedAt == nil || !capabilities.Freshness.VerifiedAt.Equal(verified) {
		t.Fatalf("evidence = %+v %+v", capabilities.Provenance, capabilities.Freshness)
	}
}

func TestAdaptCoreProviderEvidenceVerifiesOnlySuccessfulMatchingModel(t *testing.T) {
	discoveredAt := int64(100)
	verifiedAt := int64(120)
	evidence := AdaptCoreProviderEvidence("provider", []ModelInfo{
		{ID: "verified", Capabilities: map[string]any{"chat": true}},
		{ID: "unverified", Capabilities: map[string]any{"image": true}},
	}, []iam.ProviderCheck{
		{Operation: iam.CheckCatalogSync, Success: true, CheckedAt: discoveredAt},
		{Operation: iam.CheckVerify, Success: false, Model: "unverified", CheckedAt: 110},
		{Operation: iam.CheckVerify, Success: true, Model: "verified", CheckedAt: verifiedAt},
	})

	if len(evidence.Catalog.Models) != 2 {
		t.Fatalf("models = %+v", evidence.Catalog.Models)
	}
	for _, model := range evidence.Catalog.Models {
		if model.Capabilities == nil || model.Capabilities.Freshness.DiscoveredAt == nil {
			t.Fatalf("missing discovered_at for %q: %+v", model.ID, model.Capabilities)
		}
		if model.ID == "verified" && (model.Capabilities.Freshness.VerifiedAt == nil || model.Capabilities.Freshness.VerifiedAt.Unix() != verifiedAt) {
			t.Fatalf("verified model freshness = %+v", model.Capabilities.Freshness)
		}
		if model.ID == "unverified" && model.Capabilities.Freshness.VerifiedAt != nil {
			t.Fatalf("failed probe produced verification: %+v", model.Capabilities.Freshness)
		}
	}
}

func TestCatalogModelsWithTypedCapabilitiesAddsDiscoverySnapshot(t *testing.T) {
	discovered := time.Date(2026, time.September, 20, 12, 0, 0, 0, time.UTC)
	legacy := []ModelInfo{{
		ID: "speech", Capabilities: map[string]any{"audio": true, "tts": true},
		SupportedSurfaces: []string{"/v1/audio/speech"},
	}}

	models := catalogModelsWithTypedCapabilities(legacy, discovered)
	if legacy[0].TypedCapabilities != nil {
		t.Fatal("catalog conversion mutated provider result")
	}
	capabilities := models[0].TypedCapabilities
	if capabilities == nil || capabilities.Operations.AudioOut != core.SupportSupported ||
		capabilities.Operations.AudioIn != core.SupportUnknown ||
		capabilities.Freshness.DiscoveredAt == nil || !capabilities.Freshness.DiscoveredAt.Equal(discovered) {
		t.Fatalf("persisted capability snapshot = %+v", capabilities)
	}
}

func TestModelCapabilityHelpersPreferTypedEvidence(t *testing.T) {
	model := ModelInfo{Capabilities: map[string]any{"vision": false, "image": false, "video": false}, TypedCapabilities: &core.ModelCapabilities{}}
	model.TypedCapabilities.Inputs.Image = core.SupportSupported
	model.TypedCapabilities.Operations.Image = core.SupportSupported
	model.TypedCapabilities.Operations.Video = core.SupportSupported
	if !ModelSupportsImageInput(model) || !ModelSupportsOperation(model, core.ModelOperationImage) || !ModelSupportsOperation(model, core.ModelOperationVideo) {
		t.Fatalf("typed capability evidence was ignored: %+v", model.TypedCapabilities)
	}
	model.TypedCapabilities.Inputs.Image = core.SupportUnsupported
	model.TypedCapabilities.Operations.Image = core.SupportUnsupported
	if ModelSupportsImageInput(model) || ModelSupportsOperation(model, core.ModelOperationImage) {
		t.Fatal("legacy metadata overrode typed unsupported evidence")
	}
}
