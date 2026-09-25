package providers

import (
	"net/http"
	"strings"

	"llmgw/internal/config"

	codexauth "github.com/xibodev/llm-provider-auth/codex"
)

// codexBrowserAuthorizeURL is the canonical sign-in page of the browser flow.
// The codex package of llm-provider-auth runs only the device flow, so it has
// no constant for it.
const codexBrowserAuthorizeURL = "https://auth.openai.com/oauth/authorize"

// CodexEndpoints replaces the canonical OpenAI endpoints of the gateway's
// Codex calls. Empty fields keep the canonical endpoint.
type CodexEndpoints struct {
	// OAuth replaces the device authorization, token and revocation URLs.
	// The browser flow redeems and refreshes at the same token URL.
	OAuth codexauth.Endpoints
	// BrowserAuthorizeURL replaces the browser flow's sign-in page.
	BrowserAuthorizeURL string
	// ResponsesBaseURL and ModelsURL replace the Codex API URLs the gateway
	// passes to core.
	ResponsesBaseURL string
	ModelsURL        string
	// HTTPClient performs the OAuth requests and the core Runtime's Codex
	// requests. Nil keeps each library's default client for OAuth and a
	// client with the provider's timeout for Codex.
	HTTPClient *http.Client
}

// SetCodexEndpointsForTests points the installed Runtime's Codex calls at
// test servers until the test ends. It takes only the Cleanup method of a
// testing.TB, so the gateway binary does not link the testing package.
//
// The core Runtime builds its Codex provider and refresh from the endpoints
// once per settings generation, so both swaps publish the settings again,
// unchanged, to have it rebuild them.
func SetCodexEndpointsForTests(t interface{ Cleanup(func()) }, endpoints CodexEndpoints) {
	runtime := Current()
	previous := runtime.codexEndpoints.swap(endpoints)
	config.Update(func(*config.Settings) {})
	t.Cleanup(func() {
		runtime.codexEndpoints.swap(previous)
		config.Update(func(*config.Settings) {})
	})
}

// currentCodexEndpoints returns the installed Runtime's endpoints with every
// empty URL set to its canonical value.
func currentCodexEndpoints() CodexEndpoints {
	return Current().codexEndpoints.get().withDefaults()
}

// withDefaults returns the endpoints with every empty URL set to its
// canonical value.
func (e CodexEndpoints) withDefaults() CodexEndpoints {
	for _, field := range []struct {
		value     *string
		canonical string
	}{
		{&e.OAuth.UserCodeURL, codexauth.UserCodeURL},
		{&e.OAuth.DeviceTokenURL, codexauth.DeviceTokenURL},
		{&e.OAuth.OAuthTokenURL, codexauth.OAuthTokenURL},
		{&e.OAuth.RevokeURL, codexauth.RevokeURL},
		{&e.BrowserAuthorizeURL, codexBrowserAuthorizeURL},
		{&e.ResponsesBaseURL, codexauth.ResponsesBaseURL},
		{&e.ModelsURL, codexauth.ModelsURL},
	} {
		if strings.TrimSpace(*field.value) == "" {
			*field.value = field.canonical
		}
	}
	return e
}

// codexOAuth returns the llm-provider-auth configuration for one OAuth client.
// Callers pass the client the grant belongs to: the configured one for a new
// sign-in, the one that started a pending flow, or the one stored with a
// connection.
func codexOAuth(clientID string) codexauth.Config {
	endpoints := currentCodexEndpoints()
	return codexauth.Config{ClientID: clientID, Endpoints: endpoints.OAuth, HTTPClient: endpoints.HTTPClient}
}
