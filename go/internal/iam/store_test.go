package iam

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

const v034MigrationSevenSQL = `
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
`

const partialProviderConnectionsSQL = `
CREATE TABLE provider_connections (
    id TEXT PRIMARY KEY,
    principal_id TEXT NOT NULL,
    provider_id TEXT NOT NULL,
    status TEXT NOT NULL,
    is_default INTEGER NOT NULL
);
CREATE INDEX idx_provider_connections_owner
ON provider_connections(principal_id,provider_id);
CREATE INDEX idx_provider_connections_source
ON provider_connections(provider_id,status);
CREATE UNIQUE INDEX idx_provider_connections_active_default
ON provider_connections(principal_id,provider_id)
WHERE is_default=1;
`

const partialProviderCredentialBindingsSQL = `
CREATE TABLE provider_credential_bindings (
    credential_id TEXT NOT NULL
);
CREATE INDEX idx_provider_credential_bindings_credential
ON provider_credential_bindings(credential_id);
`

func TestDBCreatesControlPlaneSchema(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	ResetForTests()
	t.Cleanup(ResetForTests)

	db, err := DB()
	if err != nil {
		t.Fatalf("DB: %v", err)
	}
	for _, table := range []string{
		"principals", "projects", "project_memberships", "api_keys",
		"provider_credentials", "provider_connections", "usage_events", "audit_events",
		"provider_account_state", "provider_quota_snapshots", "alert_rules", "outbox_events",
	} {
		var got string
		err := db.QueryRow(
			"SELECT name FROM sqlite_master WHERE type='table' AND name=?", table,
		).Scan(&got)
		if err != nil || got != table {
			t.Fatalf("table %s missing: got=%q err=%v", table, got, err)
		}
	}
	var version int
	if err := db.QueryRow("SELECT MAX(version) FROM schema_migrations").Scan(&version); err != nil {
		t.Fatalf("schema version: %v", err)
	}
	if version != len(migrations) {
		t.Fatalf("schema version = %d, want %d", version, len(migrations))
	}
	path := filepath.Join(os.Getenv("LLMGW_STATE_DIR"), "gateway.db")
	if info, err := os.Stat(path); err != nil {
		t.Fatalf("database file: %v", err)
	} else if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("database permissions = %o, want no group/other bits", info.Mode().Perm())
	}
}

func TestMigrationsPreserveExistingIdentityData(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:6] {
		if _, err := db.Exec(migration.sql); err != nil {
			t.Fatalf("apply migration %d: %v", migration.version, err)
		}
		if _, err := db.Exec(
			"INSERT INTO schema_migrations(version,applied_at) VALUES(?,0)", migration.version,
		); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`
INSERT INTO principals(id,kind,display_name,status,created_at,updated_at)
VALUES('prn_existing','service','Existing','active',0,0)`); err != nil {
		t.Fatal(err)
	}
	if err := applyMigrations(db); err != nil {
		t.Fatal(err)
	}
	var name string
	if err := db.QueryRow(
		"SELECT display_name FROM principals WHERE id='prn_existing'",
	).Scan(&name); err != nil || name != "Existing" {
		t.Fatalf("existing principal name=%q err=%v", name, err)
	}
	var table string
	if err := db.QueryRow(`
SELECT name FROM sqlite_master
WHERE type='table' AND name='provider_credential_bindings'`).Scan(&table); err != nil {
		t.Fatal(err)
	}
}

func TestAPIKeyRecoveryMigrationPreservesHashOnlyKeys(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:13] {
		if _, err := db.Exec(migration.sql); err != nil {
			t.Fatalf("apply migration %d: %v", migration.version, err)
		}
		if _, err := db.Exec(
			"INSERT INTO schema_migrations(version,applied_at) VALUES(?,0)", migration.version,
		); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`
INSERT INTO principals(id,kind,display_name,status,created_at,updated_at)
VALUES('prn_key','service','Existing key','active',0,0);
INSERT INTO projects(id,slug,name,status,created_at,updated_at)
VALUES('prj_key','existing-key','Existing key','active',0,0);
INSERT INTO project_memberships(project_id,principal_id,role,created_at)
VALUES('prj_key','prn_key','member',0);
INSERT INTO api_keys(id,prefix,secret_hash,project_id,principal_id,name,status,created_at)
VALUES('key_existing','llmgw_existing...',X'0102','prj_key','prn_key','Existing','active',0);`); err != nil {
		t.Fatal(err)
	}
	if err := applyMigrations(db); err != nil {
		t.Fatal(err)
	}
	var prefix string
	var hash, ciphertext, nonce []byte
	if err := db.QueryRow(`
SELECT prefix,secret_hash,secret_ciphertext,secret_nonce
FROM api_keys WHERE id='key_existing'`).Scan(&prefix, &hash, &ciphertext, &nonce); err != nil {
		t.Fatal(err)
	}
	if prefix != "llmgw_existing..." || !bytes.Equal(hash, []byte{1, 2}) {
		t.Fatalf("existing key changed: prefix=%q hash=%x", prefix, hash)
	}
	if len(ciphertext) != 0 || len(nonce) != 0 {
		t.Fatal("existing hash-only key unexpectedly gained recovery material")
	}
}

func TestMigrationsUpgradeV034Schema(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:6] {
		if _, err := db.Exec(migration.sql); err != nil {
			t.Fatalf("apply migration %d: %v", migration.version, err)
		}
		if _, err := db.Exec(
			"INSERT INTO schema_migrations(version,applied_at) VALUES(?,?)",
			migration.version, migration.version,
		); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(v034MigrationSevenSQL); err != nil {
		t.Fatalf("apply v0.3.4 migration 7: %v", err)
	}
	if _, err := db.Exec(
		"INSERT INTO schema_migrations(version,applied_at) VALUES(7,7)",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
INSERT INTO principals(id,kind,display_name,status,created_at,updated_at)
VALUES('prn_owner','human','Owner','active',0,0);
INSERT INTO projects(id,slug,name,status,created_at,updated_at)
VALUES('prj_existing','existing','Existing','active',0,0);
INSERT INTO provider_credentials(
    id,principal_id,provider_id,credential_kind,ciphertext,nonce,status,created_at,updated_at
) VALUES('cred_existing','prn_owner','provider-a','api_key',X'01',X'02','active',0,0);
INSERT INTO provider_credential_bindings(
    project_id,provider_id,principal_kind,credential_id,status,created_at,updated_at
) VALUES('prj_existing','provider-a','service','cred_existing','active',0,0);`); err != nil {
		t.Fatal(err)
	}

	if err := applyMigrations(db); err != nil {
		t.Fatalf("upgrade v0.3.4 schema: %v", err)
	}
	if err := applyMigrations(db); err != nil {
		t.Fatalf("repeat upgraded migrations: %v", err)
	}

	var count, minVersion, maxVersion int
	if err := db.QueryRow(`
SELECT COUNT(*), MIN(version), MAX(version) FROM schema_migrations`,
	).Scan(&count, &minVersion, &maxVersion); err != nil {
		t.Fatal(err)
	}
	if count != len(migrations) || minVersion != 1 || maxVersion != len(migrations) {
		t.Fatalf(
			"migration set count=%d min=%d max=%d, want count=%d min=1 max=%d",
			count, minVersion, maxVersion, len(migrations), len(migrations),
		)
	}

	var principalID, connectionName, source string
	var isDefault int
	if err := db.QueryRow(`
SELECT principal_id,connection_name,source,is_default
FROM provider_connections WHERE id='cred_existing'`,
	).Scan(&principalID, &connectionName, &source, &isDefault); err != nil {
		t.Fatal(err)
	}
	if principalID != "prn_owner" || connectionName != "default" ||
		source != "migration" || isDefault != 1 {
		t.Fatalf(
			"migrated connection principal=%q name=%q source=%q default=%d",
			principalID, connectionName, source, isDefault,
		)
	}
	var bindingStatus string
	if err := db.QueryRow(`
SELECT status FROM provider_credential_bindings
WHERE project_id='prj_existing' AND provider_id='provider-a' AND principal_kind='service'`,
	).Scan(&bindingStatus); err != nil {
		t.Fatal(err)
	}
	if bindingStatus != "active" {
		t.Fatalf("binding status=%q", bindingStatus)
	}
	var accountRows int
	if err := db.QueryRow(`
SELECT COUNT(*) FROM provider_account_state WHERE connection_id='cred_existing'`,
	).Scan(&accountRows); err != nil {
		t.Fatal(err)
	}
	if accountRows != 1 {
		t.Fatalf("provider account rows=%d, want 1", accountRows)
	}
	for _, check := range []struct {
		table  string
		column string
	}{
		{table: "provider_account_state", column: "credential_revision"},
		{table: "provider_quota_snapshots", column: "credential_revision"},
	} {
		var columns int
		query := "SELECT COUNT(*) FROM pragma_table_info('" + check.table + "') WHERE name = ?"
		if err := db.QueryRow(query, check.column).Scan(&columns); err != nil {
			t.Fatal(err)
		}
		if columns != 1 {
			t.Fatalf("%s.%s columns=%d, want 1", check.table, check.column, columns)
		}
	}
	var integrity string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil {
		t.Fatal(err)
	}
	if integrity != "ok" {
		t.Fatalf("integrity_check=%q", integrity)
	}
	rows, err := db.Query("PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("foreign_key_check returned a violation")
	}
}

func TestMigrationsRejectAmbiguousPartialV034Upgrade(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:6] {
		if _, err := db.Exec(migration.sql); err != nil {
			t.Fatalf("apply migration %d: %v", migration.version, err)
		}
		if _, err := db.Exec(
			"INSERT INTO schema_migrations(version,applied_at) VALUES(?,?)",
			migration.version, migration.version,
		); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(v034MigrationSevenSQL); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		"INSERT INTO schema_migrations(version,applied_at) VALUES(7,7)",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(partialProviderConnectionsSQL); err != nil {
		t.Fatal(err)
	}

	err = applyMigrations(db)
	if err == nil {
		t.Fatal("partial provider_connections migration should fail closed")
	}
	if got := err.Error(); got !=
		"migration 7 is recorded but the provider_connections migration is incomplete" {
		t.Fatalf("migration error=%q", got)
	}
	var maxVersion int
	if err := db.QueryRow("SELECT MAX(version) FROM schema_migrations").Scan(&maxVersion); err != nil {
		t.Fatal(err)
	}
	if maxVersion != 7 {
		t.Fatalf("max migration version=%d, want 7", maxVersion)
	}
}

func TestMigrationsRejectMalformedLegacyBindingSchema(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:6] {
		if _, err := db.Exec(migration.sql); err != nil {
			t.Fatalf("apply migration %d: %v", migration.version, err)
		}
		if _, err := db.Exec(
			"INSERT INTO schema_migrations(version,applied_at) VALUES(?,?)",
			migration.version, migration.version,
		); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(partialProviderCredentialBindingsSQL); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		"INSERT INTO schema_migrations(version,applied_at) VALUES(7,7)",
	); err != nil {
		t.Fatal(err)
	}

	err = applyMigrations(db)
	if err == nil {
		t.Fatal("malformed legacy binding migration should fail closed")
	}
	if got := err.Error(); got !=
		"legacy provider credential binding migration is incomplete" {
		t.Fatalf("migration error=%q", got)
	}
	var maxVersion int
	if err := db.QueryRow("SELECT MAX(version) FROM schema_migrations").Scan(&maxVersion); err != nil {
		t.Fatal(err)
	}
	if maxVersion != 7 {
		t.Fatalf("max migration version=%d, want 7", maxVersion)
	}
}

func TestDBEnforcesForeignKeys(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	ResetForTests()
	t.Cleanup(ResetForTests)

	db, err := DB()
	if err != nil {
		t.Fatalf("DB: %v", err)
	}
	_, err = db.Exec(`
INSERT INTO project_memberships(project_id, principal_id, role, created_at)
VALUES('missing-project','missing-principal','member',0)`)
	if err == nil {
		t.Fatal("foreign-key violation should fail")
	}
}
