package iam

import (
	"database/sql"
	"fmt"
	"time"

	"llmgw/internal/diagnostics"
)

// ProviderCheck is the last recorded outcome of one lifecycle operation for a
// configured provider. Checks are isolated by human scope and bound to the
// credential/config generation that produced them.
type ProviderCheck struct {
	ProviderID         string `json:"provider_id"`
	Operation          string `json:"operation"`
	ScopeKey           string `json:"-"`
	ConnectionID       string `json:"-"`
	CredentialRevision int64  `json:"-"`
	Generation         int64  `json:"-"`
	Success            bool   `json:"success"`
	Detail             string `json:"detail,omitempty"`
	Model              string `json:"model,omitempty"`
	LatencyMS          int64  `json:"latency_ms"`
	CheckedAt          int64  `json:"checked_at"`
}

const (
	CheckReachability = "reachability"
	CheckCatalogSync  = "catalog_sync"
	CheckCacheReset   = "cache_reset"
	CheckVerify       = "verify"

	maxProviderCheckChars = 500
)

func validCheckOperation(operation string) bool {
	switch operation {
	case CheckReachability, CheckCatalogSync, CheckCacheReset, CheckVerify:
		return true
	}
	return false
}

// RecordProviderCheck atomically writes the result only while both the
// provider generation and, when present, connection revision remain current.
func RecordProviderCheck(check ProviderCheck) error {
	if check.ProviderID == "" {
		return fmt.Errorf("provider id is required")
	}
	if !validCheckOperation(check.Operation) {
		return fmt.Errorf("invalid check operation %q", check.Operation)
	}
	if check.Generation < 0 {
		return fmt.Errorf("provider check generation must be non-negative")
	}
	if check.CredentialRevision > 0 &&
		(check.ScopeKey == "" || check.ConnectionID == "") {
		return fmt.Errorf("scoped provider checks require a connection and credential revision")
	}
	if check.CheckedAt == 0 {
		check.CheckedAt = time.Now().Unix()
	}
	check.Detail = diagnostics.SanitizeTextLimit(check.Detail, maxProviderCheckChars)
	check.Model = diagnostics.SanitizeIdentifierLimit(check.Model, maxProviderCheckChars)
	success := 0
	if check.Success {
		success = 1
	}
	db, err := DB()
	if err != nil {
		return err
	}
	if _, err := db.Exec(`
INSERT INTO provider_check_generations(provider_id,scope_key,generation)
VALUES(?,?,0)
ON CONFLICT(provider_id,scope_key) DO NOTHING`,
		check.ProviderID, check.ScopeKey,
	); err != nil {
		return err
	}
	_, err = db.Exec(`
INSERT INTO provider_checks(
  provider_id,operation,scope_key,connection_id,credential_revision,
  generation,success,detail,model,latency_ms,checked_at
)
SELECT ?,?,?,?,?,?,?,?,?,?,?
WHERE EXISTS(
  SELECT 1 FROM provider_check_generations
  WHERE provider_id=? AND scope_key=? AND generation=?
)
AND (?=0 OR EXISTS(
  SELECT 1
  FROM provider_connections c
  JOIN provider_account_state s ON s.connection_id=c.id
  WHERE c.id=? AND c.principal_id=? AND c.provider_id=?
    AND c.status='active' AND c.is_default=1 AND s.credential_revision=?
))
ON CONFLICT(provider_id,operation,scope_key) DO UPDATE SET
  connection_id=excluded.connection_id,
  credential_revision=excluded.credential_revision,
  generation=excluded.generation,
  success=excluded.success,
  detail=excluded.detail,
  model=excluded.model,
  latency_ms=excluded.latency_ms,
  checked_at=excluded.checked_at`,
		check.ProviderID, check.Operation, check.ScopeKey,
		check.ConnectionID, check.CredentialRevision, check.Generation,
		success, check.Detail, check.Model, check.LatencyMS, check.CheckedAt,
		check.ProviderID, check.ScopeKey, check.Generation,
		check.CredentialRevision,
		check.ConnectionID, check.ScopeKey, check.ProviderID, check.CredentialRevision,
	)
	return err
}

// ProviderCheckGeneration returns the current lifecycle generation, creating
// its zero-valued fence before a check starts so later invalidation can advance
// it even while that check is in flight.
func ProviderCheckGeneration(providerID, scopeKey string) (int64, error) {
	db, err := DB()
	if err != nil {
		return 0, err
	}
	if _, err := db.Exec(`
INSERT INTO provider_check_generations(provider_id,scope_key,generation)
VALUES(?,?,0)
ON CONFLICT(provider_id,scope_key) DO NOTHING`,
		providerID, scopeKey,
	); err != nil {
		return 0, err
	}
	var generation int64
	err = db.QueryRow(`
SELECT generation FROM provider_check_generations
WHERE provider_id=? AND scope_key=?`,
		providerID, scopeKey,
	).Scan(&generation)
	return generation, err
}

// LastProviderChecks returns the latest recorded check per operation for every
// provider. Empty scope aggregates all checks for administrators; a non-empty
// scope isolates one human portal.
func LastProviderChecks(scopeKey string) (map[string][]ProviderCheck, error) {
	db, err := DB()
	if err != nil {
		return nil, err
	}
	query := `
SELECT provider_id,operation,scope_key,connection_id,credential_revision,
       generation,success,detail,model,latency_ms,checked_at
FROM provider_checks`
	args := []any{}
	if scopeKey != "" {
		query += " WHERE scope_key=?"
		args = append(args, scopeKey)
	}
	query += " ORDER BY provider_id,checked_at"
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]ProviderCheck{}
	for rows.Next() {
		var check ProviderCheck
		var success int
		if err := rows.Scan(
			&check.ProviderID, &check.Operation, &check.ScopeKey,
			&check.ConnectionID, &check.CredentialRevision, &check.Generation,
			&success, &check.Detail, &check.Model, &check.LatencyMS, &check.CheckedAt,
		); err != nil {
			return nil, err
		}
		check.Success = success == 1
		check.Detail = diagnostics.SanitizeTextLimit(check.Detail, maxProviderCheckChars)
		check.Model = diagnostics.SanitizeIdentifierLimit(check.Model, maxProviderCheckChars)
		out[check.ProviderID] = append(out[check.ProviderID], check)
	}
	return out, rows.Err()
}

func InvalidateProviderChecks(providerID string) error {
	db, err := DB()
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := invalidateProviderChecksTx(tx, providerID, "", true); err != nil {
		return err
	}
	return tx.Commit()
}

func invalidateProviderChecksTx(
	tx *sql.Tx, providerID, scopeKey string, allScopes bool,
) error {
	if allScopes {
		if _, err := tx.Exec(`
INSERT INTO provider_check_generations(provider_id,scope_key,generation)
VALUES(?,'',1)
ON CONFLICT(provider_id,scope_key) DO UPDATE SET generation=generation+1`,
			providerID,
		); err != nil {
			return err
		}
		if _, err := tx.Exec(`
UPDATE provider_check_generations SET generation=generation+1
WHERE provider_id=? AND scope_key!=''`, providerID); err != nil {
			return err
		}
		_, err := tx.Exec("DELETE FROM provider_checks WHERE provider_id=?", providerID)
		return err
	}
	if _, err := tx.Exec(`
INSERT INTO provider_check_generations(provider_id,scope_key,generation)
VALUES(?,?,1)
ON CONFLICT(provider_id,scope_key) DO UPDATE SET generation=generation+1`,
		providerID, scopeKey,
	); err != nil {
		return err
	}
	_, err := tx.Exec(
		"DELETE FROM provider_checks WHERE provider_id=? AND scope_key=?",
		providerID, scopeKey,
	)
	return err
}

// DeleteProviderChecks removes lifecycle results while retaining incremented
// generation tombstones so in-flight requests cannot resurrect deleted state.
func DeleteProviderChecks(providerID string) error {
	db, err := DB()
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := invalidateProviderChecksTx(tx, providerID, "", true); err != nil {
		return err
	}
	return tx.Commit()
}
