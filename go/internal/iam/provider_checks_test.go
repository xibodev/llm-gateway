package iam

import (
	"database/sql"
	"path/filepath"
	"testing"
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
