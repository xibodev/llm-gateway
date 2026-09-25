package providers

import (
	"context"
	"slices"

	"github.com/xibodev/llm-provider-auth/tokenstore"
	core "github.com/xibodev/llmgw-core"
)

// connectionStore is the credential store of a type whose facade the
// provider factory builds with the credential it resolves for an instance:
// the IAM store with the factory's precedence, the caller's connection, then
// the system connection, then the configured key, read from the config:
// namespace. When none resolves it reports core.ErrNoCredential, on which the
// core Runtime sends the request without a credential. Its reads mark
// nothing, because the factory marks the credential it builds a facade with,
// which is where those types recorded a use.
type connectionStore struct {
	open func() (core.CredentialStore, error)
	// tokenTypes are the token types of the connections the factory builds
	// the type's facade with. A connection of another kind is refused with
	// refusal, as the factory refuses it, should one replace the connection
	// a facade was built with.
	tokenTypes []string
	refusal    string
}

var _ core.CredentialStore = connectionStore{}

// Resolve implements core.CredentialStore.
func (s connectionStore) Resolve(ctx context.Context, caller core.Caller, instance string) (string, error) {
	store, err := s.open()
	if err != nil {
		return "", err
	}
	return store.Resolve(ctx, caller, instance)
}

// Load implements tokenstore.Store.
func (s connectionStore) Load(ctx context.Context, key string) (tokenstore.Record, error) {
	store, err := s.open()
	if err != nil {
		return tokenstore.Record{}, err
	}
	record, err := store.Load(ctx, key)
	if err == nil && !slices.Contains(s.tokenTypes, record.TokenType) {
		return tokenstore.Record{}, &ConfigError{Msg: s.refusal}
	}
	return record, err
}

// Save, ReplaceIfCurrent, RevokeIfCurrent and Lease implement
// tokenstore.Store. None of the stored kinds refreshes, so only the core
// Runtime's handling of a rejected credential leases one; they pass to the
// IAM store all the same.

func (s connectionStore) Save(ctx context.Context, key string, record tokenstore.Record) (tokenstore.Record, error) {
	store, err := s.open()
	if err != nil {
		return tokenstore.Record{}, err
	}
	return store.Save(ctx, key, record)
}

func (s connectionStore) ReplaceIfCurrent(ctx context.Context, key, revision string, record tokenstore.Record) (tokenstore.Record, error) {
	store, err := s.open()
	if err != nil {
		return tokenstore.Record{}, err
	}
	return store.ReplaceIfCurrent(ctx, key, revision, record)
}

func (s connectionStore) RevokeIfCurrent(ctx context.Context, key, revision string) error {
	store, err := s.open()
	if err != nil {
		return err
	}
	return store.RevokeIfCurrent(ctx, key, revision)
}

func (s connectionStore) Lease(ctx context.Context, key string) (func(), error) {
	store, err := s.open()
	if err != nil {
		return nil, err
	}
	return store.Lease(ctx, key)
}
