package iam

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

type migration struct {
	version int
	sql     string
}

const providerConnectionsTableSchema = `
CREATE TABLE provider_connections (
    id TEXT PRIMARY KEY,
    principal_id TEXT NOT NULL REFERENCES principals(id) ON DELETE CASCADE,
    provider_id TEXT NOT NULL,
    connection_name TEXT NOT NULL,
    credential_kind TEXT NOT NULL,
    source TEXT NOT NULL DEFAULT 'user'
        CHECK (source IN ('user','admin','config','migration')),
    private_to_principal INTEGER NOT NULL DEFAULT 1
        CHECK (private_to_principal IN (0,1)),
    is_default INTEGER NOT NULL DEFAULT 0 CHECK (is_default IN (0,1)),
    ciphertext BLOB NOT NULL,
    nonce BLOB NOT NULL,
    key_version INTEGER NOT NULL DEFAULT 1,
    aad_version INTEGER NOT NULL DEFAULT 2 CHECK (aad_version IN (1,2)),
    status TEXT NOT NULL DEFAULT 'active'
        CHECK (status IN ('active','disabled','revoked')),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    last_used_at INTEGER,
    UNIQUE (principal_id, provider_id, connection_name)
)`

const providerConnectionsOwnerIndexSchema = `
CREATE INDEX idx_provider_connections_owner
ON provider_connections(principal_id,provider_id,status)`

const providerConnectionsSourceIndexSchema = `
CREATE INDEX idx_provider_connections_source
ON provider_connections(source,provider_id,status)`

const providerConnectionsDefaultIndexSchema = `
CREATE UNIQUE INDEX idx_provider_connections_active_default
ON provider_connections(principal_id,provider_id)
WHERE is_default=1 AND status='active'`

const providerCredentialBindingsTableSchema = `
CREATE TABLE provider_credential_bindings (
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    provider_id TEXT NOT NULL,
    principal_kind TEXT NOT NULL CHECK (principal_kind IN ('human','service','system')),
    credential_id TEXT NOT NULL REFERENCES provider_credentials(id) ON DELETE CASCADE,
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','disabled','revoked')),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (project_id, provider_id, principal_kind)
)`

const providerCredentialBindingsIndexSchema = `
CREATE INDEX idx_provider_credential_bindings_credential
ON provider_credential_bindings(credential_id)`

var migrations = []migration{
	{
		version: 1,
		sql: `
CREATE TABLE principals (
    id TEXT PRIMARY KEY,
    kind TEXT NOT NULL CHECK (kind IN ('human','service','system')),
    external_subject TEXT UNIQUE,
    email TEXT,
    display_name TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','disabled')),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE projects (
    id TEXT PRIMARY KEY,
    slug TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','disabled')),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE project_memberships (
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    principal_id TEXT NOT NULL REFERENCES principals(id) ON DELETE CASCADE,
    role TEXT NOT NULL CHECK (role IN ('owner','admin','member','viewer')),
    created_at INTEGER NOT NULL,
    PRIMARY KEY (project_id, principal_id)
);

CREATE TABLE api_keys (
    id TEXT PRIMARY KEY,
    prefix TEXT NOT NULL UNIQUE,
    secret_hash BLOB NOT NULL UNIQUE,
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    principal_id TEXT NOT NULL REFERENCES principals(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','disabled','revoked')),
    created_at INTEGER NOT NULL,
    expires_at INTEGER,
    last_used_at INTEGER,
    allowed_models_json TEXT NOT NULL DEFAULT '[]',
    allowed_providers_json TEXT NOT NULL DEFAULT '[]',
    rpm INTEGER NOT NULL DEFAULT 0 CHECK (rpm >= 0),
    daily_requests INTEGER NOT NULL DEFAULT 0 CHECK (daily_requests >= 0),
    monthly_requests INTEGER NOT NULL DEFAULT 0 CHECK (monthly_requests >= 0),
    daily_input_tokens INTEGER NOT NULL DEFAULT 0 CHECK (daily_input_tokens >= 0),
    daily_output_tokens INTEGER NOT NULL DEFAULT 0 CHECK (daily_output_tokens >= 0),
    monthly_total_tokens INTEGER NOT NULL DEFAULT 0 CHECK (monthly_total_tokens >= 0),
    daily_cost_microusd INTEGER NOT NULL DEFAULT 0 CHECK (daily_cost_microusd >= 0),
    monthly_cost_microusd INTEGER NOT NULL DEFAULT 0 CHECK (monthly_cost_microusd >= 0),
    daily_credits_milli INTEGER NOT NULL DEFAULT 0 CHECK (daily_credits_milli >= 0),
    monthly_credits_milli INTEGER NOT NULL DEFAULT 0 CHECK (monthly_credits_milli >= 0)
);

CREATE TABLE provider_credentials (
    id TEXT PRIMARY KEY,
    principal_id TEXT NOT NULL REFERENCES principals(id) ON DELETE CASCADE,
    provider_id TEXT NOT NULL,
    credential_kind TEXT NOT NULL,
    ciphertext BLOB NOT NULL,
    nonce BLOB NOT NULL,
    key_version INTEGER NOT NULL DEFAULT 1,
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','disabled','revoked')),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    last_used_at INTEGER,
    UNIQUE (principal_id, provider_id)
);

CREATE TABLE usage_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id TEXT UNIQUE,
    ts INTEGER NOT NULL,
    endpoint TEXT NOT NULL,
    status_code INTEGER NOT NULL DEFAULT 0,
    latency_ms INTEGER NOT NULL DEFAULT 0,
    requested_model TEXT,
    routed_model TEXT,
    provider TEXT,
    project_id TEXT REFERENCES projects(id) ON DELETE SET NULL,
    principal_id TEXT REFERENCES principals(id) ON DELETE SET NULL,
    key_id TEXT REFERENCES api_keys(id) ON DELETE SET NULL,
    input_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    cost_microusd INTEGER NOT NULL DEFAULT 0,
    credits_milli INTEGER NOT NULL DEFAULT 0,
    error_code TEXT,
    is_stub INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE audit_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    ts INTEGER NOT NULL,
    actor_principal_id TEXT REFERENCES principals(id) ON DELETE SET NULL,
    actor_key_id TEXT REFERENCES api_keys(id) ON DELETE SET NULL,
    action TEXT NOT NULL,
    target_type TEXT,
    target_id TEXT,
    result TEXT NOT NULL,
    detail_json TEXT NOT NULL DEFAULT '{}'
);

CREATE TABLE alert_rules (
    id TEXT PRIMARY KEY,
    project_id TEXT REFERENCES projects(id) ON DELETE CASCADE,
    principal_id TEXT REFERENCES principals(id) ON DELETE CASCADE,
    kind TEXT NOT NULL,
    threshold INTEGER NOT NULL,
    period TEXT NOT NULL,
    enabled INTEGER NOT NULL DEFAULT 1,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE TABLE outbox_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    ts INTEGER NOT NULL,
    kind TEXT NOT NULL,
    project_id TEXT REFERENCES projects(id) ON DELETE SET NULL,
    principal_id TEXT REFERENCES principals(id) ON DELETE SET NULL,
    payload_json TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','delivered','failed')),
    attempts INTEGER NOT NULL DEFAULT 0,
    available_at INTEGER NOT NULL,
    delivered_at INTEGER,
    last_error TEXT
);

CREATE INDEX idx_principals_email ON principals(email);
CREATE INDEX idx_memberships_principal ON project_memberships(principal_id);
CREATE INDEX idx_keys_project ON api_keys(project_id);
CREATE INDEX idx_keys_principal ON api_keys(principal_id);
CREATE INDEX idx_keys_status ON api_keys(status);
CREATE INDEX idx_usage_ts ON usage_events(ts);
CREATE INDEX idx_usage_project_ts ON usage_events(project_id, ts);
CREATE INDEX idx_usage_principal_ts ON usage_events(principal_id, ts);
CREATE INDEX idx_usage_key_ts ON usage_events(key_id, ts);
CREATE INDEX idx_usage_model_ts ON usage_events(routed_model, ts);
CREATE INDEX idx_audit_ts ON audit_events(ts);
CREATE INDEX idx_outbox_status_available ON outbox_events(status, available_at);
`,
	},
	{
		version: 2,
		sql: `
CREATE TABLE control_metadata (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL,
    updated_at INTEGER NOT NULL
);
`,
	},
	{
		version: 3,
		sql: `
CREATE TABLE quota_counters (
    key_id TEXT NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
    period TEXT NOT NULL CHECK (period IN ('minute','day','month')),
    period_start INTEGER NOT NULL,
    requests INTEGER NOT NULL DEFAULT 0,
    input_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    cost_microusd INTEGER NOT NULL DEFAULT 0,
    credits_milli INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (key_id, period, period_start)
);
CREATE INDEX idx_quota_period ON quota_counters(period, period_start);
`,
	},
	{
		version: 4,
		sql: `
ALTER TABLE alert_rules ADD COLUMN metric TEXT NOT NULL DEFAULT 'requests';
ALTER TABLE outbox_events ADD COLUMN dedupe_key TEXT;
CREATE UNIQUE INDEX idx_outbox_dedupe ON outbox_events(dedupe_key);
`,
	},
	{
		version: 5,
		sql: `
CREATE TABLE project_policies (
    project_id TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    allowed_models_json TEXT NOT NULL DEFAULT '[]',
    allowed_providers_json TEXT NOT NULL DEFAULT '[]',
    rpm INTEGER NOT NULL DEFAULT 0 CHECK (rpm >= 0),
    daily_requests INTEGER NOT NULL DEFAULT 0 CHECK (daily_requests >= 0),
    monthly_requests INTEGER NOT NULL DEFAULT 0 CHECK (monthly_requests >= 0),
    daily_input_tokens INTEGER NOT NULL DEFAULT 0 CHECK (daily_input_tokens >= 0),
    daily_output_tokens INTEGER NOT NULL DEFAULT 0 CHECK (daily_output_tokens >= 0),
    monthly_total_tokens INTEGER NOT NULL DEFAULT 0 CHECK (monthly_total_tokens >= 0),
    daily_cost_microusd INTEGER NOT NULL DEFAULT 0 CHECK (daily_cost_microusd >= 0),
    monthly_cost_microusd INTEGER NOT NULL DEFAULT 0 CHECK (monthly_cost_microusd >= 0),
    daily_credits_milli INTEGER NOT NULL DEFAULT 0 CHECK (daily_credits_milli >= 0),
    monthly_credits_milli INTEGER NOT NULL DEFAULT 0 CHECK (monthly_credits_milli >= 0),
    updated_at INTEGER NOT NULL
);

CREATE TABLE project_quota_counters (
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    period TEXT NOT NULL CHECK (period IN ('minute','day','month')),
    period_start INTEGER NOT NULL,
    requests INTEGER NOT NULL DEFAULT 0,
    input_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    cost_microusd INTEGER NOT NULL DEFAULT 0,
    credits_milli INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (project_id, period, period_start)
);
CREATE INDEX idx_project_quota_period
ON project_quota_counters(period, period_start);
`,
	},
	{
		version: 6,
		sql: `
ALTER TABLE outbox_events ADD COLUMN claimed_by TEXT;
ALTER TABLE outbox_events ADD COLUMN lease_until INTEGER;
CREATE INDEX idx_outbox_lease ON outbox_events(status, available_at, lease_until);
`,
	},
	{
		version: 7,
		sql: `
CREATE TABLE provider_connections (
    id TEXT PRIMARY KEY,
    principal_id TEXT NOT NULL REFERENCES principals(id) ON DELETE CASCADE,
    provider_id TEXT NOT NULL,
    connection_name TEXT NOT NULL,
    credential_kind TEXT NOT NULL,
    source TEXT NOT NULL DEFAULT 'user'
        CHECK (source IN ('user','admin','config','migration')),
    private_to_principal INTEGER NOT NULL DEFAULT 1
        CHECK (private_to_principal IN (0,1)),
    is_default INTEGER NOT NULL DEFAULT 0 CHECK (is_default IN (0,1)),
    ciphertext BLOB NOT NULL,
    nonce BLOB NOT NULL,
    key_version INTEGER NOT NULL DEFAULT 1,
    aad_version INTEGER NOT NULL DEFAULT 2 CHECK (aad_version IN (1,2)),
    status TEXT NOT NULL DEFAULT 'active'
        CHECK (status IN ('active','disabled','revoked')),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    last_used_at INTEGER,
    UNIQUE (principal_id, provider_id, connection_name)
);

INSERT INTO provider_connections(
    id,principal_id,provider_id,connection_name,credential_kind,source,
    private_to_principal,is_default,ciphertext,nonce,key_version,aad_version,
    status,created_at,updated_at,last_used_at
)
SELECT
    id,principal_id,provider_id,'default',credential_kind,'migration',
    1,1,ciphertext,nonce,key_version,1,status,created_at,updated_at,last_used_at
FROM provider_credentials;

CREATE INDEX idx_provider_connections_owner
ON provider_connections(principal_id,provider_id,status);
CREATE INDEX idx_provider_connections_source
ON provider_connections(source,provider_id,status);
CREATE UNIQUE INDEX idx_provider_connections_active_default
ON provider_connections(principal_id,provider_id)
WHERE is_default=1 AND status='active';
`,
	},
	{
		version: 8,
		sql: `
CREATE TABLE provider_account_state (
    connection_id TEXT PRIMARY KEY REFERENCES provider_connections(id) ON DELETE CASCADE,
    priority INTEGER NOT NULL DEFAULT 100 CHECK (priority >= 0),
    health_status TEXT NOT NULL DEFAULT 'unknown'
        CHECK (health_status IN ('unknown','healthy','degraded','cooldown','error')),
    consecutive_failures INTEGER NOT NULL DEFAULT 0 CHECK (consecutive_failures >= 0),
    cooldown_until INTEGER,
    token_expires_at INTEGER,
    account_label TEXT,
    account_tier TEXT,
    proxy_ref TEXT,
    last_quota_refresh INTEGER,
    last_success_at INTEGER,
    last_failure_at INTEGER,
    last_health_event_at_ns INTEGER,
    last_failure_code TEXT,
    updated_at INTEGER NOT NULL
);

INSERT INTO provider_account_state(connection_id, updated_at)
SELECT id, updated_at FROM provider_connections;

CREATE INDEX idx_provider_account_routing
ON provider_account_state(health_status,cooldown_until,priority);
CREATE INDEX idx_provider_account_quota_refresh
ON provider_account_state(last_quota_refresh);
`,
	},
	{
		version: 9,
		sql: `
CREATE TABLE provider_quota_snapshots (
    id TEXT PRIMARY KEY,
    connection_id TEXT NOT NULL REFERENCES provider_connections(id) ON DELETE CASCADE,
    metric TEXT NOT NULL,
    unit TEXT NOT NULL,
    window TEXT NOT NULL,
    label TEXT,
    limit_value REAL,
    used_value REAL,
    remaining_value REAL,
    reset_at INTEGER,
    source TEXT NOT NULL,
    confidence TEXT NOT NULL
        CHECK (confidence IN ('verified','manual','unknown')),
    refreshed_at INTEGER NOT NULL,
    expires_at INTEGER,
    metadata_json TEXT NOT NULL DEFAULT '{}',
    UNIQUE (connection_id, metric, window)
);

CREATE INDEX idx_provider_quota_connection
ON provider_quota_snapshots(connection_id,refreshed_at);
CREATE INDEX idx_provider_quota_refresh
ON provider_quota_snapshots(expires_at,refreshed_at);
`,
	},
	{
		version: 10,
		sql: `
ALTER TABLE provider_account_state
ADD COLUMN credential_revision INTEGER NOT NULL DEFAULT 1;

ALTER TABLE provider_quota_snapshots
ADD COLUMN credential_revision INTEGER NOT NULL DEFAULT 1;
`,
	},
	{
		version: 11,
		sql: `
CREATE TABLE provider_checks (
    provider_id TEXT NOT NULL,
    operation TEXT NOT NULL CHECK (operation IN ('reachability','catalog_sync','cache_reset','verify')),
    success INTEGER NOT NULL CHECK (success IN (0,1)),
    detail TEXT NOT NULL DEFAULT '',
    model TEXT NOT NULL DEFAULT '',
    latency_ms INTEGER NOT NULL DEFAULT 0,
    checked_at INTEGER NOT NULL,
    PRIMARY KEY (provider_id, operation)
);
`,
	},
	{
		version: 12,
		sql: `
CREATE TABLE provider_credential_bindings (
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    provider_id TEXT NOT NULL,
    principal_kind TEXT NOT NULL CHECK (principal_kind IN ('human','service','system')),
    credential_id TEXT NOT NULL REFERENCES provider_credentials(id) ON DELETE CASCADE,
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','disabled','revoked')),
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (project_id, provider_id, principal_kind)
);
CREATE INDEX idx_provider_credential_bindings_credential
ON provider_credential_bindings(credential_id);
`,
	},
	{
		version: 13,
		sql: `
CREATE TABLE provider_checks_v2 (
    provider_id TEXT NOT NULL,
    operation TEXT NOT NULL CHECK (operation IN ('reachability','catalog_sync','cache_reset','verify')),
    scope_key TEXT NOT NULL DEFAULT '',
    connection_id TEXT NOT NULL DEFAULT '',
    credential_revision INTEGER NOT NULL DEFAULT 0 CHECK (credential_revision >= 0),
    generation INTEGER NOT NULL DEFAULT 0 CHECK (generation >= 0),
    success INTEGER NOT NULL CHECK (success IN (0,1)),
    detail TEXT NOT NULL DEFAULT '',
    model TEXT NOT NULL DEFAULT '',
    latency_ms INTEGER NOT NULL DEFAULT 0,
    checked_at INTEGER NOT NULL,
    PRIMARY KEY (provider_id, operation, scope_key)
);
DROP TABLE provider_checks;
ALTER TABLE provider_checks_v2 RENAME TO provider_checks;
CREATE TABLE provider_check_generations (
    provider_id TEXT NOT NULL,
    scope_key TEXT NOT NULL DEFAULT '',
    generation INTEGER NOT NULL DEFAULT 0 CHECK (generation >= 0),
    PRIMARY KEY (provider_id, scope_key)
);
`,
	},
	{
		version: 14,
		sql: `
ALTER TABLE api_keys ADD COLUMN secret_ciphertext BLOB;
ALTER TABLE api_keys ADD COLUMN secret_nonce BLOB;
`,
	},
	{
		version: 15,
		sql: `
CREATE INDEX idx_outbox_delivered_retention
ON outbox_events(delivered_at) WHERE status='delivered';
`,
	},
}

func SchemaVersion() int { return len(migrations) }

func migrationApplied(db *sql.DB, version int) (bool, error) {
	var count int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM schema_migrations WHERE version = ?", version,
	).Scan(&count); err != nil {
		return false, err
	}
	return count != 0, nil
}

func schemaObjectMatches(
	db *sql.DB, objectType, name, expectedSQL string,
) (bool, bool, error) {
	var actualSQL sql.NullString
	err := db.QueryRow(`
SELECT sql FROM sqlite_master WHERE type = ? AND name = ?`,
		objectType, name,
	).Scan(&actualSQL)
	if err == sql.ErrNoRows {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	normalize := func(value string) string {
		value = strings.TrimSuffix(strings.TrimSpace(value), ";")
		return strings.Join(strings.Fields(value), " ")
	}
	return true, actualSQL.Valid && normalize(actualSQL.String) == normalize(expectedSQL), nil
}

func providerConnectionsMigrationComplete(db *sql.DB) (bool, error) {
	objects := []struct {
		objectType string
		name       string
		sql        string
	}{
		{"table", "provider_connections", providerConnectionsTableSchema},
		{"index", "idx_provider_connections_owner", providerConnectionsOwnerIndexSchema},
		{"index", "idx_provider_connections_source", providerConnectionsSourceIndexSchema},
		{"index", "idx_provider_connections_active_default", providerConnectionsDefaultIndexSchema},
	}
	for _, object := range objects {
		exists, matches, err := schemaObjectMatches(
			db, object.objectType, object.name, object.sql,
		)
		if err != nil {
			return false, err
		}
		if !exists || !matches {
			return false, nil
		}
	}
	var missingConnections int
	if err := db.QueryRow(`
SELECT COUNT(*)
FROM provider_credentials AS credential
LEFT JOIN provider_connections AS connection ON connection.id = credential.id
WHERE connection.id IS NULL`).Scan(&missingConnections); err != nil {
		return false, err
	}
	return missingConnections == 0, nil
}

func legacyProviderBindingsMigrationComplete(db *sql.DB) (bool, error) {
	objects := []struct {
		objectType string
		name       string
		sql        string
	}{
		{"table", "provider_credential_bindings", providerCredentialBindingsTableSchema},
		{"index", "idx_provider_credential_bindings_credential", providerCredentialBindingsIndexSchema},
	}
	for _, object := range objects {
		exists, matches, err := schemaObjectMatches(
			db, object.objectType, object.name, object.sql,
		)
		if err != nil {
			return false, err
		}
		if !exists || !matches {
			return false, nil
		}
	}
	return true, nil
}

func normalizeLegacyProviderBindingMigration(db *sql.DB) error {
	versionSevenApplied, err := migrationApplied(db, 7)
	if err != nil {
		return fmt.Errorf("check legacy migration 7: %w", err)
	}
	versionTwelveApplied, err := migrationApplied(db, 12)
	if err != nil {
		return fmt.Errorf("check migration 12: %w", err)
	}
	if !versionSevenApplied || versionTwelveApplied {
		return nil
	}

	connectionsExist, _, err := schemaObjectMatches(
		db, "table", "provider_connections", providerConnectionsTableSchema,
	)
	if err != nil {
		return fmt.Errorf("check provider_connections: %w", err)
	}
	bindingsExist, _, err := schemaObjectMatches(
		db, "table", "provider_credential_bindings", providerCredentialBindingsTableSchema,
	)
	if err != nil {
		return fmt.Errorf("check provider_credential_bindings: %w", err)
	}
	if connectionsExist {
		complete, err := providerConnectionsMigrationComplete(db)
		if err != nil {
			return fmt.Errorf("validate provider_connections migration: %w", err)
		}
		if !complete {
			return fmt.Errorf(
				"migration 7 is recorded but the provider_connections migration is incomplete",
			)
		}
		if bindingsExist {
			return fmt.Errorf(
				"migration 7 is ambiguous: both provider_connections and the legacy binding table exist",
			)
		}
		return nil
	}
	if !bindingsExist {
		return fmt.Errorf(
			"migration 7 is recorded but neither the current nor legacy schema object exists",
		)
	}
	complete, err := legacyProviderBindingsMigrationComplete(db)
	if err != nil {
		return fmt.Errorf("validate legacy provider credential binding migration: %w", err)
	}
	if !complete {
		return fmt.Errorf(
			"legacy provider credential binding migration is incomplete",
		)
	}

	// v0.3.4 assigned version 7 to the binding migration that is now version 12.
	// Preserve that applied migration before versions 7-11 add provider accounts.
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin legacy migration normalization: %w", err)
	}
	result, err := tx.Exec(
		"UPDATE schema_migrations SET version = 12 WHERE version = 7",
	)
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("normalize legacy migration 7: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("count normalized migration rows: %w", err)
	}
	if affected != 1 {
		_ = tx.Rollback()
		return fmt.Errorf("normalize legacy migration 7: updated %d rows, want 1", affected)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit legacy migration normalization: %w", err)
	}
	return nil
}

func applyMigrations(db *sql.DB) error {
	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS schema_migrations (
    version INTEGER PRIMARY KEY,
    applied_at INTEGER NOT NULL
)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	if err := normalizeLegacyProviderBindingMigration(db); err != nil {
		return err
	}
	for _, m := range migrations {
		exists, err := migrationApplied(db, m.version)
		if err != nil {
			return fmt.Errorf("check migration %d: %w", m.version, err)
		}
		if exists {
			continue
		}
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", m.version, err)
		}
		if _, err := tx.Exec(m.sql); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %d: %w", m.version, err)
		}
		if _, err := tx.Exec(
			"INSERT INTO schema_migrations(version, applied_at) VALUES(?, ?)",
			m.version, time.Now().Unix(),
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %d: %w", m.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", m.version, err)
		}
	}
	return nil
}
