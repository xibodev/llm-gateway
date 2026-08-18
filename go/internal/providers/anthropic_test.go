package providers

import "testing"

func TestAnthropicNativePayloadPreservesThinkingAndOutputConfig(t *testing.T) {
	p := AnthropicNativeProvider{}
	payload := p.payload(
		"claude-opus-4.8",
		[]Message{{"role": "user", "content": "hi"}},
		false,
		Kwargs{
			"thinking":      map[string]any{"type": "adaptive"},
			"output_config": map[string]any{"effort": "xhigh"},
		},
	)

	thinking, ok := payload["thinking"].(map[string]any)
	if !ok || thinking["type"] != "adaptive" {
		t.Fatalf("thinking = %#v, want adaptive map", payload["thinking"])
	}
	outputConfig, ok := payload["output_config"].(map[string]any)
	if !ok || outputConfig["effort"] != "xhigh" {
		t.Fatalf("output_config = %#v, want xhigh map", payload["output_config"])
	}
}
