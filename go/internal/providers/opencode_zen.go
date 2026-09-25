package providers

import (
	"context"
	"strings"

	core "github.com/xibodev/llmgw-core"
	corezen "github.com/xibodev/llmgw-core/providers/zen"
)

func ensureZenInvocation(ctx context.Context) (context.Context, error) {
	return corezen.EnsureInvocationIdentity(ctx, nil)
}

// ensureProviderZenInvocation gives a Zen provider's operation the invocation
// identity every request of it carries, retries included.
func ensureProviderZenInvocation(ctx context.Context, provider Provider) (context.Context, error) {
	for provider != nil {
		if _, ok := provider.(*zenProvider); ok {
			return ensureZenInvocation(ctx)
		}
		unwrapper, ok := provider.(interface{ Unwrap() Provider })
		if !ok {
			return ctx, nil
		}
		provider = unwrapper.Unwrap()
	}
	return ctx, nil
}

const (
	zenAccessAnonymous = "anonymous"
	zenAccessKeyed     = "keyed"
	zenRequestOrdinary = string(corezen.RequestOrdinary)
	zenRequestTitle    = string(corezen.RequestTitle)
)

func zenAccessMode(isZen, anonymous bool) string {
	if !isZen {
		return "not_zen"
	}
	if anonymous {
		return zenAccessAnonymous
	}
	return zenAccessKeyed
}

func zenChatRequestMode(messages []Message) string {
	return string(corezen.ClassifyChat(messages))
}

func zenResponsesRequestMode(payload map[string]any) string {
	return string(corezen.ClassifyResponses(payload))
}

func newAnonymousZenClient(baseURL, metadataURL string, timeout float64) (*corezen.Client, error) {
	if timeout <= 0 || timeout > 10 {
		timeout = 10
	}
	return corezen.New(corezen.Config{
		Endpoints: corezen.Endpoints{
			BaseURL:     baseURL,
			MetadataURL: metadataURL,
		},
		HTTPClient:      httpClient(timeout),
		CapabilityTTL:   catalogTTL,
		MaxResponseSize: catalogMaxResponseBytes,
	})
}

// anonymousZenModels lists the models anonymous access admits: those the
// models.dev catalog prices at zero and the live catalog lists, each marked
// free and given the endpoint of its native surface.
func anonymousZenModels(baseURL, metadataURL string, timeout float64) ([]ModelInfo, error) {
	client, err := newAnonymousZenClient(baseURL, metadataURL, timeout)
	if err != nil {
		return nil, catalogError("catalog_metadata_transport_error", "OpenCode model metadata URL is invalid.", 0)
	}
	evidence, err := client.DiscoverVerified(context.Background())
	if err != nil {
		return nil, anonymousZenCatalogError(err)
	}

	out := make([]ModelInfo, 0, len(evidence.Models))
	for _, model := range evidence.Models {
		row := ModelInfo{ID: model.ID, Vendor: corezen.DefaultProviderID}
		row.Free = true
		if strings.TrimSpace(model.Description) != "" && model.Description != model.ID {
			row.Label = model.Description
		}
		row.Capabilities = zenLegacyCapabilities(model.Capabilities)
		row.TypedCapabilities = model.Capabilities
		if surface, ok := corezen.NativeSurface(model); ok {
			switch surface {
			case core.ModelSurfaceResponses:
				row.SupportedSurfaces = []string{"/responses"}
			case core.ModelSurfaceChatCompletions:
				row.SupportedSurfaces = []string{"/chat/completions"}
			}
		}
		out = append(out, row)
	}
	return out, nil
}

func anonymousZenCatalogError(err error) error {
	message := err.Error()
	switch {
	case strings.Contains(message, "HTTP"):
		return catalogError("catalog_metadata_http_error", "OpenCode model metadata returned an HTTP error.", 0)
	case strings.Contains(message, "exceeds size limit"):
		return catalogError("catalog_metadata_not_discoverable", "OpenCode model metadata exceeded the size limit.", 0)
	case strings.Contains(message, "decode models.dev"):
		return catalogError("catalog_metadata_invalid_json", "OpenCode model metadata was not valid JSON.", 0)
	case strings.Contains(message, "models.dev"):
		return catalogError("catalog_metadata_invalid_shape", "OpenCode model metadata had an invalid shape.", 0)
	default:
		return catalogError("catalog_metadata_transport_error", "OpenCode model metadata could not be reached.", 0)
	}
}

func zenLegacyCapabilities(capabilities *core.ModelCapabilities) map[string]any {
	if capabilities == nil {
		return nil
	}
	out := map[string]any{}
	if capabilities.Tools == core.SupportSupported {
		out["tool_calls"] = true
	}
	if capabilities.Reasoning == core.SupportSupported {
		out["reasoning"] = true
	}
	if capabilities.StructuredOutput == core.SupportSupported {
		out["structured_outputs"] = true
	}
	if capabilities.Inputs.Image == core.SupportSupported {
		out["vision"] = true
	}
	if capabilities.Limits.ContextTokens != nil {
		out["context_window"] = *capabilities.Limits.ContextTokens
	}
	if capabilities.Limits.MaxOutputTokens != nil {
		out["max_output_tokens"] = *capabilities.Limits.MaxOutputTokens
	}
	return out
}

func isZenBaseURL(base string) bool {
	u := strings.ToLower(strings.TrimSpace(base))
	return strings.HasPrefix(u, "https://opencode.ai/zen") || strings.HasPrefix(u, "http://opencode.ai/zen")
}
