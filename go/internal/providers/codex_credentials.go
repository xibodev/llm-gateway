package providers

import (
	"context"
	"errors"
	"strings"

	codexauth "github.com/xibodev/llm-provider-auth/codex"
	"github.com/xibodev/llm-provider-auth/tokenstore"
	core "github.com/xibodev/llmgw-core"
	coreproviders "github.com/xibodev/llmgw-core/providers"
)

// codexStore is the credential store behind the core Runtime's Codex
// instances and the gateway's Codex catalog and console refresh: the
// gateway's connections, opened per operation, plus what the Codex path did
// around a refresh that tokenstore.Coordinator does not. It pins the
// account (codexCall.pin), drops the caches a write invalidates, reports
// failures as the Codex path reported them, and records the failures of a
// refresh that follows an upstream 401 on the operation's codexCall.
type codexStore struct {
	runtime *Runtime
	open    func() (core.CredentialStore, error)
}

var _ core.CredentialStore = codexStore{}

// reportedError is a failure as the gateway reports it. errors.As finds
// report, a gateway error, first; errors.Is and tokenstore.IsTerminal still
// see cause, so the Coordinator decides on the failure it knows.
type reportedError struct{ report, cause error }

func (e *reportedError) Error() string   { return e.report.Error() }
func (e *reportedError) Unwrap() []error { return []error{e.report, e.cause} }

func errNoCodexConnection() error {
	return &ConfigError{Msg: "openai_codex: this principal has no active private Codex connection"}
}

func errNoCodexRefreshToken() error {
	return &ConfigError{Msg: "openai_codex: no refresh token is available"}
}

func errCodexAccountChanged() error {
	return invocation("openai_codex: account changed during refresh")
}

// Resolve implements core.CredentialStore. Codex serves only a caller's own
// connection, so a caller without one fails here. The failure must not match
// core.ErrNoCredential, on which the Runtime sends the request without a
// credential.
func (s codexStore) Resolve(ctx context.Context, caller core.Caller, instance string) (string, error) {
	store, err := s.open()
	if err != nil {
		return "", codexLoadFailure(err)
	}
	key, err := store.Resolve(ctx, caller, instance)
	if errors.Is(err, core.ErrNoCredential) {
		return "", errNoCodexConnection()
	}
	if err != nil {
		return "", codexLoadFailure(err)
	}
	return key, nil
}

// Load implements tokenstore.Store.
func (s codexStore) Load(ctx context.Context, key string) (tokenstore.Record, error) {
	call := codexCallFrom(ctx)
	store, err := s.open()
	var record tokenstore.Record
	if err == nil {
		record, err = store.Load(ctx, key)
	}
	if err == nil {
		err = call.pin(record)
	}
	if err != nil {
		err = codexLoadFailure(err)
		call.reject(err)
		return tokenstore.Record{}, err
	}
	if strings.TrimSpace(record.RefreshToken) == "" {
		// The Coordinator refuses to refresh such a record without saying
		// so to the Runtime. If it does not serve the record instead, this
		// is why the refresh failed.
		call.reject(errNoCodexRefreshToken())
	}
	credentialCollectorFrom(ctx).observe(key, record.Revision)
	return record, nil
}

// Save implements tokenstore.Store. Nothing in the gateway saves through it.
func (s codexStore) Save(ctx context.Context, key string, record tokenstore.Record) (tokenstore.Record, error) {
	store, err := s.open()
	if err != nil {
		return tokenstore.Record{}, err
	}
	return store.Save(ctx, key, record)
}

// ReplaceIfCurrent implements tokenstore.Store. A conflict stays a conflict,
// because the Coordinator reloads on one.
func (s codexStore) ReplaceIfCurrent(ctx context.Context, key, revision string, record tokenstore.Record) (tokenstore.Record, error) {
	call := codexCallFrom(ctx)
	store, err := s.open()
	var stored tokenstore.Record
	if err == nil {
		stored, err = store.ReplaceIfCurrent(ctx, key, revision, record)
	}
	if errors.Is(err, tokenstore.ErrConflict) || isContextError(err) {
		return tokenstore.Record{}, err
	}
	if err != nil {
		err = &reportedError{report: invocation("openai_codex: store refreshed connection: " + err.Error()), cause: err}
		call.reject(err)
		return tokenstore.Record{}, err
	}
	call.forget(s.runtime)
	credentialCollectorFrom(ctx).observe(key, stored.Revision)
	return stored, nil
}

// RevokeIfCurrent implements tokenstore.Store. The Coordinator revokes a
// credential whose refresh grant the provider rejected for good.
func (s codexStore) RevokeIfCurrent(ctx context.Context, key, revision string) error {
	store, err := s.open()
	if err == nil {
		err = store.RevokeIfCurrent(ctx, key, revision)
	}
	if err == nil {
		codexCallFrom(ctx).forget(s.runtime)
	}
	return err
}

// Lease implements tokenstore.Store.
func (s codexStore) Lease(ctx context.Context, key string) (func(), error) {
	store, err := s.open()
	if err != nil {
		return nil, err
	}
	return store.Lease(ctx, key)
}

// codexRefresh returns how a Coordinator refreshes a Codex credential: core's
// Codex refresh, with the endpoints of this Runtime and clientID as the client
// of a grant that names none, plus the rules of the Codex path it lacks. The
// failures it returns keep what the Coordinator decides on, such as whether
// the provider rejected the grant for good, and it records them on the
// operation's codexCall.
func (rt *Runtime) codexRefresh(clientID string) tokenstore.RefreshFunc {
	endpoints := rt.codexEndpoints.get().withDefaults()
	refresh := coreproviders.NewCodexRefresh(codexauth.Config{
		ClientID: clientID, Endpoints: endpoints.OAuth, HTTPClient: endpoints.HTTPClient,
	})
	return func(ctx context.Context, current tokenstore.Record) (tokenstore.Record, error) {
		refreshed, err := refreshCodexRecord(ctx, refresh, current)
		if err != nil {
			codexCallFrom(ctx).reject(err)
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

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
