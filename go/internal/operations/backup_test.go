package operations

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"llmgw/internal/iam"
)

func TestBackupInspectRestoreRoundTrip(t *testing.T) {
	state := t.TempDir()
	configPath := filepath.Join(state, "config.yaml")
	t.Setenv("LLMGW_STATE_DIR", state)
	t.Setenv("LLMGW_CONFIG", configPath)
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	principal, err := iam.CreatePrincipal("service", "service:backup", "", "Backup service")
	if err != nil {
		t.Fatal(err)
	}
	project, err := iam.CreateProject("backup", "Backup")
	if err != nil {
		t.Fatal(err)
	}
	if err := iam.SetMembership(project.ID, principal.ID, "member"); err != nil {
		t.Fatal(err)
	}
	issued, err := iam.IssueKey(iam.KeyCreate{ProjectID: project.ID, PrincipalID: principal.ID, Name: "backup"})
	if err != nil {
		t.Fatal(err)
	}
	configBytes := []byte("providers:\n  echo:\n    type: echo\nendpoints:\n  safe:\n    failover:\n      - {provider: echo, model: echo-default}\n")
	if err := os.WriteFile(configPath, configBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	secret := "fixture-provider-secret"
	if err := os.WriteFile(filepath.Join(state, "secrets.json"), []byte(`{"echo":"`+secret+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	archive := filepath.Join(t.TempDir(), "state.tar.gz")
	inspection, err := CreateBackup(archive)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Counts["projects"] != 1 || inspection.Counts["api_keys"] != 1 {
		t.Fatalf("inspection counts = %+v", inspection.Counts)
	}
	if inspection.SchemaVersion != iam.SchemaVersion() {
		t.Fatalf("schema=%d want %d", inspection.SchemaVersion, iam.SchemaVersion())
	}

	iam.ResetForTests()
	if err := os.WriteFile(configPath, []byte("providers: {}\nendpoints: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(state, "gateway.db")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "secrets.json"), []byte(`{"other":"changed"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreBackup(archive); err != nil {
		t.Fatal(err)
	}
	gotConfig, _ := os.ReadFile(configPath)
	if !bytes.Equal(gotConfig, configBytes) {
		t.Fatalf("restored config = %q", gotConfig)
	}
	gotSecrets, _ := os.ReadFile(filepath.Join(state, "secrets.json"))
	if !strings.Contains(string(gotSecrets), secret) {
		t.Fatal("provider secret was not restored")
	}
	iam.ResetForTests()
	resolved, found, err := iam.ResolveAPIKey(issued.Token)
	if err != nil || !found || resolved.ProjectID != project.ID {
		t.Fatalf("restored key: found=%v principal=%+v err=%v", found, resolved, err)
	}
}

func TestBackupRejectsCorruptPayload(t *testing.T) {
	state := t.TempDir()
	t.Setenv("LLMGW_STATE_DIR", state)
	t.Setenv("LLMGW_CONFIG", filepath.Join(state, "config.yaml"))
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "state.tar.gz")
	if _, err := CreateBackup(archive); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)/2] ^= 0xff
	if err := os.WriteFile(archive, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectBackup(archive); err == nil {
		t.Fatal("corrupt backup passed inspection")
	}
}

func TestBackupRejectsUnknownArchiveEntry(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "bad.tar.gz")
	file, _ := os.Create(archive)
	gzipWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzipWriter)
	manifest, _ := json.Marshal(BackupManifest{Format: 1, CreatedAt: "fixture", Files: []BackupEntry{{Name: "gateway.db", Size: 0, SHA256: strings.Repeat("0", 64)}}})
	_ = tarWriter.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o600, Size: int64(len(manifest))})
	_, _ = tarWriter.Write(manifest)
	_ = tarWriter.WriteHeader(&tar.Header{Name: "../outside", Mode: 0o600, Size: 0})
	_ = tarWriter.Close()
	_ = gzipWriter.Close()
	_ = file.Close()
	if _, err := InspectBackup(archive); err == nil {
		t.Fatal("unknown traversal entry passed inspection")
	}
}

func TestStateLockIsExclusive(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	first, err := AcquireStateLock()
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	second, err := AcquireStateLock()
	if second != nil {
		_ = second.Release()
	}
	if !errors.Is(err, ErrStateInUse) {
		t.Fatalf("second lock error = %v", err)
	}
}

func TestInspectDoesNotPrintStoredValues(t *testing.T) {
	state := t.TempDir()
	t.Setenv("LLMGW_STATE_DIR", state)
	t.Setenv("LLMGW_CONFIG", filepath.Join(state, "config.yaml"))
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	secret := "sentinel-secret-value"
	if err := os.WriteFile(filepath.Join(state, "secrets.json"), []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "state.tar.gz")
	inspection, err := CreateBackup(archive)
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	_, _ = io.WriteString(&output, strings.Join(inspection.Files, ","))
	for name, count := range inspection.Counts {
		_, _ = io.WriteString(&output, name+string(rune(count)))
	}
	if strings.Contains(output.String(), secret) {
		t.Fatal("inspection exposed stored secret")
	}
}
