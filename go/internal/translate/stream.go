package translate

import (
	"encoding/json"
	"strings"
)

// sse formats one Anthropic SSE event (two lines + blank).
func sse(event string, data map[string]any) string {
	b, _ := json.Marshal(data)
	return "event: " + event + "\ndata: " + string(b) + "\n\n"
}

// streamState threads mutable state through the OpenAI->Anthropic translator.
type streamState struct {
	messageID      string
	model          string
	started        bool
	textIndex      int
	textIndexSet   bool
	textOpen       bool
	toolBlocks     map[int]map[string]any
	toolIndexByOAI map[int]int
	toolOrder      []int
	nextBlockIndex int
	stopReason     string
	inputTokens    int
	outputTokens   int
}

func newStreamState(model string) *streamState {
	return &streamState{
		messageID:      anthropicMessageID(),
		model:          model,
		stopReason:     "end_turn",
		toolBlocks:     map[int]map[string]any{},
		toolIndexByOAI: map[int]int{},
	}
}

func (s *streamState) reserveBlockIndex() int {
	i := s.nextBlockIndex
	s.nextBlockIndex++
	return i
}

func emitMessageStart(s *streamState) string {
	return sse("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": s.messageID, "type": "message", "role": "assistant",
			"content": []any{}, "model": s.model,
			"stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": s.inputTokens, "output_tokens": 0},
		},
	})
}

func openTextBlock(s *streamState) []string {
	if s.textOpen {
		return nil
	}
	if !s.textIndexSet {
		s.textIndex = s.reserveBlockIndex()
		s.textIndexSet = true
	}
	s.textOpen = true
	return []string{sse("content_block_start", map[string]any{
		"type": "content_block_start", "index": s.textIndex,
		"content_block": map[string]any{"type": "text", "text": ""},
	})}
}

func closeTextBlock(s *streamState) []string {
	if !s.textOpen || !s.textIndexSet {
		return nil
	}
	s.textOpen = false
	return []string{sse("content_block_stop", map[string]any{
		"type": "content_block_stop", "index": s.textIndex,
	})}
}

func openToolBlock(s *streamState, oaiIndex int, toolID, name string) []string {
	if _, ok := s.toolIndexByOAI[oaiIndex]; ok {
		return nil
	}
	blockIndex := s.reserveBlockIndex()
	s.toolIndexByOAI[oaiIndex] = blockIndex
	s.toolBlocks[blockIndex] = map[string]any{"id": toolID, "name": name, "args": ""}
	s.toolOrder = append(s.toolOrder, blockIndex)
	return []string{sse("content_block_start", map[string]any{
		"type": "content_block_start", "index": blockIndex,
		"content_block": map[string]any{"type": "tool_use", "id": toolID, "name": name, "input": map[string]any{}},
	})}
}

func emitToolPartial(s *streamState, oaiIndex int, partial string) []string {
	blockIndex, ok := s.toolIndexByOAI[oaiIndex]
	if !ok {
		return nil
	}
	s.toolBlocks[blockIndex]["args"] = s.toolBlocks[blockIndex]["args"].(string) + partial
	return []string{sse("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": blockIndex,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": partial},
	})}
}

func closeAllToolBlocks(s *streamState) []string {
	var out []string
	for _, blockIndex := range s.toolOrder {
		out = append(out, sse("content_block_stop", map[string]any{
			"type": "content_block_stop", "index": blockIndex,
		}))
	}
	return out
}

// OpenAIStreamToAnthropicSSE translates OpenAI SSE chunk payloads (JSON strings,
// data: prefix already stripped) into Anthropic SSE event blocks, invoking emit
// for each output event.
func OpenAIStreamToAnthropicSSE(chunks func() (string, bool), model string, emit func(string)) {
	s := newStreamState(model)
	sawAnyDelta := false

	for {
		raw, ok := chunks()
		if !ok {
			break
		}
		if raw == "" {
			continue
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(raw), &chunk) != nil {
			continue
		}
		if um, ok := asMap(chunk["usage"]); ok {
			if v := toInt(um["prompt_tokens"]); v != 0 {
				s.inputTokens = v
			}
			if v := toInt(um["completion_tokens"]); v != 0 {
				s.outputTokens = v
			}
		}
		choices, _ := asList(chunk["choices"])
		for _, ch := range choices {
			choice, ok := asMap(ch)
			if !ok {
				continue
			}
			delta, _ := asMap(choice["delta"])
			if delta == nil {
				delta = map[string]any{}
			}
			finishReason := choice["finish_reason"]

			if !s.started {
				s.started = true
				emit(emitMessageStart(s))
				emit(sse("ping", map[string]any{"type": "ping"}))
			}

			if textPiece, ok := asStr(delta["content"]); ok && textPiece != "" {
				sawAnyDelta = true
				for _, e := range openTextBlock(s) {
					emit(e)
				}
				emit(sse("content_block_delta", map[string]any{
					"type": "content_block_delta", "index": s.textIndex,
					"delta": map[string]any{"type": "text_delta", "text": textPiece},
				}))
				inc := len(textPiece) / 4
				if inc < 1 {
					inc = 1
				}
				s.outputTokens += inc
			}

			if toolCalls, ok := asList(delta["tool_calls"]); ok {
				for _, tc := range toolCalls {
					tcm, ok := asMap(tc)
					if !ok {
						continue
					}
					oaiIndex := toInt(tcm["index"])
					fn, _ := asMap(tcm["function"])
					if fn == nil {
						fn = map[string]any{}
					}
					if _, exists := s.toolIndexByOAI[oaiIndex]; !exists {
						toolID, _ := asStr(tcm["id"])
						if toolID == "" {
							toolID = anthropicToolUseID()
						}
						name, _ := asStr(fn["name"])
						for _, e := range openToolBlock(s, oaiIndex, toolID, name) {
							emit(e)
						}
					}
					if argsPiece, ok := asStr(fn["arguments"]); ok && argsPiece != "" {
						sawAnyDelta = true
						for _, e := range emitToolPartial(s, oaiIndex, argsPiece) {
							emit(e)
						}
					}
				}
			}

			if fr, ok := asStr(finishReason); ok && fr != "" {
				s.stopReason = OpenAIFinishReasonToAnthropic(finishReason)
			}
		}
	}

	if !s.started {
		emit(emitMessageStart(s))
	}
	if s.textOpen {
		for _, e := range closeTextBlock(s) {
			emit(e)
		}
	}
	for _, e := range closeAllToolBlocks(s) {
		emit(e)
	}
	if len(s.toolBlocks) > 0 && s.stopReason == "end_turn" && !sawAnyDelta {
		s.stopReason = "tool_use"
	}
	emit(sse("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": s.stopReason, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": s.outputTokens},
	}))
	emit(sse("message_stop", map[string]any{"type": "message_stop"}))
}

// ---- REVERSE: OpenAI -> Anthropic (native anthropic backend) ------------ //

var stopReasonToFinish = map[string]string{
	"end_turn": "stop", "max_tokens": "length", "tool_use": "tool_calls", "stop_sequence": "stop",
}

// AnthropicStopReasonToOpenAI maps an Anthropic stop_reason to an OpenAI finish_reason.
func AnthropicStopReasonToOpenAI(reason any) string {
	s, ok := asStr(reason)
	if !ok {
		return ""
	}
	if v, ok := stopReasonToFinish[s]; ok {
		return v
	}
	return "stop"
}

func openaiContentToText(content any) string {
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

// OpenAIToolsToAnthropic converts OpenAI tools to Anthropic tool shape.
func OpenAIToolsToAnthropic(tools []any) []any {
	if len(tools) == 0 {
		return nil
	}
	var out []any
	for _, tool := range tools {
		tm, ok := asMap(tool)
		if !ok {
			continue
		}
		var fn map[string]any
		if tm["type"] == "function" {
			fn, _ = asMap(tm["function"])
		} else {
			fn = tm
		}
		if fn == nil {
			continue
		}
		name, _ := asStr(fn["name"])
		if name == "" {
			continue
		}
		desc, _ := fn["description"].(string)
		var schema any = fn["parameters"]
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{"name": name, "description": desc, "input_schema": schema})
	}
	return out
}

// OpenAIMessagesToAnthropic splits an OpenAI message list into Anthropic (system, messages).
func OpenAIMessagesToAnthropic(messages []map[string]any) (string, []map[string]any) {
	var systemParts []string
	out := []map[string]any{}
	for _, msg := range messages {
		role, _ := asStr(msg["role"])
		content := msg["content"]
		switch role {
		case "system", "developer":
			if text := openaiContentToText(content); text != "" {
				systemParts = append(systemParts, text)
			}
		case "assistant":
			blocks := []any{}
			var text string
			if s, ok := content.(string); ok {
				text = s
			} else {
				text = openaiContentToText(content)
			}
			if text != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": text})
			}
			if calls, ok := asList(msg["tool_calls"]); ok {
				for _, call := range calls {
					cm, ok := asMap(call)
					if !ok {
						continue
					}
					fn, _ := asMap(cm["function"])
					if fn == nil {
						fn = map[string]any{}
					}
					var toolInput any
					args, _ := asStr(fn["arguments"])
					if args == "" {
						args = "{}"
					}
					if json.Unmarshal([]byte(args), &toolInput) != nil {
						toolInput = map[string]any{}
					}
					if _, ok := toolInput.(map[string]any); !ok {
						toolInput = map[string]any{}
					}
					id, _ := asStr(cm["id"])
					if id == "" {
						id = anthropicToolUseID()
					}
					name, _ := asStr(fn["name"])
					blocks = append(blocks, map[string]any{
						"type": "tool_use", "id": id, "name": name, "input": toolInput,
					})
				}
			}
			if len(blocks) == 0 {
				blocks = []any{map[string]any{"type": "text", "text": ""}}
			}
			out = append(out, map[string]any{"role": "assistant", "content": blocks})
		case "tool":
			tid, _ := asStr(msg["tool_call_id"])
			block := map[string]any{
				"type": "tool_result", "tool_use_id": tid, "content": openaiContentToText(content),
			}
			if len(out) > 0 && out[len(out)-1]["role"] == "user" {
				if lst, ok := out[len(out)-1]["content"].([]any); ok {
					out[len(out)-1]["content"] = append(lst, block)
					continue
				}
			}
			out = append(out, map[string]any{"role": "user", "content": []any{block}})
		default:
			out = append(out, map[string]any{"role": "user", "content": openaiContentToText(content)})
		}
	}
	system := strings.Join(systemParts, "\n\n")
	return system, out
}

// AnthropicResponseToOpenAI converts a non-streaming Anthropic Message to an OpenAI completion.
func AnthropicResponseToOpenAI(response map[string]any, model string) map[string]any {
	var textParts []string
	var toolCalls []any
	if blocks, ok := asList(response["content"]); ok {
		for _, block := range blocks {
			bm, ok := asMap(block)
			if !ok {
				continue
			}
			switch bm["type"] {
			case "text":
				if t, ok := asStr(bm["text"]); ok {
					textParts = append(textParts, t)
				}
			case "tool_use":
				id, _ := asStr(bm["id"])
				name, _ := asStr(bm["name"])
				var input any = bm["input"]
				if input == nil {
					input = map[string]any{}
				}
				toolCalls = append(toolCalls, map[string]any{
					"id": id, "type": "function",
					"function": map[string]any{"name": name, "arguments": jsonString(input)},
				})
			}
		}
	}
	message := map[string]any{"role": "assistant"}
	if joined := strings.Join(textParts, ""); joined != "" {
		message["content"] = joined
	} else {
		message["content"] = nil
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	usage, _ := asMap(response["usage"])
	prompt := toInt(usage["input_tokens"])
	completion := toInt(usage["output_tokens"])
	id, _ := asStr(response["id"])
	if id == "" {
		id = chatCmplID()
	}
	finish := AnthropicStopReasonToOpenAI(response["stop_reason"])
	if finish == "" {
		finish = "stop"
	}
	return map[string]any{
		"id": id, "object": "chat.completion", "model": model,
		"choices": []any{map[string]any{
			"index": 0, "message": message, "finish_reason": finish,
		}},
		"usage": map[string]any{
			"prompt_tokens": prompt, "completion_tokens": completion,
			"total_tokens": prompt + completion,
		},
	}
}

// AnthropicSSEToOpenAIChunks translates Anthropic SSE data lines into OpenAI
// chat.completion.chunk JSON strings, invoking emit for each.
func AnthropicSSEToOpenAIChunks(lines func() (string, bool), model string, emit func(string)) {
	chatID := chatCmplID()
	toolOrdinalByBlock := map[int]int{}
	nextToolOrdinal := 0
	var stopReason any
	started := false

	chunk := func(delta map[string]any, finishReason any) string {
		b, _ := json.Marshal(map[string]any{
			"id": chatID, "object": "chat.completion.chunk", "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finishReason}},
		})
		return string(b)
	}

	for {
		raw, ok := lines()
		if !ok {
			break
		}
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(line[len("data:"):])
		if payload == "" {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(payload), &obj) != nil {
			continue
		}
		switch obj["type"] {
		case "message_start":
			if !started {
				started = true
				emit(chunk(map[string]any{"role": "assistant", "content": ""}, nil))
			}
		case "content_block_start":
			block, _ := asMap(obj["content_block"])
			if block != nil && block["type"] == "tool_use" {
				ordinal := nextToolOrdinal
				nextToolOrdinal++
				toolOrdinalByBlock[toInt(obj["index"])] = ordinal
				id, _ := asStr(block["id"])
				name, _ := asStr(block["name"])
				emit(chunk(map[string]any{"tool_calls": []any{map[string]any{
					"index": ordinal, "id": id, "type": "function",
					"function": map[string]any{"name": name, "arguments": ""},
				}}}, nil))
			}
		case "content_block_delta":
			delta, _ := asMap(obj["delta"])
			switch delta["type"] {
			case "text_delta":
				if text, ok := asStr(delta["text"]); ok && text != "" {
					if !started {
						started = true
						emit(chunk(map[string]any{"role": "assistant", "content": ""}, nil))
					}
					emit(chunk(map[string]any{"content": text}, nil))
				}
			case "input_json_delta":
				if partial, ok := asStr(delta["partial_json"]); ok && partial != "" {
					ordinal := toolOrdinalByBlock[toInt(obj["index"])]
					emit(chunk(map[string]any{"tool_calls": []any{map[string]any{
						"index": ordinal, "function": map[string]any{"arguments": partial},
					}}}, nil))
				}
			}
		case "message_delta":
			if inner, ok := asMap(obj["delta"]); ok {
				if sr := inner["stop_reason"]; sr != nil {
					stopReason = sr
				}
			}
		}
	}

	if !started {
		emit(chunk(map[string]any{"role": "assistant", "content": ""}, nil))
	}
	finish := AnthropicStopReasonToOpenAI(stopReason)
	if finish == "" {
		finish = "stop"
	}
	emit(chunk(map[string]any{}, finish))
}
