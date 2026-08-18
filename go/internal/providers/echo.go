package providers

import (
	"encoding/json"
	"fmt"
)

// EchoProvider is a built-in stub used by the dev harness + tests. It returns a
// deterministic echo:<prompt> reply so the gateway boots without any keys.
type EchoProvider struct{}

func (EchoProvider) IsStub() bool { return true }

func lastContent(messages []Message) string {
	if len(messages) == 0 {
		return ""
	}
	if c, ok := messages[len(messages)-1]["content"].(string); ok {
		return c
	}
	return ""
}

func (EchoProvider) Complete(model string, messages []Message, kw Kwargs) (map[string]any, error) {
	content := "echo:" + lastContent(messages)
	return map[string]any{
		"id": "chatcmpl-echo", "object": "chat.completion", "model": model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": content},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
	}, nil
}

func (EchoProvider) Stream(model string, messages []Message, kw Kwargs) (StreamIter, error) {
	text := "echo:" + lastContent(messages)
	half := len(text) / 2
	if half < 1 {
		half = 1
	}
	if half > len(text) {
		half = len(text)
	}
	mk := func(delta map[string]any, finish any) string {
		b, _ := json.Marshal(map[string]any{
			"id": "chatcmpl-echo", "object": "chat.completion.chunk", "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
		})
		return string(b)
	}
	chunks := []string{
		mk(map[string]any{"content": text[:half]}, nil),
		mk(map[string]any{"content": text[half:]}, nil),
		mk(map[string]any{}, "stop"),
	}
	return &sliceIter{chunks: chunks}, nil
}

func (EchoProvider) ListModels() []ModelInfo {
	return []ModelInfo{
		{ID: "echo-small", Vendor: "echo", Label: "Echo (small)"},
		{ID: "echo-default", Vendor: "echo", Label: "Echo (default)"},
		{ID: "echo-strong", Vendor: "echo", Label: "Echo (strong)"},
		{ID: "echo-deep", Vendor: "echo", Label: "Echo (deep)"},
	}
}

var _ = fmt.Sprintf
