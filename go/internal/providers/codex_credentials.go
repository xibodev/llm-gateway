package providers

import (
	"context"
	"errors"

	codexauth "github.com/xibodev/llm-provider-auth/codex"
	"github.com/xibodev/llm-provider-auth/tokenstore"
	core "github.com/xibodev/llmgw-core"
	coreproviders "github.com/xibodev/llmgw-core/providers"
)

func errNoCodexConnection() error {
	return &ConfigError{Msg: "openai_codex: this principal has no active private Codex connection"}
}

func errNoCodexRefreshToken() error {
	return &ConfigError{Msg: "openai_codex: no refresh token is available"}
}

func errCodexAccountChanged() error {
	return invocation("openai_codex: account changed during refresh")
}

// codexRefresh returns how a Coordinator refreshes a Codex credential: core's
// Codex refresh, with the endpoints of this Runtime and clientID as the client
// of a grant that names none, plus the rules of the Codex path it lacks. The
// failures it returns keep what the Coordinator decides on, such as whether
// the provider rejected the grant for good, and it records them on the
// operation's oauthCall.
func (rt *Runtime) codexRefresh(clientID string) tokenstore.RefreshFunc {
	endpoints := rt.codexEndpoints.get().withDefaults()
	refresh := coreproviders.NewCodexRefresh(codexauth.Config{
		ClientID: clientID, Endpoints: endpoints.OAuth, HTTPClient: endpoints.HTTPClient,
	})
	return func(ctx context.Context, current tokenstore.Record) (tokenstore.Record, error) {
		refreshed, err := refreshCodexRecord(ctx, refresh, current)
		if err != nil {
			oauthCallFrom(ctx).reject(err)
		}
		return refreshed, err
	}
}

func refreshCodexRecord(ctx context.Context, refresh tokenstore.RefreshFunc, current tokenstore.Record) (tokenstore.Record, error) {
	// Core refreshes a record that names no client with the configured
	// one. The gateway refuses: such a connection predates stored client
	// profiles, and its grant may belong to another client, so it must be
	// authorized again.
	if _, err := codexGrantClient(
		current.Metadata[core.CredentialMetadataOAuthProfile], current.Metadata[core.CredentialMetadataOAuthClientID],
	); err != nil {
		return tokenstore.Record{}, &ConfigError{Msg: "openai_codex: " + err.Error()}
	}
	refreshed, err := refresh(ctx, current)
	if err != nil {
		return tokenstore.Record{}, &reportedError{report: codexRefreshInvocationError(err), cause: err}
	}
	// The Coordinator refuses another account too, but its refusal never
	// reaches the caller of a replay.
	if codexAccountMismatch(current.AccountID, refreshed.AccountID) {
		return tokenstore.Record{}, errCodexAccountChanged()
	}
	return refreshed, nil
}

// codexLoadFailure reports a failure to read the caller's connection as the
// Codex path did. A missing connection keeps tokenstore.ErrNotFound as its
// cause, which the Store contract promises.
func codexLoadFailure(err error) error {
	var gatewayErr *InvocationError
	if errors.As(err, &gatewayErr) || isContextError(err) {
		return err
	}
	if errors.Is(err, tokenstore.ErrNotFound) {
		return &reportedError{report: errNoCodexConnection(), cause: err}
	}
	return &reportedError{report: invocation("openai_codex: load private OAuth connection: " + err.Error()), cause: err}
}
