package iam

import (
	"encoding/json"
	"testing"
	"time"

	"llmgw/internal/config"
)

func setupAnonymousProviderAutomationTest(t *testing.T) {
	t.Helper()
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	ResetForTests()
	t.Cleanup(ResetForTests)
	if _, err := Initialize(); err != nil {
		t.Fatal(err)
	}
}

func TestManagedMarkerIsBackfilledAndSurvivesAuditRetention(t *testing.T) {
	setupAnonymousProviderAutomationTest(t)
	db, err := DB()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version=18`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM control_metadata WHERE key=?`,
		"anonymous_provider_automation.managed.legacy"); err != nil {
		t.Fatal(err)
	}
	detail, _ := json.Marshal(map[string]any{"source": "automation"})
	if _, err := db.Exec(`INSERT INTO audit_events(ts,action,target_type,target_id,result,detail_json)
VALUES(1,'provider_automation.connect','provider','legacy','success',?)`, string(detail)); err != nil {
		t.Fatal(err)
	}
	if err := applyMigrations(db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM audit_events`); err != nil {
		t.Fatal(err)
	}
	managed, err := AnonymousProviderManaged("legacy")
	if err != nil || !managed {
		t.Fatalf("managed=%v err=%v", managed, err)
	}
	if err := ClearAnonymousProviderManaged("legacy"); err != nil {
		t.Fatal(err)
	}
	managed, err = AnonymousProviderManaged("legacy")
	if err != nil || managed {
		t.Fatalf("cleared managed=%v err=%v", managed, err)
	}
}

func TestAnonymousProviderAutomationOverrideRoundTrips(t *testing.T) {
	setupAnonymousProviderAutomationTest(t)
	if _, set, err := AnonymousProviderAutomationOverride(); err != nil || set {
		t.Fatalf("initial set=%v err=%v", set, err)
	}
	for _, value := range []string{"on", "off"} {
		if err := SetAnonymousProviderAutomationOverride(value); err != nil {
			t.Fatal(err)
		}
		got, set, err := AnonymousProviderAutomationOverride()
		if err != nil || !set || got != value {
			t.Fatalf("override=%q set=%v err=%v", got, set, err)
		}
	}
	if err := SetAnonymousProviderAutomationOverride("inherit"); err != nil {
		t.Fatal(err)
	}
	if _, set, err := AnonymousProviderAutomationOverride(); err != nil || set {
		t.Fatalf("inherit set=%v err=%v", set, err)
	}
}

func TestAnonymousProviderDailyClaimSurvivesRestart(t *testing.T) {
	setupAnonymousProviderAutomationTest(t)
	now := time.Unix(1_800_000_000, 0)
	claimed, err := ClaimAnonymousProviderCheck("zen", now, 24*time.Hour)
	if err != nil || !claimed {
		t.Fatalf("first claim=%v err=%v", claimed, err)
	}
	if claimed, err = ClaimAnonymousProviderCheck("zen", now.Add(23*time.Hour), 24*time.Hour); err != nil || claimed {
		t.Fatalf("early claim=%v err=%v", claimed, err)
	}
	ResetForTests()
	if claimed, err = ClaimAnonymousProviderCheck("zen", now.Add(24*time.Hour), 24*time.Hour); err != nil || !claimed {
		t.Fatalf("due claim=%v err=%v state=%s", claimed, err, config.StateDir())
	}
}
