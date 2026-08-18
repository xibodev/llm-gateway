package api

import (
	"encoding/json"
	"net/http"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/providers"
	"llmgw/internal/router"
	"llmgw/internal/translate"
)

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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 422, "invalid request body")
		return
	}
	openaiMessages := translate.AnthropicMessagesToOpenAI(req.Messages, req.System)
	msgs := make([]providers.Message, 0, len(openaiMessages)+1)
	if pre := config.Get().GatewayPreamble; pre != "" {
		msgs = append(msgs, providers.Message{"role": "system", "content": pre})
	}
	for _, m := range openaiMessages {
		msgs = append(msgs, providers.Message(m))
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
	kw := anthropicKwargs(&req)

	targets, polStatus, polMsg := enforceKeyPolicy(principal, req.Model, resolution.Category, targets)
	if polStatus != 0 {
		recordFailureUsage("anthropic.messages", req.Model, principal, polStatus, "policy", started)
		writeError(w, polStatus, polMsg)
		return
	}

	if req.Stream {
		streamMessagesSSE(w, targets, msgs, req.Model, principal, kw, started)
		return
	}

	response, served, err := router.ExecuteComplete(targets, msgs, req.Model, principal, kw)
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
	writeJSON(w, 200, translate.OpenAIResponseToAnthropic(response, served.Model))
}

func streamMessagesSSE(w http.ResponseWriter, targets []router.Target, msgs []providers.Message, requested string, principal *config.Principal, kw providers.Kwargs, started time.Time) {
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(200)

	it, served, err := router.ExecuteStream(targets, msgs, requested, principal, kw)
	if err != nil {
		recordFailureUsage(
			"anthropic.messages", requested, principal, upstreamErrorStatus(err),
			"upstream", started,
		)
		payload := map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": "Upstream provider request failed."}}
		_, _ = w.Write([]byte("event: error\ndata: " + jsonStr(payload) + "\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		return
	}

	usageAcc := map[string]int{"prompt_tokens": 0, "completion_tokens": 0}
	// pull func peeks usage as it forwards OpenAI chunks into the translator
	pull := func() (string, bool) {
		chunk, more := it.Next()
		if !more {
			return "", false
		}
		accumulateStreamUsage(chunk, usageAcc)
		return chunk, true
	}
	translate.OpenAIStreamToAnthropicSSE(pull, served.Model, func(event string) {
		_, _ = w.Write([]byte(event))
		if flusher != nil {
			flusher.Flush()
		}
	})
	_ = it.Close()
	router.RecordUsage(router.UsageRecord{
		Endpoint: "anthropic.messages", RequestedModel: requested, RoutedModel: served.Model,
		Provider: served.Provider, Project: principal.Project, Key: principal.Key,
		ProjectID: principal.ProjectID, PrincipalID: principal.PrincipalID, KeyID: principal.KeyID,
		InputTokens: usageAcc["prompt_tokens"], OutputTokens: usageAcc["completion_tokens"],
		LatencyMS: time.Since(started).Milliseconds(), IsStub: isStub(served.Provider),
	})
}

func handleCountTokens(w http.ResponseWriter, r *http.Request) {
	if _, ok := authed(w, r); !ok {
		return
	}
	var req anthropicRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 422, "invalid request body")
		return
	}
	openaiMessages := translate.AnthropicMessagesToOpenAI(req.Messages, req.System)
	total := 0
	for _, m := range openaiMessages {
		if c, ok := m["content"].(string); ok {
			total += len(c)
		}
	}
	tokens := total / 4
	if tokens < 1 {
		tokens = 1
	}
	writeJSON(w, 200, map[string]any{"input_tokens": tokens})
}
