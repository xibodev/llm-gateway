package providers

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	coreproviders "github.com/xibodev/llmgw-core/providers"
)

// AnonymousProviderProfile describes one provider curated for anonymous
// automation. The reviewed profiles, free-model admission and probe-model
// selection live in llmgw-core; enrollment stays gateway policy (see
// internal/api/anonymous_provider_automation.go).
type AnonymousProviderProfile = coreproviders.AnonymousProviderProfile

func AnonymousProviderProfiles() []AnonymousProviderProfile {
	return providerRegistry.AnonymousProfiles()
}

// AnonymousVerificationModel picks the probe model among rows the catalog has
// marked free; unmarked rows are never candidates.
func AnonymousVerificationModel(registryID string, rows []ModelInfo) string {
	free := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.Free {
			free = append(free, row.ID)
		}
	}
	return coreproviders.SelectVerificationModel(registryID, free)
}

func (c openAICatalog) modelsURL(base string) string {
	return strings.TrimRight(base, "/") + "/models"
}

func (c openAICatalog) normalizeAnonymousCatalog(rows []ModelInfo, items []any) ([]ModelInfo, error) {
	if !c.anonymous {
		return rows, nil
	}
	switch c.registryID {
	case "kilo_code", "llm7", "ovh_ai_endpoints":
	default:
		return rows, nil
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
		admission := coreproviders.AdmitAnonymousModel(c.registryID, byID[row.ID])
		if !admission.Free {
			continue
		}
		row.Free = true
		row.SupportedSurfaces = []string{"/chat/completions"}
		if row.Capabilities == nil {
			row.Capabilities = map[string]any{}
		}
		if admission.ContextWindow > 0 {
			row.Capabilities["context_window"] = admission.ContextWindow
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
		admission := coreproviders.AdmitAnonymousModel("pollinations", item)
		if (anonymous && !admission.Free) || !stringListContains(item["output_modalities"], "text") {
			continue
		}
		capabilities := map[string]any{"chat": true}
		if admission.Reasoning {
			capabilities["reasoning"] = true
		}
		if admission.ToolCalls {
			capabilities["tool_calls"] = true
		}
		rows = append(rows, ModelInfo{
			ID: id, Label: id, Free: admission.Free, Capabilities: capabilities,
			SupportedSurfaces: []string{"/chat/completions"},
		})
	}
	return rows, nil
}

func stringListContains(value any, wanted string) bool {
	for _, item := range stringList(value) {
		if strings.EqualFold(item, wanted) {
			return true
		}
	}
	return false
}
