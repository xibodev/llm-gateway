// Package translate round-trips between the Anthropic Messages API and the
// OpenAI Chat Completions schema. The gateway speaks OpenAI natively; this is
// the single translator that lets Claude Code and other Anthropic clients talk
// to it, and lets the native Anthropic backend be called from OpenAI-shaped
// internals.
//
// Faithful port of llmgw.api.anthropic_translate. JSON objects are modeled as
// map[string]any / []any to mirror the Python dict handling.
package translate

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// ---- helpers ------------------------------------------------------------ //

func randHex(n int) string {
	b := make([]byte, (n+1)/2)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)[:n]
}

func anthropicMessageID() string { return "msg_" + randHex(24) }
func anthropicToolUseID() string { return "toolu_" + randHex(24) }
func chatCmplID() string         { return "chatcmpl-" + randHex(24) }

func asMap(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

func asList(v any) ([]any, bool) {
	l, ok := v.([]any)
	return l, ok
}

func asStr(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	case string:
		i, _ := strconv.Atoi(n)
		return i
	}
	return 0
}

func jsonString(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// ---- Request: Anthropic -> OpenAI --------------------------------------- //

func allowedKeys(incompatible *[]string, path string, object map[string]any, keys ...string) {
	allowed := map[string]bool{}
	for _, key := range keys {
		allowed[key] = true
	}
	for key := range object {
		if !allowed[key] {
			if path != "" {
				key = path + "." + key
			}
			*incompatible = append(*incompatible, key)
		}
	}
}

// AnthropicRequestToOpenAI strictly converts the Messages core profile. The
// returned paths name every value that an OpenAI Chat target would lose.
func AnthropicRequestToOpenAI(payload map[string]any) ([]map[string]any, map[string]any, []string) {
	var incompatible []string
	allowedKeys(&incompatible, "", payload, "model", "messages", "max_tokens", "system", "tools", "tool_choice", "stream", "temperature", "top_p", "stop_sequences", "metadata", "thinking", "output_config", "_llmgw_preamble")
	textBlocks := func(value any, path string) (string, bool) {
		if text, ok := value.(string); ok {
			return text, true
		}
		blocks, ok := value.([]any)
		if !ok {
			incompatible = append(incompatible, path)
			return "", false
		}
		var texts []string
		for i, raw := range blocks {
			block, ok := raw.(map[string]any)
			blockPath := path + "." + strconv.Itoa(i)
			if !ok || block["type"] != "text" {
				incompatible = append(incompatible, blockPath)
				continue
			}
			allowedKeys(&incompatible, blockPath, block, "type", "text")
			text, ok := block["text"].(string)
			if !ok {
				incompatible = append(incompatible, blockPath)
				continue
			}
			texts = append(texts, text)
		}
		return strings.Join(texts, "\n\n"), true
	}
	var messages []map[string]any
	if preamble, _ := payload["_llmgw_preamble"].(string); preamble != "" {
		messages = append(messages, map[string]any{"role": "system", "content": preamble})
	}
	if system := payload["system"]; system != nil {
		if text, ok := textBlocks(system, "system"); ok {
			messages = append(messages, map[string]any{"role": "system", "content": text})
		}
	}
	rawMessages, ok := payload["messages"].([]any)
	if !ok {
		incompatible = append(incompatible, "messages")
	}
	for i, raw := range rawMessages {
		path := "messages." + strconv.Itoa(i)
		message, ok := raw.(map[string]any)
		if !ok {
			incompatible = append(incompatible, path)
			continue
		}
		allowedKeys(&incompatible, path, message, "role", "content")
		role, _ := message["role"].(string)
		content := message["content"]
		if text, ok := content.(string); ok && (role == "user" || role == "assistant") {
			messages = append(messages, map[string]any{"role": role, "content": text})
			continue
		}
		blocks, ok := content.([]any)
		if !ok || (role != "user" && role != "assistant") {
			incompatible = append(incompatible, path+".content")
			continue
		}
		var parts []any
		var assistantText []string
		var calls []any
		for j, rawBlock := range blocks {
			bp := path + ".content." + strconv.Itoa(j)
			block, ok := rawBlock.(map[string]any)
			if !ok {
				incompatible = append(incompatible, bp)
				continue
			}
			switch block["type"] {
			case "text":
				allowedKeys(&incompatible, bp, block, "type", "text")
				text, ok := block["text"].(string)
				if !ok {
					incompatible = append(incompatible, bp)
					continue
				}
				if role == "assistant" {
					assistantText = append(assistantText, text)
				} else {
					parts = append(parts, map[string]any{"type": "text", "text": text})
				}
			case "image":
				allowedKeys(&incompatible, bp, block, "type", "source")
				source, _ := block["source"].(map[string]any)
				allowedKeys(&incompatible, bp+".source", source, "type", "media_type", "data")
				mime, _ := source["media_type"].(string)
				data, dataOK := source["data"].(string)
				validMime := mime == "image/jpeg" || mime == "image/png" || mime == "image/gif" || mime == "image/webp"
				if role != "user" || source["type"] != "base64" || !validMime || !dataOK {
					incompatible = append(incompatible, bp)
					continue
				}
				parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:" + mime + ";base64," + data}})
			case "tool_use":
				allowedKeys(&incompatible, bp, block, "type", "id", "name", "input")
				id, idOK := block["id"].(string)
				name, nameOK := block["name"].(string)
				if role != "assistant" || !idOK || !nameOK || block["input"] == nil {
					incompatible = append(incompatible, bp)
					continue
				}
				calls = append(calls, map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": jsonString(block["input"])}})
			case "tool_result":
				allowedKeys(&incompatible, bp, block, "type", "tool_use_id", "content", "is_error")
				id, idOK := block["tool_use_id"].(string)
				text, textOK := textBlocks(block["content"], bp+".content")
				isError, hasIsError := block["is_error"]
				if role != "user" || !idOK || !textOK || (hasIsError && isError != false) {
					incompatible = append(incompatible, bp)
					continue
				}
				if len(parts) > 0 {
					messages = append(messages, map[string]any{"role": "user", "content": parts})
					parts = nil
				}
				messages = append(messages, map[string]any{"role": "tool", "tool_call_id": id, "content": text})
			default:
				incompatible = append(incompatible, bp)
			}
		}
		if role == "assistant" {
			messages = append(messages, map[string]any{"role": role, "content": strings.Join(assistantText, ""), "tool_calls": calls})
		} else if len(parts) > 0 {
			messages = append(messages, map[string]any{"role": role, "content": parts})
		}
	}
	kw := map[string]any{}
	for source, target := range map[string]string{"max_tokens": "max_tokens", "temperature": "temperature", "top_p": "top_p", "stop_sequences": "stop"} {
		if value := payload[source]; value != nil {
			kw[target] = value
		}
	}
	if metadata := payload["metadata"]; metadata != nil {
		if value, ok := metadata.(map[string]any); ok {
			kw["metadata"] = value
		} else {
			incompatible = append(incompatible, "metadata")
		}
	}
	if rawThinking := payload["thinking"]; rawThinking != nil {
		thinking, ok := rawThinking.(map[string]any)
		allowedKeys(&incompatible, "thinking", thinking, "type")
		if !ok || thinking["type"] != "disabled" {
			incompatible = append(incompatible, "thinking")
		} else {
			kw["thinking"] = thinking
		}
	}
	if rawOutputConfig := payload["output_config"]; rawOutputConfig != nil {
		outputConfig, ok := rawOutputConfig.(map[string]any)
		allowedKeys(&incompatible, "output_config", outputConfig, "effort")
		effort, effortOK := outputConfig["effort"].(string)
		switch effort {
		case "low", "medium", "high", "xhigh", "max":
		default:
			effortOK = false
		}
		if !ok || !effortOK {
			incompatible = append(incompatible, "output_config.effort")
		} else {
			kw["output_config"] = outputConfig
			kw["reasoning_effort"] = effort
		}
	}
	if rawTools := payload["tools"]; rawTools != nil {
		tools, ok := rawTools.([]any)
		if !ok {
			incompatible = append(incompatible, "tools")
		} else {
			for i, raw := range tools {
				tool, mapOK := raw.(map[string]any)
				path := "tools." + strconv.Itoa(i)
				allowedKeys(&incompatible, path, tool, "name", "description", "input_schema")
				name, nameOK := tool["name"].(string)
				_, schemaOK := tool["input_schema"].(map[string]any)
				_, descriptionOK := tool["description"].(string)
				if !mapOK || !nameOK || name == "" || !schemaOK || (tool["description"] != nil && !descriptionOK) {
					incompatible = append(incompatible, path)
				}
			}
			converted := AnthropicToolsToOpenAI(tools)
			if len(converted) != len(tools) {
				incompatible = append(incompatible, "tools")
			} else if converted != nil {
				kw["tools"] = converted
			}
		}
	}
	if rawChoice := payload["tool_choice"]; rawChoice != nil {
		choice, ok := rawChoice.(map[string]any)
		allowedKeys(&incompatible, "tool_choice", choice, "type", "name")
		converted := AnthropicToolChoiceToOpenAI(choice)
		typeName, typeOK := choice["type"].(string)
		if !ok || !typeOK || converted == nil || (typeName != "tool" && choice["name"] != nil) {
			incompatible = append(incompatible, "tool_choice")
		} else {
			kw["tool_choice"] = converted
		}
	}
	sort.Strings(incompatible)
	unique := incompatible[:0]
	for _, path := range incompatible {
		if len(unique) == 0 || unique[len(unique)-1] != path {
			unique = append(unique, path)
		}
	}
	return messages, kw, unique
}

func systemToText(system any) string {
	switch s := system.(type) {
	case nil:
		return ""
	case string:
		return s
	case []any:
		var parts []string
		for _, block := range s {
			if bm, ok := asMap(block); ok && bm["type"] == "text" {
				if t, ok := asStr(bm["text"]); ok {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n\n")
	}
	return ""
}

func blocksToOpenAIAssistantMessage(blocks []any) map[string]any {
	var textChunks []string
	var toolCalls []any
	for _, block := range blocks {
		bm, ok := asMap(block)
		if !ok {
			continue
		}
		switch bm["type"] {
		case "text":
			if t, ok := asStr(bm["text"]); ok {
				textChunks = append(textChunks, t)
			}
		case "tool_use":
			id, _ := asStr(bm["id"])
			if id == "" {
				id = "call_" + randHex(12)
			}
			name, _ := asStr(bm["name"])
			var input any = bm["input"]
			if input == nil {
				input = map[string]any{}
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   id,
				"type": "function",
				"function": map[string]any{
					"name":      name,
					"arguments": jsonString(input),
				},
			})
		}
	}
	msg := map[string]any{"role": "assistant"}
	if len(textChunks) > 0 {
		msg["content"] = strings.Join(textChunks, "")
	} else {
		msg["content"] = nil
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	return msg
}

func toolResultContentToText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, block := range c {
			if bm, ok := asMap(block); ok && bm["type"] == "text" {
				if t, ok := asStr(bm["text"]); ok {
					parts = append(parts, t)
				}
			} else if s, ok := asStr(block); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "\n")
	case nil:
		return ""
	}
	return jsonString(content)
}

func userBlocksToOpenAIMessages(blocks []any) []map[string]any {
	var out []map[string]any
	var textChunks []string
	for _, block := range blocks {
		bm, ok := asMap(block)
		if !ok {
			continue
		}
		switch bm["type"] {
		case "text":
			if t, ok := asStr(bm["text"]); ok {
				textChunks = append(textChunks, t)
			}
		case "tool_result":
			if len(textChunks) > 0 {
				out = append(out, map[string]any{"role": "user", "content": strings.Join(textChunks, "")})
				textChunks = nil
			}
			tid, _ := asStr(bm["tool_use_id"])
			out = append(out, map[string]any{
				"role":         "tool",
				"tool_call_id": tid,
				"content":      toolResultContentToText(bm["content"]),
			})
		}
	}
	if len(textChunks) > 0 {
		out = append(out, map[string]any{"role": "user", "content": strings.Join(textChunks, "")})
	}
	return out
}

// AnthropicMessagesToOpenAI converts Anthropic messages + system into OpenAI messages.
func AnthropicMessagesToOpenAI(messages []any, system any) []map[string]any {
	out := []map[string]any{}
	if st := systemToText(system); st != "" {
		out = append(out, map[string]any{"role": "system", "content": st})
	}
	for _, msg := range messages {
		mm, ok := asMap(msg)
		if !ok {
			continue
		}
		role, _ := asStr(mm["role"])
		content := mm["content"]
		switch role {
		case "assistant":
			if s, ok := asStr(content); ok {
				out = append(out, map[string]any{"role": "assistant", "content": s})
			} else if l, ok := asList(content); ok {
				out = append(out, blocksToOpenAIAssistantMessage(l))
			}
		case "user":
			if s, ok := asStr(content); ok {
				out = append(out, map[string]any{"role": "user", "content": s})
			} else if l, ok := asList(content); ok {
				out = append(out, userBlocksToOpenAIMessages(l)...)
			}
		case "system":
			if s, ok := asStr(content); ok {
				out = append(out, map[string]any{"role": "system", "content": s})
			}
		}
	}
	return out
}

// AnthropicToolsToOpenAI converts Anthropic tools to OpenAI function tools.
func AnthropicToolsToOpenAI(tools []any) []any {
	if len(tools) == 0 {
		return nil
	}
	var out []any
	for _, tool := range tools {
		tm, ok := asMap(tool)
		if !ok {
			continue
		}
		name, _ := asStr(tm["name"])
		if name == "" {
			continue
		}
		desc, _ := tm["description"].(string)
		var params any = tm["input_schema"]
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": desc,
				"parameters":  params,
			},
		})
	}
	return out
}

// AnthropicToolChoiceToOpenAI converts Anthropic tool_choice to OpenAI's.
func AnthropicToolChoiceToOpenAI(choice map[string]any) any {
	if choice == nil {
		return nil
	}
	switch choice["type"] {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		if name, ok := asStr(choice["name"]); ok && name != "" {
			return map[string]any{"type": "function", "function": map[string]any{"name": name}}
		}
	}
	return nil
}

// ---- Response: OpenAI -> Anthropic -------------------------------------- //

var finishToStop = map[string]string{
	"stop": "end_turn", "length": "max_tokens", "tool_calls": "tool_use",
	"function_call": "tool_use", "content_filter": "end_turn",
}

// OpenAIFinishReasonToAnthropic maps an OpenAI finish_reason to Anthropic stop_reason.
func OpenAIFinishReasonToAnthropic(reason any) string {
	s, ok := asStr(reason)
	if !ok {
		return "end_turn"
	}
	if v, ok := finishToStop[s]; ok {
		return v
	}
	return "end_turn"
}

func usageFromOpenAI(usage any) map[string]any {
	um, ok := asMap(usage)
	if !ok {
		return map[string]any{"input_tokens": 0, "output_tokens": 0}
	}
	return map[string]any{
		"input_tokens":  toInt(um["prompt_tokens"]),
		"output_tokens": toInt(um["completion_tokens"]),
	}
}

// OpenAIResponseToAnthropic converts a non-streaming OpenAI completion to an
// Anthropic Message.
func OpenAIResponseToAnthropic(response map[string]any, model string) map[string]any {
	var choice map[string]any
	if choices, ok := asList(response["choices"]); ok && len(choices) > 0 {
		choice, _ = asMap(choices[0])
	}
	message, _ := asMap(choice["message"])
	if message == nil {
		message = map[string]any{}
	}
	contentBlocks := []any{}
	if text, ok := asStr(message["content"]); ok && text != "" {
		contentBlocks = append(contentBlocks, map[string]any{"type": "text", "text": text})
	}
	if calls, ok := asList(message["tool_calls"]); ok {
		for _, call := range calls {
			cm, ok := asMap(call)
			if !ok {
				continue
			}
			fn, ok := asMap(cm["function"])
			if !ok {
				continue
			}
			var toolInput any
			args, _ := asStr(fn["arguments"])
			if args == "" {
				args = "{}"
			}
			if json.Unmarshal([]byte(args), &toolInput) != nil {
				toolInput = map[string]any{"_raw_arguments": fn["arguments"]}
			}
			id, _ := asStr(cm["id"])
			if id == "" {
				id = anthropicToolUseID()
			}
			name, _ := asStr(fn["name"])
			contentBlocks = append(contentBlocks, map[string]any{
				"type": "tool_use", "id": id, "name": name, "input": toolInput,
			})
		}
	}
	id, _ := asStr(response["id"])
	if id == "" {
		id = anthropicMessageID()
	}
	var finish any
	if choice != nil {
		finish = choice["finish_reason"]
	}
	return map[string]any{
		"id": id, "type": "message", "role": "assistant", "model": model,
		"content":       contentBlocks,
		"stop_reason":   OpenAIFinishReasonToAnthropic(finish),
		"stop_sequence": nil,
		"usage":         usageFromOpenAI(response["usage"]),
	}
}
