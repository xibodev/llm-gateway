package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/providers"
)

// openAIEdgeUpstream is a synthetic Bedrock, a forced-adaptation
// OpenAI-compatible upstream and a keyless Pollinations. The path picks the
// upstream and the model the answer: a completion, or a refusal.
type openAIEdgeUpstream struct {
	mu    sync.Mutex
	calls []recordedCall
}

func (u *openAIEdgeUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	u.mu.Lock()
	u.calls = append(u.calls, recordedCall{method: r.Method, path: r.URL.Path, body: body})
	u.mu.Unlock()
	var payload map[string]any
	_ = json.Unmarshal(body, &payload)
	stream := payload["stream"] == true
	model, _ := payload["model"].(string)
	switch r.URL.Path {
	case "/br/models":
		writeJSON(w, http.StatusOK, map[string]any{"data": []any{
			map[string]any{"id": "chat-fixture", "owned_by": "fixture", "supported_endpoints": []string{"/chat/completions"}},
			map[string]any{"id": "responses-fixture", "owned_by": "fixture", "supported_endpoints": []string{"/chat/completions", "/responses"}},
		}})
	case "/ad/models":
		writeJSON(w, http.StatusOK, map[string]any{"data": []any{map[string]any{
			"id": "reasoning-fixture", "supported_endpoints": []string{"/chat/completions"},
			"capabilities": map[string]any{"supports": map[string]any{"reasoning_effort": []string{"low", "high"}}},
		}}})
	case "/po/models":
		writeJSON(w, http.StatusOK, []any{map[string]any{"name": "openai-fast", "tier": "anonymous", "output_modalities": []string{"text"}}})
	case "/br/chat/completions", "/ad/chat/completions", "/po/v1/chat/completions":
		if model == "throttled-fixture" {
			w.Header().Set("Retry-After", "7")
			writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": map[string]any{"message": "fixture rate limit", "type": "rate_limit"}})
			return
		}
		writeCharacterizationChat(w, model, stream)
	case "/br/responses":
		writeCharacterizationResponses(w, model, stream)
	default:
		http.NotFound(w, r)
	}
}

func (u *openAIEdgeUpstream) take() []recordedCall {
	u.mu.Lock()
	defer u.mu.Unlock()
	calls := u.calls
	u.calls = nil
	return calls
}

// TestOpenAIEdgeHTTPCharacterization pins what a client receives from Bedrock,
// from an OpenAI-compatible upstream with forced adaptation and from a
// keyless Pollinations, and every request the gateway sends them: the
// surfaces Bedrock serves natively and the one translated for it, the
// reasoning model's renamed output limit, the anonymous Chat path and a
// refusal's status and message. The Retry-After the upstream sent is not
// the client's to read on these paths.
func TestOpenAIEdgeHTTPCharacterization(t *testing.T) {
	fixture := setupCharacterization(t)
	upstream := &openAIEdgeUpstream{}
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	config.Update(func(s *config.Settings) {
		s.Providers["bedrock"] = &config.ProviderConfig{Type: "bedrock", Region: "us-east-1", BaseURL: server.URL + "/br", APIKey: "fixture-bedrock-key"}
		s.Providers["adapted"] = &config.ProviderConfig{Type: "openai_compatible", BaseURL: server.URL + "/ad", APIKey: "fixture-upstream-token", ForceApiSupport: true}
		s.Providers["pollinations"] = &config.ProviderConfig{Type: "openai_compatible", RegistryID: "pollinations", BaseURL: server.URL + "/po"}
	})
	providers.ResetProviders()
	for _, provider := range []string{"bedrock", "adapted", "pollinations"} {
		if rows := providers.RefreshCatalog(provider); len(rows) == 0 {
			t.Fatalf("%s catalog is empty", provider)
		}
	}
	chat := func(model string, stream bool) map[string]any {
		return map[string]any{"model": model, "stream": stream, "max_tokens": 64, "messages": []any{map[string]any{"role": "user", "content": "Say hello"}}}
	}
	responses := func(model string, stream bool) map[string]any {
		return map[string]any{"model": model, "stream": stream, "input": "Say hello"}
	}
	for _, golden := range []struct {
		name      string
		exchanges []googleExchange
	}{
		{"bedrock", []googleExchange{
			{"Chat Completions, Bedrock", "/v1/chat/completions", chat("bedrock/chat-fixture", false)},
			{"Chat Completions stream, Bedrock", "/v1/chat/completions", chat("bedrock/chat-fixture", true)},
			{"Responses, native Bedrock", "/v1/responses", responses("bedrock/responses-fixture", false)},
			{"Responses stream, native Bedrock", "/v1/responses", responses("bedrock/responses-fixture", true)},
			{"Responses translated to a Chat-only model, Bedrock", "/v1/responses", responses("bedrock/chat-fixture", false)},
			{"Refused upstream, Bedrock", "/v1/chat/completions", chat("bedrock/throttled-fixture", false)},
		}},
		{"openai-compatible-edges", []googleExchange{
			{"Chat Completions to a reasoning model, forced adaptation", "/v1/chat/completions", chat("adapted/reasoning-fixture", false)},
			{"Chat Completions stream to a reasoning model, forced adaptation", "/v1/chat/completions", chat("adapted/reasoning-fixture", true)},
			{"Chat Completions, keyless Pollinations", "/v1/chat/completions", chat("pollinations/openai-fast", false)},
			{"Stream refused upstream, keyless Pollinations", "/v1/chat/completions", chat("pollinations/throttled-fixture", true)},
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
