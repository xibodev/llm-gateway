package providers

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type catalogFixtureAuth struct {
	base       string
	prepareErr error
}

func (a catalogFixtureAuth) Prepare() (string, http.Header, error) {
	if a.prepareErr != nil {
		return "", nil, a.prepareErr
	}
	return a.base, http.Header{"Authorization": {"Bearer fixture"}}, nil
}

func TestOpenAIListModelsPrefersDisplayName(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"atlas-small","owned_by":"demo","display_name":"Atlas Small"}]}`))
	}))
	defer server.Close()

	provider := OpenAIProvider{
		auth:    bearerAuth{base: server.URL, apiKey: "fixture"},
		Timeout: 2,
	}
	rows := provider.ListModels()
	if len(rows) != 1 || rows[0].ID != "atlas-small" || rows[0].Label != "Atlas Small" {
		t.Fatalf("models=%+v", rows)
	}
}

func TestOpenAIListModelsReportsSafeFailures(t *testing.T) {
	t.Run("authentication", func(t *testing.T) {
		provider := OpenAIProvider{auth: catalogFixtureAuth{
			prepareErr: errors.New("Copilot session-token exchange returned 401"),
		}}
		_, _, err := provider.ListModelsWithError()
		code, detail, status := CatalogFailure(err)
		if code != "catalog_authentication_failed" || status != 0 ||
			detail != "Provider authentication failed before catalog access." {
			t.Fatalf("failure=(%q,%q,%d)", code, detail, status)
		}
	})

	// Nothing on this transport refreshes a key, so a rejection is reported
	// as the catalog answered it. Copilot's catalog replaces a rejected
	// session; see TestCopilotCatalogStaysOnTheGatewayPath.
	t.Run("rejected key", func(t *testing.T) {
		requests := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests++
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}))
		defer server.Close()
		provider := OpenAIProvider{auth: catalogFixtureAuth{base: server.URL}, Timeout: 2}
		_, _, err := provider.ListModelsWithError()
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
		provider := OpenAIProvider{
			auth: catalogFixtureAuth{base: server.URL}, Timeout: 2,
		}
		_, _, err := provider.ListModelsWithError()
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
		provider := OpenAIProvider{
			auth: catalogFixtureAuth{base: server.URL}, Timeout: 2,
		}
		_, _, err := provider.ListModelsWithError()
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
		provider := OpenAIProvider{
			auth: catalogFixtureAuth{base: server.URL}, Timeout: 2,
		}
		models, _, err := provider.ListModelsWithError()
		if err != nil || models == nil || len(models) != 0 {
			t.Fatalf("models=%+v err=%v", models, err)
		}
	})
}
