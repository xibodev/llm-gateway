package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
	"llmgw/internal/router"
	"llmgw/internal/translate"
)

func handleResponsesAPI(w http.ResponseWriter, r *http.Request) {
	principal, ok := authed(w, r)
	if !ok {
		return
	}
	var payload map[string]any
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid request body")
		return
	}
	raw, _ := json.Marshal(payload)
	var request responsesRequest
	if json.Unmarshal(raw, &request) != nil || strings.TrimSpace(request.Model) == "" {
		writeError(w, http.StatusUnprocessableEntity, "model is required")
		return
	}
	publicPayload := cloneResponsesPayload(payload)
	if preamble := config.Get().GatewayPreamble; preamble != "" {
		switch instructions := payload["instructions"].(type) {
		case string:
			payload["instructions"] = preamble + "\n\n" + instructions
		case nil:
			payload["instructions"] = preamble
		default:
			payload["input"] = prependResponsesDeveloperMessage(
				payload["input"], preamble,
			)
		}
	}
	responsesDispatch(w, r, payload, publicPayload, &request, principal)
}

func responsesDispatch(
	w http.ResponseWriter,
	r *http.Request,
	payload, publicPayload map[string]any,
	request *responsesRequest,
	principal *config.Principal,
) {
	started := time.Now()
	if background, _ := payload["background"].(bool); background {
		recordFailureUsage("openai.responses", request.Model, principal, 400, "background_unsupported", started)
		writeError(w, 400, "background Responses jobs are not supported by this gateway.")
		return
	}
	resolution, err := router.ResolveForPrincipal(request.Model, principal)
	if err != nil {
		if _, missing := err.(*router.ModelNotFoundError); missing {
			recordFailureUsage("openai.responses", request.Model, principal, 404, "model_not_found", started)
			writeError(w, 404, err.Error())
			return
		}
		recordFailureUsage("openai.responses", request.Model, principal, 500, "route_config", started)
		writeError(w, 500, "Gateway is not configured for the requested model.")
		return
	}
	targets, status, message := enforceKeyPolicy(
		principal, request.Model, resolution.Category, resolution.Targets,
	)
	if status != 0 {
		recordFailureUsage("openai.responses", request.Model, principal, status, "policy", started)
		writeError(w, status, message)
		return
	}
	if responsesRequestHasProviderState(payload) {
		if !isExactProviderModelResolution(request.Model, resolution) {
			recordFailureUsage("openai.responses", request.Model, principal, 400, "stateful_route", started)
			writeError(w, 400, "Stateful Responses requests require an exact provider/model target.")
			return
		}
		if principal == nil || principal.PrincipalKind != "human" ||
			strings.TrimSpace(principal.PrincipalID) == "" {
			recordFailureUsage("openai.responses", request.Model, principal, 403, "stateful_credential", started)
			writeError(w, 403, "Stateful Responses requests require a private human provider connection.")
			return
		}
		private, privateErr := iam.HasResolvablePrivateProviderConnection(
			principal.PrincipalID, resolution.Targets[0].Provider,
		)
		if privateErr != nil || !private {
			recordFailureUsage("openai.responses", request.Model, principal, 403, "stateful_credential", started)
			writeError(w, 403, "Stateful Responses requests require a private human provider connection.")
			return
		}
	}
	if responsesRequestIsMultimodal(payload) {
		targets = filterVisionTargets(targets)
		if len(targets) == 0 {
			recordFailureUsage("openai.responses", request.Model, principal, 400, "vision_unavailable", started)
			writeError(w, 400, "This request includes an image but no vision-capable model is available in the requested route.")
			return
		}
	}
	if request.Stream {
		streamResponsesSSE(
			w, targets, payload, publicPayload, request.Model, principal, started,
		)
		return
	}
	response, served, err := router.ExecuteResponsesContext(
		r.Context(), targets, payload, request.Model, principal,
	)
	if err != nil {
		recordFailureUsage(
			"openai.responses", request.Model, principal,
			upstreamErrorStatus(err), "upstream", started,
		)
		writeUpstreamError(w, err)
		return
	}
	sanitizeResponsesForCaller(response, publicPayload)
	recordFromResponses(
		request.Model, served, principal, response,
		time.Since(started).Milliseconds(),
	)
	writeJSON(w, http.StatusOK, response)
}

func cloneResponsesPayload(payload map[string]any) map[string]any {
	cloned := make(map[string]any, len(payload))
	for key, value := range payload {
		cloned[key] = value
	}
	return cloned
}

func sanitizeResponsesForCaller(
	response map[string]any, publicPayload map[string]any,
) {
	if response == nil {
		return
	}
	if instructions, ok := publicPayload["instructions"]; ok {
		response["instructions"] = instructions
	} else {
		response["instructions"] = nil
	}
}

func prependResponsesDeveloperMessage(input any, preamble string) []any {
	message := map[string]any{
		"type": "message", "role": "developer",
		"content": []any{map[string]any{
			"type": "input_text", "text": preamble,
		}},
	}
	switch current := input.(type) {
	case []any:
		return append([]any{message}, current...)
	case nil:
		return []any{message}
	case string:
		return []any{
			message,
			map[string]any{
				"type": "message", "role": "user",
				"content": []any{map[string]any{
					"type": "input_text", "text": current,
				}},
			},
		}
	default:
		return []any{message, current}
	}
}

func responsesRequestHasProviderState(payload map[string]any) bool {
	return providers.ResponsesPayloadIsStateful(payload)
}

func isExactProviderModelResolution(
	model string, resolution router.Resolution,
) bool {
	if resolution.Category != "" || len(resolution.Targets) != 1 {
		return false
	}
	providerID, nativeModel, ok := strings.Cut(strings.TrimSpace(model), "/")
	if !ok || strings.TrimSpace(nativeModel) == "" {
		return false
	}
	_, configured := config.Get().Providers[providerID]
	return configured &&
		resolution.Targets[0].Provider == providerID &&
		resolution.Targets[0].Model == nativeModel
}

func responsesRequestIsMultimodal(payload map[string]any) bool {
	return responsesPayloadContainsType(payload["input"], "input_image", "image_url")
}

func responsesPayloadContainsType(value any, types ...string) bool {
	switch current := value.(type) {
	case []any:
		for _, nested := range current {
			if responsesPayloadContainsType(nested, types...) {
				return true
			}
		}
	case map[string]any:
		currentType, _ := current["type"].(string)
		for _, candidate := range types {
			if currentType == candidate {
				return true
			}
		}
		for _, nested := range current {
			if responsesPayloadContainsType(nested, types...) {
				return true
			}
		}
	}
	return false
}

func streamResponsesSSE(
	w http.ResponseWriter,
	targets []router.Target,
	payload, publicPayload map[string]any,
	requested string,
	principal *config.Principal,
	started time.Time,
) {
	execution, served, err := router.ExecuteResponsesStream(
		targets, payload, requested, principal,
	)
	if err != nil {
		recordFailureUsage(
			"openai.responses", requested, principal,
			upstreamErrorStatus(err), "upstream", started,
		)
		writeUpstreamError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	if execution.Native {
		streamNativeResponseEvents(
			w, flusher, execution.Iter, publicPayload, requested, served, principal, started,
		)
		return
	}
	streamChatAsResponseEvents(
		w, flusher, execution.Iter, publicPayload, requested, served, principal, started,
	)
}

func writeResponseEvent(
	w http.ResponseWriter, flusher http.Flusher, event string, payload map[string]any,
) {
	encoded, _ := json.Marshal(payload)
	_, _ = w.Write([]byte("event: " + event + "\n"))
	_, _ = w.Write([]byte("data: " + string(encoded) + "\n\n"))
	if flusher != nil {
		flusher.Flush()
	}
}

func streamNativeResponseEvents(
	w http.ResponseWriter,
	flusher http.Flusher,
	iterator providers.StreamIter,
	request map[string]any,
	requested string,
	served *router.Target,
	principal *config.Principal,
	started time.Time,
) {
	var finalResponse map[string]any
	responseID := ""
	sequence := 0
	for {
		chunk, more := iterator.Next()
		if !more {
			break
		}
		var event map[string]any
		if json.Unmarshal([]byte(chunk), &event) != nil {
			continue
		}
		eventType, _ := event["type"].(string)
		if eventType == "" {
			continue
		}
		if response, _ := event["response"].(map[string]any); response != nil {
			sanitizeResponsesForCaller(response, request)
		}
		if eventType == "response.created" {
			if response, _ := event["response"].(map[string]any); response != nil {
				responseID, _ = response["id"].(string)
			}
		}
		if eventType == "response.completed" || eventType == "response.incomplete" {
			finalResponse, _ = event["response"].(map[string]any)
			writeResponseEvent(w, flusher, eventType, event)
			_ = iterator.Close()
			recordFromResponses(
				requested, served, principal, finalResponse,
				time.Since(started).Milliseconds(),
			)
			return
		}
		if eventType == "response.failed" {
			writeResponseEvent(w, flusher, eventType, event)
			_ = iterator.Close()
			recordFailureUsage("openai.responses", requested, principal, 502, "upstream_stream", started)
			return
		}
		if value := firstInt(event, "sequence_number"); value >= sequence {
			sequence = value + 1
		}
		writeResponseEvent(w, flusher, eventType, event)
	}
	streamErr := iterator.Err()
	_ = iterator.Close()
	message := "Upstream provider stream failed."
	if streamErr == nil {
		message = "Upstream provider stream ended without a terminal event."
	}
	writeResponseEvent(w, flusher, "response.failed", map[string]any{
		"type": "response.failed", "sequence_number": sequence,
		"response": translate.FailedResponseEnvelope(
			served.Model, responseID, "server_error", message, request,
		),
	})
	recordFailureUsage("openai.responses", requested, principal, 502, "upstream_stream", started)
}

func streamChatAsResponseEvents(
	w http.ResponseWriter,
	flusher http.Flusher,
	iterator providers.StreamIter,
	request map[string]any,
	requested string,
	served *router.Target,
	principal *config.Principal,
	started time.Time,
) {
	responseID := fmt.Sprintf("resp_%d", time.Now().UnixNano())
	messageID := "msg_" + strings.TrimPrefix(responseID, "resp_")
	sequence := 0
	writeResponseEvent(w, flusher, "response.created", map[string]any{
		"type": "response.created", "sequence_number": sequence,
		"response": translate.NewResponseEnvelope(
			served.Model, responseID, "in_progress", []any{}, request,
		),
	})
	sequence++
	messageItemStarted := false
	textStarted := false
	refusalStarted := false
	messageOutputIndex := 0
	var text strings.Builder
	var refusal strings.Builder
	toolCalls := map[int]map[string]any{}
	toolOutputIndexes := map[int]int{}
	finishReason := ""
	usage := map[string]any{}
	ensureMessageItem := func() {
		if messageItemStarted {
			return
		}
		messageOutputIndex = len(toolCalls)
		writeResponseEvent(w, flusher, "response.output_item.added", map[string]any{
			"type": "response.output_item.added", "sequence_number": sequence,
			"response_id": responseID, "output_index": messageOutputIndex,
			"item": map[string]any{
				"id": messageID, "type": "message",
				"status": "in_progress", "role": "assistant", "content": []any{},
			},
		})
		sequence++
		messageItemStarted = true
	}
	for {
		chunk, more := iterator.Next()
		if !more {
			break
		}
		var parsed map[string]any
		if json.Unmarshal([]byte(chunk), &parsed) != nil {
			continue
		}
		if chunkUsage, ok := parsed["usage"].(map[string]any); ok {
			usage = chunkUsage
		}
		choices, _ := parsed["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		choice, _ := choices[0].(map[string]any)
		if reason, ok := choice["finish_reason"].(string); ok && reason != "" {
			finishReason = reason
		}
		delta, _ := choice["delta"].(map[string]any)
		if content, ok := delta["content"].(string); ok && content != "" {
			ensureMessageItem()
			if !textStarted {
				writeResponseEvent(w, flusher, "response.content_part.added", map[string]any{
					"type": "response.content_part.added", "sequence_number": sequence,
					"response_id": responseID, "item_id": messageID,
					"output_index": messageOutputIndex, "content_index": 0,
					"part": map[string]any{
						"type": "output_text", "text": "", "annotations": []any{},
					},
				})
				sequence++
				textStarted = true
			}
			text.WriteString(content)
			writeResponseEvent(w, flusher, "response.output_text.delta", map[string]any{
				"type": "response.output_text.delta", "sequence_number": sequence,
				"response_id": responseID, "item_id": messageID,
				"output_index": messageOutputIndex, "content_index": 0,
				"delta": content, "logprobs": []any{},
			})
			sequence++
		}
		if value, ok := delta["refusal"].(string); ok && value != "" {
			ensureMessageItem()
			contentIndex := 0
			if textStarted {
				contentIndex = 1
			}
			if !refusalStarted {
				writeResponseEvent(w, flusher, "response.content_part.added", map[string]any{
					"type": "response.content_part.added", "sequence_number": sequence,
					"response_id": responseID, "item_id": messageID,
					"output_index": messageOutputIndex, "content_index": contentIndex,
					"part": map[string]any{"type": "refusal", "refusal": ""},
				})
				sequence++
				refusalStarted = true
			}
			refusal.WriteString(value)
			writeResponseEvent(w, flusher, "response.refusal.delta", map[string]any{
				"type": "response.refusal.delta", "sequence_number": sequence,
				"response_id": responseID, "item_id": messageID,
				"output_index": messageOutputIndex, "content_index": contentIndex,
				"delta": value,
			})
			sequence++
		}
		if rawCalls, ok := delta["tool_calls"].([]any); ok {
			for position, raw := range rawCalls {
				call, _ := raw.(map[string]any)
				index := position
				if explicit := firstInt(call, "index"); explicit != 0 || position == 0 {
					index = explicit
				}
				function, _ := call["function"].(map[string]any)
				name, _ := function["name"].(string)
				arguments, _ := function["arguments"].(string)
				current := toolCalls[index]
				if current == nil {
					callID := call["id"]
					outputIndex := index
					if messageItemStarted {
						outputIndex++
					}
					current = map[string]any{
						"id": callID, "call_id": callID,
						"type": "function_call", "status": "in_progress",
						"name": name, "arguments": "",
					}
					toolCalls[index] = current
					toolOutputIndexes[index] = outputIndex
					writeResponseEvent(w, flusher, "response.output_item.added", map[string]any{
						"type": "response.output_item.added", "sequence_number": sequence,
						"response_id": responseID, "output_index": outputIndex,
						"item": current,
					})
					sequence++
				}
				if name != "" {
					current["name"] = name
				}
				if arguments != "" {
					existing, _ := current["arguments"].(string)
					current["arguments"] = existing + arguments
					writeResponseEvent(w, flusher, "response.function_call_arguments.delta", map[string]any{
						"type":            "response.function_call_arguments.delta",
						"sequence_number": sequence, "response_id": responseID,
						"output_index": toolOutputIndexes[index],
						"item_id":      current["id"], "delta": arguments,
					})
					sequence++
				}
			}
		}
	}
	streamErr := iterator.Err()
	_ = iterator.Close()
	if streamErr != nil || finishReason == "" {
		message := "Upstream provider stream failed."
		if streamErr == nil {
			message = "Upstream provider stream ended without a finish reason."
		}
		writeResponseEvent(w, flusher, "response.failed", map[string]any{
			"type": "response.failed", "sequence_number": sequence,
			"response": translate.FailedResponseEnvelope(
				served.Model, responseID, "server_error", message, request,
			),
		})
		recordFailureUsage("openai.responses", requested, principal, 502, "upstream_stream", started)
		return
	}
	incomplete := finishReason == "length" || finishReason == "content_filter"
	if textStarted && !incomplete {
		writeResponseEvent(w, flusher, "response.output_text.done", map[string]any{
			"type": "response.output_text.done", "sequence_number": sequence,
			"response_id": responseID, "item_id": messageID,
			"output_index": messageOutputIndex, "content_index": 0,
			"text": text.String(), "logprobs": []any{},
		})
		sequence++
		writeResponseEvent(w, flusher, "response.content_part.done", map[string]any{
			"type": "response.content_part.done", "sequence_number": sequence,
			"response_id": responseID, "item_id": messageID,
			"output_index": messageOutputIndex, "content_index": 0,
			"part": map[string]any{
				"type": "output_text", "text": text.String(), "annotations": []any{},
			},
		})
		sequence++
	}
	if refusalStarted && !incomplete {
		contentIndex := 0
		if textStarted {
			contentIndex = 1
		}
		writeResponseEvent(w, flusher, "response.refusal.done", map[string]any{
			"type": "response.refusal.done", "sequence_number": sequence,
			"response_id": responseID, "item_id": messageID,
			"output_index": messageOutputIndex, "content_index": contentIndex,
			"refusal": refusal.String(),
		})
		sequence++
		writeResponseEvent(w, flusher, "response.content_part.done", map[string]any{
			"type": "response.content_part.done", "sequence_number": sequence,
			"response_id": responseID, "item_id": messageID,
			"output_index": messageOutputIndex, "content_index": contentIndex,
			"part": map[string]any{
				"type": "refusal", "refusal": refusal.String(),
			},
		})
		sequence++
	}
	chatToolCalls := []any{}
	for index := 0; index < len(toolCalls); index++ {
		call := toolCalls[index]
		if call == nil {
			continue
		}
		if !incomplete {
			call["status"] = "completed"
			writeResponseEvent(w, flusher, "response.function_call_arguments.done", map[string]any{
				"type": "response.function_call_arguments.done", "sequence_number": sequence,
				"response_id": responseID, "output_index": toolOutputIndexes[index],
				"item_id": call["id"], "arguments": call["arguments"],
			})
			sequence++
			writeResponseEvent(w, flusher, "response.output_item.done", map[string]any{
				"type": "response.output_item.done", "sequence_number": sequence,
				"response_id": responseID, "output_index": toolOutputIndexes[index],
				"item": call,
			})
			sequence++
		}
		chatToolCalls = append(chatToolCalls, map[string]any{
			"id": call["call_id"], "type": "function",
			"function": map[string]any{
				"name": call["name"], "arguments": call["arguments"],
			},
		})
	}
	message := map[string]any{"role": "assistant", "content": text.String()}
	if refusal.Len() > 0 {
		message["refusal"] = refusal.String()
	}
	if len(chatToolCalls) > 0 {
		message["tool_calls"] = chatToolCalls
	}
	chat := map[string]any{
		"id":    "chatcmpl-" + strings.TrimPrefix(responseID, "resp_"),
		"model": served.Model,
		"choices": []any{map[string]any{
			"index": 0, "message": message, "finish_reason": finishReason,
		}},
	}
	if len(usage) > 0 {
		chat["usage"] = usage
	}
	converted := translate.ChatResponseToResponsesWithRequest(
		served.Model, chat, request,
	)
	if messageItemStarted && !incomplete {
		output, _ := converted["output"].([]any)
		if len(output) > 0 {
			writeResponseEvent(w, flusher, "response.output_item.done", map[string]any{
				"type": "response.output_item.done", "sequence_number": sequence,
				"response_id": responseID, "output_index": messageOutputIndex,
				"item": output[0],
			})
			sequence++
		}
	}
	terminal := "response.completed"
	if converted["status"] == "incomplete" {
		terminal = "response.incomplete"
	}
	writeResponseEvent(w, flusher, terminal, map[string]any{
		"type": terminal, "sequence_number": sequence, "response": converted,
	})
	recordFromResponses(
		requested, served, principal, converted,
		time.Since(started).Milliseconds(),
	)
}

func recordFromResponses(
	requested string,
	served *router.Target,
	principal *config.Principal,
	response map[string]any,
	latencyMS int64,
) {
	inputTokens, outputTokens := 0, 0
	if usage, ok := response["usage"].(map[string]any); ok {
		inputTokens = firstInt(usage, "input_tokens", "prompt_tokens")
		outputTokens = firstInt(usage, "output_tokens", "completion_tokens")
	}
	router.RecordUsage(router.UsageRecord{
		Endpoint: "openai.responses", RequestedModel: requested,
		RoutedModel: served.Model, Provider: served.Provider,
		Project: principal.Project, Key: principal.Key,
		ProjectID: principal.ProjectID, PrincipalID: principal.PrincipalID,
		KeyID: principal.KeyID, InputTokens: inputTokens,
		OutputTokens: outputTokens, LatencyMS: latencyMS,
		IsStub: isStub(served.Provider),
	})
}
