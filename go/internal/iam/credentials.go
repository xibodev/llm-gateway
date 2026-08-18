package iam

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"time"

	"llmgw/internal/config"
)

type ProviderCredentialInfo struct {
	ID          string `json:"id"`
	PrincipalID string `json:"principal_id"`
	ProviderID  string `json:"provider_id"`
	Kind        string `json:"credential_kind"`
	Status      string `json:"status"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
	LastUsedAt  int64  `json:"last_used_at,omitempty"`
}

// PutProviderCredential encrypts a provider credential for one human principal.
// Service principals cannot own Copilot subscriptions.
func PutProviderCredential(
	principalID, providerID, kind, secret string,
) (ProviderCredentialInfo, error) {
	principal, ok, err := PrincipalByID(principalID)
	if err != nil {
		return ProviderCredentialInfo{}, err
	}
	if !ok {
		return ProviderCredentialInfo{}, fmt.Errorf("principal not found")
	}
	if principal.Kind != "human" {
		return ProviderCredentialInfo{}, fmt.Errorf("provider credentials require a human principal")
	}
	if principal.Status != "active" {
		return ProviderCredentialInfo{}, fmt.Errorf("principal is disabled")
	}
	return putProviderCredential(principal.ID, providerID, kind, secret)
}

// PutGatewayProviderCredential encrypts a provider credential owned by the
// gateway. Workload access still requires an explicit project/provider/kind
// binding; creating this credential never grants access by itself.
func PutGatewayProviderCredential(
	providerID, kind, secret string,
) (ProviderCredentialInfo, error) {
	principal, err := EnsurePrincipalBySubject(
		"system", "system:gateway-provider-credentials", "", "Gateway provider credentials",
	)
	if err != nil {
		return ProviderCredentialInfo{}, err
	}
	if principal.Status != "active" {
		return ProviderCredentialInfo{}, fmt.Errorf("gateway credential owner is disabled")
	}
	if principal.Kind != "system" {
		return ProviderCredentialInfo{}, fmt.Errorf("gateway credential owner must be a system principal")
	}
	return putProviderCredential(principal.ID, providerID, kind, secret)
}

func putProviderCredential(
	principalID, providerID, kind, secret string,
) (ProviderCredentialInfo, error) {
	providerID = strings.TrimSpace(providerID)
	kind = strings.TrimSpace(kind)
	secret = strings.TrimSpace(secret)
	if providerID == "" || kind == "" || secret == "" {
		return ProviderCredentialInfo{}, fmt.Errorf("provider, credential kind and secret are required")
	}
	key, err := credentialKey()
	if err != nil {
		return ProviderCredentialInfo{}, err
	}
	ciphertext, nonce, err := encryptCredential(
		key, []byte(secret), []byte(principalID+"|"+providerID),
	)
	if err != nil {
		return ProviderCredentialInfo{}, err
	}
	db, err := DB()
	if err != nil {
		return ProviderCredentialInfo{}, err
	}
	now := time.Now().Unix()
	id, err := newID("cred")
	if err != nil {
		return ProviderCredentialInfo{}, err
	}
	tx, err := db.Begin()
	if err != nil {
		return ProviderCredentialInfo{}, err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`
INSERT INTO provider_credentials(
    id,principal_id,provider_id,credential_kind,ciphertext,nonce,key_version,
    status,created_at,updated_at
) VALUES(?,?,?,?,?,?,1,'active',?,?)
ON CONFLICT(principal_id,provider_id) DO UPDATE SET
    credential_kind=excluded.credential_kind,ciphertext=excluded.ciphertext,
    nonce=excluded.nonce,key_version=excluded.key_version,status='active',
    updated_at=excluded.updated_at`,
		id, principalID, providerID, kind, ciphertext, nonce, now, now,
	)
	if err != nil {
		return ProviderCredentialInfo{}, err
	}
	if err := deleteProviderChecksForCredentialOwnerTx(
		tx, principalID, providerID,
	); err != nil {
		return ProviderCredentialInfo{}, err
	}
	if err := tx.Commit(); err != nil {
		return ProviderCredentialInfo{}, err
	}
	return providerCredentialInfo(principalID, providerID)
}

func ProviderCredentialSecret(
	principalID, providerID string,
) (string, bool, error) {
	secret, _, ok, err := ProviderCredentialSecretWithKind(principalID, providerID)
	return secret, ok, err
}

func ProviderCredentialSecretWithKind(
	principalID, providerID string,
) (string, string, bool, error) {
	db, err := DB()
	if err != nil {
		return "", "", false, err
	}
	var ciphertext, nonce []byte
	var status, kind string
	err = db.QueryRow(`
SELECT c.ciphertext,c.nonce,c.status,c.credential_kind
FROM provider_credentials c
JOIN principals p ON p.id=c.principal_id
WHERE c.principal_id=? AND c.provider_id=? AND p.status='active'`,
		principalID, providerID,
	).Scan(&ciphertext, &nonce, &status, &kind)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", "", false, nil
		}
		return "", "", false, err
	}
	if status != "active" {
		return "", "", false, nil
	}
	key, err := credentialKey()
	if err != nil {
		return "", "", false, err
	}
	plaintext, err := decryptCredential(
		key, ciphertext, nonce, []byte(principalID+"|"+providerID),
	)
	if err != nil {
		return "", "", false, fmt.Errorf("decrypt provider credential: %w", err)
	}
	_, _ = db.Exec(`
UPDATE provider_credentials SET last_used_at=? WHERE principal_id=? AND provider_id=?`,
		time.Now().Unix(), principalID, providerID,
	)
	return string(plaintext), kind, true, nil
}

func ListProviderCredentials(principalID string) ([]ProviderCredentialInfo, error) {
	db, err := DB()
	if err != nil {
		return nil, err
	}
	query := `
SELECT id,principal_id,provider_id,credential_kind,status,created_at,updated_at,
       COALESCE(last_used_at,0)
FROM provider_credentials`
	args := []any{}
	if principalID != "" {
		query += " WHERE principal_id=?"
		args = append(args, principalID)
	}
	query += " ORDER BY principal_id,provider_id"
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProviderCredentialInfo{}
	for rows.Next() {
		var info ProviderCredentialInfo
		if err := rows.Scan(
			&info.ID, &info.PrincipalID, &info.ProviderID, &info.Kind, &info.Status,
			&info.CreatedAt, &info.UpdatedAt, &info.LastUsedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, info)
	}
	return out, rows.Err()
}

func RevokeProviderCredential(principalID, providerID string) error {
	db, err := DB()
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`
UPDATE provider_credentials SET status='revoked',updated_at=?
WHERE principal_id=? AND provider_id=?`,
		time.Now().Unix(), principalID, providerID,
	)
	if err != nil {
		return err
	}
	if err := requireAffected(res, "provider credential"); err != nil {
		return err
	}
	if err := deleteProviderChecksForCredentialOwnerTx(
		tx, principalID, providerID,
	); err != nil {
		return err
	}
	return tx.Commit()
}

func SetProviderCredentialStatus(id, status string) error {
	if status != "active" && status != "disabled" && status != "revoked" {
		return fmt.Errorf("invalid provider credential status %q", status)
	}
	db, err := DB()
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var principalID, providerID string
	if err := tx.QueryRow(`
SELECT principal_id,provider_id FROM provider_credentials WHERE id=?`,
		strings.TrimSpace(id),
	).Scan(&principalID, &providerID); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("provider credential not found")
		}
		return err
	}
	res, err := tx.Exec(`
UPDATE provider_credentials SET status=?,updated_at=? WHERE id=?`,
		status, time.Now().Unix(), strings.TrimSpace(id),
	)
	if err != nil {
		return err
	}
	if err := requireAffected(res, "provider credential"); err != nil {
		return err
	}
	if err := deleteProviderChecksForCredentialOwnerTx(
		tx, principalID, providerID,
	); err != nil {
		return err
	}
	return tx.Commit()
}

func deleteProviderChecksForCredentialOwnerTx(
	tx *sql.Tx, principalID, providerID string,
) error {
	var principalKind string
	if err := tx.QueryRow(
		"SELECT kind FROM principals WHERE id=?", principalID,
	).Scan(&principalKind); err != nil {
		return err
	}
	if principalKind == "system" {
		return invalidateProviderChecksTx(tx, providerID, "", true)
	}
	return invalidateProviderChecksTx(tx, providerID, principalID, false)
}

func ProviderCredentialByID(id string) (ProviderCredentialInfo, bool, error) {
	db, err := DB()
	if err != nil {
		return ProviderCredentialInfo{}, false, err
	}
	var info ProviderCredentialInfo
	err = db.QueryRow(`
SELECT id,principal_id,provider_id,credential_kind,status,created_at,updated_at,
       COALESCE(last_used_at,0)
FROM provider_credentials WHERE id=?`, strings.TrimSpace(id)).Scan(
		&info.ID, &info.PrincipalID, &info.ProviderID, &info.Kind, &info.Status,
		&info.CreatedAt, &info.UpdatedAt, &info.LastUsedAt,
	)
	if err == sql.ErrNoRows {
		return ProviderCredentialInfo{}, false, nil
	}
	return info, err == nil, err
}

func providerCredentialInfo(
	principalID, providerID string,
) (ProviderCredentialInfo, error) {
	db, err := DB()
	if err != nil {
		return ProviderCredentialInfo{}, err
	}
	var info ProviderCredentialInfo
	err = db.QueryRow(`
SELECT id,principal_id,provider_id,credential_kind,status,created_at,updated_at,
       COALESCE(last_used_at,0)
FROM provider_credentials WHERE principal_id=? AND provider_id=?`,
		principalID, providerID,
	).Scan(
		&info.ID, &info.PrincipalID, &info.ProviderID, &info.Kind, &info.Status,
		&info.CreatedAt, &info.UpdatedAt, &info.LastUsedAt,
	)
	return info, err
}

func credentialKey() ([]byte, error) {
	raw := strings.TrimSpace(config.Get().CredentialEncryptionKey)
	if raw == "" {
		return nil, fmt.Errorf("LLMGW_CREDENTIAL_ENCRYPTION_KEY is required for BYOC credentials")
	}
	decoders := []func(string) ([]byte, error){
		base64.RawURLEncoding.DecodeString,
		base64.URLEncoding.DecodeString,
		base64.RawStdEncoding.DecodeString,
		base64.StdEncoding.DecodeString,
		hex.DecodeString,
	}
	for _, decode := range decoders {
		if key, err := decode(raw); err == nil && len(key) == 32 {
			return key, nil
		}
	}
	return nil, fmt.Errorf("LLMGW_CREDENTIAL_ENCRYPTION_KEY must encode exactly 32 bytes")
}

func encryptCredential(
	key, plaintext, additionalData []byte,
) (ciphertext, nonce []byte, err error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, nil, err
	}
	return gcm.Seal(nil, nonce, plaintext, additionalData), nonce, nil
}

func decryptCredential(
	key, ciphertext, nonce, additionalData []byte,
) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, nonce, ciphertext, additionalData)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
