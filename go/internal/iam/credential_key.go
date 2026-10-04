package iam

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"llmgw/internal/config"
)

// ErrCredentialKeyMismatch reports a configured credential encryption key
// that cannot open what the database was encrypted with.
var ErrCredentialKeyMismatch = errors.New(
	"the configured credential encryption key does not match the key this database was encrypted with",
)

// credentialKeyCheckName names, in control_metadata, a value sealed with the
// key the database's credentials are encrypted with. AES-GCM authenticates,
// so opening it proves a key at startup; without it a wrong key surfaced only
// when a request first needed a credential.
const credentialKeyCheckName = "credential_encryption_key_check"

var (
	credentialKeyCheckPlaintext = []byte("llmgw credential encryption key check")
	credentialKeyCheckAAD       = []byte("credential-key-check|v1")
)

// States InspectCredentialKey reports.
const (
	CredentialKeyMatches       = "matches"
	CredentialKeyMismatch      = "does not match"
	CredentialKeyNotRecorded   = "not recorded"
	CredentialKeyNotConfigured = "not configured"
	CredentialKeyInvalid       = "invalid"
)

func sealCredentialKeyCheck(key []byte) (string, error) {
	ciphertext, nonce, err := encryptCredential(key, credentialKeyCheckPlaintext, credentialKeyCheckAAD)
	if err != nil {
		return "", err
	}
	return "v1:" + base64.StdEncoding.EncodeToString(nonce) + ":" +
		base64.StdEncoding.EncodeToString(ciphertext), nil
}

func credentialKeyCheckOpens(check string, key []byte) bool {
	version, sealed, _ := strings.Cut(check, ":")
	encodedNonce, encodedCiphertext, found := strings.Cut(sealed, ":")
	if version != "v1" || !found {
		return false
	}
	nonce, nonceErr := base64.StdEncoding.DecodeString(encodedNonce)
	ciphertext, ciphertextErr := base64.StdEncoding.DecodeString(encodedCiphertext)
	if nonceErr != nil || ciphertextErr != nil {
		return false
	}
	plaintext, err := decryptCredential(key, ciphertext, nonce, credentialKeyCheckAAD)
	return err == nil && bytes.Equal(plaintext, credentialKeyCheckPlaintext)
}

func readCredentialKeyCheck(db interface {
	QueryRow(string, ...any) *sql.Row
}) (string, bool, error) {
	var check string
	err := db.QueryRow(
		"SELECT value FROM control_metadata WHERE key=?", credentialKeyCheckName,
	).Scan(&check)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	return check, err == nil, err
}

func writeCredentialKeyCheck(db interface {
	Exec(string, ...any) (sql.Result, error)
}, key []byte) error {
	check, err := sealCredentialKeyCheck(key)
	if err != nil {
		return err
	}
	_, err = db.Exec(`
INSERT INTO control_metadata(key,value,updated_at) VALUES(?,?,?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`,
		credentialKeyCheckName, check, time.Now().Unix(),
	)
	return err
}

// encryptedCredential is one stored value sealed with the credential
// encryption key, with the associated data it is bound to.
type encryptedCredential struct {
	ciphertext, nonce []byte
	additionalData    []byte
}

func (credential encryptedCredential) opens(key []byte) bool {
	_, err := decryptCredential(key, credential.ciphertext, credential.nonce, credential.additionalData)
	return err == nil
}

// encryptedCredentialColumns lists every column sealed with the credential
// encryption key. Each binds its value to the identity of its row, as the
// code that writes it does, so a value cannot be moved to another row.
var encryptedCredentialColumns = []struct {
	table, query string
	scan         func(rowScanner) (encryptedCredential, error)
}{
	{
		table: "provider_connections",
		query: `
SELECT id,principal_id,provider_id,connection_name,aad_version,ciphertext,nonce
FROM provider_connections ORDER BY id`,
		scan: func(row rowScanner) (encryptedCredential, error) {
			var id, principalID, providerID, name string
			var aadVersion int
			var ciphertext, nonce []byte
			err := row.Scan(&id, &principalID, &providerID, &name, &aadVersion, &ciphertext, &nonce)
			return encryptedCredential{
				ciphertext: ciphertext, nonce: nonce,
				additionalData: connectionAAD(principalID, providerID, name, aadVersion),
			}, err
		},
	},
	{
		table: "provider_credentials",
		query: `
SELECT id,principal_id,provider_id,ciphertext,nonce
FROM provider_credentials ORDER BY id`,
		scan: func(row rowScanner) (encryptedCredential, error) {
			var id, principalID, providerID string
			var ciphertext, nonce []byte
			err := row.Scan(&id, &principalID, &providerID, &ciphertext, &nonce)
			return encryptedCredential{
				ciphertext: ciphertext, nonce: nonce,
				additionalData: []byte(principalID + "|" + providerID),
			}, err
		},
	},
	{
		// A key issued without a configured encryption key keeps only its
		// hash, and has nothing sealed.
		table: "api_keys",
		query: `
SELECT id,project_id,principal_id,secret_ciphertext,secret_nonce
FROM api_keys
WHERE length(COALESCE(secret_ciphertext,''))>0 AND length(COALESCE(secret_nonce,''))>0
ORDER BY id`,
		scan: func(row rowScanner) (encryptedCredential, error) {
			var id, projectID, principalID string
			var ciphertext, nonce []byte
			err := row.Scan(&id, &projectID, &principalID, &ciphertext, &nonce)
			return encryptedCredential{
				ciphertext: ciphertext, nonce: nonce,
				additionalData: apiKeyAdditionalData(id, projectID, principalID),
			}, err
		},
	},
	{
		table: "oauth_client_profiles",
		query: `
SELECT provider_id,profile,ciphertext,nonce
FROM oauth_client_profiles ORDER BY provider_id,profile`,
		scan: func(row rowScanner) (encryptedCredential, error) {
			var providerID, profile string
			var ciphertext, nonce []byte
			err := row.Scan(&providerID, &profile, &ciphertext, &nonce)
			return encryptedCredential{
				ciphertext: ciphertext, nonce: nonce,
				additionalData: oauthClientProfileAAD(providerID, profile),
			}, err
		},
	},
}

// encryptedCredentials reads every value sealed with the credential
// encryption key.
func encryptedCredentials(db interface {
	Query(string, ...any) (*sql.Rows, error)
}) ([]encryptedCredential, error) {
	stored := []encryptedCredential{}
	for _, column := range encryptedCredentialColumns {
		rows, err := db.Query(column.query)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", column.table, err)
		}
		for rows.Next() {
			credential, err := column.scan(rows)
			if err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("read %s: %w", column.table, err)
			}
			stored = append(stored, credential)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return nil, fmt.Errorf("read %s: %w", column.table, err)
		}
	}
	return stored, nil
}

// verifyCredentialKey proves the configured credential encryption key before
// anything is decrypted with it. A database whose check value another key
// sealed is refused, unless every credential it stores opens with this key,
// as when the key changed before anything was encrypted. A database an
// earlier release wrote has no check value: it gets one once the key opens a
// credential it stores, or at once when it stores none.
func verifyCredentialKey(db *sql.DB) error {
	if strings.TrimSpace(config.Get().CredentialEncryptionKey) == "" {
		return nil
	}
	key, err := credentialKey()
	if err != nil {
		return err
	}
	check, recorded, err := readCredentialKeyCheck(db)
	if err != nil {
		return fmt.Errorf("read the credential key check: %w", err)
	}
	if recorded && credentialKeyCheckOpens(check, key) {
		return nil
	}
	stored, err := encryptedCredentials(db)
	if err != nil {
		return err
	}
	opened := false
	for _, credential := range stored {
		if credential.opens(key) {
			opened = true
			if !recorded {
				break
			}
		} else if recorded {
			return ErrCredentialKeyMismatch
		}
	}
	if !recorded && !opened && len(stored) > 0 {
		// Without a check value there is no record of the key these were
		// encrypted with, so the gateway starts as earlier releases did.
		log.Printf("warning: the configured credential encryption key opens none of the "+
			"stored credentials (%d); requests that need one fail until the key they were "+
			"encrypted with is configured", len(stored))
		return nil
	}
	if err := writeCredentialKeyCheck(db, key); err != nil {
		return fmt.Errorf("record the credential key check: %w", err)
	}
	return nil
}

// InspectCredentialKey reports how the configured credential encryption key
// relates to the check value db holds, without changing db. It reads only
// that value, so it suits a database the gateway does not serve, such as
// the one in a backup.
func InspectCredentialKey(db *sql.DB) (string, error) {
	if strings.TrimSpace(config.Get().CredentialEncryptionKey) == "" {
		return CredentialKeyNotConfigured, nil
	}
	key, err := credentialKey()
	if err != nil {
		return CredentialKeyInvalid, nil
	}
	// Databases older than control_metadata hold no check value.
	var tables int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='control_metadata'",
	).Scan(&tables); err != nil {
		return "", err
	}
	if tables == 0 {
		return CredentialKeyNotRecorded, nil
	}
	check, recorded, err := readCredentialKeyCheck(db)
	switch {
	case err != nil:
		return "", err
	case !recorded:
		return CredentialKeyNotRecorded, nil
	case credentialKeyCheckOpens(check, key):
		return CredentialKeyMatches, nil
	}
	return CredentialKeyMismatch, nil
}
