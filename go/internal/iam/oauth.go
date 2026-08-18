package iam

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var ErrOAuthProviderConnectionChanged = errors.New("OAuth provider connection changed")

// OAuthTokenEnvelope keeps provider token material inside the encrypted provider
// connection payload. Its token fields are deliberately excluded from JSON so an
// accidental API serialization cannot expose them.
type OAuthTokenEnvelope struct {
	AccessToken  string `json:"-"`
	RefreshToken string `json:"-"`
	IDToken      string `json:"-"`
	TokenType    string `json:"token_type,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"`
	AccountID    string `json:"account_id,omitempty"`
	AccountLabel string `json:"account_label,omitempty"`
	Status       string `json:"status,omitempty"`
}

type storedOAuthEnvelope struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	IDToken      string `json:"id_token,omitempty"`
	TokenType    string `json:"token_type,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"`
	AccountID    string `json:"account_id,omitempty"`
	AccountLabel string `json:"account_label,omitempty"`
	Status       string `json:"status,omitempty"`
}

// OAuthConnectionCreate supplies a complete official OAuth token envelope for
// one human-owned provider connection. It is never returned from an API.
type OAuthConnectionCreate struct {
	PrincipalID  string
	ProviderID   string
	Name         string
	Kind         string
	Source       string
	MakeDefault  bool
	AccessToken  string
	RefreshToken string
	IDToken      string
	TokenType    string
	ExpiresAt    int64
	AccountID    string
	AccountLabel string
	Status       string
}

// PutOAuthProviderConnection encrypts an official OAuth envelope in the
// existing provider_connections boundary. No schema change is needed and no
// plaintext token is stored in a response or a separate table.
func PutOAuthProviderConnection(input OAuthConnectionCreate) (ProviderConnection, error) {
	input.AccessToken = strings.TrimSpace(input.AccessToken)
	if input.AccessToken == "" {
		return ProviderConnection{}, fmt.Errorf("OAuth access token is required")
	}
	if strings.TrimSpace(input.Kind) == "" {
		input.Kind = "oauth"
	}
	if !isOAuthCredentialKind(input.Kind) {
		return ProviderConnection{}, fmt.Errorf("OAuth connection kind must identify OAuth")
	}
	stored, err := encodeOAuthEnvelope(OAuthTokenEnvelope{
		AccessToken: input.AccessToken, RefreshToken: strings.TrimSpace(input.RefreshToken),
		IDToken: strings.TrimSpace(input.IDToken), TokenType: strings.TrimSpace(input.TokenType),
		ExpiresAt: input.ExpiresAt, AccountID: strings.TrimSpace(input.AccountID),
		AccountLabel: strings.TrimSpace(input.AccountLabel), Status: strings.TrimSpace(input.Status),
	})
	if err != nil {
		return ProviderConnection{}, fmt.Errorf("encode OAuth envelope: %w", err)
	}
	connection, err := PutProviderConnection(ProviderConnectionCreate{
		PrincipalID: input.PrincipalID, ProviderID: input.ProviderID, Name: input.Name,
		Kind: input.Kind, Secret: stored, Source: input.Source,
		PrivateToPrincipal: true, MakeDefault: input.MakeDefault,
		AccountState: ProviderAccountStateSeed{
			TokenExpiresAt: &input.ExpiresAt, AccountLabel: &input.AccountLabel,
			ResetHealth: true,
		},
	})
	if err != nil {
		return ProviderConnection{}, err
	}
	applyOAuthMetadata(&connection, OAuthTokenEnvelope{
		TokenType: input.TokenType, ExpiresAt: input.ExpiresAt, AccountID: input.AccountID,
		AccountLabel: input.AccountLabel, Status: input.Status,
	})
	return connection, nil
}

// ReplaceOAuthProviderConnectionIfCurrent atomically rotates an OAuth envelope
// only when the encrypted row still matches the state the caller refreshed.
// Reauthorization or revocation racing the refresh wins instead of being
// overwritten by stale credentials.
func ReplaceOAuthProviderConnectionIfCurrent(
	expectedConnection ProviderConnection,
	expectedEnvelope OAuthTokenEnvelope,
	input OAuthConnectionCreate,
) (ProviderConnection, error) {
	if expectedConnection.ID == "" ||
		expectedConnection.PrincipalID != strings.TrimSpace(input.PrincipalID) ||
		expectedConnection.ProviderID != strings.TrimSpace(input.ProviderID) {
		return ProviderConnection{}, ErrOAuthProviderConnectionChanged
	}
	input.Name = normalizeConnectionName(input.Name)
	if input.Name != expectedConnection.Name {
		return ProviderConnection{}, ErrOAuthProviderConnectionChanged
	}
	input.AccessToken = strings.TrimSpace(input.AccessToken)
	if input.AccessToken == "" {
		return ProviderConnection{}, fmt.Errorf("OAuth access token is required")
	}
	if strings.TrimSpace(input.Kind) == "" {
		input.Kind = expectedConnection.Kind
	}
	if !isOAuthCredentialKind(input.Kind) {
		return ProviderConnection{}, fmt.Errorf("OAuth connection kind must identify OAuth")
	}
	if strings.TrimSpace(input.Source) == "" {
		input.Source = expectedConnection.Source
	}
	stored, err := encodeOAuthEnvelope(OAuthTokenEnvelope{
		AccessToken: input.AccessToken, RefreshToken: strings.TrimSpace(input.RefreshToken),
		IDToken: strings.TrimSpace(input.IDToken), TokenType: strings.TrimSpace(input.TokenType),
		ExpiresAt: input.ExpiresAt, AccountID: strings.TrimSpace(input.AccountID),
		AccountLabel: strings.TrimSpace(input.AccountLabel), Status: strings.TrimSpace(input.Status),
	})
	if err != nil {
		return ProviderConnection{}, fmt.Errorf("encode OAuth envelope: %w", err)
	}
	key, err := credentialKey()
	if err != nil {
		return ProviderConnection{}, err
	}
	db, err := DB()
	if err != nil {
		return ProviderConnection{}, err
	}
	tx, err := db.Begin()
	if err != nil {
		return ProviderConnection{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var currentCiphertext, currentNonce []byte
	var aadVersion int
	var status string
	err = tx.QueryRow(`
SELECT ciphertext,nonce,aad_version,status
FROM provider_connections WHERE id=? AND principal_id=?`,
		expectedConnection.ID, expectedConnection.PrincipalID,
	).Scan(&currentCiphertext, &currentNonce, &aadVersion, &status)
	if err == sql.ErrNoRows || status != "active" {
		return ProviderConnection{}, ErrOAuthProviderConnectionChanged
	}
	if err != nil {
		return ProviderConnection{}, err
	}
	currentRaw, err := decryptCredential(
		key, currentCiphertext, currentNonce,
		connectionAAD(expectedConnection.PrincipalID, expectedConnection.ProviderID, expectedConnection.Name, aadVersion),
	)
	if err != nil {
		return ProviderConnection{}, err
	}
	currentEnvelope, err := decodeOAuthEnvelope(string(currentRaw))
	if err != nil || !sameOAuthEnvelopeState(expectedEnvelope, currentEnvelope) {
		return ProviderConnection{}, ErrOAuthProviderConnectionChanged
	}
	newCiphertext, newNonce, err := encryptCredential(
		key, []byte(stored),
		connectionAAD(expectedConnection.PrincipalID, expectedConnection.ProviderID, expectedConnection.Name, 2),
	)
	if err != nil {
		return ProviderConnection{}, err
	}
	now := time.Now().Unix()
	result, err := tx.Exec(`
UPDATE provider_connections SET credential_kind=?,source=?,ciphertext=?,nonce=?,
 key_version=1,aad_version=2,updated_at=?
WHERE id=? AND principal_id=? AND status='active' AND ciphertext=? AND nonce=?`,
		input.Kind, input.Source, newCiphertext, newNonce, now,
		expectedConnection.ID, expectedConnection.PrincipalID, currentCiphertext, currentNonce,
	)
	if err != nil {
		return ProviderConnection{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return ProviderConnection{}, err
	}
	if affected != 1 {
		return ProviderConnection{}, ErrOAuthProviderConnectionChanged
	}
	if err := seedProviderAccountStateTx(tx, expectedConnection.ID, ProviderAccountStateSeed{
		TokenExpiresAt: &input.ExpiresAt, AccountLabel: &input.AccountLabel,
		ResetHealth: true, CredentialRotated: true,
	}, now); err != nil {
		return ProviderConnection{}, err
	}
	if err := clearProviderQuotaSnapshotsTx(tx, expectedConnection.ID, now); err != nil {
		return ProviderConnection{}, err
	}
	if err := invalidateProviderChecksTx(
		tx, expectedConnection.ProviderID, expectedConnection.PrincipalID, false,
	); err != nil {
		return ProviderConnection{}, err
	}
	if err := tx.Commit(); err != nil {
		return ProviderConnection{}, err
	}
	connection, ok, err := ProviderConnectionByName(
		expectedConnection.PrincipalID, expectedConnection.ProviderID, expectedConnection.Name,
	)
	if err != nil {
		return ProviderConnection{}, err
	}
	if !ok {
		return ProviderConnection{}, ErrOAuthProviderConnectionChanged
	}
	applyOAuthMetadata(&connection, OAuthTokenEnvelope{
		TokenType: input.TokenType, ExpiresAt: input.ExpiresAt, AccountID: input.AccountID,
		AccountLabel: input.AccountLabel, Status: input.Status,
	})
	return connection, nil
}

// RevokeOAuthProviderConnectionIfCurrent revokes only the OAuth state the
// caller observed. A concurrent reauthorization cannot be revoked by a stale
// refresh failure.
func RevokeOAuthProviderConnectionIfCurrent(
	expectedConnection ProviderConnection,
	expectedEnvelope OAuthTokenEnvelope,
) (bool, error) {
	db, err := DB()
	if err != nil {
		return false, err
	}
	key, err := credentialKey()
	if err != nil {
		return false, err
	}
	tx, err := db.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var providerID string
	var wasDefault int
	var currentCiphertext, currentNonce []byte
	var aadVersion int
	err = tx.QueryRow(`
SELECT provider_id,is_default,ciphertext,nonce,aad_version
FROM provider_connections
WHERE id=? AND principal_id=? AND status='active'`,
		expectedConnection.ID, expectedConnection.PrincipalID,
	).Scan(&providerID, &wasDefault, &currentCiphertext, &currentNonce, &aadVersion)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	currentRaw, err := decryptCredential(
		key, currentCiphertext, currentNonce,
		connectionAAD(expectedConnection.PrincipalID, expectedConnection.ProviderID, expectedConnection.Name, aadVersion),
	)
	if err != nil {
		return false, err
	}
	currentEnvelope, err := decodeOAuthEnvelope(string(currentRaw))
	if err != nil || !sameOAuthEnvelopeState(expectedEnvelope, currentEnvelope) {
		return false, nil
	}
	now := time.Now().Unix()
	result, err := tx.Exec(`
UPDATE provider_connections SET status='revoked',is_default=0,updated_at=?
WHERE id=? AND principal_id=? AND status='active' AND ciphertext=? AND nonce=?`,
		now, expectedConnection.ID, expectedConnection.PrincipalID,
		currentCiphertext, currentNonce,
	)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if affected != 1 {
		return false, nil
	}
	if err := clearProviderQuotaSnapshotsTx(tx, expectedConnection.ID, now); err != nil {
		return false, err
	}
	if wasDefault != 0 {
		if _, err := tx.Exec(`
UPDATE provider_connections SET is_default=1,updated_at=?
WHERE id=(
    SELECT id FROM provider_connections
    WHERE principal_id=? AND provider_id=? AND status='active'
    ORDER BY updated_at DESC,connection_name LIMIT 1
)`,
			now, expectedConnection.PrincipalID, providerID,
		); err != nil {
			return false, err
		}
	}
	if err := invalidateProviderChecksTx(
		tx, providerID, expectedConnection.PrincipalID, false,
	); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func sameOAuthEnvelopeState(left, right OAuthTokenEnvelope) bool {
	return bytes.Equal([]byte(left.AccessToken), []byte(right.AccessToken)) &&
		bytes.Equal([]byte(left.RefreshToken), []byte(right.RefreshToken)) &&
		bytes.Equal([]byte(left.IDToken), []byte(right.IDToken)) &&
		strings.TrimSpace(left.AccountID) == strings.TrimSpace(right.AccountID)
}

// OAuthProviderConnectionSecret decrypts the internal OAuth envelope for a
// provider adapter. It accepts legacy raw OAuth access tokens so existing
// Copilot connections continue working without a new authorization.
func OAuthProviderConnectionSecret(
	principalID, providerID, name string,
) (OAuthTokenEnvelope, ProviderConnection, bool, error) {
	envelope, connection, _, ok, err := OAuthProviderConnectionSecretWithObservation(
		principalID, providerID, name,
	)
	return envelope, connection, ok, err
}

func OAuthProviderConnectionSecretWithObservation(
	principalID, providerID, name string,
) (OAuthTokenEnvelope, ProviderConnection, ProviderAccountObservation, bool, error) {
	raw, connection, observation, ok, err := ProviderConnectionSecretWithObservation(
		principalID, providerID, name,
	)
	if err != nil || !ok {
		return OAuthTokenEnvelope{}, connection, observation, ok, err
	}
	if !isOAuthCredentialKind(connection.Kind) {
		return OAuthTokenEnvelope{}, connection, observation, false, fmt.Errorf("provider connection is not an OAuth credential")
	}
	envelope, err := decodeOAuthEnvelope(raw)
	if err != nil {
		return OAuthTokenEnvelope{}, connection, observation, false, err
	}
	applyOAuthMetadata(&connection, envelope)
	return envelope, connection, observation, true, nil
}

func encodeOAuthEnvelope(envelope OAuthTokenEnvelope) (string, error) {
	raw, err := json.Marshal(storedOAuthEnvelope{
		AccessToken: envelope.AccessToken, RefreshToken: envelope.RefreshToken, IDToken: envelope.IDToken,
		TokenType: envelope.TokenType, ExpiresAt: envelope.ExpiresAt, AccountID: envelope.AccountID,
		AccountLabel: envelope.AccountLabel, Status: envelope.Status,
	})
	return string(raw), err
}

func decodeOAuthEnvelope(raw string) (OAuthTokenEnvelope, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return OAuthTokenEnvelope{}, fmt.Errorf("OAuth token envelope is empty")
	}
	var stored storedOAuthEnvelope
	if json.Unmarshal([]byte(raw), &stored) == nil && strings.TrimSpace(stored.AccessToken) != "" {
		return OAuthTokenEnvelope{
			AccessToken: stored.AccessToken, RefreshToken: stored.RefreshToken, IDToken: stored.IDToken,
			TokenType: stored.TokenType, ExpiresAt: stored.ExpiresAt, AccountID: stored.AccountID,
			AccountLabel: stored.AccountLabel, Status: stored.Status,
		}, nil
	}
	// v7 copied pre-envelope OAuth credentials as their original raw token. Keep
	// them operational without a schema or forced reauthorization migration.
	return OAuthTokenEnvelope{AccessToken: raw, Status: "active"}, nil
}

func isOAuthCredentialKind(kind string) bool {
	return strings.Contains(strings.ToLower(strings.TrimSpace(kind)), "oauth")
}

func applyOAuthMetadata(connection *ProviderConnection, envelope OAuthTokenEnvelope) {
	connection.OAuthExpiresAt = envelope.ExpiresAt
	connection.OAuthAccountID = envelope.AccountID
	connection.OAuthAccountLabel = envelope.AccountLabel
	status := strings.TrimSpace(envelope.Status)
	if status == "" {
		status = "active"
	}
	if envelope.ExpiresAt > 0 && envelope.ExpiresAt <= time.Now().Unix() {
		status = "expired"
	}
	connection.OAuthStatus = status
}

func populateOAuthConnectionMetadata(connection *ProviderConnection) {
	if connection == nil || !isOAuthCredentialKind(connection.Kind) || connection.Status != "active" {
		return
	}
	raw, _, ok, err := providerConnectionSecret(connection.PrincipalID, connection.ProviderID, connection.Name, false)
	if err != nil || !ok {
		return
	}
	envelope, err := decodeOAuthEnvelope(raw)
	if err != nil {
		return
	}
	applyOAuthMetadata(connection, envelope)
}
