package iam

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/xibodev/llm-provider-auth/tokenstore"
)

// saveConnection stores record in the existing connection key names, as a
// new login into that connection does: it reactivates a revoked or disabled
// connection, restores it as default under PutProviderConnection's rule, and
// rotates its revision. It cannot create a connection, because a key does
// not say which principal and provider own it.
func (s *CredentialStore) saveConnection(ctx context.Context, key string, record tokenstore.Record) (tokenstore.Record, error) {
	placement, found, err := s.connectionPlacement(ctx, key)
	if err != nil {
		return tokenstore.Record{}, err
	}
	if !found {
		return tokenstore.Record{}, ErrProviderConnectionNotFound
	}
	ciphertext, nonce, stored, err := sealConnection(placement, record)
	if err != nil {
		return tokenstore.Record{}, err
	}
	now := s.now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return tokenstore.Record{}, err
	}
	defer func() { _ = tx.Rollback() }()
	// Writing first takes SQLite's write lock before anything is read, so
	// the reads below cannot go stale under another process's writer.
	result, err := tx.ExecContext(ctx,
		"UPDATE provider_connections SET updated_at=? WHERE id=? AND credential_kind=?",
		now, key, placement.kind)
	if err != nil {
		return tokenstore.Record{}, err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return tokenstore.Record{}, err
	} else if affected != 1 {
		return tokenstore.Record{}, errConnectionKindChanged
	}
	var principalStatus string
	var existingDefault, activeDefaults int
	if err := tx.QueryRowContext(ctx, `
SELECT p.status,c.is_default,
       (SELECT COUNT(*) FROM provider_connections d
        WHERE d.principal_id=c.principal_id AND d.provider_id=c.provider_id
          AND d.status='active' AND d.is_default=1)
FROM provider_connections c JOIN principals p ON p.id=c.principal_id
WHERE c.id=?`, key).Scan(&principalStatus, &existingDefault, &activeDefaults); err != nil {
		return tokenstore.Record{}, err
	}
	if principalStatus != "active" {
		return tokenstore.Record{}, fmt.Errorf("principal is disabled")
	}
	makeDefault := activeDefaults == 0 || existingDefault != 0
	if makeDefault {
		if _, err := tx.ExecContext(ctx, `
UPDATE provider_connections SET is_default=0,updated_at=?
WHERE principal_id=? AND provider_id=? AND status='active'`,
			now, placement.principalID, placement.providerID); err != nil {
			return tokenstore.Record{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE provider_connections SET ciphertext=?,nonce=?,key_version=1,aad_version=2,
 status='active',is_default=?,updated_at=?
WHERE id=?`, ciphertext, nonce, boolInt(makeDefault), now, key); err != nil {
		return tokenstore.Record{}, err
	}
	seed := accountStateSeed(placement, stored)
	seed.CredentialRotated = true
	return s.finishConnectionWrite(ctx, tx, key, placement, seed, stored, now)
}

// finishConnectionWrite applies the side effects every gateway credential
// write has, then commits and returns the stored record with its revision.
func (s *CredentialStore) finishConnectionWrite(
	ctx context.Context, tx *sql.Tx, key string, placement connectionPlacement,
	seed ProviderAccountStateSeed, stored tokenstore.Record, now int64,
) (tokenstore.Record, error) {
	if err := seedProviderAccountStateTx(tx, key, seed, now); err != nil {
		return tokenstore.Record{}, err
	}
	if err := clearProviderQuotaSnapshotsTx(tx, key, now); err != nil {
		return tokenstore.Record{}, err
	}
	if err := deleteProviderChecksForCredentialOwnerTx(
		tx, placement.principalID, placement.providerID,
	); err != nil {
		return tokenstore.Record{}, err
	}
	revision, err := connectionRevisionTx(ctx, tx, key)
	if err != nil {
		return tokenstore.Record{}, err
	}
	if err := tx.Commit(); err != nil {
		return tokenstore.Record{}, err
	}
	stored.Revision = revision
	return stored, nil
}

// casRevisionTx is the compare-and-swap every conditional write starts with.
// The revision is compared and rotated by one UPDATE, which also takes the
// write lock, so no other writer can slip in between check and write. Only
// an active connection of an active owner is current, matching Load.
func casRevisionTx(ctx context.Context, tx *sql.Tx, key string, expected, now int64) (bool, error) {
	result, err := tx.ExecContext(ctx, `
UPDATE provider_account_state SET credential_revision=credential_revision+1,updated_at=?
WHERE connection_id=? AND credential_revision=? AND EXISTS (
    SELECT 1 FROM provider_connections c JOIN principals p ON p.id=c.principal_id
    WHERE c.id=provider_account_state.connection_id
      AND c.status='active' AND p.status='active')`,
		now, key, expected)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected == 1, err
}

func (s *CredentialStore) replaceConnection(
	ctx context.Context, key, revision string, record tokenstore.Record,
) (tokenstore.Record, error) {
	expected, ok := parseCredentialRevision(revision)
	if !ok {
		return tokenstore.Record{}, tokenstore.ErrConflict
	}
	placement, found, err := s.connectionPlacement(ctx, key)
	if err != nil {
		return tokenstore.Record{}, err
	}
	if !found {
		return tokenstore.Record{}, tokenstore.ErrConflict
	}
	ciphertext, nonce, stored, err := sealConnection(placement, record)
	if err != nil {
		return tokenstore.Record{}, err
	}
	now := s.now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return tokenstore.Record{}, err
	}
	defer func() { _ = tx.Rollback() }()
	current, err := casRevisionTx(ctx, tx, key, expected, now)
	if err != nil {
		return tokenstore.Record{}, err
	}
	if !current {
		return tokenstore.Record{}, tokenstore.ErrConflict
	}
	// Every kind change also rotates the revision, so a matching revision
	// already implies the kind this record was sealed for; the kind guard
	// keeps that true even for a writer outside this store.
	result, err := tx.ExecContext(ctx, `
UPDATE provider_connections SET ciphertext=?,nonce=?,key_version=1,aad_version=2,updated_at=?
WHERE id=? AND status='active' AND credential_kind=?`,
		ciphertext, nonce, now, key, placement.kind)
	if err != nil {
		return tokenstore.Record{}, err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return tokenstore.Record{}, err
	} else if affected != 1 {
		return tokenstore.Record{}, tokenstore.ErrConflict
	}
	return s.finishConnectionWrite(ctx, tx, key, placement, accountStateSeed(placement, stored), stored, now)
}

// revokeConnection revokes the connection the way RevokeProviderConnection
// does, promoting another active connection to default, but only while the
// caller's revision is current.
func (s *CredentialStore) revokeConnection(ctx context.Context, key, revision string) error {
	expected, ok := parseCredentialRevision(revision)
	if !ok {
		// No row carries a negative revision, so the swap below fails and the
		// existence check tells a conflict from a missing credential.
		expected = -1
	}
	now := s.now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	current, err := casRevisionTx(ctx, tx, key, expected, now)
	if err != nil {
		return err
	}
	if !current {
		var active int
		if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM provider_connections c JOIN principals p ON p.id=c.principal_id
WHERE c.id=? AND c.status='active' AND p.status='active'`, key).Scan(&active); err != nil {
			return err
		}
		if active == 0 {
			return tokenstore.ErrNotFound
		}
		return tokenstore.ErrConflict
	}
	var placement connectionPlacement
	var wasDefault int
	if err := tx.QueryRowContext(ctx, `
SELECT principal_id,provider_id,is_default FROM provider_connections WHERE id=?`, key,
	).Scan(&placement.principalID, &placement.providerID, &wasDefault); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE provider_connections SET status='revoked',is_default=0,updated_at=? WHERE id=?`,
		now, key); err != nil {
		return err
	}
	if err := clearProviderQuotaSnapshotsTx(tx, key, now); err != nil {
		return err
	}
	if wasDefault != 0 {
		if _, err := tx.ExecContext(ctx, `
UPDATE provider_connections SET is_default=1,updated_at=?
WHERE id=(
    SELECT id FROM provider_connections
    WHERE principal_id=? AND provider_id=? AND status='active'
    ORDER BY updated_at DESC,connection_name LIMIT 1
)`, now, placement.principalID, placement.providerID); err != nil {
			return err
		}
	}
	if err := deleteProviderChecksForCredentialOwnerTx(
		tx, placement.principalID, placement.providerID,
	); err != nil {
		return err
	}
	return tx.Commit()
}
