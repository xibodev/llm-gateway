package providers

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"llmgw/internal/config"

	copilotauth "github.com/xibodev/llm-provider-auth/copilot"
)

// CopilotAuth returns the runtime's shared Copilot authentication client.
func (rt *Runtime) CopilotAuth() *copilotauth.Client { return rt.copilot }

// SetCopilotEndpointsForTests points the installed Runtime's Copilot client at
// test servers and returns a function that restores the previous endpoints.
func SetCopilotEndpointsForTests(endpoints copilotauth.Endpoints) (restore func()) {
	runtime := Current()
	previous := runtime.copilotEndpoints.swap(endpoints)
	return func() { runtime.copilotEndpoints.swap(previous) }
}

// copilotSettings maps gateway settings onto the library's explicit
// configuration. The library stopped reading the environment, so the fallbacks
// it used to apply itself are applied here, in the same order.
func (rt *Runtime) copilotSettings() copilotauth.Config {
	settings := config.Get()
	return copilotauth.Config{
		CacheDir:   copilotCacheDir(settings.GithubCopilotCacheDir),
		OAuthToken: copilotOAuthToken(settings.GithubCopilotOAuthToken),
		UseGhCLI:   settings.GithubCopilotUseGhCLI,
		AllowProxy: copilotEnabled(settings),
		Endpoints:  rt.copilotEndpoints.get(),
	}
}

func copilotCacheDir(configured string) string {
	if configured != "" {
		return configured
	}
	if dir := strings.TrimSpace(os.Getenv("LLMGW_GITHUB_COPILOT_CACHE_DIR")); dir != "" {
		return dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".llmgw", "cache")
}

// copilotOAuthToken keeps GITHUB_COPILOT_OAUTH_TOKEN working: settings only map
// the LLMGW_-prefixed name, and the library honoured both.
func copilotOAuthToken(configured string) string {
	if configured != "" {
		return configured
	}
	if token := strings.TrimSpace(os.Getenv("GITHUB_COPILOT_OAUTH_TOKEN")); token != "" {
		return token
	}
	return strings.TrimSpace(os.Getenv("LLMGW_GITHUB_COPILOT_OAUTH_TOKEN"))
}

// CopilotEnabled reports whether the operator opted in to the Copilot provider,
// with allow_copilot_proxy or the compatibility environment flag.
func CopilotEnabled() bool { return copilotEnabled(config.Get()) }

func copilotEnabled(settings *config.Settings) bool {
	if settings.AllowCopilotProxy {
		return true
	}
	value := strings.ToLower(strings.TrimSpace(os.Getenv("LLMGW_EXPERIMENTAL_COPILOT_PROVIDER")))
	return value == "1" || value == "true" || value == "yes" || value == "on"
}

// copilotGuidance completes the library's product-neutral errors with the
// gateway's own settings and sign-in surfaces.
func copilotGuidance(err error) string {
	switch {
	case errors.Is(err, copilotauth.ErrProxyDisabled):
		return " Enable it for your own loopback gateway with allow_copilot_proxy: true or " +
			"LLMGW_EXPERIMENTAL_COPILOT_PROVIDER=1."
	case errors.Is(err, copilotauth.ErrNoOAuthToken):
		return " Sign in via the /admin panel, set LLMGW_GITHUB_COPILOT_OAUTH_TOKEN, or " +
			"`gh auth refresh -s copilot` then `gh auth token`."
	case errors.Is(err, copilotauth.ErrOAuthTokenRejected):
		return " Sign in via the /admin panel."
	}
	return ""
}

// copilotInvocationError reports a session the shared client could not
// obtain with the gateway's guidance: a transport failure may be repeated, a
// status is kept, and the library's error is the message.
func copilotInvocationError(err error) error {
	if err == nil || IsInvocation(err) || IsConfig(err) {
		return err
	}
	message := "github_copilot: " + err.Error() + copilotGuidance(err)
	var authErr *copilotauth.AuthError
	if errors.As(err, &authErr) {
		if authErr.Transport {
			return retryableInvocation(message)
		}
		if authErr.StatusCode != 0 {
			return failoverInvocationStatus(message, authErr.StatusCode)
		}
	}
	return invocation(message)
}
