package iam

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"time"
)

// DefaultCredentialLeaseTTL is how long a refresh lease stays held when its
// holder never releases it. It exceeds the tokenstore Coordinator's default
// 90-second lease wait, which in turn must exceed the longest refresh, so a
// live holder is never overtaken mid-refresh.
const DefaultCredentialLeaseTTL = 2 * time.Minute

const (
	credentialLeasePollMin = 10 * time.Millisecond
	credentialLeasePollMax = 200 * time.Millisecond
)

// CredentialStoreOptions configures a CredentialStore.
type CredentialStoreOptions struct {
	// LeaseTTL bounds how long an abandoned refresh lease blocks others. It
	// must exceed the longest refresh. Zero uses DefaultCredentialLeaseTTL.
	LeaseTTL time.Duration
	// Now returns the current time. Nil uses time.Now. Lease expiry is
	// computed from it, so separately opened stores must agree on it.
	Now func() time.Time
	// Precedence selects the resolution order of a provider instance, which
	// the gateway chooses by provider type. Nil resolves every instance with
	// ConnectionPrecedence.
	Precedence func(instance string) CredentialPrecedence
}

// CredentialStore exposes the encrypted provider connections as the
// llm-provider-auth token store and the llmgw-core credential store.
//
// A key is the provider connection ID the gateway already reports as
// ProviderAccountObservation.ConnectionID, and a revision is that
// connection's credential_revision as a decimal string. Credentials outside
// provider_connections use the reserved, read-only key namespaces
// ProviderCredentialKeyPrefix and ConfiguredCredentialKeyPrefix.
//
// The store keeps no transaction open across a refresh. Refreshes of one key
// are serialized by a lease row with a random holder and an expiry, so every
// process sharing the database is excluded.
type CredentialStore struct {
	db         *sql.DB
	leaseTTL   time.Duration
	now        func() time.Time
	precedence func(instance string) CredentialPrecedence
}

// NewCredentialStore returns a store over db, normally the handle DB returns.
func NewCredentialStore(db *sql.DB, options CredentialStoreOptions) (*CredentialStore, error) {
	if db == nil {
		return nil, errors.New("credential store requires a database")
	}
	if options.LeaseTTL < 0 {
		return nil, errors.New("credential lease TTL must not be negative")
	}
	store := &CredentialStore{
		db: db, leaseTTL: options.LeaseTTL, now: options.Now, precedence: options.Precedence,
	}
	if store.leaseTTL == 0 {
		store.leaseTTL = DefaultCredentialLeaseTTL
	}
	if store.now == nil {
		store.now = time.Now
	}
	return store, nil
}

// checkCredentialRequest mirrors tokenstore.Memory's argument checks, so
// callers see the same errors from either store.
func checkCredentialRequest(ctx context.Context, key string) error {
	if strings.TrimSpace(key) == "" {
		return errors.New("tokenstore: key is required")
	}
	return ctx.Err()
}

// Lease implements tokenstore.Store. It polls with a short, jittered backoff
// until it holds the lease or ctx ends; the held lease outlives ctx, because
// tokenstore.Coordinator cancels the wait context as soon as Lease returns.
func (s *CredentialStore) Lease(ctx context.Context, key string) (func(), error) {
	if err := checkCredentialRequest(ctx, key); err != nil {
		return nil, err
	}
	holder, err := newID("lease")
	if err != nil {
		return nil, err
	}
	delay := credentialLeasePollMin
	for {
		acquired, err := s.tryLease(ctx, key, holder)
		if err != nil {
			// The insert may have landed before the failure was reported.
			// The holder is unique to this call, so deleting it is safe.
			s.releaseLease(key, holder)
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, fmt.Errorf("acquire credential lease: %w", err)
		}
		if acquired {
			var once sync.Once
			return func() { once.Do(func() { s.releaseLease(key, holder) }) }, nil
		}
		wait := delay/2 + rand.N(delay/2+1)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
		delay = min(delay*2, credentialLeasePollMax)
	}
}

// tryLease inserts the lease when it is absent and takes it over only when
// it has expired. One conditional write decides, so two instances can never
// both succeed.
func (s *CredentialStore) tryLease(ctx context.Context, key, holder string) (bool, error) {
	now := s.now()
	result, err := s.db.ExecContext(ctx, `
INSERT INTO credential_leases(key,holder,expires_at_ns) VALUES(?,?,?)
ON CONFLICT(key) DO UPDATE SET holder=excluded.holder,expires_at_ns=excluded.expires_at_ns
WHERE credential_leases.expires_at_ns<=?`,
		key, holder, now.Add(s.leaseTTL).UnixNano(), now.UnixNano(),
	)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected == 1, err
}

// releaseLease deletes only this holder's row: a holder whose lease expired
// and was taken over must not release its successor. A failed delete leaves
// the lease to expire, which is why release cannot report an error.
func (s *CredentialStore) releaseLease(key, holder string) {
	_, _ = s.db.ExecContext(context.Background(),
		"DELETE FROM credential_leases WHERE key=? AND holder=?", key, holder)
}
