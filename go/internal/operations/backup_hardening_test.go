package operations

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
)

func TestBackupIncludesConfiguredDurableState(t *testing.T) {
	state := t.TempDir()
	configPath := filepath.Join(state, "config.yaml")
	customUsage := filepath.Join(t.TempDir(), "custom-usage.db")
	cacheDir := filepath.Join(t.TempDir(), "copilot-cache")
	t.Setenv("LLMGW_STATE_DIR", state)
	t.Setenv("LLMGW_CONFIG", configPath)
	t.Setenv("LLMGW_GITHUB_COPILOT_CACHE_DIR", cacheDir)
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	config.Update(func(settings *config.Settings) {
		settings.Savings.Enabled = true
		settings.Savings.DBPath = customUsage
		settings.GithubCopilotCacheDir = cacheDir
	})
	if err := config.Save(); err != nil {
		t.Fatal(err)
	}
	usageDB, err := sql.Open("sqlite", customUsage)
	if err != nil {
		t.Fatal(err)
	}
	_, err = usageDB.Exec(`CREATE TABLE usage_ledger(id INTEGER PRIMARY KEY, ts INTEGER NOT NULL); INSERT INTO usage_ledger(ts) VALUES(42)`)
	_ = usageDB.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "catalog.json"), []byte(`{"fixture":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "github_copilot_oauth.json"), []byte(`{"access_token":"sentinel"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "state.tar.gz")
	inspection, err := CreateBackup(archive)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"catalog.json": false, "usage.db": false, "copilot-cache": false,
	}
	for _, name := range inspection.Files {
		if _, ok := want[name]; ok {
			want[name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Fatalf("backup omitted %s: %v", name, inspection.Files)
		}
	}
	if err := os.Remove(customUsage); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(cacheDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(state, "catalog.json")); err != nil {
		t.Fatal(err)
	}
	iam.ResetForTests()
	if _, err := RestoreBackup(archive); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		customUsage,
		filepath.Join(cacheDir, "github_copilot_oauth.json"),
		filepath.Join(state, "catalog.json"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("restored durable state %s: %v", path, err)
		}
	}
}

func TestRecoverPreparedRestoreRollsBack(t *testing.T) {
	state := t.TempDir()
	t.Setenv("LLMGW_STATE_DIR", state)
	destination := filepath.Join(state, "gateway.db")
	rollback := destination + ".restore-old"
	staged := destination + ".restore-new"
	mustWrite(t, destination, "new")
	mustWrite(t, rollback, "old")
	mustWrite(t, staged, "staged")
	entries := []restoreEntry{{Destination: destination, Staged: staged, Rollback: rollback, HadOriginal: true}}
	if err := writeRestoreJournal(restoreJournal{Phase: "prepared", Entries: entries}); err != nil {
		t.Fatal(err)
	}
	if err := RecoverInterruptedRestore(); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(destination)
	if string(raw) != "old" {
		t.Fatalf("rolled back value = %q", raw)
	}
	assertAbsent(t, rollback, staged, restoreJournalPath())
}

func TestRecoverCommittedRestoreFinishesCleanup(t *testing.T) {
	state := t.TempDir()
	t.Setenv("LLMGW_STATE_DIR", state)
	destination := filepath.Join(state, "gateway.db")
	rollback := destination + ".restore-old"
	mustWrite(t, destination, "new")
	mustWrite(t, rollback, "old")
	entries := []restoreEntry{{Destination: destination, Rollback: rollback, HadOriginal: true}}
	if err := writeRestoreJournal(restoreJournal{Phase: "committed", Entries: entries}); err != nil {
		t.Fatal(err)
	}
	if err := RecoverInterruptedRestore(); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(destination)
	if string(raw) != "new" {
		t.Fatalf("committed value = %q", raw)
	}
	assertAbsent(t, rollback, restoreJournalPath())
}

func mustWrite(t *testing.T, path, value string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertAbsent(t *testing.T, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("recovery residue %s: %v", path, err)
		}
	}
}
