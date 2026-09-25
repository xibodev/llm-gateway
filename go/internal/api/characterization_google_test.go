package api

import (
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/providers"
)

// googleCharacterizationUpstream is a synthetic AI Studio and Vertex AI. The
// model a request names picks the answer: text, an image, a refusal, or an
// empty reply that spent its budget on reasoning.
type googleCharacterizationUpstream struct {
	mu    sync.Mutex
	calls []recordedCall
}

func (u *googleCharacterizationUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	u.mu.Lock()
	u.calls = append(u.calls, recordedCall{method: r.Method, path: r.URL.Path, body: body})
	u.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	image := base64.StdEncoding.EncodeToString([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
	answer := func(status int, body string) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
	switch path := r.URL.Path; {
	case strings.HasSuffix(path, "/gemini-fixture:generateContent"):
		answer(http.StatusOK, `{"candidates":[{"content":{"role":"model","parts":[{"text":"Hello from Gemini"}]},"finishReason":"STOP"}],`+
			`"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":4,"totalTokenCount":7},"modelVersion":"gemini-fixture-001"}`)
	case strings.HasSuffix(path, "/gemini-image-fixture:generateContent"):
		answer(http.StatusOK, `{"candidates":[{"content":{"parts":[{"text":"here"},{"inlineData":{"mimeType":"image/png","data":"`+image+`"}}]}}],`+
			`"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":1290}}`)
	case strings.HasSuffix(path, "/gemini-billing-fixture:generateContent"):
		answer(http.StatusTooManyRequests, `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"Your prepayment credits are depleted."}}`)
	case strings.HasSuffix(path, "/gemini-denied-fixture:generateContent"):
		answer(http.StatusForbidden, `{"error":{"code":403,"status":"PERMISSION_DENIED","message":"denied"}}`)
	case strings.HasSuffix(path, "/gemini-thinking-fixture:generateContent"):
		answer(http.StatusOK, `{"candidates":[{"content":{"role":"model"},"finishReason":"MAX_TOKENS"}],`+
			`"usageMetadata":{"promptTokenCount":3,"totalTokenCount":35,"thoughtsTokenCount":32}}`)
	case strings.Contains(path, "/missing-"):
		answer(http.StatusNotFound, `{"error":{"code":404,"status":"NOT_FOUND","message":"Publisher model was not found."}}`)
	case strings.HasSuffix(path, ":embedContent"):
		answer(http.StatusOK, `{"embedding":{"values":[0.25,-0.5]},"usageMetadata":{"promptTokenCount":2}}`)
	case strings.HasSuffix(path, ":predict"):
		answer(http.StatusOK, `{"predictions":[{"embeddings":{"values":[0.75],"statistics":{"token_count":3}}}]}`)
	default:
		answer(http.StatusNotFound, `{"error":{"code":404,"status":"NOT_FOUND","message":"no such fixture"}}`)
	}
}

func (u *googleCharacterizationUpstream) take() []recordedCall {
	u.mu.Lock()
	defer u.mu.Unlock()
	calls := u.calls
	u.calls = nil
	return calls
}

// googleExchange is one client request of a Google golden.
type googleExchange struct {
	title, path string
	payload     map[string]any
}

// TestGoogleHTTPCharacterization pins what a client receives from AI Studio
// and Vertex AI instances, and every request the gateway sends Google for it,
// across chat and the surfaces translated to it, embeddings and images.
func TestGoogleHTTPCharacterization(t *testing.T) {
	fixture := setupCharacterization(t)
	upstream := &googleCharacterizationUpstream{}
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	config.Update(func(s *config.Settings) {
		s.Providers["studio"] = &config.ProviderConfig{Type: "ai_studio", BaseURL: server.URL + "/studio/v1beta", APIKey: "fixture-studio-key"}
		s.Providers["vertex"] = &config.ProviderConfig{
			Type: "vertex_ai", BaseURL: server.URL + "/vertex/v1", APIKey: "fixture-vertex-key",
			Project: "fixture-project", Location: "global", VertexRequestType: "paygo",
		}
	})
	providers.ResetProviders()
	chat := func(model string, extra map[string]any) map[string]any {
		payload := map[string]any{"model": model, "max_tokens": 64, "temperature": 0.2, "top_p": 0.9, "messages": []any{
			map[string]any{"role": "system", "content": "Answer <briefly> & kindly."},
			map[string]any{"role": "user", "content": "Say hello"},
		}}
		for key, value := range extra {
			payload[key] = value
		}
		return payload
	}
	for _, golden := range []struct {
		name      string
		exchanges []googleExchange
	}{
		{"google-chat", []googleExchange{
			{"Chat Completions, AI Studio", "/v1/chat/completions", chat("studio/gemini-fixture", nil)},
			{"Chat Completions stream, AI Studio", "/v1/chat/completions", chat("studio/gemini-fixture", map[string]any{"stream": true})},
			{"Chat Completions, Vertex AI", "/v1/chat/completions", chat("vertex/gemini-fixture", nil)},
			{"Messages translated to Chat, AI Studio", "/v1/messages", map[string]any{
				"model": "studio/gemini-fixture", "max_tokens": 64, "system": "Answer briefly.",
				"messages": []any{map[string]any{"role": "user", "content": "Say hello"}},
			}},
			{"Responses translated to Chat, Vertex AI", "/v1/responses", map[string]any{"model": "vertex/gemini-fixture", "input": "Say hello"}},
			{"Billing exhausted, Vertex AI", "/v1/chat/completions", chat("vertex/gemini-billing-fixture", nil)},
			{"Model not available, Vertex AI", "/v1/chat/completions", chat("vertex/missing-model-fixture", nil)},
			{"Credential rejected, AI Studio", "/v1/chat/completions", chat("studio/gemini-denied-fixture", nil)},
			{"Budget spent on reasoning, AI Studio", "/v1/chat/completions", chat("studio/gemini-thinking-fixture", nil)},
		}},
		{"google-embeddings", []googleExchange{
			{"Embeddings, AI Studio", "/v1/embeddings", map[string]any{"model": "studio/gemini-embedding-fixture", "input": "Say hello"}},
			{"Embeddings, Vertex AI", "/v1/embeddings", map[string]any{"model": "vertex/text-embedding-fixture", "input": []any{"first", "second"}}},
			{"Embeddings refused upstream, AI Studio", "/v1/embeddings", map[string]any{"model": "studio/missing-embedding-fixture", "input": "Say hello"}},
		}},
		{"google-images", []googleExchange{
			{"Image generation, Vertex AI", "/v1/images/generations", map[string]any{"model": "vertex/gemini-image-fixture", "prompt": "an origami crane", "n": 1}},
			{"Image generation from a text model, AI Studio", "/v1/images/generations", map[string]any{"model": "studio/gemini-fixture", "prompt": "an origami crane"}},
			{"Image generation refused upstream, AI Studio", "/v1/images/generations", map[string]any{"model": "studio/gemini-denied-fixture", "prompt": "an origami crane"}},
		}},
	} {
		t.Run(golden.name, func(t *testing.T) {
			var rendered strings.Builder
			for _, exchange := range golden.exchanges {
				upstream.take()
				recorder := fixture.do(http.MethodPost, exchange.path, characterizationToken, exchange.payload)
				rendered.WriteString(renderExchange(exchange.title, http.MethodPost+" "+exchange.path, recorder, upstream.take()))
			}
			assertCharacterization(t, golden.name, rendered.String())
		})
	}
}
