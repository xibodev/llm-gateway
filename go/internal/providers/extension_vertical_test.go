package providers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/xibodev/llm-provider-auth/tokenstore"
	core "github.com/xibodev/llmgw-core"
	"github.com/xibodev/llmgw-core/oauthflow"
)

func TestExtensionClientInvokeAndStream(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/extension/v1/test_provider/invoke", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Surface") != "chat_completions" || r.Header.Get("X-Model") != "gpt-test" {
			t.Errorf("unexpected headers: surface=%s, model=%s", r.Header.Get("X-Surface"), r.Header.Get("X-Model"))
		}
		if r.Header.Get("Authorization") != "Bearer test-secret" {
			t.Errorf("unexpected auth: %s", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"hello from extension"}}]}`)
	})

	mux.HandleFunc("/extension/v1/test_provider/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: chunk-1\n\n")
		flusher.Flush()
		_, _ = io.WriteString(w, "data: chunk-2\n\n")
		flusher.Flush()
	})

	mux.HandleFunc("/extension/v1/test_provider/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"models": []core.ModelInfo{
				{ID: "gpt-test", DisplayName: "GPT Test"},
			},
		})
	})

	mux.HandleFunc("/extension/v1/test_provider/refresh", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"record": tokenstore.Record{
				AccessToken:  "refreshed-access",
				RefreshToken: "refreshed-refresh",
			},
		})
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	client := &ExtensionClient{
		BaseURL:    server.URL,
		Secret:     "test-secret",
		HTTPClient: server.Client(),
	}

	// 1. Invoke
	resp, err := client.Invoke(context.Background(), "test_provider", core.Request{
		Surface:     core.ModelSurfaceChatCompletions,
		Model:       "gpt-test",
		Body:        []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
		ContentType: core.ContentTypeJSON,
	})
	if err != nil {
		t.Fatalf("invoke error: %v", err)
	}
	if string(resp.Body) != `{"choices":[{"message":{"content":"hello from extension"}}]}` {
		t.Errorf("unexpected body: %s", string(resp.Body))
	}

	// 2. Stream
	stream, err := client.Stream(context.Background(), "test_provider", core.Request{
		Surface:     core.ModelSurfaceChatCompletions,
		Model:       "gpt-test",
		Body:        []byte(`{"messages":[{"role":"user","content":"hi"}]}`),
		ContentType: core.ContentTypeJSON,
	})
	if err != nil {
		t.Fatalf("stream error: %v", err)
	}
	defer stream.Close()
	chunk1, err := stream.Next()
	if err != nil || string(chunk1) != "data: chunk-1\n\n" {
		t.Errorf("unexpected chunk 1: %s (err: %v)", string(chunk1), err)
	}
	chunk2, err := stream.Next()
	if err != nil || string(chunk2) != "data: chunk-2\n\n" {
		t.Errorf("unexpected chunk 2: %s (err: %v)", string(chunk2), err)
	}

	// 3. Models
	models, err := client.ListModels(context.Background(), "test_provider", nil)
	if err != nil || len(models) != 1 || models[0].ID != "gpt-test" {
		t.Errorf("unexpected models: %+v, err: %v", models, err)
	}

	// 4. Refresh
	refreshed, err := client.Refresh(context.Background(), "test_provider", tokenstore.Record{AccessToken: "old-access"})
	if err != nil || refreshed.AccessToken != "refreshed-access" {
		t.Errorf("unexpected refresh: %+v, err: %v", refreshed, err)
	}
}

func TestExtensionOAuthDriver(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/extension/v1/oauth_test/oauth/start", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"authorization": oauthflow.Authorization{
				UserCode:        "USER-123",
				VerificationURI: "https://example.test/verify",
			},
		})
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	client := &ExtensionClient{
		BaseURL:    server.URL,
		HTTPClient: server.Client(),
	}

	driver := client.OAuthDriver("oauth_test")
	auth, err := driver.Start(context.Background(), oauthflow.StartRequest{Method: oauthflow.MethodDevice})
	if err != nil {
		t.Fatalf("start error: %v", err)
	}
	if auth.UserCode != "USER-123" || auth.VerificationURI != "https://example.test/verify" {
		t.Errorf("unexpected auth: %+v", auth)
	}
}
