package iam

import (
	"fmt"
	"strings"
	"time"

	"llmgw/internal/diagnostics"
)

const (
	ModelEvidenceCompletion = "completion"
	ModelEvidenceUnverified = "unverified"
)

type ProviderModelEvidence struct {
	ProviderID         string `json:"provider_id"`
	ScopeKey           string `json:"-"`
	Model              string `json:"model"`
	Operation          string `json:"operation"`
	State              string `json:"state"`
	ObservedAt         int64  `json:"observed_at"`
	LatencyMS          int64  `json:"latency_ms"`
	FailureCode        string `json:"failure_code,omitempty"`
	ConnectionID       string `json:"-"`
	CredentialRevision int64  `json:"-"`
	Generation         int64  `json:"-"`
}

func RecordProviderModelEvidence(evidence ProviderModelEvidence) error {
	evidence.ProviderID = strings.TrimSpace(evidence.ProviderID)
	evidence.Model = diagnostics.SanitizeIdentifierLimit(evidence.Model, maxProviderCheckChars)
	evidence.FailureCode = diagnostics.SanitizeIdentifierLimit(evidence.FailureCode, maxProviderCheckChars)
	if evidence.ProviderID == "" || evidence.Model == "" {
		return fmt.Errorf("provider id and model are required")
	}
	if evidence.Operation != ModelEvidenceCompletion {
		return fmt.Errorf("invalid model evidence operation %q", evidence.Operation)
	}
	if evidence.State != "verified" && evidence.State != "failed" &&
		evidence.State != "unverified" && evidence.State != "stale" {
		return fmt.Errorf("invalid model evidence state %q", evidence.State)
	}
	if evidence.Generation < 0 {
		return fmt.Errorf("model evidence generation must be non-negative")
	}
	if evidence.CredentialRevision > 0 &&
		(evidence.ScopeKey == "" || evidence.ConnectionID == "") {
		return fmt.Errorf("scoped model evidence requires a connection and credential revision")
	}
	if evidence.ObservedAt == 0 {
		evidence.ObservedAt = time.Now().Unix()
	}
	db, err := DB()
	if err != nil {
		return err
	}
	result, err := db.Exec(`
INSERT INTO provider_model_evidence(
  provider_id,scope_key,model,operation,state,observed_at,latency_ms,failure_code,
  connection_id,credential_revision,generation
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
ON CONFLICT(provider_id,scope_key,model,operation) DO UPDATE SET
  state=excluded.state,observed_at=excluded.observed_at,latency_ms=excluded.latency_ms,
  failure_code=excluded.failure_code,connection_id=excluded.connection_id,
  credential_revision=excluded.credential_revision,generation=excluded.generation`,
		evidence.ProviderID, evidence.ScopeKey, evidence.Model, evidence.Operation,
		evidence.State, evidence.ObservedAt, evidence.LatencyMS, evidence.FailureCode,
		evidence.ConnectionID, evidence.CredentialRevision, evidence.Generation,
		evidence.ProviderID, evidence.ScopeKey, evidence.Generation,
		evidence.CredentialRevision, evidence.ConnectionID, evidence.ScopeKey,
		evidence.ProviderID, evidence.CredentialRevision,
	)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("provider model evidence generation changed")
	}
	return nil
}

func EnsureProviderModelUnverified(providerID, scopeKey, model, operation string, generation int64) error {
	db, err := DB()
	if err != nil {
		return err
	}
	model = diagnostics.SanitizeIdentifierLimit(model, maxProviderCheckChars)
	if strings.TrimSpace(providerID) == "" || model == "" || operation != ModelEvidenceCompletion {
		return fmt.Errorf("valid provider id, model, and operation are required")
	}
	result, err := db.Exec(`
INSERT INTO provider_model_evidence(
  provider_id,scope_key,model,operation,state,observed_at,generation
)
SELECT ?,?,?,?,'unverified',?,?
WHERE EXISTS(
  SELECT 1 FROM provider_check_generations
  WHERE provider_id=? AND scope_key=? AND generation=?
)
ON CONFLICT(provider_id,scope_key,model,operation) DO NOTHING`,
		providerID, scopeKey, model, operation, time.Now().Unix(), generation,
		providerID, scopeKey, generation)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("provider model evidence generation changed")
	}
	return nil
}

func BeginProviderModelProbes(providerID, scopeKey, operation string, models []string, generation int64) error {
	if strings.TrimSpace(providerID) == "" || operation != ModelEvidenceCompletion || generation < 0 {
		return fmt.Errorf("valid provider id, operation, and generation are required")
	}
	db, err := DB()
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current int64
	if err := tx.QueryRow(`SELECT generation FROM provider_check_generations
WHERE provider_id=? AND scope_key=?`, providerID, scopeKey).Scan(&current); err != nil {
		return err
	}
	if current != generation {
		return fmt.Errorf("provider evidence generation changed")
	}
	seen := map[string]bool{}
	for _, model := range models {
		model = diagnostics.SanitizeIdentifierLimit(model, maxProviderCheckChars)
		if model == "" || seen[model] {
			continue
		}
		seen[model] = true
		if _, err := tx.Exec(`
INSERT INTO provider_model_evidence(provider_id,scope_key,model,operation,state,observed_at,generation)
VALUES(?,?,?,?,'unverified',?,?)
ON CONFLICT(provider_id,scope_key,model,operation) DO UPDATE SET
 state='unverified',observed_at=excluded.observed_at,latency_ms=0,failure_code='',
 connection_id='',credential_revision=0,generation=excluded.generation`,
			providerID, scopeKey, model, operation, time.Now().Unix(), generation); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func ReconcileProviderModelCatalog(providerID, scopeKey, operation string, models []string, generation int64) error {
	if strings.TrimSpace(providerID) == "" || operation != ModelEvidenceCompletion || generation < 0 {
		return fmt.Errorf("valid provider id, operation, and generation are required")
	}
	db, err := DB()
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current int64
	if err := tx.QueryRow(`
SELECT generation FROM provider_check_generations WHERE provider_id=? AND scope_key=?`,
		providerID, scopeKey).Scan(&current); err != nil {
		return err
	}
	if current != generation {
		return nil
	}
	seen := map[string]bool{}
	if _, err := tx.Exec(`
UPDATE provider_model_evidence
SET state='stale',generation=?
WHERE provider_id=? AND scope_key=? AND operation=?`,
		generation, providerID, scopeKey, operation); err != nil {
		return err
	}
	for _, model := range models {
		model = diagnostics.SanitizeIdentifierLimit(model, maxProviderCheckChars)
		if model == "" || seen[model] {
			continue
		}
		seen[model] = true
		if _, err := tx.Exec(`
INSERT INTO provider_model_evidence(
  provider_id,scope_key,model,operation,state,observed_at,generation
) VALUES(?,?,?,?,'unverified',?,?)
ON CONFLICT(provider_id,scope_key,model,operation) DO UPDATE SET
	  state='unverified',observed_at=excluded.observed_at,latency_ms=0,
	  failure_code='',connection_id='',credential_revision=0,
	  generation=excluded.generation`,
			providerID, scopeKey, model, operation, time.Now().Unix(), generation); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func ProviderModelEvidenceFor(providerID, scopeKey string) (map[string]ProviderModelEvidence, error) {
	db, err := DB()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`
SELECT provider_id,scope_key,model,operation,state,observed_at,latency_ms,failure_code,
       connection_id,credential_revision,generation
FROM provider_model_evidence
WHERE provider_id=? AND scope_key=? AND operation=?
ORDER BY model`, providerID, scopeKey, ModelEvidenceCompletion)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]ProviderModelEvidence{}
	for rows.Next() {
		var evidence ProviderModelEvidence
		if err := rows.Scan(&evidence.ProviderID, &evidence.ScopeKey, &evidence.Model,
			&evidence.Operation, &evidence.State, &evidence.ObservedAt, &evidence.LatencyMS,
			&evidence.FailureCode, &evidence.ConnectionID, &evidence.CredentialRevision,
			&evidence.Generation); err != nil {
			return nil, err
		}
		evidence.Model = diagnostics.SanitizeIdentifierLimit(evidence.Model, maxProviderCheckChars)
		evidence.FailureCode = diagnostics.SanitizeIdentifierLimit(evidence.FailureCode, maxProviderCheckChars)
		out[evidence.Model] = evidence
	}
	return out, rows.Err()
}
