package providers

import (
	"context"
	"errors"
	"fmt"
	"time"

	"llmgw/internal/diagnostics"

	"github.com/xibodev/llm-provider-auth/tokenstore"
	"github.com/xibodev/llmgw-core/oauthflow"
)

// Metadata keys of the records the OAuth drivers return, for what a sign-in
// reports beyond tokenstore.Record's fields. They are the keys under which
// iam's credential store carries the same envelope fields.
const (
	OAuthMetadataAccountLabel = "account_label"
	OAuthMetadataProjectID    = "project_id"
	OAuthMetadataProfile      = "oauth_profile"
	OAuthMetadataClientID     = "oauth_client_id"
)

// Start parameters that carry a manual flow's OAuth client to its driver.
const (
	oauthParamClientID     = "client_id"
	oauthParamClientSecret = "client_secret"
	oauthParamClientMode   = "client_mode"
	oauthParamRedirectURI  = "redirect_uri"
)

// OAuthParams returns config as the start parameters of a manual flow,
// which its driver reads its OAuth client from.
func (config ProviderAuthManualConfig) OAuthParams() map[string]string {
	return map[string]string{
		oauthParamClientID: config.ClientID, oauthParamClientSecret: config.ClientSecret,
		oauthParamClientMode: config.ClientMode, oauthParamRedirectURI: config.RedirectURI,
	}
}

// OAuthManualConfig reads the OAuth client a manual flow started with back
// from its start parameters.
func OAuthManualConfig(params map[string]string) ProviderAuthManualConfig {
	return ProviderAuthManualConfig{
		ClientID: params[oauthParamClientID], ClientSecret: params[oauthParamClientSecret],
		ClientMode: params[oauthParamClientMode], RedirectURI: params[oauthParamRedirectURI],
	}
}

// OAuthDriver returns the driver that runs method for providerID, an
// instance whose registry auth adapter is adapterID. These are the flows the
// console offers: device authorization for Copilot and Codex, the Codex CLI's
// browser sign-in, whose loopback redirect the owner pastes back, and
// Antigravity's gateway callback and consumer_manual profile. Each calls
// llm-provider-auth exactly as the adapter's own flow methods do.
func (rt *Runtime) OAuthDriver(adapterID, providerID string, method oauthflow.Method) (oauthflow.Driver, error) {
	switch {
	case adapterID == "github_copilot" && method == oauthflow.MethodDevice:
		return copilotDeviceDriver{copilot: rt.copilot}, nil
	case adapterID == "openai_codex" && method == oauthflow.MethodDevice:
		return codexDeviceDriver{}, nil
	case adapterID == "openai_codex" && method == oauthflow.MethodManual:
		return codexBrowserDriver{}, nil
	case adapterID == "google_antigravity" && method == oauthflow.MethodBrowser:
		return antigravityBrowserDriver{adapter: googleAntigravityAuthAdapter{providerID: providerID}}, nil
	case adapterID == "google_antigravity" && method == oauthflow.MethodManual:
		return antigravityManualDriver{config: antigravityManualConfig}, nil
	}
	return nil, fmt.Errorf("provider %q does not offer %s authorization", providerID, method)
}

// OAuthPollNote records what the provider answered the one poll a device
// driver made. oauthflow reports only the outcome, and the console shows the
// owner the provider's own status and explanation, so a route that polls puts
// a note in the context and reads it afterwards. A poll that never reached
// the provider, because the interval had not elapsed or the flow was gone,
// leaves the note empty.
type OAuthPollNote struct {
	// Polled reports that the provider was asked.
	Polled bool
	// Status is the provider's answer: pending, slow_down, authorized,
	// denied, expired or error.
	Status string
	// Detail is the provider's sanitized explanation, if any.
	Detail string
}

type oauthPollNoteKey struct{}

// WithOAuthPollNote returns ctx carrying an empty note for one poll.
func WithOAuthPollNote(ctx context.Context) (context.Context, *OAuthPollNote) {
	note := &OAuthPollNote{}
	return context.WithValue(ctx, oauthPollNoteKey{}, note), note
}

// devicePollResult notes a provider's answer to one device poll and maps it
// onto oauthflow's. The providers answer nothing else; any other status, a
// transport error above all, is transient and keeps the flow pending.
func devicePollResult(ctx context.Context, status, detail string, record tokenstore.Record) (oauthflow.PollResult, error) {
	detail = diagnostics.SanitizeTextLimit(detail, maxProviderAuthDiagnosticChars)
	if note, ok := ctx.Value(oauthPollNoteKey{}).(*OAuthPollNote); ok {
		*note = OAuthPollNote{Polled: true, Status: status, Detail: detail}
	}
	switch status {
	case "pending":
		return oauthflow.PollResult{Status: oauthflow.PollPending}, nil
	case "slow_down":
		return oauthflow.PollResult{Status: oauthflow.PollSlowDown}, nil
	case "authorized":
		return oauthflow.PollResult{Status: oauthflow.PollApproved, Record: record}, nil
	case "denied":
		return oauthflow.PollResult{Status: oauthflow.PollDenied}, nil
	case "expired":
		return oauthflow.PollResult{Status: oauthflow.PollExpired}, nil
	}
	if detail == "" {
		detail = "device authorization poll failed"
	}
	return oauthflow.PollResult{}, errors.New(detail)
}

// seconds converts a provider's whole seconds.
func seconds(value int) time.Duration { return time.Duration(value) * time.Second }

// errIncompleteDeviceStart is what the console has always said of a device
// start that lacks a code the owner or the poll needs.
var errIncompleteDeviceStart = errors.New("official OAuth device flow returned incomplete authorization data")
