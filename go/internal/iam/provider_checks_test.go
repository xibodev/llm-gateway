package iam

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"llmgw/internal/diagnostics"
)

func TestProviderCheckMigrationScopesAndSanitizesLegacyRows(t *testing.T) {
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
	for _, migration := range migrations[:12] {
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
	if _, err := db.Exec(`
	INSERT INTO provider_checks(
	    provider_id,operation,success,detail,model,latency_ms,checked_at
	) VALUES('copilot','reachability',0,'raw secret-shaped upstream detail','',10,100)`); err != nil {
		t.Fatal(err)
	}
	if err := applyMigrations(db); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM provider_checks").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("legacy provider checks were retained: %d", count)
	}
}

func TestCredentialRotationInvalidatesScopedChecks(t *testing.T) {
	setupConnectionTest(t)
	human, err := CreatePrincipal("human", "authentik:check-rotation", "", "Owner")
	if err != nil {
		t.Fatal(err)
	}

	connection, err := PutProviderConnection(ProviderConnectionCreate{
		PrincipalID: human.ID, ProviderID: "fixture", Name: "personal",
		Kind: "api_key", Secret: "first", Source: ConnectionSourceUser,
		MakeDefault: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	observation, found, err := ActiveProviderAccountObservation(human.ID, "fixture")
	if err != nil || !found {
		t.Fatalf("observation=%+v found=%v err=%v", observation, found, err)
	}
	generation, err := ProviderCheckGeneration("fixture", human.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := RecordProviderCheck(ProviderCheck{
		ProviderID: "fixture", Operation: CheckVerify, ScopeKey: human.ID,
		ConnectionID: connection.ID, CredentialRevision: observation.CredentialRevision,
		Generation: generation, Success: true, Detail: "Provider check passed.",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := PutProviderConnection(ProviderConnectionCreate{
		PrincipalID: human.ID, ProviderID: "fixture", Name: "personal",
		Kind: "api_key", Secret: "second", Source: ConnectionSourceUser,
		MakeDefault: true,
	}); err != nil {
		t.Fatal(err)
	}
	checks, err := LastProviderChecks(human.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(checks["fixture"]) != 0 {
		t.Fatalf("rotated credential retained checks: %+v", checks)
	}
	if err := RecordProviderCheck(ProviderCheck{
		ProviderID: "fixture", Operation: CheckVerify, ScopeKey: human.ID,
		ConnectionID: connection.ID, CredentialRevision: observation.CredentialRevision,
		Generation: generation, Success: false, Detail: "stale",
	}); err != nil {
		t.Fatal(err)
	}
	checks, err = LastProviderChecks(human.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(checks["fixture"]) != 0 {
		t.Fatalf("stale check was restored: %+v", checks)
	}
}

func TestProviderDeletionRetainsGenerationTombstone(t *testing.T) {
	setupConnectionTest(t)
	generation, err := ProviderCheckGeneration("fixture", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := RecordProviderCheck(ProviderCheck{
		ProviderID: "fixture", Operation: CheckReachability,
		Generation: generation, Success: true, Detail: "current",
	}); err != nil {
		t.Fatal(err)
	}
	if err := DeleteProviderChecks("fixture"); err != nil {
		t.Fatal(err)
	}
	if err := RecordProviderCheck(ProviderCheck{
		ProviderID: "fixture", Operation: CheckReachability,
		Generation: generation, Success: false, Detail: "stale",
	}); err != nil {
		t.Fatal(err)
	}
	checks, err := LastProviderChecks("")
	if err != nil {
		t.Fatal(err)
	}
	if len(checks["fixture"]) != 0 {
		t.Fatalf("deleted provider resurrected stale checks: %+v", checks)
	}
	current, err := ProviderCheckGeneration("fixture", "")
	if err != nil || current <= generation {
		t.Fatalf("generation current=%d previous=%d err=%v", current, generation, err)
	}
}

func TestEveryCredentialStoreInvalidatesRelevantChecks(t *testing.T) {
	setupConnectionTest(t)
	human, err := CreatePrincipal("human", "authentik:all-stores", "", "Owner")
	if err != nil {
		t.Fatal(err)
	}

	if err := RecordProviderCheck(ProviderCheck{
		ProviderID: "legacy", Operation: CheckVerify, ScopeKey: human.ID,
		Success: true, Detail: "old",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := PutProviderCredential(human.ID, "legacy", "api_key", "new"); err != nil {
		t.Fatal(err)
	}
	checks, err := LastProviderChecks(human.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(checks["legacy"]) != 0 {
		t.Fatalf("legacy rotation retained checks: %+v", checks)
	}

	connection, err := PutOAuthProviderConnection(OAuthConnectionCreate{
		PrincipalID: human.ID, ProviderID: "copilot", Kind: "github_oauth",
		AccessToken: "first",
	})
	if err != nil {
		t.Fatal(err)
	}
	observation, found, err := ActiveProviderAccountObservation(human.ID, "copilot")
	if err != nil || !found {
		t.Fatalf("observation=%+v found=%v err=%v", observation, found, err)
	}
	generation, err := ProviderCheckGeneration("copilot", human.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := RecordProviderCheck(ProviderCheck{
		ProviderID: "copilot", Operation: CheckVerify, ScopeKey: human.ID,
		ConnectionID: connection.ID, CredentialRevision: observation.CredentialRevision,
		Generation: generation, Success: true, Detail: "old",
	}); err != nil {
		t.Fatal(err)
	}
	envelope, current, ok, err := OAuthProviderConnectionSecret(human.ID, "copilot", "")
	if err != nil || !ok {
		t.Fatalf("OAuth connection ok=%v err=%v", ok, err)
	}
	if _, err := ReplaceOAuthProviderConnectionIfCurrent(
		current, envelope, OAuthConnectionCreate{
			PrincipalID: human.ID, ProviderID: "copilot", Name: current.Name,
			Kind: current.Kind, Source: current.Source, MakeDefault: current.IsDefault,
			AccessToken: "second",
		},
	); err != nil {
		t.Fatal(err)
	}
	checks, err = LastProviderChecks(human.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(checks["copilot"]) != 0 {
		t.Fatalf("OAuth rotation retained checks: %+v", checks)
	}

	if err := RecordProviderCheck(ProviderCheck{
		ProviderID: "shared", Operation: CheckVerify, ScopeKey: human.ID,
		Success: true, Detail: "human",
	}); err != nil {
		t.Fatal(err)
	}
	if err := RecordProviderCheck(ProviderCheck{
		ProviderID: "shared", Operation: CheckVerify,
		Success: true, Detail: "global",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := PutGatewayProviderCredential("shared", "api_key", "system"); err != nil {
		t.Fatal(err)
	}
	all, err := LastProviderChecks("")
	if err != nil {
		t.Fatal(err)
	}
	if len(all["shared"]) != 0 {
		t.Fatalf("system rotation retained checks: %+v", all)
	}
}

func TestProviderChecksAreScopedPerPrincipal(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	ResetForTests()
	t.Cleanup(ResetForTests)
	if _, err := Initialize(); err != nil {
		t.Fatal(err)
	}

	for _, check := range []ProviderCheck{
		{
			ProviderID: "copilot", Operation: CheckReachability,
			ScopeKey: "owner-one", Success: false, Detail: "first", CheckedAt: 100,
		},
		{
			ProviderID: "copilot", Operation: CheckReachability,
			ScopeKey: "owner-two", Success: true, Detail: "second", CheckedAt: 200,
		},
	} {
		if err := RecordProviderCheck(check); err != nil {
			t.Fatal(err)
		}
	}

	first, err := LastProviderChecks("owner-one")
	if err != nil {
		t.Fatal(err)
	}
	if len(first["copilot"]) != 1 || first["copilot"][0].Success ||
		first["copilot"][0].Detail != "first" {
		t.Fatalf("owner-one checks=%+v", first)
	}
	second, err := LastProviderChecks("owner-two")
	if err != nil {
		t.Fatal(err)
	}
	if len(second["copilot"]) != 1 || !second["copilot"][0].Success ||
		second["copilot"][0].Detail != "second" {
		t.Fatalf("owner-two checks=%+v", second)
	}
	all, err := LastProviderChecks("")
	if err != nil {
		t.Fatal(err)
	}
	if len(all["copilot"]) != 2 {
		t.Fatalf("admin checks=%+v", all)
	}
}

func TestProviderChecksSanitizePersistenceAndHistoricalRows(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	ResetForTests()
	t.Cleanup(ResetForTests)
	if _, err := Initialize(); err != nil {
		t.Fatal(err)
	}
	db, err := DB()
	if err != nil {
		t.Fatal(err)
	}
	secret := "llmgw_" + strings.Repeat("a", 32)
	model := "anthropic/model-ThisIdentifierSegmentIsLongEnough"
	unsafe := "界" + strings.Repeat("x", maxProviderCheckChars-14) + " " + secret + strings.Repeat("界", 20) + " owner@example.test"
	if err := RecordProviderCheck(ProviderCheck{
		ProviderID: "fixture", Operation: CheckVerify, ScopeKey: "owner-one",
		Generation: 0, Success: true, Detail: unsafe, Model: model,
		LatencyMS: 17, CheckedAt: 123,
	}); err != nil {
		t.Fatal(err)
	}
	var rawDetail, rawModel string
	if err := db.QueryRow(`SELECT detail,model FROM provider_checks
		WHERE provider_id='fixture' AND operation='verify' AND scope_key='owner-one'`).Scan(
		&rawDetail, &rawModel,
	); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"detail": rawDetail, "model": rawModel} {
		if strings.Contains(value, "llmgw_") || strings.Contains(value, "owner@example.test") ||
			utf8.RuneCountInString(value) > maxProviderCheckChars ||
			!utf8.ValidString(value) {
			t.Fatalf("unsafe persisted %s: %q", name, value)
		}
	}
	if rawModel != model {
		t.Fatalf("functional model persisted as %q, want %q", rawModel, model)
	}
	if !strings.Contains(rawDetail, diagnostics.Redacted) {
		t.Fatalf("secret crossing limit was truncated before sanitization: %q", rawDetail)
	}

	historicalDetail := "api_key=" + secret + " owner@example.test"
	if _, err := db.Exec(`INSERT INTO provider_checks(
		provider_id,operation,scope_key,connection_id,credential_revision,generation,
		success,detail,model,latency_ms,checked_at
	) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		"fixture", CheckCatalogSync, "owner-two", "", 0, 0, 0,
		historicalDetail, "Bearer historical-token", 29, 456,
	); err != nil {
		t.Fatal(err)
	}
	checks, err := LastProviderChecks("owner-two")
	if err != nil {
		t.Fatal(err)
	}
	if len(checks["fixture"]) != 1 {
		t.Fatalf("scoped checks=%+v", checks)
	}
	got := checks["fixture"][0]
	if got.Operation != CheckCatalogSync || got.Success || got.ScopeKey != "owner-two" ||
		got.Generation != 0 || got.LatencyMS != 29 || got.CheckedAt != 456 {
		t.Fatalf("check semantics changed: %+v", got)
	}
	if strings.Contains(got.Detail, secret) || strings.Contains(got.Detail, "owner@example.test") ||
		strings.Contains(got.Model, "historical-token") {
		t.Fatalf("historical check was not sanitized: %+v", got)
	}
	ownerOne, err := LastProviderChecks("owner-one")
	if err != nil || len(ownerOne["fixture"]) != 1 || ownerOne["fixture"][0].Model != model {
		t.Fatalf("functional model did not survive read: checks=%+v err=%v", ownerOne, err)
	}
}
