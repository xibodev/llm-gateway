package providers

import (
	"context"
	"fmt"
	"strings"

	core "github.com/xibodev/llmgw-core"
	coreproviders "github.com/xibodev/llmgw-core/providers"

	"llmgw/internal/iam"
)

type providerEvidenceGenerationKey struct{}

func WithProviderEvidenceGeneration(ctx context.Context, generation int64) context.Context {
	return context.WithValue(ctx, providerEvidenceGenerationKey{}, generation)
}

// NewGatewayProviderOrchestrator builds the shared connector surface for the
// reviewed anonymous profiles supplied by the application registry.
func NewGatewayProviderOrchestrator(profiles []AnonymousProviderProfile) (*core.ProviderOrchestrator, error) {
	orchestrator := core.NewProviderOrchestrator()
	for _, profile := range profiles {
		entry, ok := RegistryProviderByID(profile.RegistryID)
		if !ok || !entry.AnonymousAutomation || entry.ID == "openai_codex" ||
			!strings.EqualFold(profile.RuntimeType, "openai_compatible") {
			return nil, fmt.Errorf("provider profile %q is not eligible for anonymous orchestration", profile.RegistryID)
		}

		adapter := coreproviders.NewAnonymousOpenAICompatibleAdapter(profile.BaseURL, nil)
		adapter.DiscoverModels = applicationModelDiscoverer(profile.ProviderID)
		adapter.SelectProbeTargets = anonymousProbeSelector(profile)
		if usesApplicationAnonymousAdapter(profile.RegistryID) {
			adapter.Complete = applicationCompletionRuntime(profile.ProviderID)
		}
		if err := orchestrator.Register(profile.ProviderID, adapter); err != nil {
			return nil, err
		}
	}
	return orchestrator, nil
}

func usesApplicationAnonymousAdapter(registryID string) bool {
	return registryID == "opencode_zen" || registryID == "pollinations"
}

func applicationModelDiscoverer(providerID string) core.ProviderModelDiscoverer {
	return func(ctx context.Context, _ core.ProviderConnection) ([]core.ModelInfo, error) {
		rows, _, err := RefreshCatalogForPrincipalWithError(providerID, gatewayCaller())
		if err != nil {
			_, _, status := CatalogFailure(err)
			return nil, core.NewProviderOperationError("provider catalog", status, "", err)
		}
		models := make([]core.ModelInfo, 0, len(rows))
		modelIDs := make([]string, 0, len(rows))
		for _, row := range rows {
			modelIDs = append(modelIDs, row.ID)
			models = append(models, core.ModelInfo{
				ID: row.ID, Object: "model", OwnedBy: row.Vendor, Description: row.Label,
			})
		}
		generation, ok := ctx.Value(providerEvidenceGenerationKey{}).(int64)
		if !ok {
			return nil, fmt.Errorf("provider evidence generation is required")
		}
		if err := iam.ReconcileProviderModelCatalog(
			providerID, "", iam.ModelEvidenceCompletion, modelIDs, generation,
		); err != nil {
			return nil, fmt.Errorf("prepare model evidence: %w", err)
		}
		return models, nil
	}
}

func anonymousProbeSelector(profile AnonymousProviderProfile) core.ProviderProbeSelector {
	return func(_ core.ProviderConnection, models []core.ModelInfo) []core.Target {
		seen := make(map[string]bool, len(models))
		targets := make([]core.Target, 0, len(models))
		for _, model := range models {
			id := strings.TrimSpace(model.ID)
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			targets = append(targets, core.Target{Provider: profile.ProviderID, Model: id})
		}
		return targets
	}
}

func applicationCompletionRuntime(providerID string) core.ProviderCompletionRuntime {
	return func(ctx context.Context, _ core.ProviderConnection, target core.Target, payload map[string]any) (map[string]any, error) {
		provider, err := GetProvider(providerID)
		if err != nil {
			return nil, core.NewProviderOperationError("provider initialization", 0, "", err)
		}
		messages := []Message{}
		for _, raw := range anySlice(payload["messages"]) {
			if message, ok := raw.(map[string]any); ok {
				messages = append(messages, message)
			}
		}
		kwargs := Kwargs{}
		for key, value := range payload {
			if key != "messages" {
				kwargs[key] = value
			}
		}
		response, err := CompleteProviderContext(ctx, provider, target.Model, messages, kwargs)
		if err != nil {
			return nil, core.NewProviderOperationError(
				"provider completion", UpstreamStatus(err), InvocationRetryAfter(err), err,
			)
		}
		return response, nil
	}
}

func anySlice(value any) []any {
	items, _ := value.([]any)
	return items
}
