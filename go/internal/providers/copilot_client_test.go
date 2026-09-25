package providers

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"llmgw/internal/config"
)

// withCopilotSettings applies Copilot settings for one test and restores the
// previous values afterwards.
func withCopilotSettings(t *testing.T, apply func(*config.Settings)) {
	t.Helper()
	previous := *config.Get()
	t.Cleanup(func() {
		config.Update(func(s *config.Settings) {
			s.GithubCopilotCacheDir = previous.GithubCopilotCacheDir
			s.GithubCopilotOAuthToken = previous.GithubCopilotOAuthToken
			s.GithubCopilotUseGhCLI = previous.GithubCopilotUseGhCLI
			s.AllowCopilotProxy = previous.AllowCopilotProxy
		})
	})
	config.Update(apply)
}

// TestCopilotSettingsPreserveLibraryEnvironmentFallbacks pins the fallbacks the
// auth library applied itself before it stopped reading the environment.
func TestCopilotSettingsPreserveLibraryEnvironmentFallbacks(t *testing.T) {
	environmentCache := t.TempDir()
	t.Setenv("LLMGW_GITHUB_COPILOT_CACHE_DIR", environmentCache)
	t.Setenv("GITHUB_COPILOT_OAUTH_TOKEN", "environment-token")
	t.Setenv("LLMGW_GITHUB_COPILOT_OAUTH_TOKEN", "llmgw-environment-token")
	t.Setenv("LLMGW_EXPERIMENTAL_COPILOT_PROVIDER", "yes")
	withCopilotSettings(t, func(s *config.Settings) {
		s.GithubCopilotCacheDir, s.GithubCopilotOAuthToken = "", ""
		s.GithubCopilotUseGhCLI, s.AllowCopilotProxy = false, false
	})
	got := Current().copilotSettings()
	if got.CacheDir != environmentCache || got.OAuthToken != "environment-token" || !got.AllowProxy || got.UseGhCLI {
		t.Fatalf("environment fallbacks: %+v", got)
	}

	withCopilotSettings(t, func(s *config.Settings) {
		s.GithubCopilotCacheDir, s.GithubCopilotOAuthToken = "configured-dir", "configured-token"
		s.GithubCopilotUseGhCLI = true
	})
	got = Current().copilotSettings()
	if got.CacheDir != "configured-dir" || got.OAuthToken != "configured-token" || !got.UseGhCLI {
		t.Fatalf("settings must win over the environment: %+v", got)
	}

	t.Setenv("LLMGW_GITHUB_COPILOT_CACHE_DIR", "")
	t.Setenv("GITHUB_COPILOT_OAUTH_TOKEN", "")
	t.Setenv("LLMGW_EXPERIMENTAL_COPILOT_PROVIDER", "")
	withCopilotSettings(t, func(s *config.Settings) {
		s.GithubCopilotCacheDir, s.GithubCopilotOAuthToken = "", ""
	})
	got = Current().copilotSettings()
	home, _ := os.UserHomeDir()
	if got.CacheDir != filepath.Join(home, ".llmgw", "cache") || got.OAuthToken != "llmgw-environment-token" || got.AllowProxy {
		t.Fatalf("defaults: %+v", got)
	}
}

// TestCopilotErrorsKeepGatewayGuidance pins the exact messages the gateway
// returned when the guidance lived in the auth library. Core reports the
// library's error as its cause, which the facade completes with the guidance.
func TestCopilotErrorsKeepGatewayGuidance(t *testing.T) {
	t.Setenv("LLMGW_EXPERIMENTAL_COPILOT_PROVIDER", "")
	t.Setenv("GITHUB_COPILOT_OAUTH_TOKEN", "")
	t.Setenv("LLMGW_GITHUB_COPILOT_OAUTH_TOKEN", "")
	upstream := newCopilotUpstream(t)
	runtime := installCopilot(t, upstream, &config.ProviderConfig{Type: "github_copilot"})
	config.Update(func(s *config.Settings) { s.GithubCopilotOAuthToken, s.AllowCopilotProxy = "", false })
	provider := copilotFacade(t, runtime, gatewayCaller())
	complete := func() error {
		_, err := provider.Complete("chat-model", []Message{{"role": "user", "content": "hi"}}, nil)
		return err
	}

	assertCopilotMessage(t, complete(), "github_copilot: github_copilot provider is disabled by default "+
		"(personal-use grey area). Enable it for your own loopback gateway with allow_copilot_proxy: "+
		"true or LLMGW_EXPERIMENTAL_COPILOT_PROVIDER=1.")

	config.Update(func(s *config.Settings) { s.AllowCopilotProxy = true })
	assertCopilotMessage(t, complete(), "github_copilot: no GitHub Copilot OAuth token available. Sign in via "+
		"the /admin panel, set LLMGW_GITHUB_COPILOT_OAUTH_TOKEN, or `gh auth refresh -s copilot` then "+
		"`gh auth token`.")

	upstream.setRefuse(func(string) int { return http.StatusUnauthorized })
	config.Update(func(s *config.Settings) { s.GithubCopilotOAuthToken = "synthetic-oauth" })
	err := complete()
	assertCopilotMessage(t, err, "github_copilot: Copilot session-token exchange returned 401: the OAuth "+
		"token is invalid or lacks Copilot access. Sign in via the /admin panel.")
	var invocationErr *InvocationError
	if errors.As(err, &invocationErr) && (invocationErr.Status != http.StatusUnauthorized || !invocationErr.FailoverEligible) {
		t.Fatalf("401 must stay failover-eligible with its status: %+v", invocationErr)
	}
	if _, calls := upstream.take(); len(calls) != 0 {
		t.Fatalf("a session Copilot never issued reached the API: %+v", calls)
	}
}

func assertCopilotMessage(t *testing.T, err error, want string) {
	t.Helper()
	var invocationErr *InvocationError
	if !errors.As(err, &invocationErr) || invocationErr.Msg != want {
		t.Fatalf("error=%v\nwant message %q", err, want)
	}
}
