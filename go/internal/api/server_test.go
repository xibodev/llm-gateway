package api

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestConsoleRoutesPreserveLegacySurfaces(t *testing.T) {
	handler := NewServer()

	admin := httptest.NewRecorder()
	handler.ServeHTTP(admin, httptest.NewRequest(http.MethodGet, "/admin", nil))
	if admin.Code != http.StatusFound || admin.Header().Get("Location") != "/console" {
		t.Fatalf("admin redirect status=%d location=%q", admin.Code, admin.Header().Get("Location"))
	}

	console := httptest.NewRecorder()
	handler.ServeHTTP(console, httptest.NewRequest(http.MethodGet, "/console", nil))
	if console.Code != http.StatusOK {
		t.Fatalf("console status=%d", console.Code)
	}
	consoleHTML := console.Body.String()
	if !strings.Contains(consoleHTML, "id=\"app\"") {
		t.Fatalf("console body does not contain app mount: %q", consoleHTML)
	}
	if strings.Contains(consoleHTML, "http://") || strings.Contains(consoleHTML, "https://") {
		t.Fatal("console entry document includes an external asset URL")
	}

	portal := httptest.NewRecorder()
	handler.ServeHTTP(portal, httptest.NewRequest(http.MethodGet, "/portal", nil))
	if portal.Code != http.StatusOK || portal.Body.String() != consoleHTML {
		t.Fatalf("portal does not serve the same local bundle: status=%d", portal.Code)
	}

	for _, legacyPath := range []string{"/admin-legacy", "/portal-legacy"} {
		legacy := httptest.NewRecorder()
		handler.ServeHTTP(legacy, httptest.NewRequest(http.MethodGet, legacyPath, nil))
		if legacy.Code != http.StatusOK || !strings.Contains(legacy.Body.String(), "<") {
			t.Fatalf("legacy route %s status=%d", legacyPath, legacy.Code)
		}
	}
}

func TestConsoleAssetsAreServedFromEmbeddedDist(t *testing.T) {
	handler := NewServer()
	index := httptest.NewRecorder()
	handler.ServeHTTP(index, httptest.NewRequest(http.MethodGet, "/console", nil))

	asset := regexp.MustCompile(`src="(/console/assets/[^"]+)"`).FindStringSubmatch(index.Body.String())
	if len(asset) != 2 {
		t.Fatalf("console index has no local script asset: %q", index.Body.String())
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, asset[1], nil))
	if response.Code != http.StatusOK || response.Body.Len() == 0 {
		t.Fatalf("asset %s status=%d bytes=%d", asset[1], response.Code, response.Body.Len())
	}
}
