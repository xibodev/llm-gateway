package iam

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/gcpauth"
)

const (
	ConnectionSourceUser      = "user"
	ConnectionSourceAdmin     = "admin"
	ConnectionSourceConfig    = "config"
	ConnectionSourceMigration = "migration"

	defaultConnectionName  = "default"
	systemPrincipalSubject = "llmgw:system"
)

var (
	ErrCredentialEncryptionNotConfigured = errors.New("credential encryption is not configured")
	ErrProviderConnectionNotFound        = errors.New("provider connection not found")
	connectionNamePattern                = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
)

type ProviderConnection struct {
	ID                 string `json:"id"`
	PrincipalID        string `json:"principal_id"`
	PrincipalKind      string `json:"principal_kind,omitempty"`
	ProviderID         string `json:"provider_id"`
	Name               string `json:"connection_name"`
	Kind               string `json:"credential_kind"`
	Source             string `json:"source"`
	PrivateToPrincipal bool   `json:"private_to_principal"`
	IsDefault          bool   `json:"is_default"`
	Status             string `json:"status"`
	CreatedAt          int64  `json:"created_at"`
	UpdatedAt          int64  `json:"updated_at"`
	LastUsedAt         int64  `json:"last_used_at,omitempty"`
	OAuthExpiresAt     int64  `json:"oauth_expires_at,omitempty"`
	OAuthAccountID     string `json:"oauth_account_id,omitempty"`
	OAuthAccountLabel  string `json:"oauth_account_label,omitempty"`
	OAuthStatus        string `json:"oauth_status,omitempty"`
}

type ProviderConnectionCreate struct {
	PrincipalID        string
	ProviderID         string
	Name               string
	Kind               string
	Secret             string
	Source             string
	PrivateToPrincipal bool
	MakeDefault        bool
	AccountState       ProviderAccountStateSeed
}

// PutProviderConnection creates or replaces one named credential connection.
// Human-owned connections are always private; service principals cannot own
// provider credentials.
func PutProviderConnection(input ProviderConnectionCreate) (ProviderConnection, error) {
	input.PrincipalID = strings.TrimSpace(input.PrincipalID)
	input.ProviderID = strings.TrimSpace(input.ProviderID)
	input.Name = normalizeConnectionName(input.Name)
	input.Kind = strings.TrimSpace(input.Kind)
	input.Secret = strings.TrimSpace(input.Secret)
	input.Source = strings.TrimSpace(input.Source)
	if input.Source == "" {
		input.Source = ConnectionSourceUser
	}
	if input.PrincipalID == "" || input.ProviderID == "" || input.Kind == "" || input.Secret == "" {
		return ProviderConnection{}, fmt.Errorf("principal, provider, credential kind and secret are required")
	}
	if !connectionNamePattern.MatchString(input.Name) {
		return ProviderConnection{}, fmt.Errorf("connection name must use lowercase letters, numbers, '.', '_' or '-'")
	}
	switch input.Source {
	case ConnectionSourceUser, ConnectionSourceAdmin, ConnectionSourceConfig, ConnectionSourceMigration:
	default:
		return ProviderConnection{}, fmt.Errorf("invalid connection source %q", input.Source)
	}

	principal, ok, err := PrincipalByID(input.PrincipalID)
	if err != nil {
		return ProviderConnection{}, err
	}
	if !ok {
		return ProviderConnection{}, fmt.Errorf("principal not found")
	}
	if principal.Status != "active" {
		return ProviderConnection{}, fmt.Errorf("principal is disabled")
	}
	switch principal.Kind {
	case "human":
		input.PrivateToPrincipal = true
	case "system":
		if input.PrivateToPrincipal {
			return ProviderConnection{}, fmt.Errorf("system connections cannot be private to a human principal")
		}
	case "service":
		return ProviderConnection{}, fmt.Errorf("service principals cannot own provider connections")
	default:
		return ProviderConnection{}, fmt.Errorf("unsupported principal kind %q", principal.Kind)
	}
	if strings.Contains(strings.ToLower(input.Kind), "oauth") && principal.Kind != "human" {
		return ProviderConnection{}, fmt.Errorf("OAuth subscriptions require a human principal")
	}
	// Parse a service-account key before storing it. A malformed key that is
	// merely encrypted would surface much later, as a failure on the request
	// path, where the cause is far harder to see.
	if strings.EqualFold(input.Kind, gcpauth.CredentialKind) {
		if _, err := gcpauth.Parse([]byte(input.Secret)); err != nil {
			return ProviderConnection{}, err
		}
	}

	key, err := credentialKey()
	if err != nil {
		if strings.TrimSpace(config.Get().CredentialEncryptionKey) == "" {
			return ProviderConnection{}, ErrCredentialEncryptionNotConfigured
		}
		return ProviderConnection{}, err
	}
	aad := connectionAAD(input.PrincipalID, input.ProviderID, input.Name, 2)
	ciphertext, nonce, err := encryptCredential(key, []byte(input.Secret), aad)
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

	var activeDefaults int
	if err := tx.QueryRow(`
SELECT COUNT(*) FROM provider_connections
WHERE principal_id=? AND provider_id=? AND status='active' AND is_default=1`,
		input.PrincipalID, input.ProviderID,
	).Scan(&activeDefaults); err != nil {
		return ProviderConnection{}, err
	}
	var existingDefault int
	err = tx.QueryRow(`
SELECT is_default FROM provider_connections
WHERE principal_id=? AND provider_id=? AND connection_name=?`,
		input.PrincipalID, input.ProviderID, input.Name,
	).Scan(&existingDefault)
	if err != nil && err != sql.ErrNoRows {
		return ProviderConnection{}, err
	}
	makeDefault := input.MakeDefault || activeDefaults == 0 || existingDefault != 0
	if makeDefault {
		if _, err := tx.Exec(`
UPDATE provider_connections SET is_default=0,updated_at=?
WHERE principal_id=? AND provider_id=? AND status='active'`,
			time.Now().Unix(), input.PrincipalID, input.ProviderID,
		); err != nil {
			return ProviderConnection{}, err
		}
	}

	id, err := newID("conn")
	if err != nil {
		return ProviderConnection{}, err
	}
	now := time.Now().Unix()
	_, err = tx.Exec(`
INSERT INTO provider_connections(
    id,principal_id,provider_id,connection_name,credential_kind,source,
    private_to_principal,is_default,ciphertext,nonce,key_version,aad_version,
    status,created_at,updated_at
) VALUES(?,?,?,?,?,?,?,?,?,?,1,2,'active',?,?)
ON CONFLICT(principal_id,provider_id,connection_name) DO UPDATE SET
    credential_kind=excluded.credential_kind,source=excluded.source,
    private_to_principal=excluded.private_to_principal,
    is_default=excluded.is_default,ciphertext=excluded.ciphertext,
    nonce=excluded.nonce,key_version=excluded.key_version,
    aad_version=excluded.aad_version,status='active',updated_at=excluded.updated_at`,
		id, input.PrincipalID, input.ProviderID, input.Name, input.Kind, input.Source,
		boolInt(input.PrivateToPrincipal), boolInt(makeDefault), ciphertext, nonce, now, now,
	)
	if err != nil {
		return ProviderConnection{}, err
	}
	var persistedConnectionID string
	if err := tx.QueryRow(`
SELECT id FROM provider_connections
WHERE principal_id=? AND provider_id=? AND connection_name=?`,
		input.PrincipalID, input.ProviderID, input.Name,
	).Scan(&persistedConnectionID); err != nil {
		return ProviderConnection{}, err
	}
	accountState := input.AccountState
	accountState.CredentialRotated = true
	accountState.ResetHealth = true
	if err := seedProviderAccountStateTx(tx, persistedConnectionID, accountState, now); err != nil {
		return ProviderConnection{}, err
	}
	if err := clearProviderQuotaSnapshotsTx(tx, persistedConnectionID, now); err != nil {
		return ProviderConnection{}, err
	}
	if err := deleteProviderChecksForCredentialOwnerTx(
		tx, input.PrincipalID, input.ProviderID,
	); err != nil {
		return ProviderConnection{}, err
	}
	if err := tx.Commit(); err != nil {
		return ProviderConnection{}, err
	}
	connection, ok, err := ProviderConnectionByName(input.PrincipalID, input.ProviderID, input.Name)
	if err != nil {
		return ProviderConnection{}, err
	}
	if !ok {
		return ProviderConnection{}, fmt.Errorf("provider connection was not persisted")
	}
	return connection, nil
}

func ListProviderConnections(principalID, providerID string) ([]ProviderConnection, error) {
	db, err := DB()
	if err != nil {
		return nil, err
	}
	query := `
SELECT c.id,c.principal_id,p.kind,c.provider_id,c.connection_name,
       c.credential_kind,c.source,c.private_to_principal,c.is_default,c.status,
       c.created_at,c.updated_at,COALESCE(c.last_used_at,0)
FROM provider_connections c
JOIN principals p ON p.id=c.principal_id`
	conditions := []string{}
	args := []any{}
	if principalID = strings.TrimSpace(principalID); principalID != "" {
		conditions = append(conditions, "c.principal_id=?")
		args = append(args, principalID)
	}
	if providerID = strings.TrimSpace(providerID); providerID != "" {
		conditions = append(conditions, "c.provider_id=?")
		args = append(args, providerID)
	}
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	query += " ORDER BY c.principal_id,c.provider_id,c.is_default DESC,c.connection_name"
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProviderConnection{}
	for rows.Next() {
		connection, err := scanProviderConnection(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, connection)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		populateOAuthConnectionMetadata(&out[i])
	}
	return out, nil
}

func ProviderConnectionByName(
	principalID, providerID, name string,
) (ProviderConnection, bool, error) {
	db, err := DB()
	if err != nil {
		return ProviderConnection{}, false, err
	}
	row := db.QueryRow(`
SELECT c.id,c.principal_id,p.kind,c.provider_id,c.connection_name,
       c.credential_kind,c.source,c.private_to_principal,c.is_default,c.status,
       c.created_at,c.updated_at,COALESCE(c.last_used_at,0)
FROM provider_connections c
JOIN principals p ON p.id=c.principal_id
WHERE c.principal_id=? AND c.provider_id=? AND c.connection_name=?`,
		strings.TrimSpace(principalID), strings.TrimSpace(providerID), normalizeConnectionName(name),
	)
	connection, err := scanProviderConnection(row)
	if err == sql.ErrNoRows {
		return ProviderConnection{}, false, nil
	}
	return connection, err == nil, err
}

func ProviderConnectionSecret(
	principalID, providerID, name string,
) (string, ProviderConnection, bool, error) {
	secret, connection, _, ok, err := ProviderConnectionSecretWithObservation(
		principalID, providerID, name,
	)
	return secret, connection, ok, err
}

func ProviderConnectionSecretWithObservation(
	principalID, providerID, name string,
) (string, ProviderConnection, ProviderAccountObservation, bool, error) {
	return providerConnectionSecretObserved(principalID, providerID, name, true)
}

// providerConnectionSecret reads a decrypted connection. Metadata reads pass
// markUsed=false so listing connections never changes operational usage data.
func providerConnectionSecret(
	principalID, providerID, name string, markUsed bool,
) (string, ProviderConnection, bool, error) {
	secret, connection, _, ok, err := providerConnectionSecretObserved(
		principalID, providerID, name, markUsed,
	)
	return secret, connection, ok, err
}

func providerConnectionSecretObserved(
	principalID, providerID, name string, markUsed bool,
) (string, ProviderConnection, ProviderAccountObservation, bool, error) {
	db, err := DB()
	if err != nil {
		return "", ProviderConnection{}, ProviderAccountObservation{}, false, err
	}
	principalID = strings.TrimSpace(principalID)
	providerID = strings.TrimSpace(providerID)
	name = strings.TrimSpace(name)
	query := `
SELECT c.id,c.principal_id,p.kind,c.provider_id,c.connection_name,
       c.credential_kind,c.source,c.private_to_principal,c.is_default,c.status,
       c.created_at,c.updated_at,COALESCE(c.last_used_at,0),
       c.ciphertext,c.nonce,c.aad_version,COALESCE(s.credential_revision,0)
FROM provider_connections c
JOIN principals p ON p.id=c.principal_id
LEFT JOIN provider_account_state s ON s.connection_id=c.id
WHERE c.principal_id=? AND c.provider_id=? AND c.status='active'
  AND p.status='active'`
	args := []any{principalID, providerID}
	if name != "" {
		query += " AND c.connection_name=?"
		args = append(args, normalizeConnectionName(name))
	}
	query += " ORDER BY c.is_default DESC,c.updated_at DESC,c.connection_name LIMIT 1"
	var connection ProviderConnection
	var private, isDefault int
	var ciphertext, nonce []byte
	var aadVersion int
	var observation ProviderAccountObservation
	err = db.QueryRow(query, args...).Scan(
		&connection.ID, &connection.PrincipalID, &connection.PrincipalKind,
		&connection.ProviderID, &connection.Name, &connection.Kind, &connection.Source,
		&private, &isDefault, &connection.Status, &connection.CreatedAt,
		&connection.UpdatedAt, &connection.LastUsedAt, &ciphertext, &nonce, &aadVersion,
		&observation.CredentialRevision,
	)
	if err == sql.ErrNoRows {
		return "", ProviderConnection{}, ProviderAccountObservation{}, false, nil
	}
	if err != nil {
		return "", ProviderConnection{}, ProviderAccountObservation{}, false, err
	}
	observation.ConnectionID = connection.ID
	connection.PrivateToPrincipal = private != 0
	connection.IsDefault = isDefault != 0
	key, err := credentialKey()
	if err != nil {
		return "", ProviderConnection{}, ProviderAccountObservation{}, false, err
	}
	plaintext, err := decryptCredential(
		key, ciphertext, nonce,
		connectionAAD(connection.PrincipalID, connection.ProviderID, connection.Name, aadVersion),
	)
	if err != nil {
		return "", ProviderConnection{}, ProviderAccountObservation{}, false, fmt.Errorf("decrypt provider connection: %w", err)
	}
	if markUsed {
		now := time.Now().Unix()
		if _, err := db.Exec(
			"UPDATE provider_connections SET last_used_at=? WHERE id=?", now, connection.ID,
		); err != nil {
			return "", ProviderConnection{}, ProviderAccountObservation{}, false, err
		}
		connection.LastUsedAt = now
	}
	return string(plaintext), connection, observation, true, nil
}

func RevokeProviderConnection(principalID, connectionID string) error {
	db, err := DB()
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var providerID string
	var wasDefault int
	err = tx.QueryRow(`
SELECT provider_id,is_default FROM provider_connections
WHERE id=? AND principal_id=? AND status!='revoked'`,
		strings.TrimSpace(connectionID), strings.TrimSpace(principalID),
	).Scan(&providerID, &wasDefault)
	if err == sql.ErrNoRows {
		return ErrProviderConnectionNotFound
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`
UPDATE provider_connections SET status='revoked',is_default=0,updated_at=?
WHERE id=? AND principal_id=?`,
		time.Now().Unix(), strings.TrimSpace(connectionID), strings.TrimSpace(principalID),
	); err != nil {
		return err
	}
	if err := clearProviderQuotaSnapshotsTx(tx, strings.TrimSpace(connectionID), time.Now().Unix()); err != nil {
		return err
	}
	if wasDefault != 0 {
		if _, err := tx.Exec(`
UPDATE provider_connections SET is_default=1,updated_at=?
WHERE id=(
    SELECT id FROM provider_connections
    WHERE principal_id=? AND provider_id=? AND status='active'
    ORDER BY updated_at DESC,connection_name LIMIT 1
)`,
			time.Now().Unix(), strings.TrimSpace(principalID), providerID,
		); err != nil {
			return err
		}
	}
	if err := deleteProviderChecksForCredentialOwnerTx(
		tx, strings.TrimSpace(principalID), providerID,
	); err != nil {
		return err
	}
	return tx.Commit()
}

func RevokeDefaultProviderConnection(principalID, providerID string) error {
	connection, ok, err := defaultProviderConnection(principalID, providerID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrProviderConnectionNotFound
	}
	return RevokeProviderConnection(principalID, connection.ID)
}

func EnsureSystemPrincipal() (Principal, error) {
	return EnsurePrincipalBySubject("system", systemPrincipalSubject, "", "Gateway system")
}

// SeedSystemProviderConnectionsFromConfig copies configured system credentials
// into the encrypted store only when no connection exists. Once seeded, the DB
// is authoritative and later config reloads never overwrite it.
func SeedSystemProviderConnectionsFromConfig() (int, error) {
	if strings.TrimSpace(config.Get().CredentialEncryptionKey) == "" {
		return 0, nil
	}
	principal, err := EnsureSystemPrincipal()
	if err != nil {
		return 0, err
	}
	seeded := 0
	for providerID, providerConfig := range config.Get().Providers {
		secret := strings.TrimSpace(config.ResolveProviderAPIKey(providerID, providerConfig))
		if secret == "" {
			continue
		}
		exists, err := anyProviderConnection(principal.ID, providerID)
		if err != nil {
			return seeded, err
		}
		if exists {
			continue
		}
		if _, err := PutProviderConnection(ProviderConnectionCreate{
			PrincipalID: principal.ID, ProviderID: providerID, Name: defaultConnectionName,
			Kind: "api_key", Secret: secret, Source: ConnectionSourceConfig,
			MakeDefault: true,
		}); err != nil {
			return seeded, err
		}
		seeded++
	}
	return seeded, nil
}

// PutSystemProviderConnection is the explicit operator rotation path. Unlike
// startup config seeding, it intentionally replaces the current system default.
func PutSystemProviderConnection(providerID, kind, secret string) (bool, error) {
	if strings.TrimSpace(config.Get().CredentialEncryptionKey) == "" {
		return false, nil
	}
	principal, err := EnsureSystemPrincipal()
	if err != nil {
		return false, err
	}
	_, err = PutProviderConnection(ProviderConnectionCreate{
		PrincipalID: principal.ID, ProviderID: providerID, Name: defaultConnectionName,
		Kind: kind, Secret: secret, Source: ConnectionSourceAdmin, MakeDefault: true,
	})
	return err == nil, err
}

func SystemProviderConnectionSecret(
	providerID string,
) (string, ProviderConnection, bool, error) {
	principal, ok, err := PrincipalBySubject(systemPrincipalSubject)
	if err != nil || !ok {
		return "", ProviderConnection{}, false, err
	}
	return ProviderConnectionSecret(principal.ID, providerID, "")
}

func SystemProviderConnectionExists(providerID string) (bool, error) {
	principal, ok, err := PrincipalBySubject(systemPrincipalSubject)
	if err != nil || !ok {
		return false, err
	}
	connection, found, err := defaultProviderConnection(principal.ID, providerID)
	return found && connection.Status == "active", err
}

func RevokeSystemProviderConnection(providerID string) error {
	principal, ok, err := PrincipalBySubject(systemPrincipalSubject)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	connections, err := ListProviderConnections(principal.ID, providerID)
	if err != nil {
		return err
	}
	for _, connection := range connections {
		if connection.Status != "active" {
			continue
		}
		if err := RevokeProviderConnection(principal.ID, connection.ID); err != nil {
			return err
		}
	}
	return nil
}

func defaultProviderConnection(
	principalID, providerID string,
) (ProviderConnection, bool, error) {
	db, err := DB()
	if err != nil {
		return ProviderConnection{}, false, err
	}
	row := db.QueryRow(`
SELECT c.id,c.principal_id,p.kind,c.provider_id,c.connection_name,
       c.credential_kind,c.source,c.private_to_principal,c.is_default,c.status,
       c.created_at,c.updated_at,COALESCE(c.last_used_at,0)
FROM provider_connections c
JOIN principals p ON p.id=c.principal_id
WHERE c.principal_id=? AND c.provider_id=? AND c.status='active'
ORDER BY c.is_default DESC,c.updated_at DESC,c.connection_name LIMIT 1`,
		strings.TrimSpace(principalID), strings.TrimSpace(providerID),
	)
	connection, err := scanProviderConnection(row)
	if err == sql.ErrNoRows {
		return ProviderConnection{}, false, nil
	}
	return connection, err == nil, err
}

// ActiveProviderConnection returns the active default connection selected for
// one principal/provider pair without decrypting or marking the credential used.
func ActiveProviderConnection(
	principalID, providerID string,
) (ProviderConnection, bool, error) {
	return defaultProviderConnection(principalID, providerID)
}

func HasActivePrivateProviderConnection(
	principalID, providerID string,
) (bool, error) {
	db, err := DB()
	if err != nil {
		return false, err
	}
	var count int
	err = db.QueryRow(`
SELECT COUNT(*)
FROM provider_connections c
JOIN principals p ON p.id=c.principal_id
WHERE c.principal_id=? AND c.provider_id=? AND c.status='active'
  AND c.private_to_principal=1 AND p.kind='human' AND p.status='active'`,
		strings.TrimSpace(principalID), strings.TrimSpace(providerID),
	).Scan(&count)
	return count > 0, err
}

func HasResolvablePrivateProviderConnection(
	principalID, providerID string,
) (bool, error) {
	if strings.TrimSpace(config.Get().CredentialEncryptionKey) == "" {
		return false, nil
	}
	secret, connection, ok, err := providerConnectionSecret(
		principalID, providerID, "", false,
	)
	if err != nil || !ok {
		return false, err
	}
	return connection.PrincipalKind == "human" &&
		connection.PrivateToPrincipal &&
		strings.TrimSpace(secret) != "", nil
}

func anyProviderConnection(principalID, providerID string) (bool, error) {
	db, err := DB()
	if err != nil {
		return false, err
	}
	var count int
	err = db.QueryRow(`
SELECT COUNT(*) FROM provider_connections WHERE principal_id=? AND provider_id=?`,
		principalID, providerID,
	).Scan(&count)
	return count > 0, err
}

func scanProviderConnection(scanner interface{ Scan(...any) error }) (ProviderConnection, error) {
	var connection ProviderConnection
	var private, isDefault int
	err := scanner.Scan(
		&connection.ID, &connection.PrincipalID, &connection.PrincipalKind,
		&connection.ProviderID, &connection.Name, &connection.Kind, &connection.Source,
		&private, &isDefault, &connection.Status, &connection.CreatedAt,
		&connection.UpdatedAt, &connection.LastUsedAt,
	)
	connection.PrivateToPrincipal = private != 0
	connection.IsDefault = isDefault != 0
	return connection, err
}

func normalizeConnectionName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return defaultConnectionName
	}
	return name
}

func connectionAAD(principalID, providerID, name string, version int) []byte {
	if version <= 1 {
		return []byte(principalID + "|" + providerID)
	}
	return []byte(principalID + "|" + providerID + "|" + normalizeConnectionName(name))
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
