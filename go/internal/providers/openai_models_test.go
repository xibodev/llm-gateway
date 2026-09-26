package providers

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOpenAIListModelsPrefersDisplayName(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" || r.Header.Get("Authorization") != "Bearer fixture" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"atlas-small","owned_by":"demo","display_name":"Atlas Small"}]}`))
	}))
	defer server.Close()

	rows := openAICatalogFixture(server.URL, 2).ListModels()
	if len(rows) != 1 || rows[0].ID != "atlas-small" || rows[0].Label != "Atlas Small" {
		t.Fatalf("models=%+v", rows)
	}
}

// The catalog's failures carry safe codes and details. A credential that
// could not be prepared is reported by the facade that prepares one, as
// Copilot's is; see TestCopilotCatalogStaysOnTheGatewayPath.
func TestOpenAIListModelsReportsSafeFailures(t *testing.T) {
	// Nothing on this path refreshes a key, so a rejection is reported as
	// the catalog answered it. Copilot's catalog replaces a rejected
	// session; see TestCopilotCatalogStaysOnTheGatewayPath.
	t.Run("rejected key", func(t *testing.T) {
		requests := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests++
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}))
		defer server.Close()
		_, _, err := listModelsWithError(openAICatalogFixture(server.URL, 2))
		code, _, status := CatalogFailure(err)
		if code != "catalog_http_error" || status != http.StatusUnauthorized || requests != 1 {
			t.Fatalf("failure=(%q,%d) requests=%d", code, status, requests)
		}
	})

	t.Run("http status", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{"error":{"message":"catalog unavailable"}}`, http.StatusBadGateway)
		}))
		defer server.Close()
		_, _, err := listModelsWithError(openAICatalogFixture(server.URL, 2))
		code, detail, status := CatalogFailure(err)
		if code != "catalog_http_error" || status != http.StatusBadGateway ||
			detail == "" {
			t.Fatalf("failure=(%q,%q,%d)", code, detail, status)
		}
	})

	t.Run("invalid json", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("not-json"))
		}))
		defer server.Close()
		_, _, err := listModelsWithError(openAICatalogFixture(server.URL, 2))
		code, _, _ := CatalogFailure(err)
		if code != "catalog_invalid_json" {
			t.Fatalf("code=%q", code)
		}
	})

	t.Run("legitimate empty catalog", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[]}`))
		}))
		defer server.Close()
		models, _, err := listModelsWithError(openAICatalogFixture(server.URL, 2))
		if err != nil || models == nil || len(models) != 0 {
			t.Fatalf("models=%+v err=%v", models, err)
		}
	})
}
