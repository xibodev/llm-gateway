package iam

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"llmgw/internal/config"

	"github.com/xibodev/llm-provider-auth/tokenstore"
	core "github.com/xibodev/llmgw-core"
)

// Reserved key namespaces for credentials that are not provider connections.
// Connection IDs never contain a colon, so the namespaces cannot collide with
// them. Both are read-only: the gateway writes those credentials through its
// own admin and configuration paths, and none of them can be refreshed.
const (
	// ProviderCredentialKeyPrefix names a provider_credentials row by ID: a
	// legacy personal credential or a gateway-owned credential that a project
	// binding shares. The prefix is needed because the v7 migration copied
	// those rows into provider_connections under the same IDs.
	ProviderCredentialKeyPrefix = "provider_credential:"
	// ConfiguredCredentialKeyPrefix names a provider's API key from YAML, an
	// environment reference or secrets.json, by provider ID. Load reads it at
	// call time, so a configuration reload applies on the next request.
	ConfiguredCredentialKeyPrefix = "config:"
)

// readOnlyCredentialRevision is the revision of every read-only record. No
// stored connection has revision 0, and this store never writes those
// records, so there is no write for a revision to fence.
const readOnlyCredentialRevision = "0"

// ErrReadOnlyCredential reports a write to a reserved, read-only key.
var ErrReadOnlyCredential = errors.New("credential is read-only in the credential store")

var _ core.CredentialStore = (*CredentialStore)(nil)

func readOnlyCredentialKey(key string) bool {
	return strings.HasPrefix(key, ProviderCredentialKeyPrefix) ||
		strings.HasPrefix(key, ConfiguredCredentialKeyPrefix)
}

// Load implements tokenstore.Store. A credential that is not active, or whose
// owner is disabled, is not found, exactly as the gateway's resolvers skip it.
func (s *CredentialStore) Load(ctx context.Context, key string) (tokenstore.Record, error) {
	if err := checkCredentialRequest(ctx, key); err != nil {
		return tokenstore.Record{}, err
	}
	if id, ok := strings.CutPrefix(key, ProviderCredentialKeyPrefix); ok {
		return s.loadProviderCredential(ctx, id)
	}
	if providerID, ok := strings.CutPrefix(key, ConfiguredCredentialKeyPrefix); ok {
		return loadConfiguredCredential(providerID)
	}
	return s.loadConnection(ctx, key)
}

// Save implements tokenstore.Store for connection keys; see saveConnection.
func (s *CredentialStore) Save(ctx context.Context, key string, record tokenstore.Record) (tokenstore.Record, error) {
	if err := checkCredentialRequest(ctx, key); err != nil {
		return tokenstore.Record{}, err
	}
	if readOnlyCredentialKey(key) {
		return tokenstore.Record{}, ErrReadOnlyCredential
	}
	return s.saveConnection(ctx, key, record)
}

// ReplaceIfCurrent implements tokenstore.Store. It stores record only while
// revision is the connection's current credential revision.
func (s *CredentialStore) ReplaceIfCurrent(
	ctx context.Context, key, revision string, record tokenstore.Record,
) (tokenstore.Record, error) {
	if err := checkCredentialRequest(ctx, key); err != nil {
		return tokenstore.Record{}, err
	}
	if readOnlyCredentialKey(key) {
		return tokenstore.Record{}, ErrReadOnlyCredential
	}
	return s.replaceConnection(ctx, key, revision, record)
}

// RevokeIfCurrent implements tokenstore.Store. It revokes the connection
// only while revision is current.
func (s *CredentialStore) RevokeIfCurrent(ctx context.Context, key, revision string) error {
	if err := checkCredentialRequest(ctx, key); err != nil {
		return err
	}
	if readOnlyCredentialKey(key) {
		return ErrReadOnlyCredential
	}
	return s.revokeConnection(ctx, key, revision)
}

func (s *CredentialStore) loadProviderCredential(ctx context.Context, id string) (tokenstore.Record, error) {
	var ownerID, providerID, kind string
	var ciphertext, nonce []byte
	err := s.db.QueryRowContext(ctx, `
SELECT c.principal_id,c.provider_id,c.credential_kind,c.ciphertext,c.nonce
FROM provider_credentials c JOIN principals p ON p.id=c.principal_id
WHERE c.id=? AND c.status='active' AND p.status='active'`, id,
	).Scan(&ownerID, &providerID, &kind, &ciphertext, &nonce)
	if err == sql.ErrNoRows {
		return tokenstore.Record{}, tokenstore.ErrNotFound
	}
	if err != nil {
		return tokenstore.Record{}, err
	}
	secret, err := decryptStoredCredential(ownerID, providerID, ciphertext, nonce)
	if err != nil {
		return tokenstore.Record{}, err
	}
	record := legacyCredentialRecord(kind, secret)
	record.Revision = readOnlyCredentialRevision
	return record, nil
}

// configuredCredential is the configured API key of providerID, or "" when
// none is configured, read exactly as the provider factory reads it.
func configuredCredential(providerID string) string {
	if providerID == "" {
		return ""
	}
	return config.ResolveProviderAPIKey(providerID, config.Get().Providers[providerID])
}

func loadConfiguredCredential(providerID string) (tokenstore.Record, error) {
	secret := configuredCredential(providerID)
	if strings.TrimSpace(secret) == "" {
		return tokenstore.Record{}, tokenstore.ErrNotFound
	}
	record := core.APIKeyRecord(secret)
	record.Revision = readOnlyCredentialRevision
	return record, nil
}
