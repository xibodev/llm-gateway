package providers

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

type AnonymousProviderProfile struct {
	RegistryID         string
	ProviderID         string
	RuntimeType        string
	BaseURL            string
	VerificationModels []string
}

var anonymousVerificationModels = map[string][]string{
	"opencode_zen":     {"ling-3.0-flash-fin-free", "muse-spark-1.2-contributor-free", "nemotron-3.5-lightning-free"},
	"kilo_code":        {"kilo-auto/free", "liquid/lfm-2.5-2.6b:free", "cohere/north-mini-code:free"},
	"llm7":             {"codestral-latest", "mistral-Nemo-Instruct-2407", "minimax-m2.7"},
	"ovh_ai_endpoints": {"Qwen3.8-27B", "Mistral-Nemo-Instruct-2407", "gpt-oss-20b"},
	"pollinations":     {"openai-fast"},
}

func AnonymousProviderProfiles() []AnonymousProviderProfile {
	profiles := []AnonymousProviderProfile{}
	for _, entry := range ProviderRegistry() {
		if !entry.AnonymousAutomation {
			continue
		}
		profiles = append(profiles, AnonymousProviderProfile{
			RegistryID: entry.ID, ProviderID: entry.DefaultProviderID,
			RuntimeType: entry.RuntimeType, BaseURL: entry.DefaultBaseURL,
			VerificationModels: append([]string(nil), anonymousVerificationModels[entry.ID]...),
		})
	}
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].RegistryID < profiles[j].RegistryID })
	return profiles
}

func AnonymousVerificationModel(registryID string, rows []ModelInfo) string {
	free := map[string]string{}
	for _, row := range rows {
		if row.Free {
			free[strings.ToLower(row.ID)] = row.ID
		}
	}
	for _, preferred := range anonymousVerificationModels[registryID] {
		if model := free[strings.ToLower(preferred)]; model != "" {
			return model
		}
	}
	ids := make([]string, 0, len(free))
	for _, id := range free {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if len(ids) > 0 {
		return ids[0]
	}
	return ""
}

func (p OpenAIProvider) chatURL(base string) string {
	if p.registryID == "pollinations" {
		return strings.TrimRight(base, "/") + "/v1/chat/completions"
	}
	return strings.TrimRight(base, "/") + "/chat/completions"
}

func (p OpenAIProvider) modelsURL(base string) string {
	return strings.TrimRight(base, "/") + "/models"
}

func (p OpenAIProvider) normalizeAnonymousCatalog(rows []ModelInfo, items []any) ([]ModelInfo, error) {
	if !p.anonymous {
		return rows, nil
	}
	if p.registryID == "opencode_zen" {
		return p.filterAnonymousZenModels(rows)
	}
	byID := make(map[string]map[string]any, len(items))
	for _, item := range items {
		row, _ := item.(map[string]any)
		id, _ := row["id"].(string)
		if id == "" {
			id, _ = row["name"].(string)
		}
		byID[id] = row
	}
	out := make([]ModelInfo, 0, len(rows))
	for _, row := range rows {
		raw := byID[row.ID]
		include := false
		switch p.registryID {
		case "kilo_code":
			include, _ = raw["isFree"].(bool)
			pricing, _ := raw["pricing"].(map[string]any)
			include = include && zeroStringNumber(pricing["prompt"]) && zeroStringNumber(pricing["completion"]) &&
				nestedStringListContains(raw, "architecture", "output_modalities", "text")
		case "llm7":
			tier, _ := raw["tier"].(string)
			modelType, _ := raw["model_type"].(string)
			usageBasedOnly, usageKnown := raw["usage_based_only"].(bool)
			include = strings.EqualFold(tier, "turbo") && strings.EqualFold(modelType, "chat") &&
				stringListContains(raw["schema_endpoints"], "openai") &&
				usageKnown && !usageBasedOnly
		case "ovh_ai_endpoints":
			pricing, _ := raw["pricing"].(map[string]any)
			include = intOf(raw["context_length"]) > 0 && intOf(raw["max_completion_tokens"]) > 0 &&
				zeroStringNumber(pricing["prompt"]) && zeroStringNumber(pricing["completion"])
		default:
			return rows, nil
		}
		if !include {
			continue
		}
		row.Free = true
		row.SupportedSurfaces = []string{"/chat/completions"}
		if row.Capabilities == nil {
			row.Capabilities = map[string]any{}
		}
		if contextWindow := intOf(raw["context_length"]); contextWindow > 0 {
			row.Capabilities["context_window"] = contextWindow
		}
		row.Capabilities["chat"] = true
		out = append(out, row)
	}
	return out, nil
}

func decodePollinationsCatalog(response *http.Response, anonymous bool) ([]ModelInfo, error) {
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, catalogMaxResponseBytes+1))
	if err != nil {
		return nil, catalogError("catalog_transport_error", "Provider catalog response could not be read.", response.StatusCode)
	}
	if len(raw) > catalogMaxResponseBytes {
		return nil, catalogError("catalog_not_discoverable", "Provider catalog response exceeded the size limit.", response.StatusCode)
	}
	var items []map[string]any
	if json.Unmarshal(raw, &items) != nil || items == nil {
		return nil, catalogError("catalog_invalid_json", "Provider catalog response was not valid JSON.", response.StatusCode)
	}
	rows := []ModelInfo{}
	for _, item := range items {
		id, ok := item["name"].(string)
		if !ok || strings.TrimSpace(id) == "" {
			return nil, catalogError("catalog_invalid_shape", "Provider catalog response contained an invalid model row.", response.StatusCode)
		}
		tier, _ := item["tier"].(string)
		isAnonymous := strings.EqualFold(tier, "anonymous")
		if (anonymous && !isAnonymous) || !stringListContains(item["output_modalities"], "text") {
			continue
		}
		capabilities := map[string]any{"chat": true}
		if value, ok := item["reasoning"].(bool); ok && value {
			capabilities["reasoning"] = true
		}
		if value, ok := item["tools"].(bool); ok && value {
			capabilities["tool_calls"] = true
		}
		rows = append(rows, ModelInfo{
			ID: id, Label: id, Free: isAnonymous, Capabilities: capabilities,
			SupportedSurfaces: []string{"/chat/completions"},
		})
	}
	return rows, nil
}

func zeroStringNumber(value any) bool {
	switch current := value.(type) {
	case string:
		number, err := strconv.ParseFloat(strings.TrimSpace(current), 64)
		return err == nil && number == 0 && !math.IsNaN(number) && !math.IsInf(number, 0)
	case float64:
		return current == 0 && !math.IsNaN(current) && !math.IsInf(current, 0)
	default:
		return false
	}
}

func nestedStringListContains(value map[string]any, objectKey, listKey, wanted string) bool {
	nested, _ := value[objectKey].(map[string]any)
	return stringListContains(nested[listKey], wanted)
}

func stringListContains(value any, wanted string) bool {
	for _, item := range stringList(value) {
		if strings.EqualFold(item, wanted) {
			return true
		}
	}
	return false
}
