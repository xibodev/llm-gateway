package translate

import (
	"encoding/json"
	"testing"
)

func TestChatToResponses_TextAndInstructions(t *testing.T) {
	msgs := []map[string]any{
		{"role": "system", "content": "be terse"},
		{"role": "user", "content": "hello"},
	}
	kw := map[string]any{"max_tokens": 128, "reasoning_effort": "high", "temperature": 0.5}
	p := ChatToResponses("gpt-5.5", msgs, kw, false)

	if p["instructions"] != "be terse" {
		t.Fatalf("instructions = %v", p["instructions"])
	}
	if p["max_output_tokens"] != 128 {
		t.Fatalf("max_output_tokens = %v", p["max_output_tokens"])
	}
	if r, ok := p["reasoning"].(map[string]any); !ok || r["effort"] != "high" {
		t.Fatalf("reasoning = %v", p["reasoning"])
	}
	input, ok := p["input"].([]any)
	if !ok || len(input) != 1 {
		t.Fatalf("input = %v", p["input"])
	}
	first := input[0].(map[string]any)
	if first["role"] != "user" || first["content"] != "hello" {
		t.Fatalf("input[0] = %v", first)
	}
	if _, ok := p["max_tokens"]; ok {
		t.Fatal("chat max_tokens must not leak into responses payload")
	}
}

func TestChatToResponses_ToolsAndHistory(t *testing.T) {
	msgs := []map[string]any{
		{"role": "user", "content": "weather?"},
		{"role": "assistant", "content": "", "tool_calls": []any{
			map[string]any{"id": "call_1", "type": "function",
				"function": map[string]any{"name": "get_weather", "arguments": `{"city":"x"}`}},
		}},
		{"role": "tool", "tool_call_id": "call_1", "content": "sunny"},
	}
	kw := map[string]any{"tools": []any{
		map[string]any{"type": "function", "function": map[string]any{
			"name": "get_weather", "description": "w", "parameters": map[string]any{"type": "object"}}},
	}, "tool_choice": map[string]any{"type": "function", "function": map[string]any{"name": "get_weather"}}}

	p := ChatToResponses("gpt-5.5", msgs, kw, false)

	tools := p["tools"].([]any)
	tool0 := tools[0].(map[string]any)
	if tool0["type"] != "function" || tool0["name"] != "get_weather" {
		t.Fatalf("translated tool = %v", tool0)
	}
	if _, ok := tool0["function"]; ok {
		t.Fatal("responses tool must be flat (no nested function)")
	}
	tc := p["tool_choice"].(map[string]any)
	if tc["type"] != "function" || tc["name"] != "get_weather" {
		t.Fatalf("tool_choice = %v", tc)
	}

	input := p["input"].([]any)
	if len(input) != 3 {
		t.Fatalf("want 3 input items, got %d: %v", len(input), input)
	}
	fc := input[1].(map[string]any)
	if fc["type"] != "function_call" || fc["call_id"] != "call_1" || fc["name"] != "get_weather" {
		t.Fatalf("function_call item = %v", fc)
	}
	fco := input[2].(map[string]any)
	if fco["type"] != "function_call_output" || fco["call_id"] != "call_1" || fco["output"] != "sunny" {
		t.Fatalf("function_call_output item = %v", fco)
	}
}

func TestResponsesToChat_Text(t *testing.T) {
	resp := map[string]any{
		"id":     "resp_1",
		"object": "response",
		"output": []any{
			map[string]any{"type": "reasoning", "content": []any{}},
			map[string]any{"type": "message", "role": "assistant", "content": []any{
				map[string]any{"type": "output_text", "text": "Hi there!"},
			}},
		},
		"usage": map[string]any{"input_tokens": float64(10), "output_tokens": float64(3), "total_tokens": float64(13)},
	}
	chat := ResponsesToChat("gpt-5.5", resp)
	if chat["object"] != "chat.completion" {
		t.Fatalf("object = %v", chat["object"])
	}
	choice := chat["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)
	if msg["content"] != "Hi there!" {
		t.Fatalf("content = %v", msg["content"])
	}
	if choice["finish_reason"] != "stop" {
		t.Fatalf("finish = %v", choice["finish_reason"])
	}
	u := chat["usage"].(map[string]any)
	if u["prompt_tokens"] != 10 || u["completion_tokens"] != 3 || u["total_tokens"] != 13 {
		t.Fatalf("usage = %v", u)
	}
}

func TestResponsesToChat_ToolCall(t *testing.T) {
	resp := map[string]any{
		"id": "resp_2",
		"output": []any{
			map[string]any{"type": "function_call", "call_id": "call_9",
				"name": "do_thing", "arguments": `{"a":1}`},
		},
	}
	chat := ResponsesToChat("gpt-5.5", resp)
	choice := chat["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish = %v", choice["finish_reason"])
	}
	msg := choice["message"].(map[string]any)
	tc0 := msg["tool_calls"].([]any)[0].(map[string]any)
	if tc0["id"] != "call_9" || tc0["type"] != "function" {
		t.Fatalf("tool_call = %v", tc0)
	}
	fn := tc0["function"].(map[string]any)
	if fn["name"] != "do_thing" || fn["arguments"] != `{"a":1}` {
		t.Fatalf("function = %v", fn)
	}
}

func TestResponsesToChat_IncompleteMaxTokens(t *testing.T) {
	resp := map[string]any{
		"status":             "incomplete",
		"incomplete_details": map[string]any{"reason": "max_output_tokens"},
		"output": []any{
			map[string]any{"type": "message", "role": "assistant", "content": []any{
				map[string]any{"type": "output_text", "text": "partial"},
			}},
		},
	}
	chat := ResponsesToChat("gpt-5.5", resp)
	choice := chat["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "length" {
		t.Fatalf("finish = %v", choice["finish_reason"])
	}
}

func TestResponsesToChatChunks(t *testing.T) {
	resp := map[string]any{
		"id": "resp_3",
		"output": []any{
			map[string]any{"type": "message", "role": "assistant", "content": []any{
				map[string]any{"type": "output_text", "text": "stream me"},
			}},
		},
		"usage": map[string]any{"input_tokens": float64(2), "output_tokens": float64(2), "total_tokens": float64(4)},
	}
	chunks := ResponsesToChatChunks("gpt-5.5", resp)
	if len(chunks) != 3 {
		t.Fatalf("want 3 chunks, got %d", len(chunks))
	}
	var c0 map[string]any
	json.Unmarshal([]byte(chunks[0]), &c0)
	if c0["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["role"] != "assistant" {
		t.Fatalf("chunk0 = %v", c0)
	}
	var c1 map[string]any
	json.Unmarshal([]byte(chunks[1]), &c1)
	if c1["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["content"] != "stream me" {
		t.Fatalf("chunk1 = %v", c1)
	}
	var c2 map[string]any
	json.Unmarshal([]byte(chunks[2]), &c2)
	last := c2["choices"].([]any)[0].(map[string]any)
	if last["finish_reason"] != "stop" {
		t.Fatalf("chunk2 finish = %v", last["finish_reason"])
	}
	if _, ok := c2["usage"].(map[string]any); !ok {
		t.Fatalf("chunk2 missing usage: %v", c2)
	}
}

func TestPreferredEndpoint(t *testing.T) {
	cases := []struct {
		eps  []string
		want string
	}{
		{nil, "chat"},
		{[]string{"/chat/completions"}, "chat"},
		{[]string{"/responses", "/chat/completions", "ws:/responses"}, "chat"},
		{[]string{"/responses", "ws:/responses"}, "responses"},
	}
	for _, c := range cases {
		if got := PreferredEndpoint(c.eps); got != c.want {
			t.Errorf("PreferredEndpoint(%v) = %q, want %q", c.eps, got, c.want)
		}
	}
}
