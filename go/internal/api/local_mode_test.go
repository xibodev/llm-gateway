package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
)

func useLocalMode(t *testing.T) {
	t.Helper()
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	old := *config.Get()
	t.Cleanup(func() {
		config.Update(func(s *config.Settings) { *s = old })
		iam.ResetForTests()
		providers.ResetProviders()
	})
	config.Update(func(s *config.Settings) {
		s.APIKey, s.APIKeys, s.AllowUnauthenticatedAPI = "", nil, true
		s.Providers = map[string]*config.ProviderConfig{}
		s.Endpoints = map[string]*config.EndpointConfig{}
	})
	providers.ResetProviders()
}

func TestLocalModeServesBrowsersOnlyFromLoopbackOrigins(t *testing.T) {
	useLocalMode(t)
	project, err := iam.CreateProject("tools", "Tools")
	if err != nil {
		t.Fatal(err)
	}
	owner, err := iam.CreatePrincipal("service", "fixture:service", "", "Service")
	if err != nil {
		t.Fatal(err)
	}
	if err := iam.SetMembership(project.ID, owner.ID, "member"); err != nil {
		t.Fatal(err)
	}
	issued, err := iam.IssueKey(iam.KeyCreate{ProjectID: project.ID, PrincipalID: owner.ID, Name: "service"})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, origin, token string
		served              bool
	}{
		{"no origin", "", "", true},
		{"no origin with a placeholder key", "", "placeholder", true},
		{"localhost", "http://localhost:3000", "", true},
		{"name under localhost", "http://app.localhost:5173", "", true},
		{"IPv4 loopback", "http://127.0.0.1:8787", "placeholder", true},
		{"other IPv4 loopback address", "http://127.20.30.40", "", true},
		{"IPv6 loopback", "http://[::1]:8787", "", true},
		{"public site", "https://example.com", "", false},
		{"public site with a placeholder key", "https://example.com", "placeholder", false},
		{"private network address", "http://192.168.1.20:8787", "", false},
		{"name that only starts with localhost", "http://localhost.example.com", "", false},
		{"name that only starts with a loopback address", "http://127.0.0.1.example.com", "", false},
		{"opaque origin", "null", "", false},
		{"gateway-issued key from a public site", "https://example.com", issued.Token, true},
	} {
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		if c.origin != "" {
			request.Header.Set("Origin", c.origin)
		}
		if c.token != "" {
			request.Header.Set("Authorization", "Bearer "+c.token)
		}
		principal, status, message := requireAPIKey(request)
		if !c.served {
			if status != http.StatusForbidden || principal != nil || message != localModeOriginMessage {
				t.Errorf("%s: status=%d message=%q served=%v, want 403", c.name, status, message, principal != nil)
			}
			continue
		}
		if status != 0 || principal == nil {
			t.Errorf("%s: status=%d message=%q, want served", c.name, status, message)
			continue
		}
		if c.token == issued.Token && principal.PrincipalID != owner.ID {
			t.Errorf("%s: served as principal %q, want the key's principal", c.name, principal.PrincipalID)
		}
	}
}

func TestLocalModeRefusesNonLoopbackOriginWithAnError(t *testing.T) {
	useLocalMode(t)
	handler := NewServer(Runtime{})
	for _, c := range []struct {
		origin string
		status int
	}{
		{"", http.StatusOK},
		{"http://localhost:8787", http.StatusOK},
		{"https://example.com", http.StatusForbidden},
	} {
		request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		if c.origin != "" {
			request.Header.Set("Origin", c.origin)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != c.status {
			t.Fatalf("origin %q: status=%d body=%s, want %d", c.origin, response.Code, response.Body.String(), c.status)
		}
		if c.status != http.StatusForbidden {
			continue
		}
		var body struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Error.Message != localModeOriginMessage {
			t.Fatalf("origin %q: error body=%s", c.origin, response.Body.String())
		}
	}
}

func TestAuthenticatedModeIgnoresTheOrigin(t *testing.T) {
	useLocalMode(t)
	config.Update(func(s *config.Settings) {
		s.APIKey, s.AllowUnauthenticatedAPI = "fixture-admin", false
	})
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Origin", "https://example.com")
	request.Header.Set("Authorization", "Bearer fixture-admin")
	if principal, status, message := requireAPIKey(request); status != 0 || principal == nil {
		t.Fatalf("status=%d message=%q, want served", status, message)
	}
}

func TestStartupWarnsWhenLocalModeListensBeyondLoopback(t *testing.T) {
	useLocalMode(t)
	for _, host := range []string{"127.0.0.1", "localhost", "::1", "[::1]"} {
		if warnings := StartupWarnings(host); len(warnings) != 0 {
			t.Errorf("listen host %s: warnings=%q, want none", host, warnings)
		}
	}
	for _, host := range []string{"0.0.0.0", "::", "192.168.1.20", "gateway"} {
		warnings := StartupWarnings(host)
		if len(warnings) != 1 || !strings.Contains(warnings[0], "unauthenticated local mode") || !strings.Contains(warnings[0], host) {
			t.Errorf("listen host %s: warnings=%q, want one local-mode warning", host, warnings)
		}
	}
	config.Update(func(s *config.Settings) { s.AllowUnauthenticatedAPI = false })
	if warnings := StartupWarnings("0.0.0.0"); len(warnings) != 0 {
		t.Errorf("authenticated mode: warnings=%q, want none", warnings)
	}
}
