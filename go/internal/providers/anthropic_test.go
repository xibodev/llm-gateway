package providers

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

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

func TestAnthropicListModelsDeclaresMessagesSurface(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-test","display_name":"Claude Test"}]}`))
	}))
	defer server.Close()
	models := (AnthropicNativeProvider{BaseURL: server.URL}).ListModels()
	if len(models) != 1 || len(models[0].SupportedSurfaces) != 1 || models[0].SupportedSurfaces[0] != "/v1/messages" {
		t.Fatalf("models=%+v", models)
	}
}
