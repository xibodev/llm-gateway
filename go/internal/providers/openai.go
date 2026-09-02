package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/translate"
)

// OpenAIProvider speaks the OpenAI wire standard (/chat/completions, /models)
// against any backend. Authentication + base URL are supplied by an OpenAIAuth
// strategy, so this one transport backs every OpenAI-compatible provider type:
// openai_compatible, bedrock, litellm, and github_copilot (which differ only in
// their auth strategy).
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
	principal  *config.Principal
}

func (OpenAIProvider) IsStub() bool { return false }

func buildOpenAIPayload(model string, messages []Message, stream bool, kw Kwargs) map[string]any {
	payload := map[string]any{"model": model, "messages": messages, "stream": stream}
	for _, key := range []string{
		"temperature", "top_p", "max_tokens", "max_completion_tokens",
		"stop", "tools", "tool_choice", "reasoning_effort", "stream_options",
		"metadata",
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
) (map[string]any, *iam.ProviderAccountObservation, error) {
	return p.CompleteContextWithObservation(context.Background(), model, messages, kw)
}

func (p OpenAIProvider) CompleteContextWithObservation(
	ctx context.Context, model string, messages []Message, kw Kwargs,
) (map[string]any, *iam.ProviderAccountObservation, error) {
	kw = withOpenAIOutputLimit(kw)
	if p.adaptEnabled(kw) {
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
	body, _ := json.Marshal(buildOpenAIPayload(model, messages, false, kw))
	do := func(b string, h http.Header) (*http.Response, error) {
		req, _ := http.NewRequestWithContext(ctx, "POST", b+"/chat/completions", bytes.NewReader(body))
		req.Header = h
		return httpClient(p.Timeout).Do(req)
	}

	resp, err := do(base, headers)
	if err != nil {
		return nil, observation, invocation("openai: upstream transport error: " + err.Error())
	}
	if resp.StatusCode == 401 && p.auth.CanRefresh() {
		resp.Body.Close()
		if ctx.Err() != nil {
			return nil, observation, invocation("openai: request canceled: " + ctx.Err().Error())
		}
		if rerr := p.auth.Refresh(); rerr == nil {
			if ctx.Err() != nil {
				return nil, observation, invocation("openai: request canceled: " + ctx.Err().Error())
			}
			if base, headers, observation, err = prepareOpenAIAuth(p.auth); err == nil {
				applyVisionHeader(headers, messages)
				if resp, err = do(base, headers); err != nil {
					return nil, observation, invocation("openai: upstream transport error on retry: " + err.Error())
				}
			}
		}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, observation, invocationStatus(fmt.Sprintf("openai: upstream returned %d: %s", resp.StatusCode, redact(extractError(raw))), resp.StatusCode)
	}
	var out map[string]any
	if json.Unmarshal(raw, &out) != nil {
		return nil, observation, invocation("openai: invalid JSON in upstream response")
	}
	return out, observation, nil
}

func (p OpenAIProvider) Stream(model string, messages []Message, kw Kwargs) (StreamIter, error) {
	return p.StreamContext(context.Background(), model, messages, kw)
}

func (p OpenAIProvider) StreamContext(ctx context.Context, model string, messages []Message, kw Kwargs) (StreamIter, error) {
	kw = withOpenAIOutputLimit(kw)
	if p.adaptEnabled(kw) {
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
	body, _ := json.Marshal(buildOpenAIPayload(model, messages, true, kw))
	do := func(b string, h http.Header) (*http.Response, error) {
		req, _ := http.NewRequestWithContext(ctx, "POST", b+"/chat/completions", bytes.NewReader(body))
		req.Header = h
		return httpClient(p.Timeout).Do(req)
	}

	resp, err := do(base, headers)
	if err != nil {
		return nil, invocation("openai: streaming transport error: " + err.Error())
	}
	if resp.StatusCode == 401 && p.auth.CanRefresh() {
		resp.Body.Close()
		if rerr := p.auth.Refresh(); rerr == nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if base, headers, err = p.auth.Prepare(); err == nil {
				applyVisionHeader(headers, messages)
				if resp, err = do(base, headers); err != nil {
					return nil, invocation("openai: streaming transport error on retry: " + err.Error())
				}
			}
		}
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, invocationStatus(fmt.Sprintf("openai: upstream returned %d: %s", resp.StatusCode, redact(extractError(raw))), resp.StatusCode)
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
	[]ModelInfo, *iam.ProviderAccountObservation, error,
) {
	base, headers, observation, err := prepareOpenAIAuth(p.auth)
	if err != nil {
		return nil, observation, catalogError(
			"catalog_authentication_failed",
			"Provider authentication failed before catalog access.",
			0,
		)
	}
	timeout := p.Timeout
	if timeout > 10 {
		timeout = 10
	}
	get := func(b string, h http.Header) (*http.Response, error) {
		req, _ := http.NewRequest("GET", b+"/models", nil)
		req.Header = h
		return httpClient(timeout).Do(req)
	}

	resp, err := get(base, headers)
	if err != nil {
		return nil, observation, catalogError(
			"catalog_transport_error",
			"Provider catalog request could not reach the upstream service.",
			0,
		)
	}
	if resp.StatusCode == 401 && p.auth.CanRefresh() {
		resp.Body.Close()
		if refreshErr := p.auth.Refresh(); refreshErr != nil {
			return nil, observation, catalogError(
				"catalog_refresh_failed",
				"Provider credential refresh failed.",
				http.StatusUnauthorized,
			)
		}
		if base, headers, observation, err = prepareOpenAIAuth(p.auth); err != nil {
			return nil, observation, catalogError(
				"catalog_authentication_failed",
				"Provider authentication failed after credential refresh.",
				http.StatusUnauthorized,
			)
		}
		if resp, err = get(base, headers); err != nil {
			return nil, observation, catalogError(
				"catalog_transport_error",
				"Provider catalog retry could not reach the upstream service.",
				0,
			)
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, observation, catalogError(
			"catalog_http_error",
			fmt.Sprintf("Provider catalog returned HTTP %d.", resp.StatusCode),
			resp.StatusCode,
		)
	}
	body, err := decodeJSON(resp.Body)
	if err != nil {
		return nil, observation, catalogError(
			"catalog_invalid_json",
			"Provider catalog response was not valid JSON.",
			resp.StatusCode,
		)
	}
	items, ok := body["data"].([]any)
	if !ok {
		return nil, observation, catalogError(
			"catalog_invalid_shape",
			"Provider catalog response did not contain a data array.",
			resp.StatusCode,
		)
	}
	out := []ModelInfo{}
	for _, entry := range items {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		id, _ := m["id"].(string)
		if id == "" {
			id, _ = m["name"].(string)
		}
		if id == "" {
			continue
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
// flag overrides the provider-level opt-in.
func (p OpenAIProvider) adaptEnabled(kw Kwargs) bool {
	if v, ok := kw["_force_api_support"]; ok {
		return kwBool(v)
	}
	return p.forceAdapt
}

// planAdapt decides how to adapt a request for a model, from the catalog.
// Unknown models / missing catalog fall back to the native chat path.
func (p OpenAIProvider) planAdapt(model string) adaptPlan {
	if p.providerID == "" {
		return adaptPlan{endpoint: "chat"}
	}
	mi, ok := CatalogLookupForPrincipal(p.providerID, model, p.principal)
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

// applyVisionHeader unlocks image processing on GitHub Copilot (which rejects
// images without it). Harmless to other OpenAI-compatible providers.
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

func (p OpenAIProvider) completeViaResponses(model string, messages []Message, kw Kwargs) (map[string]any, error) {
	response, _, err := p.completeViaResponsesWithObservation(model, messages, kw)
	return response, err
}

func (p OpenAIProvider) completeViaResponsesWithObservation(
	model string, messages []Message, kw Kwargs,
) (map[string]any, *iam.ProviderAccountObservation, error) {
	return p.completeViaResponsesContextWithObservation(context.Background(), model, messages, kw)
}

func (p OpenAIProvider) completeViaResponsesContextWithObservation(
	ctx context.Context, model string, messages []Message, kw Kwargs,
) (map[string]any, *iam.ProviderAccountObservation, error) {
	payload := translate.ChatToResponses(model, messages, kw, false)
	resp, observation, err := p.callResponsesPayloadContext(ctx, payload, true)
	if err != nil {
		return nil, observation, err
	}
	chat := translate.ResponsesToChat(model, resp)
	chat["forced_support"] = map[string]any{"req_api": "chat", "resp_api": "responses"}
	return chat, observation, nil
}

func (p OpenAIProvider) streamViaResponses(model string, messages []Message, kw Kwargs) (StreamIter, error) {
	return p.streamViaResponsesContext(context.Background(), model, messages, kw)
}

func (p OpenAIProvider) streamViaResponsesContext(ctx context.Context, model string, messages []Message, kw Kwargs) (StreamIter, error) {
	payload := translate.ChatToResponses(model, messages, kw, false)
	resp, _, err := p.callResponsesPayloadContext(ctx, payload, true)
	if err != nil {
		return nil, err
	}
	return &sliceIter{chunks: translate.ResponsesToChatChunks(model, resp)}, nil
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
) (map[string]any, *iam.ProviderAccountObservation, error) {
	payload := translate.ChatToResponses(model, messages, kw, false)
	return p.callResponsesPayload(payload, true)
}

func (p OpenAIProvider) CompleteResponses(
	model string, payload map[string]any,
) (map[string]any, *iam.ProviderAccountObservation, error) {
	return p.CompleteResponsesContext(context.Background(), model, payload)
}

func (p OpenAIProvider) CompleteResponsesContext(
	ctx context.Context, model string, payload map[string]any,
) (map[string]any, *iam.ProviderAccountObservation, error) {
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
) (StreamIter, *iam.ProviderAccountObservation, error) {
	return p.StreamResponsesContext(context.Background(), model, payload)
}

func (p OpenAIProvider) StreamResponsesContext(
	ctx context.Context, model string, payload map[string]any,
) (StreamIter, *iam.ProviderAccountObservation, error) {
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
	info, ok := CatalogLookupForPrincipal(p.providerID, model, p.principal)
	if !ok {
		return false
	}
	for _, endpoint := range info.SupportedSurfaces {
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
) (map[string]any, *iam.ProviderAccountObservation, error) {
	return p.callResponsesPayload(payload, false)
}

func (p OpenAIProvider) callResponsesPayload(
	payload map[string]any, allowUnsupportedParameterRetry bool,
) (map[string]any, *iam.ProviderAccountObservation, error) {
	return p.callResponsesPayloadContext(context.Background(), payload, allowUnsupportedParameterRetry)
}

func (p OpenAIProvider) callResponsesPayloadContext(
	ctx context.Context, payload map[string]any, allowUnsupportedParameterRetry bool,
) (map[string]any, *iam.ProviderAccountObservation, error) {
	base, headers, observation, err := prepareOpenAIAuth(p.auth)
	if err != nil {
		return nil, observation, err
	}
	headers.Set("Accept", "application/json")
	applyResponsesVisionHeader(headers, payload)
	post := func(b string, h http.Header) (*http.Response, error) {
		body, _ := json.Marshal(payload)
		req, _ := http.NewRequestWithContext(ctx, "POST", b+"/responses", bytes.NewReader(body))
		req.Header = h
		return httpClient(p.Timeout).Do(req)
	}

	resp, err := post(base, headers)
	if err != nil {
		return nil, observation, invocation("openai: responses transport error: " + err.Error())
	}
	if resp.StatusCode == 401 && p.auth.CanRefresh() {
		resp.Body.Close()
		if ctx.Err() != nil {
			return nil, observation, invocation("openai: responses request canceled: " + ctx.Err().Error())
		}
		if rerr := p.auth.Refresh(); rerr == nil {
			if ctx.Err() != nil {
				return nil, observation, invocation("openai: responses request canceled: " + ctx.Err().Error())
			}
			if base, headers, observation, err = prepareOpenAIAuth(p.auth); err == nil {
				headers.Set("Accept", "application/json")
				applyResponsesVisionHeader(headers, payload)
				if resp, err = post(base, headers); err != nil {
					return nil, observation, invocation("openai: responses transport error on retry: " + err.Error())
				}
			}
		}
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if allowUnsupportedParameterRetry &&
		resp.StatusCode == http.StatusBadRequest &&
		stripRejectedParams(payload, raw) && ctx.Err() == nil {
		if resp2, e := post(base, headers); e == nil {
			raw, _ = io.ReadAll(resp2.Body)
			resp2.Body.Close()
			resp = resp2
		}
	}
	if resp.StatusCode >= 400 {
		return nil, observation, invocationStatus(fmt.Sprintf("openai: responses endpoint returned %d: %s", resp.StatusCode, redact(extractError(raw))), resp.StatusCode)
	}
	var out map[string]any
	if json.Unmarshal(raw, &out) != nil {
		return nil, observation, invocation("openai: invalid JSON in responses payload")
	}
	return out, observation, nil
}

func (p OpenAIProvider) streamResponsesPayload(
	payload map[string]any,
) (StreamIter, *iam.ProviderAccountObservation, error) {
	return p.streamResponsesPayloadContext(context.Background(), payload)
}

func (p OpenAIProvider) streamResponsesPayloadContext(
	ctx context.Context, payload map[string]any,
) (StreamIter, *iam.ProviderAccountObservation, error) {
	base, headers, observation, err := prepareOpenAIAuth(p.auth)
	if err != nil {
		return nil, observation, err
	}
	headers.Set("Accept", "text/event-stream")
	applyResponsesVisionHeader(headers, payload)
	post := func(b string, h http.Header) (*http.Response, error) {
		body, _ := json.Marshal(payload)
		request, _ := http.NewRequestWithContext(ctx, "POST", b+"/responses", bytes.NewReader(body))
		request.Header = h
		return httpClient(p.Timeout).Do(request)
	}
	response, err := post(base, headers)
	if err != nil {
		return nil, observation, invocation(
			"openai: responses streaming transport error: " + err.Error(),
		)
	}
	if response.StatusCode == http.StatusUnauthorized && p.auth.CanRefresh() {
		response.Body.Close()
		if refreshErr := p.auth.Refresh(); refreshErr == nil {
			if ctx.Err() != nil {
				return nil, observation, ctx.Err()
			}
			if base, headers, observation, err = prepareOpenAIAuth(p.auth); err == nil {
				headers.Set("Accept", "text/event-stream")
				applyResponsesVisionHeader(headers, payload)
				response, err = post(base, headers)
			}
		}
		if err != nil {
			return nil, observation, invocation(
				"openai: responses streaming retry failed: " + err.Error(),
			)
		}
	}
	if response.StatusCode >= 400 {
		raw, _ := io.ReadAll(response.Body)
		response.Body.Close()
		return nil, observation, invocationStatus(
			fmt.Sprintf(
				"openai: responses endpoint returned %d: %s",
				response.StatusCode, redact(extractError(raw)),
			),
			response.StatusCode,
		)
	}
	return newHTTPStreamIter(response, "responses"), observation, nil
}

func (p CodexProvider) CompleteContext(ctx context.Context, model string, messages []Message, kw Kwargs) (map[string]any, error) {
	response, _, err := p.CompleteContextWithObservation(ctx, model, messages, kw)
	return response, err
}

func (p CodexProvider) CompleteContextWithObservation(ctx context.Context, model string, messages []Message, kw Kwargs) (map[string]any, *iam.ProviderAccountObservation, error) {
	return p.inner.completeViaResponsesContextWithObservation(ctx, model, messages, kw)
}

func (p CodexProvider) CompleteResponsesContext(ctx context.Context, model string, payload map[string]any) (map[string]any, *iam.ProviderAccountObservation, error) {
	request := cloneMap(payload)
	request["model"] = model
	request["stream"] = false
	delete(request, "force_api_support")
	return p.inner.callResponsesPayloadContext(ctx, request, false)
}

func (p CodexProvider) StreamContext(ctx context.Context, model string, messages []Message, kw Kwargs) (StreamIter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return p.inner.streamViaResponsesContext(ctx, model, messages, kw)
}

func (p CodexProvider) StreamResponsesContext(ctx context.Context, model string, payload map[string]any) (StreamIter, *iam.ProviderAccountObservation, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	request := cloneMap(payload)
	request["model"] = model
	request["stream"] = true
	delete(request, "force_api_support")
	return p.inner.streamResponsesPayloadContext(ctx, request)
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
