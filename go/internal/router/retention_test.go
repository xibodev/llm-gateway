package router

import (
	"testing"

	"llmgw/internal/config"
)

func TestPruneAuxiliaryHistory(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	ResetTelemetryState()
	ResetSavingsState()
	oldSavings := config.Get().Savings
	config.Update(func(settings *config.Settings) {
		settings.Savings.Enabled = true
		settings.Savings.DBPath = ""
	})
	t.Cleanup(func() {
		ResetTelemetryState()
		ResetSavingsState()
		config.Update(func(settings *config.Settings) { settings.Savings = oldSavings })
	})

	telemetry, err := telConn()
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 537; index++ {
		if _, err := telemetry.Exec(`INSERT INTO failover_events(ts,throttled) VALUES(10,0)`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := telemetry.Exec(`INSERT INTO failover_events(ts,throttled) VALUES(20,0)`); err != nil {
		t.Fatal(err)
	}
	if deleted, err := PruneTelemetryBefore(20); err != nil || deleted != 537 {
		t.Fatalf("telemetry deleted=%d err=%v", deleted, err)
	}

	savings, err := savingsConn()
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 537; index++ {
		if _, err := savings.Exec(`INSERT INTO usage_ledger(ts) VALUES(10)`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := savings.Exec(`INSERT INTO usage_ledger(ts) VALUES(20)`); err != nil {
		t.Fatal(err)
	}
	if deleted, err := PruneSavingsBefore(20); err != nil || deleted != 537 {
		t.Fatalf("savings deleted=%d err=%v", deleted, err)
	}
}
