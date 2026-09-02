package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/providers"
	"llmgw/internal/router"
	"llmgw/internal/translate"
)

const maxAnthropicTokenCountBodyBytes = 32 << 20 // 32 MiB

type anthropicRequest struct {
	Model         string         `json:"model"`
	Messages      []any          `json:"messages"`
	MaxTokens     any            `json:"max_tokens"`
	System        any            `json:"system"`
	Tools         []any          `json:"tools"`
	ToolChoice    map[string]any `json:"tool_choice"`
	Stream        bool           `json:"stream"`
	Temperature   any            `json:"temperature"`
	TopP          any            `json:"top_p"`
	StopSequences []any          `json:"stop_sequences"`
	Metadata      any            `json:"metadata"`
	Thinking      map[string]any `json:"thinking"`
	OutputConfig  map[string]any `json:"output_config"`
	Raw           map[string]any `json:"-"`
}

func anthropicKwargs(req *anthropicRequest) providers.Kwargs {
	kw := providers.Kwargs{}
	if req.Temperature != nil {
		kw["temperature"] = req.Temperature
	}
	if req.MaxTokens != nil {
		kw["max_tokens"] = req.MaxTokens
	}
	if req.TopP != nil {
		kw["top_p"] = req.TopP
	}
	if len(req.StopSequences) > 0 {
		kw["stop"] = req.StopSequences
	}
	if req.Metadata != nil {
		kw["metadata"] = req.Metadata
	}
	if len(req.Thinking) > 0 {
		kw["thinking"] = req.Thinking
	}
	if len(req.OutputConfig) > 0 {
		kw["output_config"] = req.OutputConfig
		if effort, ok := req.OutputConfig["effort"].(string); ok && effort != "" {
			// Copilot's OpenAI-family chat and Responses endpoints express
			// Claude's output_config.effort as reasoning_effort/reasoning.effort.
			kw["reasoning_effort"] = effort
		}
	}
	if tools := translate.AnthropicToolsToOpenAI(req.Tools); tools != nil {
		kw["tools"] = tools
	}
	if tc := translate.AnthropicToolChoiceToOpenAI(req.ToolChoice); tc != nil {
		kw["tool_choice"] = tc
	}
	return kw
}

func handleMessages(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	principal, ok := authed(w, r)
	if !ok {
		return
	}
	var req anthropicRequest
	var raw map[string]any
	decoder := json.NewDecoder(r.Body)
	decoder.UseNumber()
	if err := decoder.Decode(&raw); err != nil {
		writeError(w, 422, "invalid request body")
		return
	}
	encoded, _ := json.Marshal(raw)
	if json.Unmarshal(encoded, &req) != nil || req.Model == "" {
		writeError(w, 422, "model is required")
		return
	}
	if pre := config.Get().GatewayPreamble; pre != "" {
		raw["_llmgw_preamble"] = pre
	}

	resolution, err := router.ResolveForPrincipal(req.Model, principal)
	if err != nil {
		if _, ok := err.(*router.ModelNotFoundError); ok {
			recordFailureUsage("anthropic.messages", req.Model, principal, 404, "model_not_found", started)
			writeError(w, 404, err.Error())
			return
		}
		recordFailureUsage("anthropic.messages", req.Model, principal, 500, "route_config", started)
		writeError(w, 500, "Gateway is not configured for the requested model.")
		return
	}
	targets := resolution.Targets
	targets, polStatus, polMsg := enforceKeyPolicy(principal, req.Model, resolution.Category, targets)
	if polStatus != 0 {
		recordFailureUsage("anthropic.messages", req.Model, principal, polStatus, "policy", started)
		writeError(w, polStatus, polMsg)
		return
	}

	if req.Stream {
		converted, kw, incompatible := translate.AnthropicRequestToOpenAI(raw)
		if len(incompatible) > 0 {
			recordFailureUsage("anthropic.messages", req.Model, principal, 400, "compatibility", started)
			writeError(w, 400, "Streaming cannot preserve Anthropic fields: "+strings.Join(incompatible, ", "))
			return
		}
		msgs := make([]providers.Message, len(converted))
		for i := range converted {
			msgs[i] = providers.Message(converted[i])
		}
		streamMessagesSSE(w, r.Context(), targets, msgs, req.Model, principal, providers.Kwargs(kw), started)
		return
	}

	response, served, err := router.ExecuteAnthropicMessagesContext(r.Context(), targets, raw, req.Model, principal)
	if err != nil {
		status := upstreamErrorStatus(err)
		recordFailureUsage("anthropic.messages", req.Model, principal, status, "upstream", started)
		writeUpstreamError(w, err)
		return
	}
	recordFromResponse(
		"anthropic.messages", req.Model, served, principal, response,
		time.Since(started).Milliseconds(),
	)
	writeJSON(w, 200, response)
}

func streamMessagesSSE(w http.ResponseWriter, ctx context.Context, targets []router.Target, msgs []providers.Message, requested string, principal *config.Principal, kw providers.Kwargs, started time.Time) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	it, served, err := router.ExecuteStreamContext(ctx, targets, msgs, requested, principal, kw)
	if err != nil {
		if ctx.Err() != nil {
			recordClientCancelled("anthropic.messages", requested, principal, started)
			return
		}
		recordFailureUsage(
			"anthropic.messages", requested, principal, upstreamErrorStatus(err),
			"upstream", started,
		)
		payload := map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": "Upstream provider request failed."}}
		_ = writeAndFlush(w, []byte("event: error\ndata: "+jsonStr(payload)+"\n\n"))
		return
	}
	defer it.Close()
	w.WriteHeader(http.StatusOK)

	usageAcc := map[string]int{"prompt_tokens": 0, "completion_tokens": 0}
	var writeErr error
	terminalWritten := false
	// pull func peeks usage as it forwards OpenAI chunks into the translator
	pull := func() (string, bool) {
		if writeErr != nil || ctx.Err() != nil {
			return "", false
		}
		chunk, more := it.Next()
		if ctx.Err() != nil {
			return "", false
		}
		if !more {
			return "", false
		}
		accumulateStreamUsage(chunk, usageAcc)
		return chunk, true
	}
	translate.OpenAIStreamToAnthropicSSE(pull, served.Model, func(event string) {
		if writeErr != nil {
			return
		}
		terminal := strings.HasPrefix(event, "event: message_stop\n")
		if err := ctx.Err(); err != nil {
			writeErr = err
			return
		}
		writeErr = writeAndFlush(w, []byte(event))
		if writeErr == nil && terminal {
			terminalWritten = true
		}
	})
	if writeErr != nil || !terminalWritten {
		recordClientCancelled("anthropic.messages", requested, principal, started)
		return
	}
	router.RecordUsage(router.UsageRecord{
		Endpoint: "anthropic.messages", RequestedModel: requested, RoutedModel: served.Model,
		Provider: served.Provider, Project: principal.Project, Key: principal.Key,
		ProjectID: principal.ProjectID, PrincipalID: principal.PrincipalID, KeyID: principal.KeyID,
		InputTokens: usageAcc["prompt_tokens"], OutputTokens: usageAcc["completion_tokens"],
		LatencyMS: time.Since(started).Milliseconds(), IsStub: isStub(served.Provider),
	})
}

func handleCountTokens(w http.ResponseWriter, r *http.Request) {
	principal, ok := authed(w, r)
	if !ok {
		return
	}
	var raw map[string]any
	r.Body = http.MaxBytesReader(w, r.Body, maxAnthropicTokenCountBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.UseNumber()
	if err := decoder.Decode(&raw); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeError(w, 422, "invalid request body")
		return
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeError(w, 422, "invalid request body")
		return
	}
	model, _ := raw["model"].(string)
	if strings.TrimSpace(model) == "" {
		writeError(w, 422, "model is required")
		return
	}
	resolution, err := router.ResolveForPrincipal(model, principal)
	if err != nil {
		if _, ok := err.(*router.ModelNotFoundError); ok {
			writeError(w, 404, err.Error())
		} else {
			writeError(w, 500, "Gateway is not configured for the requested model.")
		}
		return
	}
	targets, status, message := authorizeKeyPolicy(principal, model, resolution.Category, resolution.Targets)
	if status != 0 {
		writeError(w, status, message)
		return
	}
	target := targets[0]
	provider, err := providers.GetProviderForPrincipal(target.Provider, principal)
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	if providers.SupportsAnthropicTokenCount(provider) {
		count, err := providers.CountAnthropicTokens(provider, target.Model, raw, r.Header.Get("anthropic-version"), r.Header.Values("anthropic-beta"))
		if err != nil {
			if errors.Is(err, providers.ErrInvalidAnthropicTokenCount) {
				writeError(w, http.StatusBadGateway, "Upstream provider returned an invalid token count.")
				return
			}
			writeUpstreamError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"input_tokens": count})
		return
	}
	countPayload := map[string]any{}
	for _, field := range []string{"system", "messages", "tools", "tool_choice", "thinking", "output_config"} {
		if value, exists := raw[field]; exists {
			countPayload[field] = value
		}
	}
	compact, _ := json.Marshal(countPayload)
	tokens := len(compact) / 4
	if len(compact)%4 != 0 {
		tokens++
	}
	if tokens < 1 {
		tokens = 1
	}
	w.Header().Set("X-LLMGW-Token-Count", "estimate")
	writeJSON(w, 200, map[string]any{"input_tokens": tokens})
}
