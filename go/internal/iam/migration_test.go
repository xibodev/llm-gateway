package iam

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMigrateLegacyKeysHashesAndRemovesPlaintext(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LLMGW_STATE_DIR", dir)
	ResetForTests()
	t.Cleanup(ResetForTests)

	token := "llmgw_legacy_plaintext_token_123"
	legacy := map[string]legacyKeyEntry{
		token: {
			Project: "Project A", Name: "claude-cli", Created: 123,
			AllowedModels: []string{"claude-smart"}, RPM: 7, DailyRequests: 20,
		},
	}

	raw, _ := json.Marshal(legacy)
	path := filepath.Join(dir, "keys.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := Initialize()
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if result.Keys != 1 || result.Projects != 1 || result.Principals != 1 {
		t.Fatalf("migration result = %+v", result)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("plaintext keys file still exists: %v", err)
	}
	resolved, ok, err := ResolveAPIKey(token)
	if err != nil || !ok {
		t.Fatalf("resolve migrated key: ok=%v err=%v", ok, err)
	}
	if resolved.Project != "project-a" || resolved.Key != "claude-cli" {
		t.Fatalf("resolved identity = %+v", resolved)
	}
	if resolved.RPM != 7 || resolved.DailyRequests != 20 {
		t.Fatalf("resolved policy = %+v", resolved)
	}
	keys, err := ListAPIKeys("")
	if err != nil || len(keys) != 1 {
		t.Fatalf("keys = %+v err=%v", keys, err)
	}
	if keys[0].Prefix == token || keys[0].Prefix == "" {
		t.Fatalf("key prefix leaked or missing: %q", keys[0].Prefix)
	}

	// Idempotent after the plaintext file is gone.
	again, err := Initialize()
	if err != nil || again.Keys != 0 {
		t.Fatalf("second Initialize = %+v err=%v", again, err)
	}
	if err := os.WriteFile(path, []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Initialize(); err != nil {
		t.Fatalf("completed migration reparsed corrupt leftover: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("corrupt migrated leftover still exists: %v", err)
	}
}

func TestInitializeImportsLegacyUsageAndQuotaCounters(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LLMGW_STATE_DIR", dir)
	ResetForTests()
	t.Cleanup(ResetForTests)

	token := "llmgw_legacy_usage_token_123"
	raw, _ := json.Marshal(map[string]legacyKeyEntry{
		token: {Project: "project-a", Name: "worker", Created: 100},
	})
	if err := os.WriteFile(filepath.Join(dir, "keys.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	legacyDB, err := sql.Open("sqlite", filepath.Join(dir, "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = legacyDB.Exec(`
CREATE TABLE usage_ledger (
 id INTEGER PRIMARY KEY AUTOINCREMENT, ts INTEGER NOT NULL, endpoint TEXT,
 requested_model TEXT, routed_model TEXT, provider TEXT, project TEXT,
 key_name TEXT, input_tokens INTEGER, output_tokens INTEGER, cost_usd REAL,
 baseline_cost_usd REAL, is_stub INTEGER
);
INSERT INTO usage_ledger(
 ts,endpoint,requested_model,routed_model,provider,project,key_name,
 input_tokens,output_tokens,cost_usd,is_stub
) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		time.Date(2026, 7, 10, 1, 0, 0, 0, time.UTC).Unix(), "openai.chat",
		"smart", "gpt-5.6-sol", "copilot", "project-a", "worker", 20, 5, 0.25, 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	_ = legacyDB.Close()

	result, err := Initialize()
	if err != nil {
		t.Fatal(err)
	}
	if result.Keys != 1 || result.UsageEvents != 1 {
		t.Fatalf("result=%+v", result)
	}
	stats, err := UsageStats(0)
	if err != nil {
		t.Fatal(err)
	}
	total := stats["totals"].(UsageTotals)
	if total.Requests != 1 || total.InputTokens != 20 || total.CostMicroUSD != 250000 {
		t.Fatalf("totals=%+v", total)
	}
	db, _ := DB()
	var requests, inputTokens int64
	if err := db.QueryRow(`
SELECT requests,input_tokens FROM quota_counters WHERE period='day'`,
	).Scan(&requests, &inputTokens); err != nil {
		t.Fatal(err)
	}
	if requests != 1 || inputTokens != 20 {
		t.Fatalf("quota counter requests=%d input=%d", requests, inputTokens)
	}
	if err := db.QueryRow(`
SELECT requests,input_tokens FROM project_quota_counters WHERE period='day'`,
	).Scan(&requests, &inputTokens); err != nil {
		t.Fatal(err)
	}
	if requests != 1 || inputTokens != 20 {
		t.Fatalf("project quota counter requests=%d input=%d", requests, inputTokens)
	}
}
