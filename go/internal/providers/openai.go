package providers

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/xibodev/llm-translate"
)

// llmgw-core serves the requests of every instance on the OpenAI wire: its
// OpenAICompatible those of the OpenAI-compatible and Bedrock instances, and
// its Zen and Copilot theirs. The gateway still lists their catalogs on its
// own path, and the Copilot and Zen facades plan requests with the payload
// and adaptation helpers here.

// buildOpenAIPayload is the Chat body the gateway sends over the OpenAI
// wire: the model, the messages, the stream flag and the forwarded fields
// that are set. Core's OpenAI-compatible providers build the same body, and
// the Copilot facade hands core this one.
func buildOpenAIPayload(model string, messages []Message, stream bool, kw Kwargs) map[string]any {
	payload := map[string]any{"model": model, "messages": messages, "stream": stream}
	for _, key := range []string{
		"temperature", "top_p", "max_tokens", "max_completion_tokens",
		"stop", "tools", "tool_choice", "reasoning_effort", "stream_options",
		"metadata", "parallel_tool_calls", "thinking",
	} {
		if v, ok := kw[key]; ok && v != nil {
			payload[key] = v
		}
	}
	return payload
}

func withOpenAIOutputLimit(kw Kwargs) Kwargs {
	value, ok := kw["_max_output_tokens"]
	if !ok || value == nil {
		return kw
	}
	out := Kwargs{}
	for key, item := range kw {
		if key != "_max_output_tokens" {
			out[key] = item
		}
	}
	if out["max_tokens"] == nil && out["max_completion_tokens"] == nil {
		out["max_completion_tokens"] = value
	}
	return out
}

// openAICatalog is an OpenAI-wire catalog the gateway lists on its own path:
// GET /models at base with header, read into the rows /v1/models presents,
// with the untyped capabilities a row reports. It lists the catalogs of the
// OpenAI-compatible and Bedrock instances, of OpenCode Zen with a key and of
// GitHub Copilot, each with the credential its facade resolved, whose
// observation it reports. registryID and anonymous select the anonymous
// registry entries' rows and Pollinations' array.
type openAICatalog struct {
	base        string
	header      http.Header
	observation *CredentialObservation
	// timeout bounds a request, at ten seconds at most; zero never times out.
	timeout    float64
	registryID string
	anonymous  bool
}

func (c openAICatalog) list() ([]ModelInfo, *CredentialObservation, error) {
	observation := c.observation
	timeout := c.timeout
	if timeout > 10 {
		timeout = 10
	}
	req, _ := http.NewRequest("GET", c.modelsURL(c.base), nil)
	if c.header != nil {
		req.Header = c.header.Clone()
	}
	resp, err := httpClient(timeout).Do(req)
	if err != nil {
		return nil, observation, catalogError(
			"catalog_transport_error",
			"Provider catalog request could not reach the upstream service.",
			0,
		)
	}
	if resp.StatusCode >= 400 {
		resp.Body.Close()
		return nil, observation, catalogError(
			"catalog_http_error",
			fmt.Sprintf("Provider catalog returned HTTP %d.", resp.StatusCode),
			resp.StatusCode,
		)
	}
	if c.registryID == "pollinations" {
		rows, decodeErr := decodePollinationsCatalog(resp, c.anonymous)
		return rows, observation, decodeErr
	}
	body, err := decodeCatalogResponse(resp, "data", "id", "name")
	if err != nil {
		return nil, observation, err
	}
	items := body["data"].([]any)
	out := []ModelInfo{}
	for _, entry := range items {
		m := entry.(map[string]any)
		id, _ := m["id"].(string)
		if strings.TrimSpace(id) == "" {
			id, _ = m["name"].(string)
		}
		vendor, _ := m["vendor"].(string)
		if vendor == "" {
			vendor, _ = m["owned_by"].(string)
		}
		row := ModelInfo{ID: id, Vendor: vendor}
		if displayName, ok := m["display_name"].(string); ok && displayName != "" && displayName != id {
			row.Label = displayName
		} else if name, ok := m["name"].(string); ok && name != id {
			row.Label = name
		}
		if caps := extractCapabilities(m["capabilities"]); len(caps) > 0 {
			row.Capabilities = caps
		}
		// "supported_endpoints" is the field name this generic OpenAI-compatible
		// upstream (openai_compatible/bedrock/litellm/github_copilot) advertises
		// in its own catalog response — not this gateway's wire key, so it is
		// read as-is regardless of the supported_surfaces rename below.
		if eps := stringList(m["supported_endpoints"]); len(eps) > 0 {
			row.SupportedSurfaces = eps
		}
		out = append(out, row)
	}
	if c.anonymous {
		out, err = c.normalizeAnonymousCatalog(out, items)
		if err != nil {
			return nil, observation, err
		}
	}
	return out, observation, nil
}

func stringList(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// extractCapabilities distills a Copilot/OpenAI capabilities block into the bits
// an operator wants: reasoning_effort levels, headline feature flags, and the
// context window. Empty for models that report nothing useful.
func extractCapabilities(raw any) map[string]any {
	caps, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	out := map[string]any{}
	if supports, ok := caps["supports"].(map[string]any); ok {
		if reasoning, ok := supports["reasoning_effort"].([]any); ok && len(reasoning) > 0 {
			levels := []string{}
			for _, r := range reasoning {
				if s, ok := r.(string); ok {
					levels = append(levels, s)
				}
			}
			out["reasoning_effort"] = levels
		}
		for _, flag := range []string{"streaming", "tool_calls", "vision", "structured_outputs", "parallel_tool_calls"} {
			if v, ok := supports[flag].(bool); ok {
				out[flag] = v
			}
		}
	}
	if limits, ok := caps["limits"].(map[string]any); ok {
		if ctx := intOf(limits["max_context_window_tokens"]); ctx > 0 {
			out["context_window"] = ctx
		} else if ctx := intOf(limits["max_prompt_tokens"]); ctx > 0 {
			out["context_window"] = ctx
		}
		if mx := intOf(limits["max_output_tokens"]); mx > 0 {
			out["max_output_tokens"] = mx
		}
	}
	if family, ok := caps["family"].(string); ok && family != "" {
		out["family"] = family
	}
	return out
}

// ---- API adaptation (opt-in via force_api_support) ---------------------- //

type adaptPlan struct {
	endpoint        string // "chat" | "responses"
	renameMaxTokens bool   // chat path: send max_completion_tokens instead of max_tokens
}

func kwBool(v any) bool { b, _ := v.(bool); return b }

// adaptEnabled resolves whether adaptation is on: a per-request force_api_support
// flag overrides the provider-level opt-in, configured.
func adaptEnabled(kw Kwargs, configured bool) bool {
	if v, ok := kw["_force_api_support"]; ok {
		return kwBool(v)
	}
	return configured
}

// adaptPlanFor is the plan for the catalog row mi, which ok says exists.
func adaptPlanFor(mi ModelInfo, ok bool) adaptPlan {
	if !ok {
		return adaptPlan{endpoint: "chat"}
	}
	ep := translate.PreferredEndpoint(mi.SupportedSurfaces)
	rename := ep == "chat" && hasCapability(mi, "reasoning_effort")
	return adaptPlan{endpoint: ep, renameMaxTokens: rename}
}

func hasCapability(mi ModelInfo, key string) bool {
	if mi.Capabilities == nil {
		return false
	}
	_, ok := mi.Capabilities[key]
	return ok
}

func withRenamedMaxTokens(kw Kwargs) Kwargs {
	if _, ok := kw["max_tokens"]; !ok {
		return kw
	}
	out := make(Kwargs, len(kw))
	for k, v := range kw {
		out[k] = v
	}
	out["max_completion_tokens"] = out["max_tokens"]
	delete(out, "max_tokens")
	return out
}

func chatToResponsesWithReport(model string, messages []Message, kw Kwargs) translate.ConversionResult[map[string]any] {
	reportable := make(Kwargs, len(kw))
	for key, value := range kw {
		if !strings.HasPrefix(key, "_") {
			reportable[key] = value
		}
	}
	return translate.ChatToResponsesWithReport(model, messages, reportable, false)
}

// listsResponses reports a catalog row's surfaces that include Responses.
func listsResponses(surfaces []string) bool {
	for _, endpoint := range surfaces {
		switch strings.ToLower(strings.TrimSpace(endpoint)) {
		case "/responses", "/v1/responses", "ws:/responses":
			return true
		}
	}
	return false
}
