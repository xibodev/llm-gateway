package providers

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	core "github.com/xibodev/llmgw-core"
)

// AdaptModelCapabilities converts the gateway's compatibility metadata into the
// bounded core contract. Missing keys remain unknown; only explicit false values
// become unsupported.
func AdaptModelCapabilities(
	legacy map[string]any, surfaces []string, discoveredAt, verifiedAt time.Time,
) *core.ModelCapabilities {
	capabilities := &core.ModelCapabilities{
		SchemaVersion: core.ModelCapabilitiesSchemaVersion,
		Provenance: core.ModelCapabilityProvenance{
			Source: core.ModelCapabilitySourceInferred, Confidence: core.ModelCapabilityConfidenceMedium,
		},
	}
	capabilities.Operations.Chat = legacySupport(legacy, "chat")
	capabilities.Operations.Embeddings = legacySupport(legacy, "embedding", "embeddings")
	capabilities.Operations.Image = legacySupport(legacy, "image")
	capabilities.Operations.AudioIn = legacySupport(legacy, "transcription", "stt", "asr", "audio_in")
	capabilities.Operations.AudioOut = legacySupport(legacy, "tts", "speech", "audio_out")
	capabilities.Operations.Video = legacySupport(legacy, "video")
	capabilities.Operations.TokenCount = legacySupport(legacy, "token_count")

	capabilities.Inputs.Text = legacySupport(legacy, "text")
	capabilities.Inputs.Image = legacySupport(legacy, "vision")
	capabilities.Tools = legacySupport(legacy, "tool_calls", "tools")
	capabilities.Reasoning = legacySupport(legacy, "reasoning", "reasoning_effort")
	capabilities.StructuredOutput = legacySupport(legacy, "structured_outputs", "structured_output")
	capabilities.Streaming = legacySupport(legacy, "streaming")
	capabilities.StatefulResponses = legacySupport(legacy, "stateful_responses")

	for _, raw := range surfaces {
		surface := strings.ToLower(strings.TrimSpace(raw))
		switch {
		case surface == "/chat/completions" || surface == "/v1/chat/completions":
			capabilities.Surfaces.ChatCompletions = core.SupportSupported
			markSupported(&capabilities.Operations.Chat)
		case surface == "/responses" || surface == "/v1/responses" || surface == "ws:/responses":
			capabilities.Surfaces.Responses = core.SupportSupported
			markSupported(&capabilities.Operations.Chat)
		case surface == "/messages" || surface == "/v1/messages":
			capabilities.Surfaces.Messages = core.SupportSupported
			markSupported(&capabilities.Operations.Chat)
		case strings.Contains(surface, "/embeddings"):
			markSupported(&capabilities.Operations.Embeddings)
		case strings.Contains(surface, "/images/"):
			markSupported(&capabilities.Operations.Image)
		case strings.Contains(surface, "/audio/transcriptions"):
			markSupported(&capabilities.Operations.AudioIn)
		case strings.Contains(surface, "/audio/speech"):
			markSupported(&capabilities.Operations.AudioOut)
		case strings.Contains(surface, "/videos/"):
			markSupported(&capabilities.Operations.Video)
		case strings.Contains(surface, "count_tokens"):
			markSupported(&capabilities.Operations.TokenCount)
		}
	}
	if capabilities.Operations.Chat == core.SupportSupported && capabilities.Inputs.Text == core.SupportUnknown {
		capabilities.Inputs.Text = core.SupportSupported
	}
	capabilities.Limits.ContextTokens = legacyInt64(legacy, "context_window", "context_tokens")
	capabilities.Limits.MaxOutputTokens = legacyInt64(legacy, "max_output_tokens")
	if !discoveredAt.IsZero() {
		discovered := discoveredAt.UTC()
		capabilities.Freshness.DiscoveredAt = &discovered
	}
	if !verifiedAt.IsZero() {
		verified := verifiedAt.UTC()
		capabilities.Freshness.VerifiedAt = &verified
	}
	return capabilities
}

func legacySupport(values map[string]any, keys ...string) core.Support {
	foundUnsupported := false
	for _, key := range keys {
		value, found := values[key]
		if !found {
			continue
		}
		if enabled, ok := value.(bool); ok {
			if enabled {
				return core.SupportSupported
			}
			foundUnsupported = true
			continue
		}
		switch current := value.(type) {
		case string:
			if strings.TrimSpace(current) != "" {
				return core.SupportSupported
			}
		case []string:
			if len(current) > 0 {
				return core.SupportSupported
			}
		case []any:
			if len(current) > 0 {
				return core.SupportSupported
			}
		}
	}
	if foundUnsupported {
		return core.SupportUnsupported
	}
	return core.SupportUnknown
}

func legacyInt64(values map[string]any, keys ...string) *int64 {
	for _, key := range keys {
		value, found := values[key]
		if !found {
			continue
		}
		var parsed int64
		switch current := value.(type) {
		case int:
			parsed = int64(current)
		case int64:
			parsed = current
		case float64:
			parsed = int64(current)
		case json.Number:
			parsed, _ = current.Int64()
		case string:
			parsed, _ = strconv.ParseInt(current, 10, 64)
		}
		if parsed > 0 {
			return &parsed
		}
	}
	return nil
}

func markSupported(value *core.Support) {
	*value = core.SupportSupported
}
