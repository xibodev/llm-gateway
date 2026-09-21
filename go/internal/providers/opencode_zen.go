package providers

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	zenAccessAnonymous = "anonymous"
	zenAccessKeyed     = "keyed"
	zenRequestOrdinary = "ordinary_assistant"
	zenRequestTitle    = "title_generation"
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
	for _, message := range messages {
		role, _ := message["role"].(string)
		content, _ := message["content"].(string)
		if (role == "system" || role == "developer") && strings.Contains(content, "You are a title generator") {
			return zenRequestTitle
		}
	}
	return zenRequestOrdinary
}

func zenResponsesRequestMode(payload map[string]any) string {
	instructions, _ := payload["instructions"].(string)
	if strings.Contains(instructions, "You are a title generator") {
		return zenRequestTitle
	}
	return zenRequestOrdinary
}

const openCodeModelsDevURL = "https://models.dev/api.json"

type openCodeModelsDevProvider struct {
	NPM    string                            `json:"npm"`
	Models map[string]openCodeModelsDevModel `json:"models"`
}

type openCodeModelsDevModel struct {
	ID               string         `json:"id"`
	Name             string         `json:"name"`
	Family           string         `json:"family"`
	Status           string         `json:"status"`
	Cost             map[string]any `json:"cost"`
	Reasoning        bool           `json:"reasoning"`
	ToolCall         bool           `json:"tool_call"`
	StructuredOutput bool           `json:"structured_output"`
	Modalities       struct {
		Input []string `json:"input"`
	} `json:"modalities"`
	Limit struct {
		Context int `json:"context"`
		Output  int `json:"output"`
	} `json:"limit"`
	Provider *struct {
		NPM string `json:"npm"`
	} `json:"provider"`
}

func (p OpenAIProvider) filterAnonymousZenModels(upstream []ModelInfo) ([]ModelInfo, error) {
	metadataURL := strings.TrimSpace(p.metadataURL)
	if metadataURL == "" {
		metadataURL = openCodeModelsDevURL
	}
	request, err := http.NewRequest(http.MethodGet, metadataURL, nil)
	if err != nil {
		return nil, catalogError("catalog_metadata_transport_error", "OpenCode model metadata URL is invalid.", 0)
	}
	request.Header.Set("Accept", "application/json")
	timeout := p.Timeout
	if timeout <= 0 || timeout > 10 {
		timeout = 10
	}
	response, err := httpClient(timeout).Do(request)
	if err != nil {
		return nil, catalogError("catalog_metadata_transport_error", "OpenCode model metadata could not be reached.", 0)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, catalogError(
			"catalog_metadata_http_error",
			fmt.Sprintf("OpenCode model metadata returned HTTP %d.", response.StatusCode),
			response.StatusCode,
		)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, catalogMaxResponseBytes+1))
	if err != nil {
		return nil, catalogError("catalog_metadata_transport_error", "OpenCode model metadata could not be read.", response.StatusCode)
	}
	if len(raw) > catalogMaxResponseBytes {
		return nil, catalogError("catalog_metadata_not_discoverable", "OpenCode model metadata exceeded the size limit.", response.StatusCode)
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(raw, &root) != nil {
		return nil, catalogError("catalog_metadata_invalid_json", "OpenCode model metadata was not valid JSON.", response.StatusCode)
	}
	providerRaw, ok := root["opencode"]
	if !ok {
		return nil, catalogError("catalog_metadata_invalid_shape", "OpenCode model metadata did not contain the opencode provider.", response.StatusCode)
	}
	var provider openCodeModelsDevProvider
	if json.Unmarshal(providerRaw, &provider) != nil || provider.Models == nil {
		return nil, catalogError("catalog_metadata_invalid_shape", "OpenCode model metadata had an invalid provider shape.", response.StatusCode)
	}

	selected := make(map[string]openCodeModelsDevModel)
	for key, model := range provider.Models {
		if model.ID != key || !openCodeModelIsActive(model.Status) || !zeroModelsDevCost(model.Cost) {
			continue
		}
		npm := strings.TrimSpace(provider.NPM)
		if model.Provider != nil && strings.TrimSpace(model.Provider.NPM) != "" {
			npm = strings.TrimSpace(model.Provider.NPM)
		}
		switch npm {
		case "@ai-sdk/openai", "@ai-sdk/openai-compatible":
			selected[key] = model
		}
	}

	out := make([]ModelInfo, 0, len(selected))
	for _, row := range upstream {
		model, ok := selected[row.ID]
		if !ok {
			continue
		}
		row.Free = true
		if strings.TrimSpace(model.Name) != "" && model.Name != model.ID {
			row.Label = model.Name
		}
		row.Capabilities = openCodeModelCapabilities(model)
		npm := strings.TrimSpace(provider.NPM)
		if model.Provider != nil && strings.TrimSpace(model.Provider.NPM) != "" {
			npm = strings.TrimSpace(model.Provider.NPM)
		}
		if npm == "@ai-sdk/openai" {
			row.SupportedSurfaces = []string{"/responses"}
		} else {
			row.SupportedSurfaces = []string{"/chat/completions"}
		}
		out = append(out, row)
	}
	return out, nil
}

func openCodeModelIsActive(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "", "beta":
		return true
	default:
		return false
	}
}

func zeroModelsDevCost(cost map[string]any) bool {
	if cost == nil || !zeroNumber(cost["input"]) || !zeroNumber(cost["output"]) {
		return false
	}
	for key, value := range cost {
		switch key {
		case "input", "output", "reasoning", "cache_read", "cache_write", "input_audio", "output_audio":
			if !zeroNumber(value) {
				return false
			}
		case "context_over_200k":
			nested, ok := value.(map[string]any)
			if !ok || !zeroModelsDevCost(nested) {
				return false
			}
		case "tiers":
			tiers, ok := value.([]any)
			if !ok {
				return false
			}
			for _, rawTier := range tiers {
				tier, ok := rawTier.(map[string]any)
				if !ok {
					return false
				}
				copy := make(map[string]any, len(tier))
				for tierKey, tierValue := range tier {
					if tierKey != "tier" {
						copy[tierKey] = tierValue
					}
				}
				if !zeroModelsDevCost(copy) {
					return false
				}
			}
		default:
			return false
		}
	}
	return true
}

var openCodeCoreChatTools = []any{
	map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "bash",
			"description": "Executes a given bash/powershell command.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{
						"type":        "string",
						"description": "The command to execute",
					},
				},
				"required": []any{"command"},
			},
		},
	},
	map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "read",
			"description": "Read a file from the local filesystem.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"filePath": map[string]any{
						"type":        "string",
						"description": "The absolute path to the file to read",
					},
				},
				"required": []any{"filePath"},
			},
		},
	},
}

var openCodeCoreResponsesTools = []any{
	map[string]any{
		"type":        "function",
		"name":        "bash",
		"description": "Executes a given bash/powershell command.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "The command to execute",
				},
			},
			"required": []any{"command"},
		},
	},
	map[string]any{
		"type":        "function",
		"name":        "read",
		"description": "Read a file from the local filesystem.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"filePath": map[string]any{
					"type":        "string",
					"description": "The absolute path to the file to read",
				},
			},
			"required": []any{"filePath"},
		},
	},
}

func ensureOpenCodeChatTools(tools []any) []any {
	hasBash := false
	hasRead := false
	for _, item := range tools {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		if fn, ok := m["function"].(map[string]any); ok {
			if n, ok := fn["name"].(string); ok && n != "" {
				name = n
			}
		}
		if name == "bash" {
			hasBash = true
		}
		if name == "read" {
			hasRead = true
		}
	}
	out := append([]any(nil), tools...)
	if !hasBash {
		out = append(out, openCodeCoreChatTools[0])
	}
	if !hasRead {
		out = append(out, openCodeCoreChatTools[1])
	}
	return out
}

func ensureOpenCodeResponsesTools(tools []any) []any {
	hasBash := false
	hasRead := false
	for _, item := range tools {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		if fn, ok := m["function"].(map[string]any); ok {
			if n, ok := fn["name"].(string); ok && n != "" {
				name = n
			}
		}
		if name == "bash" {
			hasBash = true
		}
		if name == "read" {
			hasRead = true
		}
	}
	out := append([]any(nil), tools...)
	if !hasBash {
		out = append(out, openCodeCoreResponsesTools[0])
	}
	if !hasRead {
		out = append(out, openCodeCoreResponsesTools[1])
	}
	return out
}

func adaptAnonymousZenResponsesPayload(payload map[string]any) map[string]any {
	out := cloneMap(payload)
	out["stream"] = true
	if zenResponsesRequestMode(out) == zenRequestTitle {
		return out
	}

	var callerTools []any
	if rawTools, ok := out["tools"].([]any); ok {
		callerTools = rawTools
	}
	out["tools"] = ensureOpenCodeResponsesTools(callerTools)
	if len(callerTools) == 0 {
		if _, supplied := out["tool_choice"]; !supplied {
			out["tool_choice"] = "none"
		}
	}
	return out
}

func zeroNumber(value any) bool {
	number, ok := value.(float64)
	return ok && number == 0
}

func isZenBaseURL(base string) bool {
	u := strings.ToLower(strings.TrimSpace(base))
	return strings.HasPrefix(u, "https://opencode.ai/zen") || strings.HasPrefix(u, "http://opencode.ai/zen")
}

func openCodeModelCapabilities(model openCodeModelsDevModel) map[string]any {
	capabilities := map[string]any{}
	if model.Family != "" {
		capabilities["family"] = model.Family
	}
	if model.Limit.Context > 0 {
		capabilities["context_window"] = model.Limit.Context
	}
	if model.Limit.Output > 0 {
		capabilities["max_output_tokens"] = model.Limit.Output
	}
	if model.ToolCall {
		capabilities["tool_calls"] = true
	}
	if model.Reasoning {
		capabilities["reasoning"] = true
	}
	if model.StructuredOutput {
		capabilities["structured_outputs"] = true
	}
	for _, modality := range model.Modalities.Input {
		if modality == "image" {
			capabilities["vision"] = true
			break
		}
	}
	return capabilities
}

const openCodeAnonymousUserAgent = "opencode/1.18.31 ai-sdk/provider-utils/4.0.40 runtime/bun/1.3.14"

func adaptAnonymousZenChat(messages []Message, kw Kwargs) ([]Message, Kwargs) {
	outKw := Kwargs{}
	for k, v := range kw {
		outKw[k] = v
	}

	var callerTools []any
	if raw, ok := outKw["tools"].([]any); ok {
		callerTools = raw
	}
	if zenChatRequestMode(messages) == zenRequestTitle {
		return messages, outKw
	}
	outKw["tools"] = ensureOpenCodeChatTools(callerTools)
	if len(callerTools) == 0 {
		if _, supplied := outKw["tool_choice"]; !supplied {
			outKw["tool_choice"] = "none"
		}
	}
	return messages, outKw
}
