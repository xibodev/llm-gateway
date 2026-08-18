package iam

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"llmgw/internal/config"
)

type ProviderCredentialBinding struct {
	ProjectID     string `json:"project_id"`
	ProviderID    string `json:"provider_id"`
	PrincipalKind string `json:"principal_kind"`
	CredentialID  string `json:"credential_id"`
	Status        string `json:"status"`
	CreatedAt     int64  `json:"created_at"`
	UpdatedAt     int64  `json:"updated_at"`
}

func SetProviderCredentialBinding(
	projectID, providerID, principalKind, credentialID string,
) (ProviderCredentialBinding, error) {
	projectID = strings.TrimSpace(projectID)
	providerID = strings.TrimSpace(providerID)
	principalKind = strings.TrimSpace(principalKind)
	credentialID = strings.TrimSpace(credentialID)
	if projectID == "" || providerID == "" || credentialID == "" {
		return ProviderCredentialBinding{}, fmt.Errorf("project, provider and credential are required")
	}
	if principalKind != "service" {
		return ProviderCredentialBinding{}, fmt.Errorf("shared provider credential bindings require service principal kind")
	}
	db, err := DB()
	if err != nil {
		return ProviderCredentialBinding{}, err
	}
	if err := validateBindingTargets(db, projectID, providerID, credentialID); err != nil {
		return ProviderCredentialBinding{}, err
	}
	now := time.Now().Unix()
	_, err = db.Exec(`
INSERT INTO provider_credential_bindings(
 project_id,provider_id,principal_kind,credential_id,status,created_at,updated_at
) VALUES(?,?,?,?,'active',?,?)
ON CONFLICT(project_id,provider_id,principal_kind) DO UPDATE SET
 credential_id=excluded.credential_id,status='active',updated_at=excluded.updated_at`,
		projectID, providerID, principalKind, credentialID, now, now,
	)
	if err != nil {
		return ProviderCredentialBinding{}, err
	}
	return providerCredentialBinding(projectID, providerID, principalKind)
}

func validateBindingTargets(
	db *sql.DB, projectID, providerID, credentialID string,
) error {
	var projectStatus, credentialProvider, credentialStatus, ownerKind, ownerStatus string
	err := db.QueryRow(`
SELECT p.status,c.provider_id,c.status,o.kind,o.status
FROM projects p
JOIN provider_credentials c ON c.id=?
JOIN principals o ON o.id=c.principal_id
WHERE p.id=?`, credentialID, projectID).Scan(
		&projectStatus, &credentialProvider, &credentialStatus, &ownerKind, &ownerStatus,
	)
	if err == sql.ErrNoRows {
		return fmt.Errorf("project or provider credential not found")
	}
	if err != nil {
		return err
	}
	if projectStatus != "active" || credentialStatus != "active" || ownerStatus != "active" {
		return fmt.Errorf("project and provider credential must be active")
	}
	if credentialProvider != providerID {
		return fmt.Errorf("provider credential does not match provider")
	}
	if ownerKind != "system" {
		return fmt.Errorf("shared provider credential must be gateway-owned")
	}
	return nil
}

func SetProviderCredentialBindingStatus(
	projectID, providerID, principalKind, status string,
) error {
	if status != "active" && status != "disabled" && status != "revoked" {
		return fmt.Errorf("invalid provider credential binding status %q", status)
	}
	db, err := DB()
	if err != nil {
		return err
	}
	res, err := db.Exec(`
UPDATE provider_credential_bindings SET status=?,updated_at=?
WHERE project_id=? AND provider_id=? AND principal_kind=?`,
		status, time.Now().Unix(), strings.TrimSpace(projectID),
		strings.TrimSpace(providerID), strings.TrimSpace(principalKind),
	)
	if err != nil {
		return err
	}
	return requireAffected(res, "provider credential binding")
}

func ListProviderCredentialBindings(projectID string) ([]ProviderCredentialBinding, error) {
	db, err := DB()
	if err != nil {
		return nil, err
	}
	query := `
SELECT project_id,provider_id,principal_kind,credential_id,status,created_at,updated_at
FROM provider_credential_bindings`
	args := []any{}
	if projectID = strings.TrimSpace(projectID); projectID != "" {
		query += " WHERE project_id=?"
		args = append(args, projectID)
	}
	query += " ORDER BY project_id,provider_id,principal_kind"
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProviderCredentialBinding{}
	for rows.Next() {
		var binding ProviderCredentialBinding
		if err := rows.Scan(
			&binding.ProjectID, &binding.ProviderID, &binding.PrincipalKind,
			&binding.CredentialID, &binding.Status, &binding.CreatedAt, &binding.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, binding)
	}
	return out, rows.Err()
}

// ResolveProviderCredentialSecret applies the credential resolution order:
// a human's active default connection first, then the legacy single-credential
// store, then an exact active project/provider/principal-kind binding to a
// gateway-owned credential.
func ResolveProviderCredentialSecret(
	principal *config.Principal, providerID string,
) (string, bool, error) {
	secret, _, ok, err := ResolveProviderCredentialSecretWithObservation(
		principal, providerID,
	)
	return secret, ok, err
}

func ResolveProviderOAuthCredentialSecretWithObservation(
	principal *config.Principal, providerID string,
) (string, *ProviderAccountObservation, bool, error) {
	if principal == nil || strings.TrimSpace(principal.PrincipalID) == "" {
		return "", nil, false, nil
	}
	providerID = strings.TrimSpace(providerID)
	if principal.PrincipalKind == "human" {
		raw, connection, observation, ok, err := ProviderConnectionSecretWithObservation(
			principal.PrincipalID, providerID, "",
		)
		if err != nil {
			return "", nil, false, err
		}
		if ok {
			if !isOAuthCredentialKind(connection.Kind) {
				return "", nil, false, fmt.Errorf(
					"active provider connection is not an OAuth credential",
				)
			}
			envelope, err := decodeOAuthEnvelope(raw)
			if err != nil {
				return "", nil, false, err
			}
			var observed *ProviderAccountObservation
			if observation.CredentialRevision > 0 {
				observed = &observation
			}
			return envelope.AccessToken, observed, true, nil
		}
		secret, kind, ok, err := ProviderCredentialSecretWithKind(
			principal.PrincipalID, providerID,
		)
		if err != nil || !ok {
			return secret, nil, ok, err
		}
		if !isOAuthCredentialKind(kind) {
			return "", nil, false, fmt.Errorf(
				"legacy provider credential is not an OAuth credential",
			)
		}
		return secret, nil, true, nil
	}
	secret, kind, ok, err := boundProviderCredentialSecretWithKind(
		principal, providerID,
	)
	if err != nil || !ok {
		return secret, nil, ok, err
	}
	if !isOAuthCredentialKind(kind) {
		return "", nil, false, fmt.Errorf(
			"bound provider credential is not an OAuth credential",
		)
	}
	return secret, nil, true, nil
}

func ResolveProviderCredentialSecretWithObservation(
	principal *config.Principal, providerID string,
) (string, *ProviderAccountObservation, bool, error) {
	if principal == nil || strings.TrimSpace(principal.PrincipalID) == "" {
		return "", nil, false, nil
	}
	providerID = strings.TrimSpace(providerID)
	if principal.PrincipalKind == "human" {
		raw, connection, observation, ok, err := ProviderConnectionSecretWithObservation(
			principal.PrincipalID, providerID, "",
		)
		if err != nil {
			return "", nil, false, err
		}
		if ok {
			var observed *ProviderAccountObservation
			if observation.CredentialRevision > 0 {
				observed = &observation
			}
			if isOAuthCredentialKind(connection.Kind) {
				envelope, err := decodeOAuthEnvelope(raw)
				if err != nil {
					return "", nil, false, fmt.Errorf("decode provider OAuth connection: %w", err)
				}
				return envelope.AccessToken, observed, true, nil
			}
			return raw, observed, true, nil
		}
		if secret, ok, err := ProviderCredentialSecret(
			principal.PrincipalID, providerID,
		); err != nil || ok {
			return secret, nil, ok, err
		}
	}
	secret, ok, err := boundProviderCredentialSecret(principal, providerID)
	return secret, nil, ok, err
}

func boundProviderCredentialSecret(
	principal *config.Principal, providerID string,
) (string, bool, error) {
	secret, _, ok, err := boundProviderCredentialSecretWithKind(principal, providerID)
	return secret, ok, err
}

func boundProviderCredentialSecretWithKind(
	principal *config.Principal, providerID string,
) (string, string, bool, error) {
	if strings.TrimSpace(principal.ProjectID) == "" || !validPrincipalKind(principal.PrincipalKind) {
		return "", "", false, nil
	}
	db, err := DB()
	if err != nil {
		return "", "", false, err
	}
	var credentialID, ownerID, kind string
	var ciphertext, nonce []byte
	err = db.QueryRow(`
SELECT c.id,c.principal_id,c.credential_kind,c.ciphertext,c.nonce
FROM provider_credential_bindings b
JOIN projects p ON p.id=b.project_id AND p.status='active'
JOIN project_memberships m ON m.project_id=p.id AND m.principal_id=?
JOIN principals n ON n.id=m.principal_id AND n.status='active' AND n.kind=?
JOIN provider_credentials c ON c.id=b.credential_id
 AND c.provider_id=b.provider_id AND c.status='active'
JOIN principals o ON o.id=c.principal_id AND o.kind='system' AND o.status='active'
WHERE b.project_id=? AND b.provider_id=? AND b.principal_kind=? AND b.status='active'`,
		principal.PrincipalID, principal.PrincipalKind, principal.ProjectID,
		providerID, principal.PrincipalKind,
	).Scan(&credentialID, &ownerID, &kind, &ciphertext, &nonce)
	if err == sql.ErrNoRows {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	secret, err := decryptStoredCredential(ownerID, providerID, ciphertext, nonce)
	if err != nil {
		return "", "", false, err
	}
	_, _ = db.Exec(
		"UPDATE provider_credentials SET last_used_at=? WHERE id=?",
		time.Now().Unix(), credentialID,
	)
	return secret, kind, true, nil
}

func decryptStoredCredential(
	ownerID, providerID string, ciphertext, nonce []byte,
) (string, error) {
	key, err := credentialKey()
	if err != nil {
		return "", err
	}
	plaintext, err := decryptCredential(
		key, ciphertext, nonce, []byte(ownerID+"|"+providerID),
	)
	if err != nil {
		return "", fmt.Errorf("decrypt provider credential: %w", err)
	}
	return string(plaintext), nil
}

func providerCredentialBinding(
	projectID, providerID, principalKind string,
) (ProviderCredentialBinding, error) {
	db, err := DB()
	if err != nil {
		return ProviderCredentialBinding{}, err
	}
	var binding ProviderCredentialBinding
	err = db.QueryRow(`
SELECT project_id,provider_id,principal_kind,credential_id,status,created_at,updated_at
FROM provider_credential_bindings
WHERE project_id=? AND provider_id=? AND principal_kind=?`,
		projectID, providerID, principalKind,
	).Scan(
		&binding.ProjectID, &binding.ProviderID, &binding.PrincipalKind,
		&binding.CredentialID, &binding.Status, &binding.CreatedAt, &binding.UpdatedAt,
	)
	return binding, err
}

func validPrincipalKind(kind string) bool {
	return kind == "human" || kind == "service" || kind == "system"
}
