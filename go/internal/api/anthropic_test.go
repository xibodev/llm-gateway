package api

import "testing"

func TestAnthropicKwargsMapsAdaptiveEffort(t *testing.T) {
	req := &anthropicRequest{
		Thinking:     map[string]any{"type": "adaptive"},
		OutputConfig: map[string]any{"effort": "xhigh"},
	}
	kw := anthropicKwargs(req)

	if got := kw["reasoning_effort"]; got != "xhigh" {
		t.Fatalf("reasoning_effort = %v, want xhigh", got)
	}
	if got, ok := kw["thinking"].(map[string]any); !ok || got["type"] != "adaptive" {
		t.Fatalf("thinking = %#v, want adaptive map", kw["thinking"])
	}
	if got, ok := kw["output_config"].(map[string]any); !ok || got["effort"] != "xhigh" {
		t.Fatalf("output_config = %#v, want xhigh map", kw["output_config"])
	}
}
