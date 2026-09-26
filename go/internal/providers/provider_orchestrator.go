package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	core "github.com/xibodev/llmgw-core"
	"github.com/xibodev/llmgw-core/anonymous"
	coreproviders "github.com/xibodev/llmgw-core/providers"
)

// The anonymous-provider automation runs on llmgw-core's anonymous
// orchestrator (see internal/api/anonymous_provider_automation.go), which
// reads catalogs and sends probes through the Catalog and Invoker here.

// usesApplicationAnonymousAdapter reports the profiles probed through their
// instance's own provider: core's keyless client posts to /chat/completions,
// which OpenCode Zen and Pollinations do not serve as the gateway does.
func usesApplicationAnonymousAdapter(registryID string) bool {
	return registryID == "opencode_zen" || registryID == "pollinations"
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
