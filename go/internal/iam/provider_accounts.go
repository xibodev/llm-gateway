package iam

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"llmgw/internal/config"
)

const DefaultProviderAccountPriority = 100

var providerAccountHealthStates = map[string]bool{
	"unknown": true, "healthy": true, "degraded": true, "cooldown": true, "error": true,
}

// ProviderAccountState contains safe operational metadata for one encrypted
// provider connection. It never contains credentials or token payloads.
type ProviderAccountState struct {
	ConnectionID        string `json:"connection_id"`
	Priority            int    `json:"priority"`
	CredentialRevision  int64  `json:"credential_revision"`
	HealthStatus        string `json:"health_status"`
	ConsecutiveFailures int    `json:"consecutive_failures"`
	CooldownUntil       int64  `json:"cooldown_until,omitempty"`
	TokenExpiresAt      int64  `json:"token_expires_at,omitempty"`
	AccountLabel        string `json:"account_label,omitempty"`
	AccountTier         string `json:"account_tier,omitempty"`
	ProxyRef            string `json:"proxy_ref,omitempty"`
	LastQuotaRefresh    int64  `json:"last_quota_refresh,omitempty"`
	LastSuccessAt       int64  `json:"last_success_at,omitempty"`
	LastFailureAt       int64  `json:"last_failure_at,omitempty"`
	LastHealthEventAtNS int64  `json:"last_health_event_at_ns,omitempty"`
	LastFailureCode     string `json:"last_failure_code,omitempty"`
	UpdatedAt           int64  `json:"updated_at"`
}

// ProviderAccountStateSeed initializes safe metadata in the same transaction as
// a credential insert/rotation. Zero values preserve an existing state row.
type ProviderAccountStateSeed struct {
	Priority          int
	HealthStatus      string
	TokenExpiresAt    *int64
	AccountLabel      *string
	AccountTier       *string
	ResetHealth       bool
	CredentialRotated bool
}

// ProviderAccountStateUpdate applies a partial operational-state update.
type ProviderAccountStateUpdate struct {
	Priority            *int
	HealthStatus        *string
	ConsecutiveFailures *int
	CooldownUntil       *int64
	TokenExpiresAt      *int64
	AccountLabel        *string
	AccountTier         *string
	ProxyRef            *string
	LastQuotaRefresh    *int64
	LastSuccessAt       *int64
	LastFailureAt       *int64
	LastHealthEventAtNS *int64
	LastFailureCode     *string
}

// ProviderAccountObservation identifies the exact credential revision selected
// before an upstream request. Health results are applied only while this
// observation remains current.
type ProviderAccountObservation struct {
	ConnectionID       string
	CredentialRevision int64
}

func ActiveProviderAccountObservation(
	principalID, providerID string,
) (ProviderAccountObservation, bool, error) {
	db, err := DB()
	if err != nil {
		return ProviderAccountObservation{}, false, err
	}
	var observation ProviderAccountObservation
	err = db.QueryRow(`
SELECT c.id,s.credential_revision
FROM provider_connections c
JOIN principals p ON p.id=c.principal_id AND p.status='active'
JOIN provider_account_state s ON s.connection_id=c.id
WHERE c.principal_id=? AND c.provider_id=? AND c.status='active'
ORDER BY c.is_default DESC,c.updated_at DESC,c.connection_name LIMIT 1`,
		strings.TrimSpace(principalID), strings.TrimSpace(providerID),
	).Scan(&observation.ConnectionID, &observation.CredentialRevision)
	if err == sql.ErrNoRows {
		return ProviderAccountObservation{}, false, nil
	}
	return observation, err == nil, err
}

func ProviderAccountStateByConnection(connectionID string) (ProviderAccountState, bool, error) {
	db, err := DB()
	if err != nil {
		return ProviderAccountState{}, false, err
	}

	state, err := providerAccountStateFromScanner(db.QueryRow(`
SELECT connection_id,priority,credential_revision,health_status,consecutive_failures,
       COALESCE(cooldown_until,0),COALESCE(token_expires_at,0),
       COALESCE(account_label,''),COALESCE(account_tier,''),COALESCE(proxy_ref,''),
       COALESCE(last_quota_refresh,0),COALESCE(last_success_at,0),
       COALESCE(last_failure_at,0),COALESCE(last_health_event_at_ns,0),
       COALESCE(last_failure_code,''),updated_at
FROM provider_account_state WHERE connection_id=?`,
		strings.TrimSpace(connectionID),
	))
	if err == sql.ErrNoRows {
		return ProviderAccountState{}, false, nil
	}
	return state, err == nil, err
}

// BackfillProviderAccountOAuthMetadata reconciles safe expiry/account labels
// from existing encrypted OAuth envelopes after schema migration.
func BackfillProviderAccountOAuthMetadata() (int, error) {
	if strings.TrimSpace(config.Get().CredentialEncryptionKey) == "" {
		return 0, nil
	}
	db, err := DB()
	if err != nil {
		return 0, err
	}
	key, err := credentialKey()
	if err != nil {
		return 0, err
	}
	rows, err := db.Query(`
SELECT id,principal_id,provider_id,connection_name,credential_kind,
       ciphertext,nonce,aad_version
FROM provider_connections WHERE status='active'`)
	if err != nil {
		return 0, err
	}
	type accountRef struct {
		id, principalID, providerID, name, kind string
		ciphertext, nonce                       []byte
		aadVersion                              int
	}
	refs := []accountRef{}
	for rows.Next() {
		var ref accountRef
		if err := rows.Scan(
			&ref.id, &ref.principalID, &ref.providerID, &ref.name, &ref.kind,
			&ref.ciphertext, &ref.nonce, &ref.aadVersion,
		); err != nil {
			rows.Close()
			return 0, err
		}
		if isOAuthCredentialKind(ref.kind) {
			refs = append(refs, ref)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}

	updated := 0
	for _, ref := range refs {
		raw, err := decryptCredential(
			key, ref.ciphertext, ref.nonce,
			connectionAAD(ref.principalID, ref.providerID, ref.name, ref.aadVersion),
		)
		if err != nil {
			return updated, err
		}
		envelope, err := decodeOAuthEnvelope(string(raw))
		if err != nil {
			return updated, err
		}
		state, found, err := ProviderAccountStateByConnection(ref.id)
		if err != nil {
			return updated, err
		}
		if found && state.TokenExpiresAt == envelope.ExpiresAt &&
			state.AccountLabel == strings.TrimSpace(envelope.AccountLabel) {
			continue
		}
		expiresAt := envelope.ExpiresAt
		accountLabel := envelope.AccountLabel
		if _, err := UpdateProviderAccountState(ref.id, ProviderAccountStateUpdate{
			TokenExpiresAt: &expiresAt, AccountLabel: &accountLabel,
		}); err != nil {
			return updated, err
		}
		updated++
	}
	return updated, nil
}

func UpdateProviderAccountState(
	connectionID string, update ProviderAccountStateUpdate,
) (ProviderAccountState, error) {
	return mutateProviderAccountState(connectionID, func(state *ProviderAccountState) (bool, error) {
		if update.Priority != nil {
			state.Priority = *update.Priority
		}
		if update.HealthStatus != nil {
			state.HealthStatus = *update.HealthStatus
		}
		if update.ConsecutiveFailures != nil {
			state.ConsecutiveFailures = *update.ConsecutiveFailures
		}
		if update.CooldownUntil != nil {
			state.CooldownUntil = *update.CooldownUntil
		}
		if update.TokenExpiresAt != nil {
			state.TokenExpiresAt = *update.TokenExpiresAt
		}
		if update.AccountLabel != nil {
			state.AccountLabel = *update.AccountLabel
		}
		if update.AccountTier != nil {
			state.AccountTier = *update.AccountTier
		}
		if update.ProxyRef != nil {
			state.ProxyRef = *update.ProxyRef
		}
		if update.LastQuotaRefresh != nil {
			state.LastQuotaRefresh = *update.LastQuotaRefresh
		}
		if update.LastSuccessAt != nil {
			state.LastSuccessAt = *update.LastSuccessAt
		}
		if update.LastFailureAt != nil {
			state.LastFailureAt = *update.LastFailureAt
		}
		if update.LastHealthEventAtNS != nil {
			state.LastHealthEventAtNS = *update.LastHealthEventAtNS
		}
		if update.LastFailureCode != nil {
			state.LastFailureCode = *update.LastFailureCode
		}
		return true, nil
	})
}

func RecordProviderAccountSuccess(connectionID string, at time.Time) (ProviderAccountState, error) {
	eventAt := at.Unix()
	eventAtNS := at.UnixNano()
	if eventAtNS <= 0 {
		return ProviderAccountState{}, fmt.Errorf("provider account success timestamp must be positive")
	}
	return mutateProviderAccountState(connectionID, func(state *ProviderAccountState) (bool, error) {
		if eventAtNS <= state.LastHealthEventAtNS {
			return false, nil
		}
		state.HealthStatus = "healthy"
		state.ConsecutiveFailures = 0
		state.CooldownUntil = 0
		state.LastSuccessAt = eventAt
		state.LastHealthEventAtNS = eventAtNS
		state.LastFailureCode = ""
		return true, nil
	})
}

func RecordProviderAccountSuccessIfCurrent(
	observation ProviderAccountObservation, at time.Time,
) (bool, error) {
	return recordProviderAccountIfCurrent(observation, func(state *ProviderAccountState) {
		eventAt := at.Unix()
		eventAtNS := at.UnixNano()
		if eventAtNS <= state.LastHealthEventAtNS {
			return
		}
		state.HealthStatus = "healthy"
		state.ConsecutiveFailures = 0
		state.CooldownUntil = 0
		state.LastSuccessAt = eventAt
		state.LastHealthEventAtNS = eventAtNS
		state.LastFailureCode = ""
	})
}

func RecordProviderAccountFailure(
	connectionID, failureCode string, cooldownUntil time.Time, at time.Time,
) (ProviderAccountState, error) {
	eventAt := at.Unix()
	eventAtNS := at.UnixNano()
	if eventAtNS <= 0 {
		return ProviderAccountState{}, fmt.Errorf("provider account failure timestamp must be positive")
	}
	return mutateProviderAccountState(connectionID, func(state *ProviderAccountState) (bool, error) {
		if eventAtNS <= state.LastHealthEventAtNS {
			return false, nil
		}
		state.ConsecutiveFailures++
		state.LastFailureAt = eventAt
		state.LastHealthEventAtNS = eventAtNS
		state.LastFailureCode = strings.TrimSpace(failureCode)
		state.CooldownUntil = cooldownUntil.Unix()
		if state.CooldownUntil > state.LastFailureAt {
			state.HealthStatus = "cooldown"
		} else {
			state.HealthStatus = "error"
			state.CooldownUntil = 0
		}
		return true, nil
	})
}

func RecordProviderAccountFailureIfCurrent(
	observation ProviderAccountObservation, failureCode string, at time.Time,
) (bool, error) {
	return recordProviderAccountIfCurrent(observation, func(state *ProviderAccountState) {
		eventAt := at.Unix()
		eventAtNS := at.UnixNano()
		if eventAtNS <= state.LastHealthEventAtNS {
			return
		}
		state.ConsecutiveFailures++
		state.LastFailureAt = eventAt
		state.LastHealthEventAtNS = eventAtNS
		state.LastFailureCode = strings.TrimSpace(failureCode)
		state.HealthStatus = "error"
		state.CooldownUntil = 0
	})
}

func recordProviderAccountIfCurrent(
	observation ProviderAccountObservation, mutation func(*ProviderAccountState),
) (bool, error) {
	if strings.TrimSpace(observation.ConnectionID) == "" ||
		observation.CredentialRevision <= 0 {
		return false, nil
	}
	db, err := DB()
	if err != nil {
		return false, err
	}
	tx, err := db.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	state, found, err := providerAccountStateByConnectionTx(tx, observation.ConnectionID)
	if err != nil {
		return false, err
	}
	if !found || state.CredentialRevision != observation.CredentialRevision {
		return false, nil
	}
	before := state
	mutation(&state)
	if state == before {
		return false, nil
	}
	state.UpdatedAt = time.Now().Unix()
	if err := validateProviderAccountState(&state); err != nil {
		return false, err
	}
	if err := saveProviderAccountStateTx(tx, state); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func mutateProviderAccountState(
	connectionID string, mutation func(*ProviderAccountState) (bool, error),
) (ProviderAccountState, error) {
	connectionID = strings.TrimSpace(connectionID)
	if connectionID == "" {
		return ProviderAccountState{}, fmt.Errorf("connection id is required")
	}
	db, err := DB()
	if err != nil {
		return ProviderAccountState{}, err
	}
	tx, err := db.Begin()
	if err != nil {
		return ProviderAccountState{}, err
	}
	defer func() { _ = tx.Rollback() }()

	state, found, err := providerAccountStateByConnectionTx(tx, connectionID)
	if err != nil {
		return ProviderAccountState{}, err
	}
	if !found {
		var exists int
		if err := tx.QueryRow(
			"SELECT COUNT(*) FROM provider_connections WHERE id=?", connectionID,
		).Scan(&exists); err != nil {
			return ProviderAccountState{}, err
		}
		if exists == 0 {
			return ProviderAccountState{}, ErrProviderConnectionNotFound
		}
		state = defaultProviderAccountState(connectionID, time.Now().Unix())
	}
	changed, err := mutation(&state)
	if err != nil {
		return ProviderAccountState{}, err
	}
	if !changed {
		return state, nil
	}
	state.UpdatedAt = time.Now().Unix()
	if err := validateProviderAccountState(&state); err != nil {
		return ProviderAccountState{}, err
	}
	if err := saveProviderAccountStateTx(tx, state); err != nil {
		return ProviderAccountState{}, err
	}
	if err := tx.Commit(); err != nil {
		return ProviderAccountState{}, err
	}
	return state, nil
}

func seedProviderAccountStateTx(
	tx *sql.Tx, connectionID string, seed ProviderAccountStateSeed, now int64,
) error {
	state, found, err := providerAccountStateByConnectionTx(tx, connectionID)
	if err != nil {
		return err
	}
	if !found {
		state = defaultProviderAccountState(connectionID, now)
	}
	if seed.CredentialRotated && found {
		state.CredentialRevision++
	}
	if seed.Priority > 0 {
		state.Priority = seed.Priority
	}
	if seed.ResetHealth {
		state.HealthStatus = "unknown"
		state.ConsecutiveFailures = 0
		state.CooldownUntil = 0
		state.LastSuccessAt = 0
		state.LastFailureAt = 0
		state.LastHealthEventAtNS = 0
		state.LastFailureCode = ""
	} else if value := strings.TrimSpace(seed.HealthStatus); value != "" {
		state.HealthStatus = value
	}
	if seed.TokenExpiresAt != nil {
		state.TokenExpiresAt = *seed.TokenExpiresAt
	}
	if seed.AccountLabel != nil {
		state.AccountLabel = *seed.AccountLabel
	}
	if seed.AccountTier != nil {
		state.AccountTier = *seed.AccountTier
	}
	state.UpdatedAt = now
	if err := validateProviderAccountState(&state); err != nil {
		return err
	}
	return saveProviderAccountStateTx(tx, state)
}

func providerAccountStateByConnectionTx(
	tx *sql.Tx, connectionID string,
) (ProviderAccountState, bool, error) {
	state, err := providerAccountStateFromScanner(tx.QueryRow(`
SELECT connection_id,priority,credential_revision,health_status,consecutive_failures,
       COALESCE(cooldown_until,0),COALESCE(token_expires_at,0),
       COALESCE(account_label,''),COALESCE(account_tier,''),COALESCE(proxy_ref,''),
       COALESCE(last_quota_refresh,0),COALESCE(last_success_at,0),
       COALESCE(last_failure_at,0),COALESCE(last_health_event_at_ns,0),
       COALESCE(last_failure_code,''),updated_at
FROM provider_account_state WHERE connection_id=?`, connectionID))
	if err == sql.ErrNoRows {
		return ProviderAccountState{}, false, nil
	}
	return state, err == nil, err
}

func providerAccountStateFromScanner(
	scanner interface{ Scan(...any) error },
) (ProviderAccountState, error) {
	var state ProviderAccountState
	err := scanner.Scan(
		&state.ConnectionID, &state.Priority, &state.CredentialRevision,
		&state.HealthStatus, &state.ConsecutiveFailures,
		&state.CooldownUntil, &state.TokenExpiresAt, &state.AccountLabel, &state.AccountTier,
		&state.ProxyRef, &state.LastQuotaRefresh, &state.LastSuccessAt,
		&state.LastFailureAt, &state.LastHealthEventAtNS, &state.LastFailureCode, &state.UpdatedAt,
	)
	return state, err
}

func saveProviderAccountStateTx(tx *sql.Tx, state ProviderAccountState) error {
	_, err := tx.Exec(`
INSERT INTO provider_account_state(
    connection_id,priority,credential_revision,health_status,consecutive_failures,cooldown_until,
    token_expires_at,account_label,account_tier,proxy_ref,last_quota_refresh,
    last_success_at,last_failure_at,last_health_event_at_ns,last_failure_code,updated_at
) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(connection_id) DO UPDATE SET
    priority=excluded.priority,credential_revision=excluded.credential_revision,
    health_status=excluded.health_status,
    consecutive_failures=excluded.consecutive_failures,cooldown_until=excluded.cooldown_until,
    token_expires_at=excluded.token_expires_at,account_label=excluded.account_label,
    account_tier=excluded.account_tier,proxy_ref=excluded.proxy_ref,
    last_quota_refresh=excluded.last_quota_refresh,last_success_at=excluded.last_success_at,
    last_failure_at=excluded.last_failure_at,
    last_health_event_at_ns=excluded.last_health_event_at_ns,
    last_failure_code=excluded.last_failure_code,
    updated_at=excluded.updated_at`,
		state.ConnectionID, state.Priority, state.CredentialRevision,
		state.HealthStatus, state.ConsecutiveFailures,
		nullablePositiveInt64(state.CooldownUntil), nullablePositiveInt64(state.TokenExpiresAt),
		nullableString(state.AccountLabel), nullableString(state.AccountTier), nullableString(state.ProxyRef),
		nullablePositiveInt64(state.LastQuotaRefresh), nullablePositiveInt64(state.LastSuccessAt),
		nullablePositiveInt64(state.LastFailureAt), nullablePositiveInt64(state.LastHealthEventAtNS),
		nullableString(state.LastFailureCode), state.UpdatedAt,
	)
	return err
}

func defaultProviderAccountState(connectionID string, now int64) ProviderAccountState {
	return ProviderAccountState{
		ConnectionID: connectionID, Priority: DefaultProviderAccountPriority,
		CredentialRevision: 1, HealthStatus: "unknown", UpdatedAt: now,
	}
}

func validateProviderAccountState(state *ProviderAccountState) error {
	state.HealthStatus = strings.ToLower(strings.TrimSpace(state.HealthStatus))
	state.AccountLabel = strings.TrimSpace(state.AccountLabel)
	state.AccountTier = strings.TrimSpace(state.AccountTier)
	state.ProxyRef = strings.TrimSpace(state.ProxyRef)
	state.LastFailureCode = strings.TrimSpace(state.LastFailureCode)
	if state.ConnectionID == "" {
		return fmt.Errorf("connection id is required")
	}
	if state.Priority < 0 {
		return fmt.Errorf("provider account priority must be non-negative")
	}
	if state.CredentialRevision <= 0 {
		return fmt.Errorf("provider credential revision must be positive")
	}
	if !providerAccountHealthStates[state.HealthStatus] {
		return fmt.Errorf("invalid provider account health status %q", state.HealthStatus)
	}
	if state.ConsecutiveFailures < 0 {
		return fmt.Errorf("provider account failures must be non-negative")
	}
	for name, value := range map[string]int64{
		"cooldown_until": state.CooldownUntil, "token_expires_at": state.TokenExpiresAt,
		"last_quota_refresh": state.LastQuotaRefresh, "last_success_at": state.LastSuccessAt,
		"last_failure_at": state.LastFailureAt, "last_health_event_at_ns": state.LastHealthEventAtNS,
		"updated_at": state.UpdatedAt,
	} {
		if value < 0 {
			return fmt.Errorf("%s must be non-negative", name)
		}
	}
	return nil
}

func nullablePositiveInt64(value int64) any {
	if value <= 0 {
		return nil
	}
	return value
}

func nullableString(value string) any {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return value
}
