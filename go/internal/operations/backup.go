package operations

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"

	_ "modernc.org/sqlite"
)

const (
	backupFormatVersion = 1
	maxManifestBytes    = 1 << 20
	maxBackupFileBytes  = int64(16 << 30)
)

var backupFiles = map[string]func() string{
	"config.yaml": func() string { return config.ConfigFilePath() },
	"gateway.db":  func() string { return filepath.Join(config.StateDir(), "gateway.db") },
	"keys.json":   func() string { return filepath.Join(config.StateDir(), "keys.json") },
	"secrets.json": func() string {
		return filepath.Join(config.StateDir(), "secrets.json")
	},
	"telemetry.db": func() string { return filepath.Join(config.StateDir(), "telemetry.db") },
	"usage.db":     func() string { return filepath.Join(config.StateDir(), "usage.db") },
}

var sqliteBackupFiles = map[string]bool{
	"gateway.db": true, "telemetry.db": true, "usage.db": true,
}

type BackupEntry struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type BackupManifest struct {
	Format    int           `json:"format"`
	CreatedAt string        `json:"created_at"`
	Files     []BackupEntry `json:"files"`
}

type BackupInspection struct {
	Path          string
	Format        int
	CreatedAt     string
	Files         []string
	SchemaVersion int
	Counts        map[string]int
}

func DefaultBackupPath(now time.Time) string {
	name := "llmgw-" + now.UTC().Format("20060102T150405Z") + ".tar.gz"
	return filepath.Join(config.StateDir(), "backups", name)
}

func CreateBackup(path string) (BackupInspection, error) {
	lock, err := AcquireStateLock()
	if err != nil {
		return BackupInspection{}, err
	}
	defer lock.Release()

	if strings.TrimSpace(path) == "" {
		path = DefaultBackupPath(time.Now())
	}
	if _, err := os.Stat(path); err == nil {
		return BackupInspection{}, fmt.Errorf("backup already exists: %s", path)
	} else if !os.IsNotExist(err) {
		return BackupInspection{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return BackupInspection{}, fmt.Errorf("create backup directory: %w", err)
	}

	stage, err := os.MkdirTemp("", "llmgw-backup-*")
	if err != nil {
		return BackupInspection{}, err
	}
	defer os.RemoveAll(stage)

	entries := make([]BackupEntry, 0, len(backupFiles))
	for name, sourceFn := range backupFiles {
		source := sourceFn()
		if _, err := os.Stat(source); os.IsNotExist(err) {
			if name == "gateway.db" {
				return BackupInspection{}, fmt.Errorf("gateway state database does not exist")
			}
			continue
		} else if err != nil {
			return BackupInspection{}, fmt.Errorf("inspect %s: %w", name, err)
		}
		target := filepath.Join(stage, name)
		if sqliteBackupFiles[name] {
			err = snapshotSQLite(source, target)
		} else {
			err = copyFile(source, target, 0o600)
		}
		if err != nil {
			return BackupInspection{}, fmt.Errorf("stage %s: %w", name, err)
		}
		entry, err := describeFile(name, target)
		if err != nil {
			return BackupInspection{}, err
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	manifest := BackupManifest{
		Format: backupFormatVersion, CreatedAt: time.Now().UTC().Format(time.RFC3339), Files: entries,
	}
	if err := writeArchive(path, stage, manifest); err != nil {
		return BackupInspection{}, err
	}
	inspection, err := InspectBackup(path)
	if err != nil {
		_ = os.Remove(path)
		return BackupInspection{}, fmt.Errorf("verify created backup: %w", err)
	}
	inspection.Path = path
	if filepath.Clean(filepath.Dir(path)) == filepath.Clean(defaultBackupDir()) {
		_, _ = PruneDefaultBackups()
	}
	return inspection, nil
}

func defaultBackupDir() string { return filepath.Join(config.StateDir(), "backups") }

func defaultBackupKeep() int {
	value, err := strconv.Atoi(strings.TrimSpace(os.Getenv("LLMGW_BACKUP_KEEP")))
	if err != nil || value < 1 {
		return 7
	}
	return value
}

func PruneBackups(keep int) (int, error) {
	if keep < 1 {
		return 0, fmt.Errorf("at least one verified backup must be retained")
	}
	dir := defaultBackupDir()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	type candidate struct {
		path    string
		created time.Time
	}
	valid := []candidate{}
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() || !strings.HasPrefix(entry.Name(), "llmgw-") || !strings.HasSuffix(entry.Name(), ".tar.gz") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		inspection, err := InspectBackup(path)
		if err != nil {
			continue
		}
		created, err := time.Parse(time.RFC3339, inspection.CreatedAt)
		if err != nil {
			continue
		}
		valid = append(valid, candidate{path: path, created: created})
	}
	sort.Slice(valid, func(i, j int) bool { return valid[i].created.After(valid[j].created) })
	if len(valid) <= keep {
		return 0, nil
	}
	removed := 0
	for _, candidate := range valid[keep:] {
		if err := os.Remove(candidate.path); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

func PruneDefaultBackups() (int, error) {
	return PruneBackups(defaultBackupKeep())
}

func InspectBackup(path string) (BackupInspection, error) {
	extracted, manifest, err := extractArchive(path)
	if err != nil {
		return BackupInspection{}, err
	}
	defer os.RemoveAll(extracted)
	inspection, err := inspectExtracted(extracted, manifest)
	inspection.Path = path
	return inspection, err
}

func RestoreBackup(path string) (BackupInspection, error) {
	extracted, manifest, err := extractArchive(path)
	if err != nil {
		return BackupInspection{}, err
	}
	defer os.RemoveAll(extracted)
	inspection, err := inspectExtracted(extracted, manifest)
	if err != nil {
		return BackupInspection{}, err
	}
	inspection.Path = path

	lock, err := AcquireStateLock()
	if err != nil {
		return BackupInspection{}, err
	}
	defer lock.Release()

	available := map[string]bool{}
	for _, entry := range manifest.Files {
		available[entry.Name] = true
	}
	type replacement struct {
		destination string
		staged      string
		rollback    string
		installed   bool
	}
	replacements := make([]replacement, 0, len(backupFiles)+6)
	names := make([]string, 0, len(backupFiles))
	for name := range backupFiles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		destination := backupFiles[name]()
		var staged string
		if available[name] {
			if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
				return BackupInspection{}, err
			}
			file, err := os.CreateTemp(filepath.Dir(destination), ".llmgw-restore-*")
			if err != nil {
				return BackupInspection{}, err
			}
			staged = file.Name()
			_ = file.Close()
			_ = os.Remove(staged)
			if err := copyFile(filepath.Join(extracted, name), staged, 0o600); err != nil {
				return BackupInspection{}, err
			}
		}
		replacements = append(replacements, replacement{destination: destination, staged: staged})
		if sqliteBackupFiles[name] {
			for _, suffix := range []string{"-wal", "-shm", "-journal"} {
				replacements = append(replacements, replacement{destination: destination + suffix})
			}
		}
	}

	rollback := func(last int) {
		for i := last; i >= 0; i-- {
			r := &replacements[i]
			if r.installed {
				_ = os.Remove(r.destination)
			}
			if r.rollback != "" {
				_ = os.Rename(r.rollback, r.destination)
			}
			if r.staged != "" {
				_ = os.Remove(r.staged)
			}
		}
	}
	for i := range replacements {
		r := &replacements[i]
		if _, err := os.Stat(r.destination); err == nil {
			r.rollback = r.destination + fmt.Sprintf(".restore-old-%d", time.Now().UnixNano())
			if err := os.Rename(r.destination, r.rollback); err != nil {
				rollback(i - 1)
				return BackupInspection{}, fmt.Errorf("stage current state: %w", err)
			}
		} else if !os.IsNotExist(err) {
			rollback(i - 1)
			return BackupInspection{}, err
		}
		if r.staged != "" {
			if err := os.Rename(r.staged, r.destination); err != nil {
				rollback(i)
				return BackupInspection{}, fmt.Errorf("install restored state: %w", err)
			}
			r.installed = true
		}
	}
	for i := range replacements {
		if replacements[i].rollback != "" {
			_ = os.Remove(replacements[i].rollback)
		}
	}
	return inspection, nil
}

func snapshotSQLite(source, target string) error {
	db, err := sql.Open("sqlite", source)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec("PRAGMA busy_timeout = 5000"); err != nil {
		return err
	}
	quoted := strings.ReplaceAll(target, "'", "''")
	if _, err := db.Exec("VACUUM INTO '" + quoted + "'"); err != nil {
		return err
	}
	return validateSQLite(target)
}

func validateSQLite(path string) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	var integrity string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil {
		return err
	}
	if integrity != "ok" {
		return fmt.Errorf("SQLite integrity check failed")
	}
	rows, err := db.Query("PRAGMA foreign_key_check")
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		return fmt.Errorf("SQLite foreign key check failed")
	}
	return rows.Err()
}

func describeFile(name, path string) (BackupEntry, error) {
	file, err := os.Open(path)
	if err != nil {
		return BackupEntry{}, err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return BackupEntry{}, err
	}
	return BackupEntry{Name: name, Size: size, SHA256: hex.EncodeToString(hash.Sum(nil))}, nil
}

func writeArchive(path, stage string, manifest BackupManifest) (err error) {
	temp, err := os.CreateTemp(filepath.Dir(path), ".llmgw-backup-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer func() {
		_ = temp.Close()
		if err != nil {
			_ = os.Remove(tempPath)
		}
	}()
	if err = temp.Chmod(0o600); err != nil {
		return err
	}
	gzipWriter := gzip.NewWriter(temp)
	tarWriter := tar.NewWriter(gzipWriter)
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	if err = tarWriter.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o600, Size: int64(len(manifestBytes))}); err != nil {
		return err
	}
	if _, err = tarWriter.Write(manifestBytes); err != nil {
		return err
	}
	for _, entry := range manifest.Files {
		if err = tarWriter.WriteHeader(&tar.Header{Name: entry.Name, Mode: 0o600, Size: entry.Size}); err != nil {
			return err
		}
		file, openErr := os.Open(filepath.Join(stage, entry.Name))
		if openErr != nil {
			return openErr
		}
		_, copyErr := io.Copy(tarWriter, file)
		closeErr := file.Close()
		if copyErr != nil || closeErr != nil {
			return errors.Join(copyErr, closeErr)
		}
	}
	if err = tarWriter.Close(); err != nil {
		return err
	}
	if err = gzipWriter.Close(); err != nil {
		return err
	}
	if err = temp.Sync(); err != nil {
		return err
	}
	if err = temp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tempPath, path); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func extractArchive(path string) (string, BackupManifest, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", BackupManifest{}, err
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return "", BackupManifest{}, fmt.Errorf("open backup compression: %w", err)
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	header, err := tarReader.Next()
	if err != nil || header.Name != "manifest.json" || !header.FileInfo().Mode().IsRegular() || header.Size > maxManifestBytes {
		return "", BackupManifest{}, fmt.Errorf("backup manifest is missing or invalid")
	}
	manifestBytes, err := io.ReadAll(io.LimitReader(tarReader, maxManifestBytes+1))
	if err != nil || len(manifestBytes) > maxManifestBytes {
		return "", BackupManifest{}, fmt.Errorf("read backup manifest")
	}
	var manifest BackupManifest
	decoder := json.NewDecoder(strings.NewReader(string(manifestBytes)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil || manifest.Format != backupFormatVersion {
		return "", BackupManifest{}, fmt.Errorf("unsupported or invalid backup manifest")
	}
	expected := map[string]BackupEntry{}
	for _, entry := range manifest.Files {
		if _, allowed := backupFiles[entry.Name]; !allowed || entry.Size < 0 || entry.Size > maxBackupFileBytes || len(entry.SHA256) != 64 {
			return "", BackupManifest{}, fmt.Errorf("invalid backup entry metadata")
		}
		if _, duplicate := expected[entry.Name]; duplicate {
			return "", BackupManifest{}, fmt.Errorf("duplicate backup entry metadata")
		}
		expected[entry.Name] = entry
	}
	if _, ok := expected["gateway.db"]; !ok {
		return "", BackupManifest{}, fmt.Errorf("backup has no gateway database")
	}
	tempDir, err := os.MkdirTemp("", "llmgw-inspect-*")
	if err != nil {
		return "", BackupManifest{}, err
	}
	fail := func(cause error) (string, BackupManifest, error) {
		_ = os.RemoveAll(tempDir)
		return "", BackupManifest{}, cause
	}
	seen := map[string]bool{}
	for {
		header, err = tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fail(fmt.Errorf("read backup: %w", err))
		}
		entry, ok := expected[header.Name]
		if !ok || seen[header.Name] || !header.FileInfo().Mode().IsRegular() || header.Size != entry.Size {
			return fail(fmt.Errorf("unexpected or invalid backup entry"))
		}
		seen[header.Name] = true
		target := filepath.Join(tempDir, header.Name)
		out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return fail(err)
		}
		hash := sha256.New()
		written, copyErr := io.Copy(io.MultiWriter(out, hash), tarReader)
		closeErr := out.Close()
		if copyErr != nil || closeErr != nil || written != entry.Size || hex.EncodeToString(hash.Sum(nil)) != entry.SHA256 {
			return fail(fmt.Errorf("backup checksum verification failed"))
		}
	}
	if len(seen) != len(expected) {
		return fail(fmt.Errorf("backup is incomplete"))
	}
	return tempDir, manifest, nil
}

func inspectExtracted(dir string, manifest BackupManifest) (BackupInspection, error) {
	files := make([]string, 0, len(manifest.Files))
	for _, entry := range manifest.Files {
		files = append(files, entry.Name)
		if sqliteBackupFiles[entry.Name] {
			if err := validateSQLite(filepath.Join(dir, entry.Name)); err != nil {
				return BackupInspection{}, fmt.Errorf("validate %s: %w", entry.Name, err)
			}
		}
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "gateway.db"))
	if err != nil {
		return BackupInspection{}, err
	}
	defer db.Close()
	inspection := BackupInspection{
		Format: manifest.Format, CreatedAt: manifest.CreatedAt, Files: files,
		Counts: map[string]int{},
	}
	if err := db.QueryRow("SELECT COALESCE(MAX(version),0) FROM schema_migrations").Scan(&inspection.SchemaVersion); err != nil {
		return BackupInspection{}, err
	}
	if inspection.SchemaVersion > iam.SchemaVersion() {
		return BackupInspection{}, fmt.Errorf("backup schema is newer than this binary supports")
	}
	for _, table := range []string{"principals", "projects", "api_keys", "provider_connections"} {
		var count int
		if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil {
			return BackupInspection{}, err
		}
		inspection.Counts[table] = count
	}
	return inspection, nil
}

func copyFile(source, target string, mode os.FileMode) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	syncErr := out.Sync()
	closeErr := out.Close()
	return errors.Join(copyErr, syncErr, closeErr)
}
