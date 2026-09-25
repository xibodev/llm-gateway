package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"llmgw/internal/config"

	"github.com/xibodev/llm-translate"
	core "github.com/xibodev/llmgw-core"
)

// OpenAIProvider speaks the OpenAI wire standard (/chat/completions, /models)
// against any backend. Authentication + base URL are supplied by an OpenAIAuth
// strategy, so this one transport backs every OpenAI-compatible provider type:
// openai_compatible, bedrock and litellm (which differ only in their auth
// strategy).
//
// When forceAdapt (provider config) or a per-request force_api_support flag is
// set, it may translate a /chat/completions request to the model's actually
// supported OpenAI-family endpoint (e.g. /responses for gpt-5.5), decided from
// the persisted /models catalog. Off by default — adaptation is never native.
type OpenAIProvider struct {
	auth       OpenAIAuth
	Timeout    float64
	forceAdapt bool
	providerID string
	caller     core.Caller
	registryID string
	anonymous  bool
}

func (OpenAIProvider) IsStub() bool { return false }

func (p OpenAIProvider) PreservesWireNativeSurface(_ string, surface core.ModelSurface) bool {
	switch surface {
	case core.ModelSurfaceChatCompletions:
		return true
	case core.ModelSurfaceResponses:
		return true
	default:
		return false
	}
}

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

func (p OpenAIProvider) Complete(model string, messages []Message, kw Kwargs) (map[string]any, error) {
	return p.CompleteContext(context.Background(), model, messages, kw)
}

func (p OpenAIProvider) CompleteContext(ctx context.Context, model string, messages []Message, kw Kwargs) (map[string]any, error) {
	response, _, err := p.CompleteContextWithObservation(ctx, model, messages, kw)
	return response, err
}

func (p OpenAIProvider) CompleteWithObservation(
	model string, messages []Message, kw Kwargs,
) (map[string]any, *CredentialObservation, error) {
	return p.CompleteContextWithObservation(context.Background(), model, messages, kw)
}

func (p OpenAIProvider) CompleteContextWithObservation(
	ctx context.Context, model string, messages []Message, kw Kwargs,
) (map[string]any, *CredentialObservation, error) {
	kw = withOpenAIOutputLimit(kw)
	if adaptEnabled(kw, p.forceAdapt) {
		plan := p.planAdapt(model)
		if plan.endpoint == "responses" {
			return p.completeViaResponsesContextWithObservation(ctx, model, messages, kw)
		}
		if plan.renameMaxTokens {
			kw = withRenamedMaxTokens(kw)
		}
	}
	base, headers, observation, err := prepareOpenAIAuth(p.auth)
	if err != nil {
		return nil, observation, err
	}
	applyVisionHeader(headers, messages)
	buf := bytes.Buffer{}
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(buildOpenAIPayload(model, messages, false, kw))
	body := bytes.TrimRight(buf.Bytes(), "\n")
	req, _ := http.NewRequestWithContext(ctx, "POST", p.chatURL(base), bytes.NewReader(body))
	req.Header = headers
	resp, err := httpClient(p.Timeout).Do(req)
	if err != nil {
		return nil, observation, retryableInvocation("openai: upstream transport error: " + err.Error())
	}
	defer resp.Body.Close()
	raw, readErr := readInvocationResponseBody(resp, "openai")
	if readErr != nil {
		return nil, observation, readErr
	}
	if resp.StatusCode >= 400 {
		errMsg := extractError(raw)
		return nil, observation, invocationStatusRetryAfter(fmt.Sprintf("openai: upstream returned %d: %s", resp.StatusCode, redact(errMsg)), resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	var out map[string]any
	if json.Unmarshal(raw, &out) != nil || len(out) == 0 {
		return nil, observation, circuitFailureInvocation("openai: invalid JSON in upstream response")
	}
	if upstreamError := openAISoftError(out); upstreamError != "" {
		return nil, observation, retryableInvocation("openai: upstream returned a soft error: " + redact(upstreamError))
	}
	choices, ok := out["choices"].([]any)
	if !ok || len(choices) == 0 {
		return nil, observation, circuitFailureInvocation("openai: invalid chat response payload")
	}
	if _, ok := choices[0].(map[string]any); !ok {
		return nil, observation, circuitFailureInvocation("openai: invalid chat response payload")
	}
	return out, observation, nil
}

func openAISoftError(response map[string]any) string {
	raw, exists := response["error"]
	if !exists || raw == nil {
		return ""
	}
	if message, ok := raw.(string); ok {
		return strings.TrimSpace(message)
	}
	if details, ok := raw.(map[string]any); ok {
		if message, ok := details["message"].(string); ok {
			return strings.TrimSpace(message)
		}
	}
	return "upstream error"
}

func (p OpenAIProvider) Stream(model string, messages []Message, kw Kwargs) (StreamIter, error) {
	return p.StreamContext(context.Background(), model, messages, kw)
}

func (p OpenAIProvider) StreamContext(ctx context.Context, model string, messages []Message, kw Kwargs) (StreamIter, error) {
	kw = withOpenAIOutputLimit(kw)
	if adaptEnabled(kw, p.forceAdapt) {
		plan := p.planAdapt(model)
		if plan.endpoint == "responses" {
			return p.streamViaResponsesContext(ctx, model, messages, kw)
		}
		if plan.renameMaxTokens {
			kw = withRenamedMaxTokens(kw)
		}
	}
	base, headers, err := p.auth.Prepare()
	if err != nil {
		return nil, err
	}
	applyVisionHeader(headers, messages)
	buf := bytes.Buffer{}
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(buildOpenAIPayload(model, messages, true, kw))
	body := bytes.TrimRight(buf.Bytes(), "\n")
	req, _ := http.NewRequestWithContext(ctx, "POST", p.chatURL(base), bytes.NewReader(body))
	req.Header = headers
	resp, err := httpClient(p.Timeout).Do(req)
	if err != nil {
		return nil, retryableInvocation("openai: streaming transport error: " + err.Error())
	}
	if resp.StatusCode >= 400 {
		raw, readErr := readInvocationResponseBody(resp, "openai")
		resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		errMsg := extractError(raw)
		return nil, invocationStatusRetryAfter(fmt.Sprintf("openai: upstream returned %d: %s", resp.StatusCode, redact(errMsg)), resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	return newHTTPStreamIter(resp, "openai"), nil
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

func (p OpenAIProvider) ListModels() []ModelInfo {
	models, _, _ := p.ListModelsWithError()
	return models
}

func (p OpenAIProvider) ListModelsWithError() (
	[]ModelInfo, *CredentialObservation, error,
) {
	base, headers, observation, err := prepareOpenAIAuth(p.auth)
	if err != nil {
		return nil, observation, catalogError(
			"catalog_authentication_failed",
			"Provider authentication failed before catalog access.",
			0,
		)
	}
	catalog := openAICatalog{
		base: base, header: headers, observation: observation, timeout: p.Timeout,
		registryID: p.registryID, anonymous: p.anonymous,
	}
	return catalog.list()
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

// planAdapt decides how to adapt a request for a model, from the catalog.
// Unknown models / missing catalog fall back to the native chat path.
func (p OpenAIProvider) planAdapt(model string) adaptPlan {
	if p.providerID == "" {
		return adaptPlan{endpoint: "chat"}
	}
	return adaptPlanFor(CatalogLookupForPrincipal(p.providerID, model, p.caller))
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

// messagesHaveImages reports whether any message carries an image part.
func messagesHaveImages(messages []Message) bool {
	for _, m := range messages {
		parts, ok := m["content"].([]any)
		if !ok {
			continue
		}
		for _, p := range parts {
			if pm, ok := p.(map[string]any); ok {
				switch pm["type"] {
				case "image_url", "input_image", "image":
					return true
				}
			}
		}
	}
	return false
}

// applyVisionHeader marks a request that carries images with the header
// GitHub Copilot requires for them. The transport has sent it since it served
// Copilot, and other OpenAI-compatible upstreams ignore it.
func applyVisionHeader(h http.Header, messages []Message) {
	if messagesHaveImages(messages) {
		h.Set("Copilot-Vision-Request", "true")
	}
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

func (p OpenAIProvider) completeViaResponses(model string, messages []Message, kw Kwargs) (map[string]any, error) {
	response, _, err := p.completeViaResponsesWithObservation(model, messages, kw)
	return response, err
}

func (p OpenAIProvider) completeViaResponsesWithObservation(
	model string, messages []Message, kw Kwargs,
) (map[string]any, *CredentialObservation, error) {
	return p.completeViaResponsesContextWithObservation(context.Background(), model, messages, kw)
}

func (p OpenAIProvider) completeViaResponsesContextWithObservation(
	ctx context.Context, model string, messages []Message, kw Kwargs,
) (map[string]any, *CredentialObservation, error) {
	conversion := chatToResponsesWithReport(model, messages, kw)
	if err := RejectMaterialLossExceptThoughtSignatures(conversion.Report); err != nil {
		return nil, nil, &ConfigError{Msg: err.Error()}
	}
	payload := conversion.Value
	resp, observation, err := p.callResponsesPayloadContext(ctx, payload, true)
	if err != nil {
		return nil, observation, err
	}
	converted := translate.ResponsesToChatWithReport(model, resp)
	if err := converted.RejectMaterialLoss(); err != nil {
		return nil, observation, &ConfigError{Msg: err.Error()}
	}
	chat := converted.Value
	if choices, ok := chat["choices"].([]any); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			if msg, ok := choice["message"].(map[string]any); ok {
				c, _ := msg["content"].(string)
				r, _ := msg["reasoning_content"].(string)
				if strings.TrimSpace(c) == "" && strings.TrimSpace(r) != "" {
					msg["content"] = r
				}
			}
		}
	}
	chat["forced_support"] = map[string]any{"req_api": "chat", "resp_api": "responses"}
	return chat, observation, nil
}

func (p OpenAIProvider) streamViaResponses(model string, messages []Message, kw Kwargs) (StreamIter, error) {
	return p.streamViaResponsesContext(context.Background(), model, messages, kw)
}

func (p OpenAIProvider) streamViaResponsesContext(ctx context.Context, model string, messages []Message, kw Kwargs) (StreamIter, error) {
	conversion := chatToResponsesWithReport(model, messages, kw)
	if err := RejectMaterialLossExceptThoughtSignatures(conversion.Report); err != nil {
		return nil, &ConfigError{Msg: err.Error()}
	}
	payload := conversion.Value
	resp, _, err := p.callResponsesPayloadContext(ctx, payload, true)
	if err != nil {
		return nil, err
	}
	converted := translate.ResponsesToChatChunksWithReport(model, resp)
	if err := converted.RejectMaterialLoss(); err != nil {
		return nil, &ConfigError{Msg: err.Error()}
	}
	return &sliceIter{chunks: converted.Value}, nil
}

// callResponses posts a translated request to /responses. On a 400 naming a
// sampling param the model rejects (temperature/top_p), it strips it and retries
// once so an over-specified request still succeeds.
func (p OpenAIProvider) callResponses(model string, messages []Message, kw Kwargs) (map[string]any, error) {
	response, _, err := p.callResponsesWithObservation(model, messages, kw)
	return response, err
}

func (p OpenAIProvider) callResponsesWithObservation(
	model string, messages []Message, kw Kwargs,
) (map[string]any, *CredentialObservation, error) {
	conversion := chatToResponsesWithReport(model, messages, kw)
	if err := RejectMaterialLossExceptThoughtSignatures(conversion.Report); err != nil {
		return nil, nil, &ConfigError{Msg: err.Error()}
	}
	return p.callResponsesPayload(conversion.Value, true)
}

func (p OpenAIProvider) CompleteResponses(
	model string, payload map[string]any,
) (map[string]any, *CredentialObservation, error) {
	return p.CompleteResponsesContext(context.Background(), model, payload)
}

func (p OpenAIProvider) CompleteResponsesContext(
	ctx context.Context, model string, payload map[string]any,
) (map[string]any, *CredentialObservation, error) {
	if !p.supportsNativeResponses(model) {
		return nil, nil, ErrResponsesUnsupported
	}
	request := cloneMap(payload)
	request["model"] = model
	request["stream"] = false
	delete(request, "force_api_support")
	response, observation, err := p.callResponsesPayloadContext(ctx, request, false)
	if status := UpstreamStatus(err); status == http.StatusNotFound ||
		status == http.StatusMethodNotAllowed {
		return nil, observation, ErrResponsesUnsupported
	}
	return response, observation, err
}

func (p OpenAIProvider) StreamResponses(
	model string, payload map[string]any,
) (StreamIter, *CredentialObservation, error) {
	return p.StreamResponsesContext(context.Background(), model, payload)
}

func (p OpenAIProvider) StreamResponsesContext(
	ctx context.Context, model string, payload map[string]any,
) (StreamIter, *CredentialObservation, error) {
	if !p.supportsNativeResponses(model) {
		return nil, nil, ErrResponsesUnsupported
	}
	request := cloneMap(payload)
	request["model"] = model
	request["stream"] = true
	delete(request, "force_api_support")
	stream, observation, err := p.streamResponsesPayloadContext(ctx, request)
	if status := UpstreamStatus(err); status == http.StatusNotFound ||
		status == http.StatusMethodNotAllowed {
		return nil, observation, ErrResponsesUnsupported
	}
	return stream, observation, err
}

func (p OpenAIProvider) supportsNativeResponses(model string) bool {
	if providerConfig := config.Get().Providers[p.providerID]; providerConfig != nil {
		switch EffectiveRegistryID(
			p.providerID, providerConfig.RegistryID, providerConfig.Type,
		) {
		case "openai", "openai_codex":
			return true
		}
	}
	info, ok := CatalogLookupForPrincipal(p.providerID, model, p.caller)
	return ok && listsResponses(info.SupportedSurfaces)
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

func cloneMap(source map[string]any) map[string]any {
	out := make(map[string]any, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}

func (p OpenAIProvider) callResponsesPayloadWithObservation(
	payload map[string]any,
) (map[string]any, *CredentialObservation, error) {
	return p.callResponsesPayload(payload, false)
}

func (p OpenAIProvider) callResponsesPayload(
	payload map[string]any, allowUnsupportedParameterRetry bool,
) (map[string]any, *CredentialObservation, error) {
	return p.callResponsesPayloadContext(context.Background(), payload, allowUnsupportedParameterRetry)
}

func (p OpenAIProvider) callResponsesPayloadContext(
	ctx context.Context, payload map[string]any, allowUnsupportedParameterRetry bool,
) (map[string]any, *CredentialObservation, error) {
	base, headers, observation, err := prepareOpenAIAuth(p.auth)
	if err != nil {
		return nil, observation, err
	}
	headers.Set("Accept", "application/json")
	applyResponsesVisionHeader(headers, payload)
	post := func(b string, h http.Header) (*http.Response, error) {
		buf := bytes.Buffer{}
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(payload)
		body := bytes.TrimRight(buf.Bytes(), "\n")
		req, _ := http.NewRequestWithContext(ctx, "POST", b+"/responses", bytes.NewReader(body))
		req.Header = h
		return httpClient(p.Timeout).Do(req)
	}

	resp, err := post(base, headers)
	if err != nil {
		return nil, observation, retryableInvocation("openai: responses transport error: " + err.Error())
	}
	raw, readErr := readInvocationResponseBody(resp, "openai: responses")
	resp.Body.Close()
	if readErr != nil {
		return nil, observation, readErr
	}
	if allowUnsupportedParameterRetry &&
		resp.StatusCode == http.StatusBadRequest &&
		stripRejectedParams(payload, raw) && ctx.Err() == nil {
		if resp2, e := post(base, headers); e == nil {
			raw, readErr = readInvocationResponseBody(resp2, "openai: responses")
			resp2.Body.Close()
			if readErr != nil {
				return nil, observation, readErr
			}
			resp = resp2
		}
	}
	if resp.StatusCode >= 400 {
		message := fmt.Sprintf("openai: responses endpoint returned %d: %s", resp.StatusCode, redact(extractError(raw)))
		return nil, observation, invocationStatusRetryAfter(message, resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	var out map[string]any
	if json.Unmarshal(raw, &out) != nil || len(out) == 0 {
		return nil, observation, circuitFailureInvocation("openai: invalid JSON in responses payload")
	}
	if _, ok := out["output"].([]any); !ok {
		return nil, observation, circuitFailureInvocation("openai: invalid Responses payload")
	}
	return out, observation, nil
}

func (p OpenAIProvider) streamResponsesPayload(
	payload map[string]any,
) (StreamIter, *CredentialObservation, error) {
	return p.streamResponsesPayloadContext(context.Background(), payload)
}

func (p OpenAIProvider) streamResponsesPayloadContext(
	ctx context.Context, payload map[string]any,
) (StreamIter, *CredentialObservation, error) {
	base, headers, observation, err := prepareOpenAIAuth(p.auth)
	if err != nil {
		return nil, observation, err
	}
	headers.Set("Accept", "text/event-stream")
	applyResponsesVisionHeader(headers, payload)
	buf := bytes.Buffer{}
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(payload)
	body := bytes.TrimRight(buf.Bytes(), "\n")
	request, _ := http.NewRequestWithContext(ctx, "POST", base+"/responses", bytes.NewReader(body))
	request.Header = headers
	response, err := httpClient(p.Timeout).Do(request)
	if err != nil {
		return nil, observation, retryableInvocation(
			"openai: responses streaming transport error: " + err.Error(),
		)
	}
	if response.StatusCode >= 400 {
		raw, readErr := readInvocationResponseBody(response, "openai: responses")
		response.Body.Close()
		if readErr != nil {
			return nil, observation, readErr
		}
		return nil, observation, invocationStatusRetryAfter(
			fmt.Sprintf(
				"openai: responses endpoint returned %d: %s",
				response.StatusCode, redact(extractError(raw)),
			),
			response.StatusCode,
			response.Header.Get("Retry-After"),
		)
	}
	return newHTTPStreamIter(response, "responses"), observation, nil
}

func applyResponsesVisionHeader(headers http.Header, payload map[string]any) {
	if responsesPayloadHasImages(payload["input"]) {
		headers.Set("Copilot-Vision-Request", "true")
	}
}

func responsesPayloadHasImages(value any) bool {
	switch current := value.(type) {
	case []any:
		for _, item := range current {
			if responsesPayloadHasImages(item) {
				return true
			}
		}
	case map[string]any:
		switch current["type"] {
		case "input_image", "image_url":
			return true
		}
		for _, nested := range current {
			if responsesPayloadHasImages(nested) {
				return true
			}
		}
	}
	return false
}

func stripRejectedParams(payload map[string]any, body []byte) bool {
	s := strings.ToLower(string(body))
	changed := false
	for _, k := range []string{"temperature", "top_p"} {
		if _, ok := payload[k]; ok && strings.Contains(s, k) {
			delete(payload, k)
			changed = true
		}
	}
	return changed
}
