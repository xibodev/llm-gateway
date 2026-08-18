package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAnthropicMessagesToOpenAI_SystemAndText(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "user", "content": "hello"},
		map[string]any{"role": "assistant", "content": "hi there"},
	}
	out := AnthropicMessagesToOpenAI(msgs, "be nice")
	if len(out) != 3 {
		t.Fatalf("want 3 messages, got %d", len(out))
	}
	if out[0]["role"] != "system" || out[0]["content"] != "be nice" {
		t.Errorf("system message wrong: %v", out[0])
	}
	if out[1]["role"] != "user" || out[1]["content"] != "hello" {
		t.Errorf("user message wrong: %v", out[1])
	}
}

func TestAnthropicMessagesToOpenAI_ToolResult(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "run it"},
			map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "42"},
		}},
	}
	out := AnthropicMessagesToOpenAI(msgs, nil)
	if len(out) != 2 {
		t.Fatalf("want 2 (user text + tool), got %d: %v", len(out), out)
	}
	if out[0]["role"] != "user" || out[0]["content"] != "run it" {
		t.Errorf("text split wrong: %v", out[0])
	}
	if out[1]["role"] != "tool" || out[1]["tool_call_id"] != "toolu_1" || out[1]["content"] != "42" {
		t.Errorf("tool_result mapping wrong: %v", out[1])
	}
}

func TestAnthropicAssistantToolUse(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "text", "text": "calling"},
			map[string]any{"type": "tool_use", "id": "toolu_x", "name": "search", "input": map[string]any{"q": "go"}},
		}},
	}
	out := AnthropicMessagesToOpenAI(msgs, nil)
	if len(out) != 1 {
		t.Fatalf("want 1 assistant msg, got %d", len(out))
	}
	calls, ok := out[0]["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("want 1 tool_call, got %v", out[0]["tool_calls"])
	}
	fn := calls[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "search" {
		t.Errorf("tool name wrong: %v", fn)
	}
	if !strings.Contains(fn["arguments"].(string), `"q":"go"`) {
		t.Errorf("arguments not JSON-encoded: %v", fn["arguments"])
	}
}

func TestOpenAIResponseToAnthropic(t *testing.T) {
	resp := map[string]any{
		"id": "cmpl_1",
		"choices": []any{map[string]any{
			"index": 0, "finish_reason": "stop",
			"message": map[string]any{"role": "assistant", "content": "hello world"},
		}},
		"usage": map[string]any{"prompt_tokens": float64(5), "completion_tokens": float64(2)},
	}
	out := OpenAIResponseToAnthropic(resp, "claude-x")
	if out["type"] != "message" || out["role"] != "assistant" || out["model"] != "claude-x" {
		t.Errorf("envelope wrong: %v", out)
	}
	if out["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason want end_turn, got %v", out["stop_reason"])
	}
	blocks := out["content"].([]any)
	if len(blocks) != 1 || blocks[0].(map[string]any)["text"] != "hello world" {
		t.Errorf("content wrong: %v", blocks)
	}
	usage := out["usage"].(map[string]any)
	if usage["input_tokens"] != 5 || usage["output_tokens"] != 2 {
		t.Errorf("usage wrong: %v", usage)
	}
}

func TestOpenAIStreamToAnthropicSSE(t *testing.T) {
	chunks := []string{
		`{"choices":[{"index":0,"delta":{"content":"Hel"},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":null}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	}
	i := 0
	pull := func() (string, bool) {
		if i >= len(chunks) {
			return "", false
		}
		c := chunks[i]
		i++
		return c, true
	}
	var events []string
	OpenAIStreamToAnthropicSSE(pull, "m", func(e string) { events = append(events, e) })
	joined := strings.Join(events, "")
	for _, want := range []string{"message_start", "content_block_start", "text_delta", "content_block_stop", "message_delta", "message_stop"} {
		if !strings.Contains(joined, want) {
			t.Errorf("stream missing %q", want)
		}
	}
}

func TestRoundTripToolsToAnthropicAndBack(t *testing.T) {
	// OpenAI tools -> Anthropic -> ensure name preserved
	tools := []any{map[string]any{"type": "function", "function": map[string]any{
		"name": "lookup", "description": "d", "parameters": map[string]any{"type": "object"},
	}}}
	a := OpenAIToolsToAnthropic(tools)
	if len(a) != 1 || a[0].(map[string]any)["name"] != "lookup" {
		t.Fatalf("openai->anthropic tools wrong: %v", a)
	}
	b, _ := json.Marshal(a[0])
	if !strings.Contains(string(b), "input_schema") {
		t.Errorf("anthropic tool missing input_schema: %s", b)
	}
}
