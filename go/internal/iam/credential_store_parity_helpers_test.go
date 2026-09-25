package iam

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"
)

const parityProvider = "fixture-oauth"

// parityEnvelope is the stored OAuth state every parity connection starts with.
func parityEnvelope(name string) OAuthTokenEnvelope {
	return OAuthTokenEnvelope{
		AccessToken: "fixture-access-" + name, RefreshToken: "fixture-refresh-" + name,
		IDToken: "fixture-id-" + name, TokenType: "Bearer", ExpiresAt: 1_900_000_000,
		AccountID: "fixture-account-" + name, AccountLabel: "fixture-label-" + name,
		ProjectID: "fixture-project", OAuthProfile: "browser_pkce", OAuthClientID: "fixture-client",
		OAuthClientMode: "public", OAuthRedirectURI: "http://localhost/callback",
		OAuthClientSecret: "fixture-client-secret", Status: "active",
	}
}

// insertParityConnection writes an OAuth connection directly, because the
// gateway API lets only humans own one and the parity cases need service and
// system owners too. Its account state is deliberately unhealthy, so a health
// reset shows.
func insertParityConnection(
	t *testing.T, db *sql.DB, id, principalID, name string, isDefault bool, updatedAt int64,
) {
	t.Helper()
	raw, err := encodeOAuthEnvelope(parityEnvelope(name))
	if err != nil {
		t.Fatal(err)
	}
	key, err := credentialKey()
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, nonce, err := encryptCredential(key, []byte(raw), connectionAAD(principalID, parityProvider, name, 2))
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO provider_connections(id,principal_id,provider_id,connection_name,credential_kind,source,
    private_to_principal,is_default,ciphertext,nonce,status,created_at,updated_at)
VALUES(?,?,?,?,'github_oauth','admin',(SELECT kind='human' FROM principals WHERE id=?),?,?,?,'active',0,?)`,
			[]any{id, principalID, parityProvider, name, principalID, boolInt(isDefault), ciphertext, nonce, updatedAt}},
		{`INSERT INTO provider_account_state(connection_id,priority,credential_revision,health_status,
    consecutive_failures,cooldown_until,token_expires_at,account_label,last_quota_refresh,
    last_success_at,last_failure_at,last_health_event_at_ns,last_failure_code,updated_at)
VALUES(?,100,3,'cooldown',2,1900000100,1900000000,?,1800000000,1800000001,1800000002,5,'http_429',0)`,
			[]any{id, "fixture-label-" + name}},
		{`INSERT INTO provider_quota_snapshots(id,connection_id,metric,unit,window,source,confidence,
    refreshed_at,credential_revision) VALUES(?,?,'requests','count','day','fixture','verified',0,3)`,
			[]any{"quota_" + id, id}},
	} {
		if _, err := db.Exec(statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
}

// seedParityChecks records checks, generations and model evidence in the
// owner's scope, the gateway-wide scope and an unrelated scope, so the scope
// each write invalidates is visible.
func seedParityChecks(t *testing.T, db *sql.DB, scopes ...string) {
	t.Helper()
	for index, scope := range scopes {
		for _, statement := range []string{
			`INSERT INTO provider_check_generations(provider_id,scope_key,generation) VALUES(?,?,?)`,
			`INSERT INTO provider_checks(provider_id,operation,scope_key,generation,success,checked_at)
VALUES(?,'verify',?,?,1,0)`,
			`INSERT INTO provider_model_evidence(provider_id,scope_key,model,operation,state,observed_at,generation)
VALUES(?,?,'fixture-model','completion','verified',0,?)`,
		} {
			if _, err := db.Exec(statement, parityProvider, scope, index+2); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// paritySnapshot renders the rows a credential write may change, without
// timestamps a write sets to now or ciphertext bytes, which differ on every
// encryption. Connection secrets are decrypted and compared instead.
func paritySnapshot(t *testing.T, db *sql.DB) map[string][]string {
	t.Helper()
	snapshot := map[string][]string{}
	for table, query := range map[string]string{
		"provider_connections": `SELECT id,principal_id,provider_id,connection_name,status,is_default,
credential_kind,source,private_to_principal,key_version,aad_version,ciphertext,nonce FROM provider_connections`,
		"provider_account_state": `SELECT connection_id,priority,credential_revision,health_status,
consecutive_failures,COALESCE(cooldown_until,-1),COALESCE(token_expires_at,-1),COALESCE(account_label,'<null>'),
COALESCE(last_quota_refresh,-1),COALESCE(last_success_at,-1),COALESCE(last_failure_at,-1),
COALESCE(last_health_event_at_ns,-1),COALESCE(last_failure_code,'<null>') FROM provider_account_state`,
		"provider_quota_snapshots":   `SELECT id,connection_id,metric,window,credential_revision FROM provider_quota_snapshots`,
		"provider_check_generations": `SELECT provider_id,scope_key,generation FROM provider_check_generations`,
		"provider_checks":            `SELECT provider_id,operation,scope_key,generation,success FROM provider_checks`,
		"provider_model_evidence":    `SELECT provider_id,scope_key,model,operation,state FROM provider_model_evidence`,
	} {
		rows, err := db.Query(query)
		if err != nil {
			t.Fatal(err)
		}
		columns, _ := rows.Columns()
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for index := range values {
				pointers[index] = &values[index]
			}
			if err := rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			if table == "provider_connections" {
				values = parityConnectionRow(t, values)
			}
			snapshot[table] = append(snapshot[table], fmt.Sprint(values...))
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		sort.Strings(snapshot[table])
	}
	return snapshot
}

// parityConnectionRow swaps a connection's ciphertext and nonce for its
// decrypted envelope in canonical form.
func parityConnectionRow(t *testing.T, values []any) []any {
	t.Helper()
	text := func(index int) string { return fmt.Sprint(values[index]) }
	aadVersion, _ := values[10].(int64)
	ciphertext, _ := values[11].([]byte)
	nonce, _ := values[12].([]byte)
	key, err := credentialKey()
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := decryptCredential(key, ciphertext, nonce,
		connectionAAD(text(1), text(2), text(3), int(aadVersion)))
	if err != nil {
		t.Fatalf("decrypt connection %s: %v", text(0), err)
	}
	envelope, err := decodeOAuthEnvelope(string(plaintext))
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := encodeOAuthEnvelope(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return append(append([]any{}, values[:11]...), " envelope=", canonical)
}

// assertSameSnapshot reports every table whose rows differ.
func assertSameSnapshot(t *testing.T, name string, want, got map[string][]string) {
	t.Helper()
	var diffs []string
	for table, rows := range want {
		if strings.Join(rows, "\n") != strings.Join(got[table], "\n") {
			diffs = append(diffs, fmt.Sprintf("%s:\n  gateway: %q\n  store:   %q", table, rows, got[table]))
		}
	}
	sort.Strings(diffs)
	if len(diffs) > 0 {
		t.Fatalf("%s: the store's write differs from the gateway's:\n%s", name, strings.Join(diffs, "\n"))
	}
}
