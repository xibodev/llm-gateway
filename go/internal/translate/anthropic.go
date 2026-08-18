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
