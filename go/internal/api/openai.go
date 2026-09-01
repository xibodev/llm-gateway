package api

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/providers"
	"llmgw/internal/router"
)

type chatRequest struct {
	Model               string           `json:"model"`
	Messages            []map[string]any `json:"messages"`
	Stream              bool             `json:"stream"`
	Temperature         any              `json:"temperature"`
	MaxTokens           any              `json:"max_tokens"`
	MaxCompletionTokens any              `json:"max_completion_tokens"`
	Tools               any              `json:"tools"`
	ToolChoice          any              `json:"tool_choice"`
	Metadata            any              `json:"metadata"`
	TopP                any              `json:"top_p"`
	Stop                any              `json:"stop"`
	ReasoningEffort     any              `json:"reasoning_effort"`
	// ForceApiSupport opts this request into experimental API adaptation
	// (e.g. reaching a Responses-only model over /chat/completions). A present
	// value overrides the provider's config-level setting.
	ForceApiSupport *bool `json:"force_api_support"`
}

type responsesRequest struct {
	Model           string         `json:"model"`
	Input           any            `json:"input"`
	Instructions    any            `json:"instructions"`
	Stream          bool           `json:"stream"`
	MaxOutputTokens any            `json:"max_output_tokens"`
	Reasoning       map[string]any `json:"reasoning"`
	Temperature     any            `json:"temperature"`
	TopP            any            `json:"top_p"`
	Tools           any            `json:"tools"`
	ToolChoice      any            `json:"tool_choice"`
	Metadata        any            `json:"metadata"`
	ForceApiSupport *bool          `json:"force_api_support"`
}

func chatKwargs(req *chatRequest) providers.Kwargs {
	kw := providers.Kwargs{}
	put := func(k string, v any) {
		if v != nil {
			kw[k] = v
		}
	}
	put("temperature", req.Temperature)
	put("max_tokens", req.MaxTokens)
	put("max_completion_tokens", req.MaxCompletionTokens)
	put("tools", req.Tools)
	put("tool_choice", req.ToolChoice)
	put("metadata", req.Metadata)
	put("top_p", req.TopP)
	put("stop", req.Stop)
	put("reasoning_effort", req.ReasoningEffort)
	if req.ForceApiSupport != nil {
		kw["_force_api_support"] = *req.ForceApiSupport
	}
	return kw
}

func providerMessages(messages []map[string]any) []providers.Message {
	out := make([]providers.Message, 0, len(messages)+1)
	if pre := config.Get().GatewayPreamble; pre != "" {
		out = append(out, providers.Message{"role": "system", "content": pre})
	}
	for _, m := range messages {
		out = append(out, providers.Message(m))
	}
	return out
}

func handleChat(w http.ResponseWriter, r *http.Request) {
	principal, ok := authed(w, r)
	if !ok {
		return
	}
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 422, "invalid request body")
		return
	}
	chatDispatch(w, r, &req, principal, "openai.chat")
}

func handleResponses(w http.ResponseWriter, r *http.Request) {
	handleResponsesAPI(w, r)
}

func responsesToChatRequest(rr *responsesRequest) *chatRequest {
	inputValue := ""
	if s, ok := rr.Input.(string); ok {
		inputValue = s
	} else {
		b, _ := json.Marshal(rr.Input)
		inputValue = string(b)
	}
	messages := make([]map[string]any, 0, 2)
	if rr.Instructions != nil {
		instructions := ""
		if s, ok := rr.Instructions.(string); ok {
			instructions = s
		} else {
			b, _ := json.Marshal(rr.Instructions)
			instructions = string(b)
		}
		if instructions != "" {
			messages = append(messages, map[string]any{"role": "system", "content": instructions})
		}
	}
	messages = append(messages, map[string]any{"role": "user", "content": inputValue})

	var reasoningEffort any
	if rr.Reasoning != nil {
		reasoningEffort = rr.Reasoning["effort"]
	}
	return &chatRequest{
		Model:               rr.Model,
		Messages:            messages,
		Stream:              rr.Stream,
		MaxCompletionTokens: rr.MaxOutputTokens,
		ReasoningEffort:     reasoningEffort,
		Temperature:         rr.Temperature,
		TopP:                rr.TopP,
		Tools:               rr.Tools,
		ToolChoice:          rr.ToolChoice,
		Metadata:            rr.Metadata,
		ForceApiSupport:     rr.ForceApiSupport,
	}
}

func chatDispatch(w http.ResponseWriter, r *http.Request, req *chatRequest, principal *config.Principal, endpoint string) {
	started := time.Now()
	resolution, err := router.ResolveForPrincipal(req.Model, principal)
	if err != nil {
		if _, ok := err.(*router.ModelNotFoundError); ok {
			recordFailureUsage(endpoint, req.Model, principal, 404, "model_not_found", started)
			writeError(w, 404, err.Error())
			return
		}
		recordFailureUsage(endpoint, req.Model, principal, 500, "route_config", started)
		writeError(w, 500, "Gateway is not configured for the requested model.")
		return
	}
	targets := resolution.Targets
	msgs := providerMessages(req.Messages)
	kw := chatKwargs(req)

	targets, polStatus, polMsg := enforceKeyPolicy(principal, req.Model, resolution.Category, targets)
	if polStatus != 0 {
		recordFailureUsage(endpoint, req.Model, principal, polStatus, "policy", started)
		writeError(w, polStatus, polMsg)
		return
	}

	// Capability-aware routing: a multimodal request must land on a vision-capable
	// target. Filter the failover chain to vision models; fail clearly if none.
	if requestIsMultimodal(req.Messages) {
		vt := filterVisionTargets(targets)
		if len(vt) == 0 {
			recordFailureUsage(endpoint, req.Model, principal, 400, "vision_unavailable", started)
			writeError(w, 400, "This request includes non-text content but no vision-capable model is available in the requested route.")
			return
		}
		targets = vt
	}

	if req.Stream {
		streamChatSSE(w, targets, msgs, req.Model, principal, kw, endpoint, started)
		return
	}

	response, served, err := router.ExecuteCompleteContext(r.Context(), targets, msgs, req.Model, principal, kw)
	if err != nil {
		recordFailureUsage(
			endpoint, req.Model, principal, upstreamErrorStatus(err), "upstream",
			started,
		)
		writeUpstreamError(w, err)
		return
	}
	if _, adapted := response["forced_support"]; adapted {
		w.Header().Set("X-LLMGW-Adapted", "chat->responses")
		// Echo the no-op provenance in the body only when the caller explicitly
		// asked; a config/global-triggered adaptation stays header-only so we
		// never surprise a strict client with an unknown field.
		if req.ForceApiSupport == nil || !*req.ForceApiSupport {
			delete(response, "forced_support")
		}
	}
	response["model"] = served.Model
	recordFromResponse(endpoint, req.Model, served, principal, response, time.Since(started).Milliseconds())
	writeJSON(w, 200, response)
}

// writeUpstreamError surfaces the real upstream status + (redacted) detail when
// available, instead of masking every failure as a generic 502.
func writeUpstreamError(w http.ResponseWriter, err error) {
	status := upstreamErrorStatus(err)
	var atf *router.AllTargetsFailed
	if errors.As(err, &atf) && status >= 400 {
		writeError(w, status, atf.Error())
		return
	}
	if providers.IsConfig(err) {
		writeError(w, 500, "Upstream provider is misconfigured.")
		return
	}
	writeError(w, status, "Upstream provider request failed.")
}

func upstreamErrorStatus(err error) int {
	var atf *router.AllTargetsFailed
	if errors.As(err, &atf) && atf.Status >= 400 {
		return atf.Status
	}
	if providers.IsConfig(err) {
		return 500
	}
	if status := providers.UpstreamStatus(err); status >= 300 && status <= 599 {
		return status
	}
	return 502
}

func streamChatSSE(w http.ResponseWriter, targets []router.Target, msgs []providers.Message, requested string, principal *config.Principal, kw providers.Kwargs, endpoint string, started time.Time) {
	it, served, err := router.ExecuteStream(targets, msgs, requested, principal, kw)
	if err != nil {
		// Pre-first-byte failure: no SSE headers sent yet, so surface a real
		// HTTP error (with the upstream status) rather than a 200 error chunk.
		recordFailureUsage(
			endpoint, requested, principal, upstreamErrorStatus(err), "upstream",
			started,
		)
		writeUpstreamError(w, err)
		return
	}

	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(200)

	usageAcc := map[string]int{"prompt_tokens": 0, "completion_tokens": 0}
	for {
		chunk, more := it.Next()
		if !more {
			break
		}
		accumulateStreamUsage(chunk, usageAcc)
		writeSSE(w, flusher, chunk)
	}
	if it.Err() != nil {
		writeSSE(w, flusher, jsonStr(providerErrorPayloadOpenAI()))
	}
	writeSSE(w, flusher, "[DONE]")
	_ = it.Close()
	router.RecordUsage(router.UsageRecord{
		Endpoint: endpoint, RequestedModel: requested, RoutedModel: served.Model,
		Provider: served.Provider, Project: principal.Project, Key: principal.Key,
		ProjectID: principal.ProjectID, PrincipalID: principal.PrincipalID, KeyID: principal.KeyID,
		InputTokens: usageAcc["prompt_tokens"], OutputTokens: usageAcc["completion_tokens"],
		LatencyMS: time.Since(started).Milliseconds(), IsStub: isStub(served.Provider),
	})
}

func writeSSE(w http.ResponseWriter, flusher http.Flusher, data string) {
	_, _ = w.Write([]byte("data: " + data + "\n\n"))
	if flusher != nil {
		flusher.Flush()
	}
}

func providerErrorPayloadOpenAI() map[string]any {
	return map[string]any{"error": map[string]any{
		"message": "Upstream provider request failed.", "type": "provider_error",
		"code": "provider_invocation_failed",
	}}
}

func jsonStr(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func accumulateStreamUsage(chunk string, acc map[string]int) {
	var obj map[string]any
	if json.Unmarshal([]byte(chunk), &obj) != nil {
		return
	}
	usage, ok := obj["usage"].(map[string]any)
	if !ok {
		return
	}
	if v := firstInt(usage, "prompt_tokens", "input_tokens"); v != 0 {
		acc["prompt_tokens"] = v
	}
	if v := firstInt(usage, "completion_tokens", "output_tokens"); v != 0 {
		acc["completion_tokens"] = v
	}
}

func firstInt(m map[string]any, keys ...string) int {
	limit := math.Ldexp(1, strconv.IntSize-1)
	fromFloat := func(n float64) int {
		if math.IsNaN(n) || math.IsInf(n, 0) || math.Trunc(n) != n || n < -limit || n >= limit {
			return 0
		}
		return int(n)
	}
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch n := v.(type) {
			case float64:
				return fromFloat(n)
			case int:
				return n
			case int64:
				if strconv.IntSize == 32 && (n > math.MaxInt32 || n < math.MinInt32) {
					return 0
				}
				return int(n)
			case json.Number:
				value, err := n.Int64()
				if err == nil {
					if strconv.IntSize == 32 && (value > math.MaxInt32 || value < math.MinInt32) {
						return 0
					}
					return int(value)
				}
				valueFloat, err := strconv.ParseFloat(string(n), 64)
				if err == nil {
					return fromFloat(valueFloat)
				}
				return 0
			}
		}
	}
	return 0
}

func recordFromResponse(endpoint, requested string, served *router.Target, principal *config.Principal, response map[string]any, latencyMS int64) {
	in, out := 0, 0
	if usage, ok := response["usage"].(map[string]any); ok {
		in = firstInt(usage, "prompt_tokens", "input_tokens")
		out = firstInt(usage, "completion_tokens", "output_tokens")
	}
	router.RecordUsage(router.UsageRecord{
		Endpoint: endpoint, RequestedModel: requested, RoutedModel: served.Model,
		Provider: served.Provider, Project: principal.Project, Key: principal.Key,
		ProjectID: principal.ProjectID, PrincipalID: principal.PrincipalID, KeyID: principal.KeyID,
		InputTokens: in, OutputTokens: out, LatencyMS: latencyMS, IsStub: isStub(served.Provider),
	})
}

func isStub(providerID string) bool {
	p, err := providers.GetProvider(providerID)
	if err != nil {
		return false
	}
	return p.IsStub()
}
