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

	"github.com/xibodev/llm-translate"
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

// messagesFallbackContext is fallbackContext for a Messages body. Its
// fallback_timeout_ms and affinity_key are the gateway's routing controls,
// not Messages fields, so they leave the body here: a native target would
// receive them, and the Chat adapter refuses a field it does not know.
func messagesFallbackContext(r *http.Request, raw map[string]any) context.Context {
	timeout := raw["fallback_timeout_ms"]
	affinity, _ := raw["affinity_key"].(string)
	delete(raw, "fallback_timeout_ms")
	delete(raw, "affinity_key")
	return fallbackContext(r, timeout, affinity)
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
		writeBodyError(w, err, 422, "invalid request body")
		return
	}
	encoded, _ := json.Marshal(raw)
	if json.Unmarshal(encoded, &req) != nil || req.Model == "" {
		writeError(w, 422, "model is required")
		return
	}
	transportMode, modeErr := requestedTransportMode(r)
	if modeErr != nil {
		writeError(w, http.StatusBadRequest, modeErr.Error())
		return
	}
	if pre := config.Get().GatewayPreamble; pre != "" && transportMode != "transparent" {
		raw["_llmgw_preamble"] = pre
	}
	ctx := messagesFallbackContext(r, raw)

	resolution, err := resolveModel(r.Context(), req.Model, principal)
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
	targets, polStatus, polMsg := authorizeKeyPolicy(principal, req.Model, resolution.Category, targets)
	if polStatus != 0 {
		recordFailureUsage("anthropic.messages", req.Model, principal, polStatus, "policy", started)
		writeError(w, polStatus, polMsg)
		return
	}
	if transportMode == "transparent" {
		target, transparentErr := exactNativeTransparentTarget(req.Model, "/v1/messages", resolution, callerOf(principal))
		if transparentErr != nil {
			recordFailureUsage("anthropic.messages", req.Model, principal, 400, "transparent_contract", started)
			writeError(w, http.StatusBadRequest, transparentErr.Error())
			return
		}
		if rejectTransparentStream(w, req.Stream) {
			recordFailureUsage("anthropic.messages", req.Model, principal, 400, "transparent_stream", started)
			return
		}
		provider, providerErr := providers.GetProviderForPrincipal(target.Provider, callerOf(principal))
		if providerErr != nil || !providers.SupportsAnthropicMessages(provider) {
			writeError(w, http.StatusBadRequest, "provider does not implement its catalog-declared native Messages surface")
			return
		}
		if !admitRequest(w, "anthropic.messages", req.Model, principal, "policy", started) {
			return
		}
		response, providerErr := providers.CompleteAnthropicMessagesContext(ctx, provider, target.Model, raw)
		if providerErr != nil {
			writeUpstreamError(w, providerErr)
			return
		}
		served := target
		w.Header().Set(transportModeHeader, "transparent")
		recordFromResponse("anthropic.messages", req.Model, &served, principal, response, time.Since(started).Milliseconds())
		writeJSON(w, http.StatusOK, response)
		return
	}

	if req.Stream {
		// A stream whose every target serves Messages natively reaches its
		// target as a request that does not stream does. Translation would
		// refuse what it cannot carry, such as cache_control and enabled
		// thinking, and drop what the stream returns, such as signatures and
		// cache usage.
		if router.ServesAnthropicMessagesNatively(targets, callerOf(principal)) {
			if !admitRequest(w, "anthropic.messages", req.Model, principal, "policy", started) {
				return
			}
			streamNativeMessagesSSE(w, ctx, targets, raw, req.Model, principal, started)
			return
		}
		conversion := translate.AnthropicRequestToOpenAIWithReport(raw)
		if lossErr := conversion.RejectMaterialLoss(); lossErr != nil {
			recordFailureUsage("anthropic.messages", req.Model, principal, 400, "compatibility", started)
			writeError(w, 400, "Streaming cannot preserve Anthropic request: "+lossErr.Error())
			return
		}
		if !admitRequest(w, "anthropic.messages", req.Model, principal, "policy", started) {
			return
		}
		converted, kw := conversion.Value.Messages, conversion.Value.Keywords
		if len(converted) < len(req.Messages) {
			converted = translate.AnthropicMessagesToOpenAI(req.Messages, req.System)
		}
		msgs := make([]providers.Message, len(converted))
		for i := range converted {
			msgs[i] = providers.Message(converted[i])
		}
		streamMessagesSSE(w, ctx, targets, msgs, req.Model, principal, providers.Kwargs(kw), started)
		return
	}
	if !admitRequest(w, "anthropic.messages", req.Model, principal, "policy", started) {
		return
	}

	response, served, err := router.ExecuteAnthropicMessagesContext(governed(ctx, principal), targets, raw, req.Model, callerOf(principal))
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
	w.Header().Set(transportModeHeader, targetTransportMode(*served, callerOf(principal), "/v1/messages"))
	writeJSON(w, 200, response)
}

func streamMessagesSSE(w http.ResponseWriter, ctx context.Context, targets []router.Target, msgs []providers.Message, requested string, principal *config.Principal, kw providers.Kwargs, started time.Time) {
	it, served, err := router.ExecuteAnthropicStreamContext(governed(ctx, principal), targets, msgs, requested, callerOf(principal), kw)
	if err != nil {
		if ctx.Err() != nil {
			recordClientCancelled("anthropic.messages", requested, principal, started)
			return
		}
		recordFailureUsage(
			"anthropic.messages", requested, principal, upstreamErrorStatus(err),
			"upstream", started,
		)
		writeUpstreamError(w, err)
		return
	}
	defer it.Close()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	meter := newStreamUsage("anthropic.messages", requested, principal, served, started, payloadBytes(msgs))
	var writeErr error
	write := func(event string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return writeAndFlush(w, []byte(event))
	}
	// ended is set once the source stream ended, as opposed to the client
	// going away. The translator then closes the message however the source
	// ended, so those closing events wait in closing until it is known.
	ended := false
	var closing []string
	// pull func peeks usage as it forwards OpenAI chunks into the translator
	pull := func() (string, bool) {
		if writeErr != nil || ctx.Err() != nil {
			return "", false
		}
		chunk, more := it.Next()
		if !more {
			ended = ctx.Err() == nil
			return "", false
		}
		meter.chatChunk(chunk)
		if ctx.Err() != nil {
			return "", false
		}
		return chunk, true
	}
	translate.OpenAIStreamToAnthropicSSE(pull, served.Model, func(event string) {
		switch {
		case ended:
			closing = append(closing, event)
		case writeErr == nil:
			writeErr = write(event)
		}
	})
	if writeErr != nil || !ended || ctx.Err() != nil {
		meter.cancelled()
		return
	}
	if err := it.Err(); err != nil {
		// Anthropic ends a stream it cannot finish with an error event and
		// no message_stop, so a client never reads it as complete.
		if write(anthropicStreamErrorEvent(err)) != nil {
			meter.cancelled()
			return
		}
		meter.failed()
		return
	}
	for _, event := range closing {
		if write(event) != nil {
			meter.cancelled()
			return
		}
	}
	meter.completed()
}

// anthropicStreamErrorEvent is the Messages error event for a stream its
// upstream failed, typed by the upstream's status where it reported one.
func anthropicStreamErrorEvent(err error) string {
	return "event: error\ndata: " + jsonStr(map[string]any{
		"type": "error", "error": map[string]any{
			"type": anthropicErrorType(upstreamErrorStatus(err)), "message": "Upstream provider stream failed.",
		},
	}) + "\n\n"
}

// streamNativeMessagesSSE serves a stream whose targets all serve Messages
// natively: payload reaches the target that serves it as a request that does
// not stream reaches it, and each record that target sends reaches the
// client as it was sent. The stream succeeds only with the upstream's
// message_stop; one that ends otherwise ends with an error event, the
// upstream's own or, when it sent none, the gateway's.
func streamNativeMessagesSSE(w http.ResponseWriter, ctx context.Context, targets []router.Target, payload map[string]any, requested string, principal *config.Principal, started time.Time) {
	it, served, err := router.ExecuteAnthropicMessagesStreamContext(governed(ctx, principal), targets, payload, requested, callerOf(principal))
	if err != nil {
		if ctx.Err() != nil {
			recordClientCancelled("anthropic.messages", requested, principal, started)
			return
		}
		recordFailureUsage(
			"anthropic.messages", requested, principal, upstreamErrorStatus(err),
			"upstream", started,
		)
		writeUpstreamError(w, err)
		return
	}
	defer it.Close()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	meter := newStreamUsage("anthropic.messages", requested, principal, served, started, messagesPromptBytes(payload))
	write := func(records string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return writeAndFlush(w, []byte(records))
	}
	// message_delta tells a client how the answer stopped, so from the first
	// one on, records wait for message_stop: a stream that fails before it
	// never carries a stop reason, as a translated one never does. Anthropic
	// sends only further deltas and pings between the two; any other record
	// goes out at once, with what waited.
	var held strings.Builder
	for {
		record, more := it.Next()
		if !more {
			break
		}
		event := anthropicRecordEvent(record)
		meter.anthropicEvent(event)
		eventType, _ := event["type"].(string)
		switch {
		case eventType == "error":
			// Anthropic ends a stream it cannot finish with an error event,
			// which tells the client what failed, and sends nothing after it.
			if write(record) != nil {
				meter.cancelled()
				return
			}
			meter.failed()
			return
		case eventType == "message_delta" || (eventType == "ping" && held.Len() > 0):
			held.WriteString(record)
			continue
		}
		if write(held.String()+record) != nil {
			meter.cancelled()
			return
		}
		held.Reset()
		if eventType == "message_stop" {
			meter.completed()
			return
		}
	}
	if ctx.Err() != nil {
		meter.cancelled()
		return
	}
	// The upstream ended the stream without message_stop or an error event,
	// so the client learns of the failure from the gateway's.
	if write(anthropicStreamErrorEvent(it.Err())) != nil {
		meter.cancelled()
		return
	}
	meter.failed()
}

// anthropicRecordEvent is the event a Messages stream record carries as its
// data, read as core's Anthropic reads it, or nil when the data is not a
// JSON object.
func anthropicRecordEvent(record string) map[string]any {
	var data []string
	for _, line := range strings.Split(record, "\n") {
		if field, value, _ := strings.Cut(strings.TrimSuffix(line, "\r"), ":"); field == "data" {
			data = append(data, strings.TrimPrefix(value, " "))
		}
	}
	var event map[string]any
	if json.Unmarshal([]byte(strings.Join(data, "\n")), &event) != nil {
		return nil
	}
	return event
}

// messagesPromptBytes is the size of what a Messages request sends as its
// prompt: its system prompt and its messages.
func messagesPromptBytes(payload map[string]any) int {
	return payloadBytes(payload["system"]) + payloadBytes(payload["messages"])
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
	resolution, err := resolveModel(r.Context(), model, principal)
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
	provider, err := providers.GetProviderForPrincipal(target.Provider, callerOf(principal))
	if err != nil {
		writeUpstreamError(w, err)
		return
	}
	if providers.SupportsAnthropicTokenCount(provider) {
		count, err := providers.CountAnthropicTokensContext(r.Context(), provider, target.Model, raw, r.Header.Get("anthropic-version"), r.Header.Values("anthropic-beta"))
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
	tokens := max(1, estimatedTokens(len(compact)))
	w.Header().Set("X-LLMGW-Token-Count", "estimate")
	writeJSON(w, 200, map[string]any{"input_tokens": tokens})
}
