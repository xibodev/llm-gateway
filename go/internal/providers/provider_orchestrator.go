package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	core "github.com/xibodev/llmgw-core"
	"github.com/xibodev/llmgw-core/anonymous"
	coreproviders "github.com/xibodev/llmgw-core/providers"

	"llmgw/internal/iam"
)

type providerEvidenceGenerationKey struct{}

func WithProviderEvidenceGeneration(ctx context.Context, generation int64) context.Context {
	return context.WithValue(ctx, providerEvidenceGenerationKey{}, generation)
}

// NewGatewayProviderOrchestrator builds the shared connector surface for the
// reviewed anonymous profiles supplied by the application registry. Its
// adapters discover and complete through this Runtime.
func (rt *Runtime) NewGatewayProviderOrchestrator(profiles []AnonymousProviderProfile) (*core.ProviderOrchestrator, error) {
	orchestrator := core.NewProviderOrchestrator()
	for _, profile := range profiles {
		entry, ok := RegistryProviderByID(profile.RegistryID)
		if !ok || !entry.AnonymousAutomation || entry.ID == "openai_codex" ||
			!strings.EqualFold(profile.RuntimeType, "openai_compatible") {
			return nil, fmt.Errorf("provider profile %q is not eligible for anonymous orchestration", profile.RegistryID)
		}

		adapter := coreproviders.NewAnonymousOpenAICompatibleAdapter(profile.BaseURL, nil)
		adapter.DiscoverModels = rt.applicationModelDiscoverer(profile.ProviderID)
		adapter.SelectProbeTargets = anonymousProbeSelector(profile)
		if usesApplicationAnonymousAdapter(profile.RegistryID) {
			adapter.Complete = rt.applicationCompletionRuntime(profile.ProviderID)
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

func (rt *Runtime) applicationModelDiscoverer(providerID string) core.ProviderModelDiscoverer {
	catalog := rt.AnonymousCatalog()
	return func(ctx context.Context, _ core.ProviderConnection) ([]core.ModelInfo, error) {
		models, err := catalog.Discover(ctx, gatewayCaller(), providerID)
		if err != nil {
			return nil, err
		}
		modelIDs := make([]string, 0, len(models))
		for _, model := range models {
			modelIDs = append(modelIDs, model.ID)
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

func (rt *Runtime) applicationCompletionRuntime(providerID string) core.ProviderCompletionRuntime {
	return func(ctx context.Context, _ core.ProviderConnection, target core.Target, payload map[string]any) (map[string]any, error) {
		return rt.applicationProbe(ctx, gatewayCaller(), providerID, target.Model, payload)
	}
}

// AnonymousCatalog is the Catalog of llmgw-core's anonymous orchestrator on
// the gateway's catalog path. It refreshes the instance's catalog, so a check
// reports the models the instance's transport admits now.
func (rt *Runtime) AnonymousCatalog() anonymous.Catalog {
	return anonymous.CatalogFunc(func(_ context.Context, caller core.Caller, instance string) ([]core.ModelInfo, error) {
		rows, _, err := rt.RefreshCatalogForPrincipalWithError(instance, caller)
		if err != nil {
			// A CatalogError does not classify itself, so its status
			// reaches the orchestrator in an operation error.
			_, _, status := CatalogFailure(err)
			return nil, core.NewProviderOperationError("provider catalog", status, "", err)
		}
		models := make([]core.ModelInfo, 0, len(rows))
		for _, row := range rows {
			models = append(models, core.ModelInfo{
				ID: row.ID, Object: "model", OwnedBy: row.Vendor, Description: row.Label,
			})
		}
		return models, nil
	})
}

// AnonymousInvoker is the anonymous orchestrator's Invoker for profiles. It
// sends each probe on the path the gateway has always probed the profile on:
// OpenCode Zen and Pollinations through the instance's own provider, whose
// transport serves their bespoke APIs, and every other profile with
// llmgw-core's keyless OpenAI-compatible client at the profile's base URL.
func (rt *Runtime) AnonymousInvoker(profiles []AnonymousProviderProfile) anonymous.Invoker {
	probes := make(map[string]anonymousProbe, len(profiles))
	for _, profile := range profiles {
		probes[profile.ProviderID] = keylessProbe(profile.BaseURL)
		if usesApplicationAnonymousAdapter(profile.RegistryID) {
			probes[profile.ProviderID] = rt.applicationProbe
		}
	}
	return anonymous.InvokerFunc(func(ctx context.Context, caller core.Caller, instance string, request core.Request) (core.Response, error) {
		probe, ok := probes[instance]
		if !ok {
			return core.Response{}, fmt.Errorf("provider %q has no anonymous profile", instance)
		}
		// The orchestrator encodes the connector's payload with the model and
		// streaming off. Both paths were handed the payload alone, and both
		// complete without streaming, so each gets the payload it always got.
		var payload map[string]any
		if err := json.Unmarshal(request.Body, &payload); err != nil {
			return core.Response{}, fmt.Errorf("decode the probe: %w", err)
		}
		delete(payload, "model")
		delete(payload, "stream")
		answer, err := probe(ctx, caller, instance, request.Model, payload)
		if err != nil {
			return core.Response{}, probeError(err)
		}
		body, err := json.Marshal(answer)
		if err != nil {
			return core.Response{}, fmt.Errorf("encode the probe's answer: %w", err)
		}
		return core.Response{Body: body, ContentType: core.ContentTypeJSON}, nil
	})
}

// anonymousProbe completes one probe of model on instance for caller.
type anonymousProbe func(ctx context.Context, caller core.Caller, instance, model string, payload map[string]any) (map[string]any, error)

// keylessProbe probes with llmgw-core's keyless OpenAI-compatible client,
// which posts to baseURL's /chat/completions without a credential, a retry or
// a circuit. Core deprecates the client as the orchestrator's adapter, which
// it no longer is; it remains the probe of the profiles whose API it speaks.
func keylessProbe(baseURL string) anonymousProbe {
	client := coreproviders.NewAnonymousOpenAICompatibleAdapter(baseURL, nil)
	return func(ctx context.Context, _ core.Caller, instance, model string, payload map[string]any) (map[string]any, error) {
		connection := core.ProviderConnection{
			ProviderID: instance, Kind: core.ProviderConnectionAnonymous, AuthKind: core.ProviderAuthAnonymous,
		}
		return client.Complete(ctx, connection, core.Target{Provider: instance, Model: model}, payload)
	}
}

// applicationProbe probes through the instance's own provider, so the probe
// takes the instance's request path, retries and circuit.
func (rt *Runtime) applicationProbe(
	ctx context.Context, caller core.Caller, instance, model string, payload map[string]any,
) (map[string]any, error) {
	provider, err := rt.GetProviderForPrincipal(instance, caller)
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
	response, err := CompleteProviderContext(ctx, provider, model, messages, kwargs)
	if err != nil {
		return nil, core.NewProviderOperationError(
			"provider completion", UpstreamStatus(err), InvocationRetryAfter(err), err,
		)
	}
	return response, nil
}

// probeError is err as the orchestrator is to read a failed probe: by the
// status and the Retry-After of the probe's operation error. The orchestrator
// reads a failure only through core.ClassifyError, and an operation error
// classifies without its Retry-After, which the automation reports. The
// orchestrator rounds the delay up to whole seconds, so an HTTP-date's is cut
// to whole seconds first and reads as the gateway read it.
func probeError(err error) error {
	var operation *core.ProviderOperationError
	if !errors.As(err, &operation) {
		return err
	}
	classification := operation.ProviderErrorClassification()
	classification.RetryAfter = retryAfterDelay(operation.Failure.RetryAfter, time.Now()).Truncate(time.Second)
	return &core.ProviderError{Message: operation.Error(), Classification: classification, Cause: err}
}

func anySlice(value any) []any {
	items, _ := value.([]any)
	return items
}
