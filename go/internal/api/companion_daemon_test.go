package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"

	core "github.com/xibodev/llmgw-core"
	"github.com/xibodev/llmgw-core/extension"
)

// The console sees the companion daemon as the gateway does: where it is,
// whether they share a secret, which providers depend on it, whether it
// answers and what it serves, and what keeps a dependent provider from
// working. The administrator's state names the dependent providers and the
// startup warnings.
func TestCompanionDaemonReportsItsStatusAndWhatKeepsProvidersFromWorking(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	old := *config.Get()
	t.Cleanup(func() { config.Update(func(s *config.Settings) { *s = old }) })
	config.Update(func(s *config.Settings) {
		s.APIKey, s.AllowUnauthenticatedAPI = "admin-secret", false
		s.Providers = map[string]*config.ProviderConfig{
			"codex":   {Type: "openai_compatible", RegistryID: "openai_codex"},
			"copilot": {Type: "github_copilot"},
			"ag":      {Type: "google_antigravity"},
			"zen":     {Type: "opencode_zen_anonymous", Disabled: true},
			"plain":   {Type: "openai_compatible", BaseURL: "http://127.0.0.1:9/v1"},
		}
		s.Endpoints = map[string]*config.EndpointConfig{}
	})
	daemon := http.NewServeMux()
	daemon.HandleFunc("GET /extension/v1/info", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, extension.InfoResponse{Version: "1.3.0", Providers: []extension.ProviderInfo{
			{ID: "openai_codex", Name: "OpenAI Codex", Surfaces: []core.ModelSurface{core.ModelSurfaceChatCompletions, core.ModelSurfaceResponses}},
			{ID: "github_copilot", Name: "GitHub Copilot", Surfaces: []core.ModelSurface{core.ModelSurfaceChatCompletions}},
		}})
	})
	serveExtensionDaemon(t, daemon)
	daemonURL := os.Getenv("LLMGW_EXTENSION_URL")
	t.Setenv("LLMGW_EXTENSION_SECRET", "")
	server := httptest.NewServer(NewServer(Runtime{}))
	defer server.Close()
	report := func() map[string]any {
		t.Helper()
		status, body := jsonRequest(t, server.URL+"/admin/api/companion-daemon", http.MethodGet, "admin-secret", nil)
		if status != http.StatusOK {
			t.Fatalf("status=%d body=%+v", status, body)
		}
		return body
	}
	strs := func(value any, field string) []string {
		out := []string{}
		for _, item := range value.([]any) {
			if field == "" {
				out = append(out, item.(string))
			} else {
				out = append(out, item.(map[string]any)[field].(string))
			}
		}
		return out
	}

	got := report()
	if got["address"] != daemonURL || got["address_configured"] != true || got["secret_set"] != false || got["probed"] != true || got["reachable"] != true || got["version"] != "1.3.0" {
		t.Fatalf("report=%+v", got)
	}
	if ids := strs(got["dependent_providers"], "id"); !reflect.DeepEqual(ids, []string{"ag", "codex", "copilot", "zen"}) {
		t.Fatalf("dependent providers=%v", ids)
	}
	if types := strs(got["dependent_providers"], "type"); !reflect.DeepEqual(types, []string{"google_antigravity", "openai_codex", "github_copilot", "opencode_zen_anonymous"}) {
		t.Fatalf("dependent types=%v", types)
	}
	if served := strs(got["served"], "id"); !reflect.DeepEqual(served, []string{"openai_codex", "github_copilot"}) {
		t.Fatalf("served=%v", served)
	}
	warnings := strs(got["warnings"], "")
	if len(warnings) != 2 || !strings.Contains(warnings[0], "LLMGW_EXTENSION_SECRET is empty") ||
		warnings[1] != "the companion daemon does not serve google_antigravity, which provider(s) ag use" {
		t.Fatalf("warnings=%q, want the missing secret and the type it does not serve, not the disabled provider's", warnings)
	}
	_, state := jsonRequest(t, server.URL+"/admin/api/state", http.MethodGet, "admin-secret", nil)
	if ids := strs(state["companion_daemon_providers"], ""); !reflect.DeepEqual(ids, []string{"ag", "codex", "copilot", "zen"}) {
		t.Fatalf("state companion_daemon_providers=%v", ids)
	}
	if startup := strs(state["startup_warnings"], ""); len(startup) != 1 || !strings.Contains(startup[0], "LLMGW_EXTENSION_SECRET is empty") {
		t.Fatalf("state startup_warnings=%q", startup)
	}

	t.Setenv("LLMGW_EXTENSION_SECRET", "fixture-shared-secret")
	if warnings := strs(report()["warnings"], ""); len(warnings) != 1 || !strings.Contains(warnings[0], "google_antigravity") {
		t.Fatalf("with a secret: warnings=%q", warnings)
	}

	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	t.Setenv("LLMGW_EXTENSION_URL", closed.URL)
	got = report()
	warnings = strs(got["warnings"], "")
	if got["reachable"] != false || got["error"] == nil || len(warnings) != 1 ||
		!strings.HasPrefix(warnings[0], "the companion daemon did not answer, so provider(s) ag, codex, copilot cannot serve requests: ") {
		t.Fatalf("an unreachable daemon: report=%+v", got)
	}

	t.Setenv("LLMGW_EXTENSION_URL", "")
	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{"plain": {Type: "openai_compatible", BaseURL: "http://127.0.0.1:9/v1"}}
	})
	got = report()
	if got["probed"] != false || got["address_configured"] != false || len(got["dependent_providers"].([]any)) != 0 || len(got["warnings"].([]any)) != 0 {
		t.Fatalf("without a dependent provider or an address: report=%+v", got)
	}

	if status, _ := jsonRequest(t, server.URL+"/admin/api/companion-daemon", http.MethodGet, "", nil); status != http.StatusUnauthorized {
		t.Fatalf("without a key: status=%d, want 401", status)
	}
}
