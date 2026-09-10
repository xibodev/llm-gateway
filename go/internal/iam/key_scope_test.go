package iam

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

func TestKeyScopeMigrationPreservesPriorVersionKeys(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LLMGW_STATE_DIR", dir)
	ResetForTests()
	t.Cleanup(ResetForTests)
	db, err := sql.Open("sqlite", filepath.Join(dir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:15] {
		if _, err := db.Exec(migration.sql); err != nil {
			t.Fatalf("migration %d: %v", migration.version, err)
		}
		if _, err := db.Exec("INSERT INTO schema_migrations(version,applied_at) VALUES(?,0)", migration.version); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`
INSERT INTO principals(id,kind,display_name,status,created_at,updated_at) VALUES('owner','human','Owner','active',1,1);
INSERT INTO projects(id,slug,name,status,created_at,updated_at) VALUES('project','scope-migration','Scope migration','active',1,1);
INSERT INTO project_memberships(project_id,principal_id,role,created_at) VALUES('project','owner','owner',1);`); err != nil {
		t.Fatal(err)
	}
	want := KeyPolicy{
		AllowedModels: []string{"smart", "echo/model"}, AllowedProviders: []string{"echo"},
		RPM: 11, DailyRequests: 12, MonthlyRequests: 13, DailyInputTokens: 14,
		DailyOutputTokens: 15, MonthlyTotalTokens: 16, DailyCostMicroUSD: 17,
		MonthlyCostMicroUSD: 18, DailyCreditsMilli: 19, MonthlyCreditsMilli: 20,
	}
	for _, status := range []string{"active", "disabled", "revoked"} {
		hash := sha256.Sum256([]byte("migration-test-" + status))
		if _, err := db.Exec(`INSERT INTO api_keys(
 id,prefix,secret_hash,project_id,principal_id,name,status,created_at,expires_at,last_used_at,
 allowed_models_json,allowed_providers_json,rpm,daily_requests,monthly_requests,daily_input_tokens,
 daily_output_tokens,monthly_total_tokens,daily_cost_microusd,monthly_cost_microusd,daily_credits_milli,monthly_credits_milli
) VALUES(?,?,?,'project','owner','Existing',?,2,4102444800,3,'["smart","echo/model"]','["echo"]',11,12,13,14,15,16,17,18,19,20)`, status, "prefix-"+status, hash[:], status); err != nil {
			t.Fatal(err)
		}
	}
	if err := applyMigrations(db); err != nil {
		t.Fatal(err)
	}
	if err := applyMigrations(db); err != nil {
		t.Fatalf("repeat migration: %v", err)
	}
	for _, status := range []string{"active", "disabled", "revoked"} {
		key, found, err := apiKeyByID(db, status)
		if err != nil || !found {
			t.Fatalf("migrated key: %v", err)
		}
		if key.Status != status || key.ExpiresAt != 4102444800 || key.CreatedAt != 2 || key.LastUsedAt != 3 || key.Revealable || !reflect.DeepEqual(key.Policy, want) {
			t.Fatalf("migration changed key: %+v", key)
		}
		var scope string
		if err := db.QueryRow("SELECT scope_json FROM api_keys WHERE id=?", status).Scan(&scope); err != nil || scope != "{}" {
			t.Fatalf("migrated scope %q: %v", scope, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"active", "disabled", "revoked"} {
		p, found, err := ResolveAPIKey("migration-test-" + status)
		if err != nil || found != (status == "active") {
			t.Fatalf("migrated authentication %s: found=%v err=%v", status, found, err)
		}
		if found && (p.RoutesOnly || len(p.AllowedRoutes) != 0 || p.RPM != want.RPM || !reflect.DeepEqual(p.AllowedModels, want.AllowedModels) || !reflect.DeepEqual(p.AllowedProviders, want.AllowedProviders)) {
			t.Fatalf("migrated principal policy: %+v", p)
		}
	}
}

func scopeTestKey(t *testing.T, policy KeyPolicy) IssuedKey {
	t.Helper()
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	ResetForTests()
	t.Cleanup(ResetForTests)
	principal, err := CreatePrincipal("human", "scope-owner", "", "Scope owner")
	if err != nil {
		t.Fatal(err)
	}
	project, err := CreateProject("scope-project", "Scope project")
	if err != nil {
		t.Fatal(err)
	}
	if err := SetMembership(project.ID, principal.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	key, err := IssueKey(KeyCreate{ProjectID: project.ID, PrincipalID: principal.ID, Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestKeyScopePersistenceAndValidation(t *testing.T) {
	policy := KeyPolicy{AllowedRoutes: []string{"smart"}, RoutesOnly: true, AdminManaged: true, AllowedProviders: []string{"echo"}, RPM: 3}
	key := scopeTestKey(t, policy)
	ResetForTests()
	stored, ok, err := APIKeyByID(key.ID)
	if err != nil || !ok || !reflect.DeepEqual(stored.Policy, policy) {
		t.Fatalf("policy round trip: %+v %v", stored.Policy, err)
	}
	for _, list := range []func() ([]APIKey, error){func() ([]APIKey, error) { return ListAPIKeys(key.ProjectID) }, func() ([]APIKey, error) { return ListPrincipalAPIKeys(key.PrincipalID) }} {
		rows, err := list()
		if err != nil || len(rows) != 1 || !reflect.DeepEqual(rows[0].Policy, policy) {
			t.Fatalf("listed policy: %+v %v", rows, err)
		}
	}
	p, found, err := ResolveAPIKey(key.Token)
	if err != nil || !found || !p.RoutesOnly || !reflect.DeepEqual(p.AllowedRoutes, policy.AllowedRoutes) || p.RPM != 3 {
		t.Fatalf("resolved scope: %+v %v", p, err)
	}
	for _, invalid := range []KeyPolicy{{RoutesOnly: true}, {RoutesOnly: true, AllowedRoutes: []string{" "}}, {AllowedRoutes: []string{" smart"}}} {
		if _, err := IssueKey(KeyCreate{ProjectID: key.ProjectID, PrincipalID: key.PrincipalID, Policy: invalid}); err == nil {
			t.Fatal("invalid scope accepted on create")
		}
		if err := UpdateAPIKey(key.ID, KeyUpdate{Policy: &invalid, Admin: true}); err == nil {
			t.Fatal("invalid scope accepted on update")
		}
	}
}

func TestKeyScopeAdminLockAndStaleOwnerUpdate(t *testing.T) {
	key := scopeTestKey(t, KeyPolicy{})
	stale := key.Policy
	ownerPolicy := KeyPolicy{RPM: 5}
	if err := UpdateAPIKey(key.ID, KeyUpdate{Policy: &ownerPolicy, OwnerPrincipalID: key.PrincipalID}); err != nil {
		t.Fatal(err)
	}
	if err := UpdateAPIKey(key.ID, KeyUpdate{Policy: &ownerPolicy, OwnerPrincipalID: key.PrincipalID, ExpectedPolicy: &stale}); !errors.Is(err, ErrAPIKeyConflict) {
		t.Fatalf("stale owner update: %v", err)
	}
	disabled := "disabled"
	expiry := int64(12345)
	if err := UpdateAPIKey(key.ID, KeyUpdate{Status: &disabled, ExpiresAt: &expiry, Admin: true}); err != nil {
		t.Fatal(err)
	}
	active := "active"
	clearExpiry := int64(0)
	for _, update := range []KeyUpdate{{Policy: &KeyPolicy{}}, {ExpiresAt: &clearExpiry}, {Status: &active}, {Status: &disabled}, {Policy: &ownerPolicy, ExpectedPolicy: &stale}} {
		update.OwnerPrincipalID = key.PrincipalID
		if err := UpdateAPIKey(key.ID, update); !errors.Is(err, ErrAPIKeyAdminManaged) {
			t.Fatalf("locked update: %v", err)
		}
	}
	stored, _, _ := APIKeyByID(key.ID)
	if !stored.Policy.AdminManaged || stored.Status != disabled || stored.ExpiresAt != expiry {
		t.Fatalf("lock lost: %+v", stored)
	}
	if err := UpdateAPIKey(key.ID, KeyUpdate{Policy: &KeyPolicy{RPM: 7}, Admin: true}); err != nil {
		t.Fatal(err)
	}
	stored, _, _ = APIKeyByID(key.ID)
	if !stored.Policy.AdminManaged {
		t.Fatal("admin replacement unlocked key")
	}
	revoked := "revoked"
	if err := UpdateAPIKey(key.ID, KeyUpdate{Status: &revoked, OwnerPrincipalID: key.PrincipalID}); err != nil {
		t.Fatal(err)
	}
}

func TestKeyScopeAdminOwnerRace(t *testing.T) {
	key := scopeTestKey(t, KeyPolicy{})
	for i := 0; i < 20; i++ {
		// Simulate an owner handler that read the policy before the admin write.
		stale := KeyPolicy{}
		adminPolicy := KeyPolicy{AllowedRoutes: []string{"smart"}, RoutesOnly: true, RPM: i + 1}
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			if err := UpdateAPIKey(key.ID, KeyUpdate{Policy: &adminPolicy, Admin: true}); err != nil {
				t.Error(err)
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			err := UpdateAPIKey(key.ID, KeyUpdate{Policy: &stale, OwnerPrincipalID: key.PrincipalID, ExpectedPolicy: &stale})
			if err != nil && !errors.Is(err, ErrAPIKeyAdminManaged) && !errors.Is(err, ErrAPIKeyConflict) {
				t.Error(err)
			}
		}()
		close(start)
		wg.Wait()
		stored, _, err := APIKeyByID(key.ID)
		if err != nil || !stored.Policy.AdminManaged || !stored.Policy.RoutesOnly || stored.Policy.RPM != i+1 {
			t.Fatalf("race broadened policy: %+v %v", stored.Policy, err)
		}
	}
}
