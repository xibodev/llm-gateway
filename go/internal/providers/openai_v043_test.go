package providers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"llmgw/internal/config"
)

type v043RefreshAuth struct{ base string }

func (auth v043RefreshAuth) Prepare() (string, http.Header, error) {
	return auth.base, http.Header{
		"Authorization": {"Bearer fixture"},
		"Content-Type":  {"application/json"},
	}, nil
}
func (v043RefreshAuth) CanRefresh() bool { return true }
func (v043RefreshAuth) Refresh() error   { return nil }

func TestV043OpenAIRetryPreservesVisionHeaders(t *testing.T) {
	for name, invoke := range map[string]func(OpenAIProvider) error{
		"chat": func(provider OpenAIProvider) error {
			_, err := provider.Complete("vision", []Message{{
				"role": "user", "content": []any{map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": "data:image/png;base64,AA=="},
				}},
			}}, Kwargs{})
			return err
		},
		"responses": func(provider OpenAIProvider) error {
			_, _, err := provider.callResponsesPayloadWithObservation(map[string]any{
				"model": "vision",
				"input": []any{map[string]any{
					"type": "message", "role": "user",
					"content": []any{map[string]any{
						"type": "input_image", "image_url": "data:image/png;base64,AA==",
					}},
				}},
			})
			return err
		},
		"responses stream": func(provider OpenAIProvider) error {
			stream, _, err := provider.streamResponsesPayload(map[string]any{
				"model": "vision", "stream": true,
				"input": []any{map[string]any{
					"type": "message", "role": "user",
					"content": []any{map[string]any{
						"type": "input_image", "image_url": "data:image/png;base64,AA==",
					}},
				}},
			})
			if err != nil {
				return err
			}
			_, _ = stream.Next()
			return stream.Close()
		},
	} {
		t.Run(name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Header.Get("Copilot-Vision-Request") != "true" {
					t.Fatalf("request %d missing vision header", requests)
				}
				if requests == 1 {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/responses" {
					if r.Header.Get("Accept") == "text/event-stream" {
						_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"completed\",\"output\":[]}}\n\n"))
						return
					}
					_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","status":"completed","output":[]}`))
					return
				}
				_, _ = w.Write([]byte(`{"id":"chat_1","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
			}))
			defer server.Close()
			if err := invoke(OpenAIProvider{
				auth: v043RefreshAuth{base: server.URL}, Timeout: 2,
			}); err != nil {
				t.Fatal(err)
			}
			if requests != 2 {
				t.Fatalf("requests=%d", requests)
			}
		})
	}
}

func TestV043OfficialOpenAIUsesNativeResponsesWithoutCatalogMetadata(t *testing.T) {
	oldProviders := config.Get().Providers
	config.Update(func(settings *config.Settings) {
		settings.Providers = map[string]*config.ProviderConfig{
			"openai": {Type: "openai_compatible", RegistryID: "openai"},
		}
	})
	t.Cleanup(func() {
		config.Update(func(settings *config.Settings) {
			settings.Providers = oldProviders
		})
	})
	if !(OpenAIProvider{providerID: "openai"}).supportsNativeResponses("future-model") {
		t.Fatal("official OpenAI should attempt its native Responses endpoint")
	}
}

func TestV043ChatPayloadPreservesFallbackControls(t *testing.T) {
	payload := buildOpenAIPayload("model", []Message{{"role": "user", "content": "hi"}}, true, Kwargs{
		"metadata":            map[string]any{"client": "fixture"},
		"parallel_tool_calls": false,
		"reasoning_effort":    "high",
		"thinking":            map[string]any{"type": "disabled"},
	})
	if value, ok := payload["parallel_tool_calls"].(bool); !ok || value {
		t.Fatalf("parallel_tool_calls=%#v", payload["parallel_tool_calls"])
	}
	if payload["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort=%#v", payload["reasoning_effort"])
	}
	thinking, ok := payload["thinking"].(map[string]any)
	if !ok || thinking["type"] != "disabled" {
		t.Fatalf("thinking=%#v", payload["thinking"])
	}
	metadata, ok := payload["metadata"].(map[string]any)
	if !ok || metadata["client"] != "fixture" {
		t.Fatalf("metadata=%#v", payload["metadata"])
	}
}
