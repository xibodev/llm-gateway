package providers

import (
	"net/http"

	"llmgw/internal/config"
	"llmgw/internal/copilotauth"
	"llmgw/internal/iam"
)

// OpenAIAuth decouples authentication from the OpenAI wire transport. It
// resolves the base URL + request headers for a call and can refresh
// credentials after a 401. This is why one transport (OpenAIProvider) serves
// every OpenAI-compatible backend — openai_compatible, bedrock, litellm, and
// github_copilot differ ONLY in how a request is authenticated + where it points.
type OpenAIAuth interface {
	// Prepare resolves the base URL and headers for a request.
	Prepare() (baseURL string, headers http.Header, err error)
	// CanRefresh reports whether a 401 is worth retrying after Refresh.
	CanRefresh() bool
	// Refresh forces re-authentication (e.g. a new session token).
	Refresh() error
}

type observedOpenAIAuth interface {
	PrepareObserved() (
		baseURL string,
		headers http.Header,
		observation *iam.ProviderAccountObservation,
		err error,
	)
}

func prepareOpenAIAuth(
	auth OpenAIAuth,
) (string, http.Header, *iam.ProviderAccountObservation, error) {
	if observed, ok := auth.(observedOpenAIAuth); ok {
		return observed.PrepareObserved()
	}
	baseURL, headers, err := auth.Prepare()
	return baseURL, headers, nil, err
}

// bearerAuth is a static base URL + optional Bearer key: openai_compatible,
// bedrock (token), litellm, localai, and any keyless local server.
type bearerAuth struct {
	base        string
	apiKey      string
	observation *iam.ProviderAccountObservation
}

func (a bearerAuth) Prepare() (string, http.Header, error) {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	if a.apiKey != "" {
		h.Set("Authorization", "Bearer "+a.apiKey)
	}
	return a.base, h, nil
}

func (a bearerAuth) PrepareObserved() (
	string, http.Header, *iam.ProviderAccountObservation, error,
) {
	baseURL, headers, err := a.Prepare()
	return baseURL, headers, a.observation, err
}

func (bearerAuth) CanRefresh() bool { return false }
func (bearerAuth) Refresh() error   { return nil }

// copilotAuth resolves a GitHub Copilot session (OAuth-derived) plus the editor
// identity headers that unlock the endpoint. The base URL comes from the
// session; a 401 is retried after forcing a new session token.
type copilotAuth struct {
	providerID string
	principal  *config.Principal
}

func (a copilotAuth) Prepare() (string, http.Header, error) {
	baseURL, headers, _, err := a.PrepareObserved()
	return baseURL, headers, err
}

func (a copilotAuth) PrepareObserved() (
	string, http.Header, *iam.ProviderAccountObservation, error,
) {
	if err := copilotauth.AssertProxyAllowed(); err != nil {
		return "", nil, nil, invocation("github_copilot: " + err.Error())
	}
	s, observation, err := a.session(false)
	if err != nil {
		return "", nil, observation, invocation("github_copilot: " + err.Error())
	}
	cfg := config.Get()
	h := http.Header{}
	h.Set("Authorization", "Bearer "+s.Token)
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "application/json")
	h.Set("Copilot-Integration-Id", cfg.GithubCopilotIntegrationID)
	h.Set("Editor-Version", cfg.GithubCopilotEditorVersion)
	h.Set("Editor-Plugin-Version", "llm-gateway/0.1")
	h.Set("OpenAI-Intent", "conversation-panel")
	h.Set("User-Agent", "GithubCopilotChat/llm-gateway")
	return s.ChatBaseURL, h, observation, nil
}

func (copilotAuth) CanRefresh() bool { return true }

func (a copilotAuth) Refresh() error {
	_, _, err := a.session(true)
	return err
}

func (a copilotAuth) session(
	force bool,
) (*copilotauth.Session, *iam.ProviderAccountObservation, error) {
	if a.principal == nil || a.principal.PrincipalID == "" {
		session, err := copilotauth.GetSession(force)
		return session, nil, err
	}
	oauth, observation, ok, err := iam.ResolveProviderOAuthCredentialSecretWithObservation(
		a.principal, a.providerID,
	)
	if err != nil {
		return nil, observation, invocation("github_copilot: load BYOC credential: " + err.Error())
	}
	if !ok {
		return nil, observation, &ConfigError{Msg: "github_copilot: this principal has no active Copilot credential"}
	}
	session, err := copilotauth.GetSessionForOAuth(oauth, force)
	if err != nil {
		return nil, observation, invocation("github_copilot: " + err.Error())
	}
	return session, observation, nil
}
