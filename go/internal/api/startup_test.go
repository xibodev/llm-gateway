package api

import (
	"strings"
	"testing"

	"llmgw/internal/config"
)

// The gateway warns when a provider the companion daemon serves runs without
// the daemon's shared secret, naming the providers but never the secret.
func TestStartupWarnsWhenCompanionDaemonProvidersRunWithoutASharedSecret(t *testing.T) {
	old := *config.Get()
	t.Cleanup(func() { config.Update(func(s *config.Settings) { *s = old }) })
	config.Update(func(s *config.Settings) {
		s.AllowUnauthenticatedAPI, s.APIKeys = false, nil
		s.Providers = map[string]*config.ProviderConfig{
			"openai":  {Type: "openai_compatible"},
			"copilot": {Type: "github_copilot"},
			"codex":   {Type: "openai_compatible", RegistryID: "openai_codex"},
		}
	})

	t.Setenv("LLMGW_EXTENSION_SECRET", "")
	warnings := StartupWarnings("127.0.0.1")
	if len(warnings) != 1 || !strings.Contains(warnings[0], "LLMGW_EXTENSION_SECRET") ||
		!strings.Contains(warnings[0], "codex, copilot ") || strings.Contains(warnings[0], "openai") {
		t.Fatalf("warnings=%q, want one naming the daemon's providers", warnings)
	}
	t.Setenv("LLMGW_EXTENSION_SECRET", "fixture-secret")
	if warnings := StartupWarnings("127.0.0.1"); len(warnings) != 0 {
		t.Fatalf("with a shared secret: warnings=%q, want none", warnings)
	}
	t.Setenv("LLMGW_EXTENSION_SECRET", "")
	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{"openai": {Type: "openai_compatible"}}
	})
	if warnings := StartupWarnings("127.0.0.1"); len(warnings) != 0 {
		t.Fatalf("without a provider the daemon serves: warnings=%q, want none", warnings)
	}
}
