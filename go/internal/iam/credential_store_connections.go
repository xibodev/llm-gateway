package iam

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"llmgw/internal/config"

	"github.com/xibodev/llm-provider-auth/tokenstore"
)

// connectionPlacement is what a key alone cannot say: who owns the
// connection, for which provider, and how its secret is encoded. None of it
// changes for a connection ID, which is why it may be read outside the write
// transaction that uses it.
type connectionPlacement struct {
	principalID, providerID, name, kind string
}

func (s *CredentialStore) connectionPlacement(ctx context.Context, key string) (connectionPlacement, bool, error) {
	var placement connectionPlacement
	err := s.db.QueryRowContext(ctx, `
SELECT principal_id,provider_id,connection_name,credential_kind
FROM provider_connections WHERE id=?`, key,
	).Scan(&placement.principalID, &placement.providerID, &placement.name, &placement.kind)
	if err == sql.ErrNoRows {
		return connectionPlacement{}, false, nil
	}
	return placement, err == nil, err
}

// parseCredentialRevision accepts only revisions this store issues, in
// canonical form, so matching stays the string equality tokenstore.Memory
// uses.
func parseCredentialRevision(revision string) (int64, bool) {
	value, err := strconv.ParseInt(revision, 10, 64)
	if err != nil || value <= 0 || strconv.FormatInt(value, 10) != revision {
		return 0, false
	}
	return value, true
}

func (s *CredentialStore) loadConnection(ctx context.Context, key string) (tokenstore.Record, error) {
	var placement connectionPlacement
	var ciphertext, nonce []byte
	var aadVersion int
	var revision int64
	// Missing account state reads as revision 0, which no write can match,
	// so such a connection is never overwritten on a guessed revision.
	err := s.db.QueryRowContext(ctx, `
SELECT c.principal_id,c.provider_id,c.connection_name,c.credential_kind,
       c.ciphertext,c.nonce,c.aad_version,COALESCE(s.credential_revision,0)
FROM provider_connections c
JOIN principals p ON p.id=c.principal_id
LEFT JOIN provider_account_state s ON s.connection_id=c.id
WHERE c.id=? AND c.status='active' AND p.status='active'`, key,
	).Scan(
		&placement.principalID, &placement.providerID, &placement.name, &placement.kind,
		&ciphertext, &nonce, &aadVersion, &revision,
	)
	if err == sql.ErrNoRows {
		return tokenstore.Record{}, tokenstore.ErrNotFound
	}
	if err != nil {
		return tokenstore.Record{}, err
	}
	encryptionKey, err := credentialKey()
	if err != nil {
		return tokenstore.Record{}, err
	}
	plaintext, err := decryptCredential(
		encryptionKey, ciphertext, nonce,
		connectionAAD(placement.principalID, placement.providerID, placement.name, aadVersion),
	)
	if err != nil {
		return tokenstore.Record{}, fmt.Errorf("decrypt provider connection: %w", err)
	}
	record, err := connectionRecord(placement.kind, string(plaintext))
	if err != nil {
		return tokenstore.Record{}, err
	}
	if err := s.markRead(ctx, "provider_connections", key); err != nil {
		return tokenstore.Record{}, err
	}
	record.Revision = strconv.FormatInt(revision, 10)
	return record, nil
}

// markRead records a read of the credential id in table as its last use when
// the store marks use. A failed write fails the read, as it does for the
// gateway's resolvers.
func (s *CredentialStore) markRead(ctx context.Context, table, id string) error {
	if !s.markUsed {
		return nil
	}
	_, err := s.db.ExecContext(ctx, "UPDATE "+table+" SET last_used_at=? WHERE id=?", s.now().Unix(), id)
	return err
}

// sealConnection encodes record for placement and encrypts it the way
// PutProviderConnection does. It returns the record as Load will read it.
func sealConnection(
	placement connectionPlacement, record tokenstore.Record,
) (ciphertext, nonce []byte, stored tokenstore.Record, err error) {
	secret, err := connectionSecret(placement.kind, record)
	if err != nil {
		return nil, nil, tokenstore.Record{}, err
	}
	stored, err = connectionRecord(placement.kind, secret)
	if err != nil {
		return nil, nil, tokenstore.Record{}, err
	}
	encryptionKey, err := credentialKey()
	if err != nil {
		if strings.TrimSpace(config.Get().CredentialEncryptionKey) == "" {
			return nil, nil, tokenstore.Record{}, ErrCredentialEncryptionNotConfigured
		}
		return nil, nil, tokenstore.Record{}, err
	}
	ciphertext, nonce, err = encryptCredential(
		encryptionKey, []byte(secret),
		connectionAAD(placement.principalID, placement.providerID, placement.name, 2),
	)
	return ciphertext, nonce, stored, err
}

// accountStateSeed carries the safe OAuth metadata a credential write
// refreshes in provider_account_state, as the gateway's OAuth writers do.
func accountStateSeed(placement connectionPlacement, stored tokenstore.Record) ProviderAccountStateSeed {
	seed := ProviderAccountStateSeed{ResetHealth: true}
	if isOAuthCredentialKind(placement.kind) {
		expiresAt := int64(0)
		if !stored.Expiry.IsZero() {
			expiresAt = stored.Expiry.Unix()
		}
		label := stored.Metadata[CredentialMetadataAccountLabel]
		seed.TokenExpiresAt, seed.AccountLabel = &expiresAt, &label
	}
	return seed
}

func connectionRevisionTx(ctx context.Context, tx *sql.Tx, key string) (string, error) {
	var revision int64
	if err := tx.QueryRowContext(ctx,
		"SELECT credential_revision FROM provider_account_state WHERE connection_id=?", key,
	).Scan(&revision); err != nil {
		return "", err
	}
	return strconv.FormatInt(revision, 10), nil
}

var errConnectionKindChanged = errors.New("provider connection changed credential kind")
