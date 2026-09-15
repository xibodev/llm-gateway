package iam

import (
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
