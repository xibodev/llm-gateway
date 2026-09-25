package providers

import (
	"context"
	"errors"
	"strings"

	"github.com/xibodev/llm-provider-auth/tokenstore"
	core "github.com/xibodev/llmgw-core"
)

// oauthStore is the credential store behind the core Runtime's instances and
// behind the gateway's own Coordinators, which refresh outside the Runtime:
// the gateway's connections, opened per operation, plus what the gateway's
// paths did around a refresh that tokenstore.Coordinator does not. The
// operation's oauthCall says which vertical it serves, and so which rules
// apply. The store pins a Codex account (oauthCall.pin), drops the caches a
// write invalidates, reports failures as each path reported them, and
// records the failures of a refresh that follows an upstream 401.
type oauthStore struct {
	runtime *Runtime
	open    func() (core.CredentialStore, error)
}

var _ core.CredentialStore = oauthStore{}

// reportedError is a failure as the gateway reports it. errors.As finds
// report, a gateway error, first; errors.Is and tokenstore.IsTerminal still
// see cause, so the Coordinator decides on the failure it knows.
type reportedError struct{ report, cause error }

func (e *reportedError) Error() string   { return e.report.Error() }
func (e *reportedError) Unwrap() []error { return []error{e.report, e.cause} }

// Resolve implements core.CredentialStore. Codex and Antigravity serve only a
// caller's own connection, so a caller without one fails here.
func (s oauthStore) Resolve(ctx context.Context, caller core.Caller, instance string) (string, error) {
	call := oauthCallFrom(ctx)
	store, err := s.open()
	if err != nil {
		return "", call.loadFailure(err)
	}
	key, err := store.Resolve(ctx, caller, instance)
	if errors.Is(err, core.ErrNoCredential) {
		return "", call.noConnection()
	}
	if err != nil {
		return "", call.loadFailure(err)
	}
	return key, nil
}

// Load implements tokenstore.Store.
func (s oauthStore) Load(ctx context.Context, key string) (tokenstore.Record, error) {
	call := oauthCallFrom(ctx)
	store, err := s.open()
	var record tokenstore.Record
	if err == nil {
		record, err = store.Load(ctx, key)
	}
	if err == nil {
		err = call.pin(record)
	}
	if err != nil {
		err = call.loadFailure(err)
		call.reject(err)
		return tokenstore.Record{}, err
	}
	if strings.TrimSpace(record.RefreshToken) == "" {
		// The Coordinator refuses to refresh such a record without saying
		// so to the Runtime. If it does not serve the record instead, this
		// is why the refresh failed.
		call.reject(call.noRefreshToken())
	}
	credentialCollectorFrom(ctx).observe(key, record.Revision)
	return record, nil
}

// Save implements tokenstore.Store. Nothing in the gateway saves through it.
func (s oauthStore) Save(ctx context.Context, key string, record tokenstore.Record) (tokenstore.Record, error) {
	store, err := s.open()
	if err != nil {
		return tokenstore.Record{}, err
	}
	return store.Save(ctx, key, record)
}

// ReplaceIfCurrent implements tokenstore.Store. A conflict stays a conflict,
// because the Coordinator reloads on one.
func (s oauthStore) ReplaceIfCurrent(ctx context.Context, key, revision string, record tokenstore.Record) (tokenstore.Record, error) {
	call := oauthCallFrom(ctx)
	store, err := s.open()
	var stored tokenstore.Record
	if err == nil {
		stored, err = store.ReplaceIfCurrent(ctx, key, revision, record)
	}
	if errors.Is(err, tokenstore.ErrConflict) || isContextError(err) {
		return tokenstore.Record{}, err
	}
	if err != nil {
		err = call.storeFailure(err)
		call.reject(err)
		return tokenstore.Record{}, err
	}
	call.forget(s.runtime)
	credentialCollectorFrom(ctx).observe(key, stored.Revision)
	return stored, nil
}

// RevokeIfCurrent implements tokenstore.Store. The Coordinator revokes a
// credential whose refresh grant the provider rejected for good.
func (s oauthStore) RevokeIfCurrent(ctx context.Context, key, revision string) error {
	store, err := s.open()
	if err == nil {
		err = store.RevokeIfCurrent(ctx, key, revision)
	}
	if err == nil {
		oauthCallFrom(ctx).forget(s.runtime)
	}
	return err
}

// Lease implements tokenstore.Store.
func (s oauthStore) Lease(ctx context.Context, key string) (func(), error) {
	store, err := s.open()
	if err != nil {
		return nil, err
	}
	return store.Lease(ctx, key)
}

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
