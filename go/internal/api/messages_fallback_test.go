package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/providers"
)

// messagesRequest posts body to /v1/messages through handler, with header.
func messagesRequest(handler http.Handler, body map[string]any, header http.Header) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer fixture-gateway-token")
	for name, values := range header {
		req.Header[name] = values
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}

// /v1/messages reads the failover budget from the header or the body, as Chat
// and Responses do: a stream that cannot open within it ends with the
// budget's 504. The body field is the gateway's, not a Messages field, so
// the adapted stream never refuses it as one it cannot preserve.
func TestMessagesStreamReadsTheFailoverBudget(t *testing.T) {
	for name, apply := range map[string]func(map[string]any, http.Header){
		"header": func(_ map[string]any, header http.Header) { header.Set("X-LLMGW-Fallback-Timeout-Ms", "100") },
		"body":   func(body map[string]any, _ http.Header) { body["fallback_timeout_ms"] = 100 },
	} {
		t.Run(name, func(t *testing.T) {
			handler := setupAPISmokeFixture(t, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				select {
				case <-r.Context().Done():
				case <-time.After(5 * time.Second):
				}
			}))
			body := map[string]any{
				"model": "fixture/model", "stream": true, "max_tokens": 16,
				"messages": []any{map[string]any{"role": "user", "content": "hi"}},
			}
			header := http.Header{}
			apply(body, header)
			started := time.Now()
			response := messagesRequest(handler, body, header)
			if response.Code != http.StatusGatewayTimeout || time.Since(started) > 3*time.Second {
				t.Fatalf("status=%d after %v body=%s, want the budget's 504", response.Code, time.Since(started), response.Body.String())
			}
		})
	}
}

// The affinity key in a Messages body chooses where the chain starts, and
// neither routing control reaches the native target that serves it.
func TestMessagesAffinityKeyStartsTheChainAndStaysHome(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	var forwarded map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		mu.Lock()
		paths, forwarded = append(paths, r.URL.Path), payload
		mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"id": "ok", "type": "message", "content": []any{}, "usage": map[string]any{}})
	}))
	defer upstream.Close()
	handler := setupAPISmokeFixture(t, http.NotFoundHandler())
	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{
			"first":  {Type: "anthropic", BaseURL: upstream.URL + "/first"},
			"second": {Type: "anthropic", BaseURL: upstream.URL + "/second"},
		}
		s.Endpoints = map[string]*config.EndpointConfig{"route": {Failover: []config.EndpointMember{
			{Provider: "first", Model: "a"}, {Provider: "second", Model: "b"},
		}}}
	})
	providers.ResetProviders()
	// The key the chain hashes to its second member.
	key := ""
	for index := 0; key == ""; index++ {
		candidate := fmt.Sprintf("affinity-%d", index)
		digest := sha256.Sum256([]byte(candidate))
		if binary.BigEndian.Uint64(digest[:8])%2 == 1 {
			key = candidate
		}
	}
	response := messagesRequest(handler, map[string]any{
		"model": "route", "max_tokens": 1, "messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"affinity_key": key, "fallback_timeout_ms": 60000,
	}, nil)
	mu.Lock()
	defer mu.Unlock()
	if response.Code != http.StatusOK || len(paths) != 1 || paths[0] != "/second/v1/messages" {
		t.Fatalf("status=%d paths=%v body=%s", response.Code, paths, response.Body.String())
	}
	for _, control := range []string{"affinity_key", "fallback_timeout_ms"} {
		if _, present := forwarded[control]; present {
			t.Fatalf("%s reached the upstream: %v", control, forwarded)
		}
	}
}
