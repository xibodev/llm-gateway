package iam

import (
	"bytes"
	"errors"
	"fmt"
)

// RekeyCount is how many values RekeyCredentials re-encrypted in one table.
type RekeyCount struct {
	Table  string
	Values int
}

// RekeyCredentials re-encrypts every value sealed with the current credential
// encryption key under next, binding each to the associated data it had, and
// records next's check value. It works in one transaction, so a value that
// does not decrypt, or a write that fails, leaves the database as it was. It
// refuses a current key that the database's check value does not open.
//
// The caller must keep the gateway from using the database meanwhile: a
// running gateway would go on sealing values with the current key.
func RekeyCredentials(current, next string) ([]RekeyCount, error) {
	currentKey, err := parseCredentialKey("LLMGW_CREDENTIAL_ENCRYPTION_KEY", current)
	if err != nil {
		return nil, err
	}
	nextKey, err := parseCredentialKey("LLMGW_NEW_CREDENTIAL_ENCRYPTION_KEY", next)
	if err != nil {
		return nil, err
	}
	if bytes.Equal(currentKey, nextKey) {
		return nil, errors.New("LLMGW_NEW_CREDENTIAL_ENCRYPTION_KEY is the key already configured")
	}
	db, err := DB()
	if err != nil {
		return nil, err
	}
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	check, recorded, err := readCredentialKeyCheck(tx)
	if err != nil {
		return nil, fmt.Errorf("read the credential key check: %w", err)
	}
	if recorded && !credentialKeyCheckOpens(check, currentKey) {
		return nil, ErrCredentialKeyMismatch
	}
	stored, err := encryptedCredentials(tx)
	if err != nil {
		return nil, err
	}
	// Every value is decrypted before any is written, so a database holding
	// one the current key does not open is refused before it changes.
	for index, credential := range stored {
		plaintext, err := decryptCredential(
			currentKey, credential.ciphertext, credential.nonce, credential.additionalData,
		)
		if err != nil {
			return nil, fmt.Errorf(
				"%s row %s does not decrypt with the configured credential encryption key; nothing was re-encrypted",
				credential.table, credential.id,
			)
		}
		stored[index].ciphertext, stored[index].nonce, err = encryptCredential(
			nextKey, plaintext, credential.additionalData,
		)
		if err != nil {
			return nil, err
		}
	}
	updates := map[string]string{}
	for _, column := range encryptedCredentialColumns {
		updates[column.table] = column.update
	}
	rekeyed := map[string]int{}
	for _, credential := range stored {
		arguments := append([]any{credential.ciphertext, credential.nonce}, credential.rowKey...)
		result, err := tx.Exec(updates[credential.table], arguments...)
		if err != nil {
			return nil, fmt.Errorf("re-encrypt %s row %s: %w", credential.table, credential.id, err)
		}
		if err := requireAffected(result, credential.table+" row "+credential.id); err != nil {
			return nil, err
		}
		rekeyed[credential.table]++
	}
	if err := writeCredentialKeyCheck(tx, nextKey); err != nil {
		return nil, fmt.Errorf("record the credential key check: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	counts := make([]RekeyCount, 0, len(encryptedCredentialColumns))
	for _, column := range encryptedCredentialColumns {
		counts = append(counts, RekeyCount{Table: column.table, Values: rekeyed[column.table]})
	}
	return counts, nil
}
