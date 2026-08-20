package translate

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// This file translates between the OpenAI Chat Completions schema (the gateway's
// internal pivot) and the OpenAI Responses API. Both are OpenAI-family standards,
// so these transforms are provider-agnostic — any backend that only exposes
// /responses for a model (e.g. Copilot's gpt-5.5) reuses them. They are pure
// functions; the decision of WHEN to apply them lives in the transport layer.

// ChatToResponses converts a Chat Completions request body into a Responses API
// request body.
func ChatToResponses(model string, messages []map[string]any, kw map[string]any, stream bool) map[string]any {
	payload := map[string]any{"model": model}
	var instructions []string
	input := make([]any, 0, len(messages))

	for _, m := range messages {
		role, _ := m["role"].(string)
		switch role {
		case "system", "developer":
			if t := contentToText(m["content"]); t != "" {
				instructions = append(instructions, t)
			}
		case "tool":
			callID, _ := m["tool_call_id"].(string)
			input = append(input, map[string]any{
				"type":    "function_call_output",
				"call_id": callID,
				"output":  contentToText(m["content"]),
			})
		case "assistant":
			if tc, ok := asList(m["tool_calls"]); ok && len(tc) > 0 {
				if t := contentToText(m["content"]); t != "" {
					input = append(input, map[string]any{"role": "assistant", "content": t})
				}
				for _, c := range tc {
					cm, ok := asMap(c)
					if !ok {
						continue
					}
					fn, _ := asMap(cm["function"])
					name, _ := fn["name"].(string)
					args, _ := fn["arguments"].(string)
					id, _ := cm["id"].(string)
					input = append(input, map[string]any{
						"type": "function_call", "call_id": id, "name": name, "arguments": args,
					})
				}
			} else {
				input = append(input, map[string]any{"role": "assistant", "content": contentToText(m["content"])})
			}
		default:
			r := role
			if r == "" {
				r = "user"
			}
			input = append(input, map[string]any{"role": r, "content": responsesUserContent(m["content"])})
		}
	}

	payload["input"] = input
	if len(instructions) > 0 {
		payload["instructions"] = strings.Join(instructions, "\n\n")
	}
	if v := firstNonNil(kw, "max_completion_tokens", "max_tokens"); v != nil {
		payload["max_output_tokens"] = v
	}
	if v := kw["reasoning_effort"]; v != nil {
		payload["reasoning"] = map[string]any{"effort": v}
	}
	if v := kw["temperature"]; v != nil {
		payload["temperature"] = v
	}
	if v := kw["top_p"]; v != nil {
		payload["top_p"] = v
	}
	if tools, ok := asList(kw["tools"]); ok && len(tools) > 0 {
		payload["tools"] = responsesTools(tools)
	}
	if tc := kw["tool_choice"]; tc != nil {
		payload["tool_choice"] = responsesToolChoice(tc)
	}
	if stream {
		payload["stream"] = true
	}
	return payload
}

// ResponsesRequestToChat converts supported Responses input/history/tools into
// the gateway's internal Chat representation. Built-in Responses-only tools
// fail explicitly instead of being silently flattened.
func ResponsesRequestToChat(
	payload map[string]any,
) ([]map[string]any, map[string]any, error) {
	allowed := map[string]bool{
		"model": true, "input": true, "instructions": true, "stream": true,
		"max_output_tokens": true, "reasoning": true, "temperature": true,
		"top_p": true, "tools": true, "tool_choice": true,
		"metadata": true, "store": true, "background": true,
		"force_api_support": true, "previous_response_id": true, "conversation": true,
	}
	for key, value := range payload {
		if !allowed[key] && value != nil {
			return nil, nil, fmt.Errorf(
				"Responses field %q requires a native Responses model", key,
			)
		}
	}
	if value := payload["previous_response_id"]; value != nil && value != "" {
		return nil, nil, fmt.Errorf(
			"previous_response_id requires a native Responses model",
		)
	}
	if value := payload["conversation"]; value != nil {
		return nil, nil, fmt.Errorf(
			"conversation state requires a native Responses model",
		)
	}
	if value := payload["store"]; value != nil {
		enabled, ok := value.(bool)
		if !ok || enabled {
			return nil, nil, fmt.Errorf("store requires a native Responses model unless false")
		}
	}
	if value := payload["background"]; value != nil {
		enabled, ok := value.(bool)
		if !ok || enabled {
			return nil, nil, fmt.Errorf("background requires a native Responses model unless false")
		}
	}
	if instructions := payload["instructions"]; instructions != nil {
		if _, ok := instructions.(string); !ok {
			return nil, nil, fmt.Errorf(
				"structured instructions require a native Responses model",
			)
		}
	}
	messages := []map[string]any{}
	if instructions := contentToText(payload["instructions"]); instructions != "" {
		messages = append(messages, map[string]any{
			"role": "system", "content": instructions,
		})
	}
	appendMessage := func(role string, content any) {
		if role == "" {
			role = "user"
		}
		messages = append(messages, map[string]any{"role": role, "content": content})
	}
	switch input := payload["input"].(type) {
	case string:
		appendMessage("user", input)
	case []any:
		for _, raw := range input {
			item, ok := asMap(raw)
			if !ok {
				return nil, nil, fmt.Errorf("Responses input item must be an object")
			}
			itemType, _ := item["type"].(string)
			switch itemType {
			case "", "message":
				role, _ := item["role"].(string)
				content, err := responsesInputToChatContent(item["content"])
				if err != nil {
					return nil, nil, err
				}
				appendMessage(role, content)
			case "function_call":
				call := map[string]any{
					"id": item["call_id"], "type": "function",
					"function": map[string]any{
						"name": item["name"], "arguments": item["arguments"],
					},
				}
				if len(messages) > 0 && messages[len(messages)-1]["role"] == "assistant" &&
					messages[len(messages)-1]["content"] == nil {
					calls, _ := messages[len(messages)-1]["tool_calls"].([]any)
					messages[len(messages)-1]["tool_calls"] = append(calls, call)
				} else {
					appendMessage("assistant", nil)
					messages[len(messages)-1]["tool_calls"] = []any{call}
				}
			case "function_call_output":
				output, err := responsesToolOutputToText(item["output"])
				if err != nil {
					return nil, nil, err
				}
				messages = append(messages, map[string]any{
					"role": "tool", "tool_call_id": item["call_id"],
					"content": output,
				})
			default:
				return nil, nil, fmt.Errorf(
					"Responses input type %q is not supported by Chat fallback", itemType,
				)
			}
		}
	case map[string]any:
		role, _ := input["role"].(string)
		content, err := responsesInputToChatContent(input["content"])
		if err != nil {
			return nil, nil, err
		}
		appendMessage(role, content)
	case nil:
		return nil, nil, fmt.Errorf("Responses input is required")
	default:
		return nil, nil, fmt.Errorf("Responses input has an unsupported shape")
	}
	kw := map[string]any{}
	if value := payload["max_output_tokens"]; value != nil {
		kw["_max_output_tokens"] = value
	}
	if reasoning, ok := asMap(payload["reasoning"]); ok {
		for key, value := range reasoning {
			if key != "effort" && value != nil {
				return nil, nil, fmt.Errorf(
					"Responses reasoning field %q requires a native Responses model",
					key,
				)
			}
		}
		if effort := reasoning["effort"]; effort != nil {
			kw["reasoning_effort"] = effort
		}
	}
	for _, key := range []string{"temperature", "top_p"} {
		if value := payload[key]; value != nil {
			kw[key] = value
		}
		if metadata, ok := asMap(payload["metadata"]); ok && len(metadata) > 0 {
			kw["metadata"] = metadata
		}
	}
	if tools, ok := asList(payload["tools"]); ok {
		converted := make([]any, 0, len(tools))
		for _, raw := range tools {
			tool, ok := asMap(raw)
			if !ok || tool["type"] != "function" {
				return nil, nil, fmt.Errorf(
					"Responses built-in tools require a native Responses model",
				)
			}
			allowedToolFields := map[string]bool{
				"type": true, "name": true, "description": true,
				"parameters": true, "strict": true,
			}
			for key, value := range tool {
				if !allowedToolFields[key] && value != nil {
					return nil, nil, fmt.Errorf(
						"Responses function tool field %q requires a native Responses model",
						key,
					)
				}
			}
			function := map[string]any{
				"name": tool["name"], "description": tool["description"],
				"parameters": tool["parameters"],
			}
			if strict := tool["strict"]; strict != nil {
				function["strict"] = strict
			}
			converted = append(converted, map[string]any{
				"type": "function", "function": function,
			})
		}
		kw["tools"] = converted
	}
	if choice := payload["tool_choice"]; choice != nil {
		if choiceMap, ok := asMap(choice); ok {
			if choiceMap["type"] != "function" {
				return nil, nil, fmt.Errorf(
					"Responses tool choice type %q requires a native Responses model",
					choiceMap["type"],
				)
			}
			kw["tool_choice"] = map[string]any{
				"type": "function", "function": map[string]any{"name": choiceMap["name"]},
			}
		} else {
			kw["tool_choice"] = choice
		}
	}
	if payload["stream"] == true {
		kw["stream_options"] = map[string]any{"include_usage": true}
	}
	return messages, kw, nil
}

func responsesToolOutputToText(value any) (string, error) {
	if text, ok := value.(string); ok {
		return text, nil
	}
	parts, ok := asList(value)
	if !ok {
		return contentToText(value), nil
	}
	var text strings.Builder
	for _, raw := range parts {
		part, ok := asMap(raw)
		if !ok {
			return "", fmt.Errorf("Responses function output part must be an object")
		}
		switch part["type"] {
		case "input_text", "text", "output_text":
			if value, _ := part["text"].(string); value != "" {
				text.WriteString(value)
			}
		default:
			return "", fmt.Errorf(
				"Responses function output type %q requires a native Responses model",
				part["type"],
			)
		}
	}
	return text.String(), nil
}

func responsesInputToChatContent(value any) (any, error) {
	if text, ok := value.(string); ok {
		return text, nil
	}
	parts, ok := asList(value)
	if !ok {
		return contentToText(value), nil
	}
	out := make([]any, 0, len(parts))
	var textOnly strings.Builder
	allText := true
	for _, raw := range parts {
		part, ok := asMap(raw)
		if !ok {
			continue
		}
		switch part["type"] {
		case "input_text", "text", "output_text":
			if value, _ := part["text"].(string); value != "" {
				textOnly.WriteString(value)
			}
			out = append(out, map[string]any{
				"type": "text", "text": part["text"],
			})
		case "input_image", "image_url":
			allText = false
			if part["file_id"] != nil {
				return nil, fmt.Errorf(
					"Responses file-backed images require a native Responses model",
				)
			}
			imageURL := part["image_url"]
			if imageURL == nil {
				imageURL = part["url"]
			}
			if imageURL == nil || imageURL == "" {
				return nil, fmt.Errorf("Responses input image requires image_url")
			}
			image := map[string]any{"url": imageURL}
			if detail := part["detail"]; detail != nil {
				image["detail"] = detail
			}
			out = append(out, map[string]any{
				"type":      "image_url",
				"image_url": image,
			})
		case "refusal", "input_file", "file":
			return nil, fmt.Errorf(
				"Responses content type %q requires a native Responses model",
				part["type"],
			)
		default:
			return nil, fmt.Errorf(
				"Responses content type %q is not supported by Chat fallback",
				part["type"],
			)
		}
	}
	if allText {
		return textOnly.String(), nil
	}
	return out, nil
}

// ResponsesToChat converts a Responses API object into a chat.completion.
func ResponsesToChat(model string, resp map[string]any) map[string]any {
	id, _ := resp["id"].(string)
	if id == "" {
		id = chatCmplID()
	}
	var text strings.Builder
	var toolCalls []any
	tcIndex := 0
	if output, ok := asList(resp["output"]); ok {
		for _, item := range output {
			im, ok := asMap(item)
			if !ok {
				continue
			}
			switch im["type"] {
			case "message":
				if content, ok := asList(im["content"]); ok {
					for _, part := range content {
						pm, ok := asMap(part)
						if !ok {
							continue
						}
						if pm["type"] == "output_text" {
							if t, ok := pm["text"].(string); ok {
								text.WriteString(t)
							}
						}
					}
				}
			case "function_call":
				callID, _ := im["call_id"].(string)
				if callID == "" {
					callID, _ = im["id"].(string)
				}
				name, _ := im["name"].(string)
				args, _ := im["arguments"].(string)
				toolCalls = append(toolCalls, map[string]any{
					"index": tcIndex, "id": callID, "type": "function",
					"function": map[string]any{"name": name, "arguments": args},
				})
				tcIndex++
			}
		}
	}
	if text.Len() == 0 && len(toolCalls) == 0 {
		if ot, ok := resp["output_text"].(string); ok {
			text.WriteString(ot)
		}
	}

	msg := map[string]any{"role": "assistant"}
	finish := "stop"
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
		msg["content"] = nil
		finish = "tool_calls"
	} else {
		msg["content"] = text.String()
	}
	if responsesIncompleteMaxTokens(resp) {
		finish = "length"
	}

	out := map[string]any{
		"id": id, "object": "chat.completion", "model": model,
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}},
	}
	if usage := responsesUsage(resp["usage"]); usage != nil {
		out["usage"] = usage
	}
	return out
}

// ChatResponseToResponses converts the gateway's internal Chat Completions
// result back into the public Responses API envelope.
func ChatResponseToResponses(model string, chat map[string]any) map[string]any {
	return ChatResponseToResponsesWithRequest(model, chat, nil)
}

func ChatResponseToResponsesWithRequest(
	model string, chat map[string]any, request map[string]any,
) map[string]any {
	id, _ := chat["id"].(string)
	if strings.HasPrefix(id, "chatcmpl-") {
		id = "resp_" + strings.TrimPrefix(id, "chatcmpl-")
	}
	if id == "" {
		id = "resp_" + strings.TrimPrefix(chatCmplID(), "chatcmpl-")
	}
	output := []any{}
	outputText := ""
	status := "completed"
	var incomplete any
	if choices, ok := asList(chat["choices"]); ok && len(choices) > 0 {
		if choice, ok := asMap(choices[0]); ok {
			if reason, _ := choice["finish_reason"].(string); reason == "length" ||
				reason == "content_filter" {
				status = "incomplete"
				incompleteReason := "max_output_tokens"
				if reason == "content_filter" {
					incompleteReason = "content_filter"
				}
				incomplete = map[string]any{"reason": incompleteReason}
			}
			if message, ok := asMap(choice["message"]); ok {
				messageContent := []any{}
				if text, ok := message["content"].(string); ok && text != "" {
					outputText = text
					messageContent = append(messageContent, map[string]any{
						"type": "output_text", "text": text, "annotations": []any{},
					})
				}
				if refusal, _ := message["refusal"].(string); refusal != "" {
					messageContent = append(messageContent, map[string]any{
						"type": "refusal", "refusal": refusal,
					})
				}
				if len(messageContent) > 0 {
					output = append(output, map[string]any{
						"id":   "msg_" + strings.TrimPrefix(id, "resp_"),
						"type": "message", "status": status, "role": "assistant",
						"content": messageContent,
					})
				}
				if calls, ok := asList(message["tool_calls"]); ok {
					for _, value := range calls {
						call, ok := asMap(value)
						if !ok {
							continue
						}
						function, _ := asMap(call["function"])
						output = append(output, map[string]any{
							"type": "function_call", "status": status,
							"id": call["id"], "call_id": call["id"],
							"name": function["name"], "arguments": function["arguments"],
						})
					}
				}
			}
		}
	}
	response := NewResponseEnvelope(model, id, status, output, request)
	response["output_text"] = outputText
	if incomplete != nil {
		response["incomplete_details"] = incomplete
	}
	if usage, ok := asMap(chat["usage"]); ok && len(usage) > 0 {
		input := firstNumber(usage, "prompt_tokens", "input_tokens")
		outputTokens := firstNumber(usage, "completion_tokens", "output_tokens")
		total := firstNumber(usage, "total_tokens")
		if total == 0 {
			total = input + outputTokens
		}
		response["usage"] = map[string]any{
			"input_tokens": input, "output_tokens": outputTokens,
			"total_tokens": total,
			"input_tokens_details": map[string]any{
				"cached_tokens": tokenDetail(usage, "prompt_tokens_details", "cached_tokens"),
			},
			"output_tokens_details": map[string]any{
				"reasoning_tokens": tokenDetail(usage, "completion_tokens_details", "reasoning_tokens"),
			},
		}
	}

	return response
}

func tokenDetail(usage map[string]any, detailsKey, valueKey string) int {
	details, _ := asMap(usage[detailsKey])
	return firstNumber(details, valueKey)
}

// NewResponseEnvelope creates the schema-complete Response shape used whenever
// the gateway adapts a Chat-only backend or synthesizes a terminal failure.
func NewResponseEnvelope(
	model, id, status string, output []any, request map[string]any,
) map[string]any {
	if id == "" {
		id = "resp_" + strings.TrimPrefix(chatCmplID(), "chatcmpl-")
	}
	value := func(key string, fallback any) any {
		if request != nil {
			if current, ok := request[key]; ok {
				return current
			}
		}
		return fallback
	}
	reasoning := value("reasoning", map[string]any{
		"effort": nil, "summary": nil,
	})
	text := value("text", map[string]any{
		"format": map[string]any{"type": "text"},
	})
	metadata := value("metadata", map[string]any{})
	return map[string]any{
		"id": id, "object": "response", "created_at": time.Now().Unix(),
		"status": status, "background": false, "error": nil,
		"incomplete_details": nil, "instructions": value("instructions", nil),
		"max_output_tokens": value("max_output_tokens", nil),
		"max_tool_calls":    value("max_tool_calls", nil), "model": model,
		"output": output, "parallel_tool_calls": value("parallel_tool_calls", true),
		"previous_response_id": value("previous_response_id", nil),
		"reasoning":            reasoning, "service_tier": value("service_tier", "default"),
		"store": value("store", false), "temperature": value("temperature", nil),
		"text": text, "tool_choice": value("tool_choice", "auto"),
		"tools": value("tools", []any{}), "top_logprobs": value("top_logprobs", 0),
		"top_p": value("top_p", nil), "truncation": value("truncation", "disabled"),
		"usage": nil, "user": value("user", nil), "metadata": metadata,
	}
}

func FailedResponseEnvelope(
	model, id, code, message string, request map[string]any,
) map[string]any {
	response := NewResponseEnvelope(model, id, "failed", []any{}, request)
	response["error"] = map[string]any{
		"code": code, "message": message,
	}
	return response
}

func firstNumber(values map[string]any, keys ...string) int {
	for _, key := range keys {
		switch value := values[key].(type) {
		case float64:
			return int(value)
		case int:
			return value
		case int64:
			return int(value)
		}
	}
	return 0
}

// ResponsesToChatChunks renders a (buffered) Responses reply as a small sequence
// of chat.completion.chunk payloads for the streaming code path.
func ResponsesToChatChunks(model string, resp map[string]any) []string {
	chat := ResponsesToChat(model, resp)
	choice := chat["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	finish := choice["finish_reason"]
	id, _ := chat["id"].(string)

	mk := func(delta map[string]any, fin any, usage any) string {
		c := map[string]any{
			"id": id, "object": "chat.completion.chunk", "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": fin}},
		}
		if usage != nil {
			c["usage"] = usage
		}
		b, _ := json.Marshal(c)
		return string(b)
	}

	chunks := []string{mk(map[string]any{"role": "assistant"}, nil, nil)}
	if tc, ok := msg["tool_calls"]; ok {
		chunks = append(chunks, mk(map[string]any{"tool_calls": tc}, nil, nil))
	} else if content, ok := msg["content"].(string); ok && content != "" {
		chunks = append(chunks, mk(map[string]any{"content": content}, nil, nil))
	}
	chunks = append(chunks, mk(map[string]any{}, finish, chat["usage"]))
	return chunks
}

// ---- helpers ------------------------------------------------------------ //

func firstNonNil(kw map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := kw[k]; ok && v != nil {
			return v
		}
	}
	return nil
}

func contentToText(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, part := range c {
			if pm, ok := asMap(part); ok {
				if t, ok := pm["text"].(string); ok {
					b.WriteString(t)
				}
			}
		}
		return b.String()
	default:
		return ""
	}
}

func responsesUserContent(v any) any {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		out := make([]any, 0, len(c))
		for _, part := range c {
			pm, ok := asMap(part)
			if !ok {
				continue
			}
			switch pm["type"] {
			case "text", "input_text":
				out = append(out, map[string]any{"type": "input_text", "text": pm["text"]})
			case "image_url", "input_image":
				img := pm["image_url"]
				if m, ok := asMap(img); ok {
					img = m["url"]
				}
				out = append(out, map[string]any{"type": "input_image", "image_url": img})
			default:
				out = append(out, part)
			}
		}
		if len(out) == 0 {
			return ""
		}
		return out
	default:
		return contentToText(v)
	}
}

func responsesTools(tools []any) []any {
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		tm, ok := asMap(t)
		if !ok {
			out = append(out, t)
			continue
		}
		if tm["type"] == "function" {
			if fn, ok := asMap(tm["function"]); ok {
				rt := map[string]any{"type": "function", "name": fn["name"]}
				if v, ok := fn["description"]; ok {
					rt["description"] = v
				}
				if v, ok := fn["parameters"]; ok {
					rt["parameters"] = v
				}
				if v, ok := fn["strict"]; ok {
					rt["strict"] = v
				}
				out = append(out, rt)
				continue
			}
		}
		out = append(out, tm)
	}
	return out
}

func responsesToolChoice(tc any) any {
	m, ok := asMap(tc)
	if !ok {
		return tc // "auto" | "none" | "required"
	}
	if m["type"] == "function" {
		if fn, ok := asMap(m["function"]); ok {
			return map[string]any{"type": "function", "name": fn["name"]}
		}
	}
	return tc
}

func responsesUsage(v any) map[string]any {
	u, ok := asMap(v)
	if !ok {
		return nil
	}
	in := toInt(u["input_tokens"])
	out := toInt(u["output_tokens"])
	total := toInt(u["total_tokens"])
	if total == 0 {
		total = in + out
	}
	return map[string]any{"prompt_tokens": in, "completion_tokens": out, "total_tokens": total}
}

func responsesIncompleteMaxTokens(resp map[string]any) bool {
	if resp["status"] != "incomplete" {
		return false
	}
	d, ok := asMap(resp["incomplete_details"])
	if !ok {
		return false
	}
	return d["reason"] == "max_output_tokens"
}

// PreferredEndpoint decides which OpenAI-family endpoint a model should use
// given its published supported_surfaces. Returns "responses" only when the
// model advertises /responses but NOT /chat/completions; otherwise "chat"
// (the safe default, including when the list is empty/unknown).
func PreferredEndpoint(supportedEndpoints []string) string {
	if len(supportedEndpoints) == 0 {
		return "chat"
	}
	hasChat, hasResponses := false, false
	for _, e := range supportedEndpoints {
		switch strings.TrimSpace(strings.ToLower(e)) {
		case "/chat/completions":
			hasChat = true
		case "/responses":
			hasResponses = true
		}
	}
	if hasResponses && !hasChat {
		return "responses"
	}
	return "chat"
}
