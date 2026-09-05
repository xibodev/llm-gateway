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
	maxBackupTotalBytes = int64(16 << 30)
)

var coreBackupFiles = map[string]func() string{
	"config.yaml": func() string { return config.ConfigFilePath() },
	"gateway.db":  func() string { return filepath.Join(config.StateDir(), "gateway.db") },
	"keys.json":   func() string { return filepath.Join(config.StateDir(), "keys.json") },
	"secrets.json": func() string {
		return filepath.Join(config.StateDir(), "secrets.json")
	},
	"catalog.json": func() string { return filepath.Join(config.StateDir(), "catalog.json") },
	"telemetry.db": func() string { return filepath.Join(config.StateDir(), "telemetry.db") },
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

	stage, err := secureTempDir("llmgw-backup-*")
	if err != nil {
		return BackupInspection{}, err
	}
	defer os.RemoveAll(stage)

	sources, err := backupSourceFiles()
	if err != nil {
		return BackupInspection{}, err
	}
	entries := make([]BackupEntry, 0, len(sources))
	for name, source := range sources {
		if _, err := os.Stat(source); os.IsNotExist(err) {
			if name == "gateway.db" {
				return BackupInspection{}, fmt.Errorf("gateway state database does not exist")
			}
			continue
		} else if err != nil {
			return BackupInspection{}, fmt.Errorf("inspect %s: %w", name, err)
		}
		target := filepath.Join(stage, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return BackupInspection{}, err
		}
		if sqliteBackupFiles[name] {
			err = snapshotSQLite(source, target)
		} else {
			err = copyFile(source, target)
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
		if _, err := PruneDefaultBackups(); err != nil {
			return inspection, fmt.Errorf("backup created but retention failed: %w", err)
		}
	}
	return inspection, nil
}

func backupSourceFiles() (map[string]string, error) {
	files := map[string]string{}
	for name, sourceFn := range coreBackupFiles {
		files[name] = sourceFn()
	}
	savingsPath := filepath.Join(config.StateDir(), "usage.db")
	if configured := strings.TrimSpace(config.Get().Savings.DBPath); configured != "" {
		savingsPath = configured
	}
	files["usage.db"] = savingsPath
	cacheDir := copilotCacheDir(config.Get())
	cacheEntries, err := os.ReadDir(cacheDir)
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("read Copilot cache: %w", err)
	}
	for _, entry := range cacheEntries {
		if entry.Type().IsRegular() && validCopilotCacheName(entry.Name()) {
			files["cache/"+entry.Name()] = filepath.Join(cacheDir, entry.Name())
		}
	}
	if err := uniqueFilePaths(files); err != nil {
		return nil, err
	}
	return files, nil
}

func backupDestinationFiles(extracted string, manifest BackupManifest) (map[string]string, error) {
	current := config.Get()
	settings := current
	if _, err := os.Stat(filepath.Join(extracted, "config.yaml")); err == nil {
		settings = config.ReadFile(filepath.Join(extracted, "config.yaml"))
	}
	destinations := map[string]string{}
	for name, destinationFn := range coreBackupFiles {
		destinations[name] = destinationFn()
	}
	savingsPath := filepath.Join(config.StateDir(), "usage.db")
	if configured := strings.TrimSpace(settings.Savings.DBPath); configured != "" {
		currentPath := strings.TrimSpace(current.Savings.DBPath)
		if currentPath == "" || filepath.Clean(currentPath) != filepath.Clean(configured) {
			return nil, fmt.Errorf("configure the archived savings database path before restore")
		}
		savingsPath = configured
	}
	destinations["usage.db"] = savingsPath
	archivedCache := strings.TrimSpace(settings.GithubCopilotCacheDir)
	currentCache := strings.TrimSpace(current.GithubCopilotCacheDir)
	cacheDir := copilotCacheDir(current)
	if archivedCache != "" {
		if currentCache == "" || filepath.Clean(currentCache) != filepath.Clean(archivedCache) {
			return nil, fmt.Errorf("configure the archived Copilot cache path before restore")
		}
		cacheDir = currentCache
	}
	for _, entry := range manifest.Files {
		if strings.HasPrefix(entry.Name, "cache/") {
			base := strings.TrimPrefix(entry.Name, "cache/")
			if !validCopilotCacheName(base) {
				return nil, fmt.Errorf("invalid Copilot cache entry")
			}
			destinations[entry.Name] = filepath.Join(cacheDir, base)
		}
	}
	if existing, err := os.ReadDir(cacheDir); err == nil {
		for _, entry := range existing {
			if entry.Type().IsRegular() && validCopilotCacheName(entry.Name()) {
				destinations["cache/"+entry.Name()] = filepath.Join(cacheDir, entry.Name())
			}
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err := uniqueFilePaths(destinations); err != nil {
		return nil, err
	}
	return destinations, nil
}

func uniqueFilePaths(files map[string]string) error {
	seen := map[string]string{}
	for name, path := range files {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		key := strings.ToLower(filepath.Clean(absolute))
		if previous, duplicate := seen[key]; duplicate {
			return fmt.Errorf("state paths for %s and %s overlap", previous, name)
		}
		seen[key] = name
	}
	return nil
}

func copilotCacheDir(settings *config.Settings) string {
	if configured := strings.TrimSpace(settings.GithubCopilotCacheDir); configured != "" {
		return configured
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".llmgw", "cache")
}

func validCopilotCacheName(name string) bool {
	return strings.HasPrefix(name, "github_copilot_") && strings.HasSuffix(name, ".json") && filepath.Base(name) == name
}

func allowedBackupName(name string) bool {
	if _, ok := coreBackupFiles[name]; ok {
		return true
	}
	if name == "usage.db" {
		return true
	}
	if strings.HasPrefix(name, "cache/") {
		return validCopilotCacheName(strings.TrimPrefix(name, "cache/"))
	}
	return false
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
	if err := RecoverInterruptedRestore(); err != nil {
		return BackupInspection{}, err
	}

	destinations, err := backupDestinationFiles(extracted, manifest)
	if err != nil {
		return BackupInspection{}, err
	}
	available := map[string]bool{}
	for _, entry := range manifest.Files {
		available[entry.Name] = true
	}
	replacements := make([]restoreEntry, 0, len(destinations)+9)
	names := make([]string, 0, len(destinations))
	for name := range destinations {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		destination := destinations[name]
		var staged string
		if available[name] {
			if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
				return BackupInspection{}, err
			}
			file, err := createSecureTempFile(filepath.Dir(destination), ".llmgw-restore-*.tmp")
			if err != nil {
				return BackupInspection{}, err
			}
			staged = file.Name()
			if err := copyToOpenFile(filepath.Join(extracted, filepath.FromSlash(name)), file); err != nil {
				_ = os.Remove(staged)
				return BackupInspection{}, err
			}
		}
		replacements = append(replacements, restoreEntry{Destination: destination, Staged: staged})
		if sqliteBackupFiles[name] {
			for _, suffix := range []string{"-wal", "-shm", "-journal"} {
				replacements = append(replacements, restoreEntry{Destination: destination + suffix})
			}
		}
	}
	for i := range replacements {
		r := &replacements[i]
		if _, err := os.Stat(r.Destination); err == nil {
			r.HadOriginal = true
			r.Rollback = uniqueRollbackPath(r.Destination)
		} else if !os.IsNotExist(err) {
			return BackupInspection{}, err
		}
	}
	journal := restoreJournal{Phase: "prepared", Entries: replacements}
	if err := writeRestoreJournal(journal); err != nil {
		return BackupInspection{}, err
	}
	journalPath := restoreJournalPath()
	for _, entry := range replacements {
		if entry.HadOriginal {
			if err := durableRename(entry.Destination, entry.Rollback); err != nil {
				rollbackErr := rollbackRestore(journalPath, replacements)
				return BackupInspection{}, errors.Join(fmt.Errorf("stage current state: %w", err), rollbackErr)
			}
		}
		if entry.Staged != "" {
			if err := durableRename(entry.Staged, entry.Destination); err != nil {
				rollbackErr := rollbackRestore(journalPath, replacements)
				return BackupInspection{}, errors.Join(fmt.Errorf("install restored state: %w", err), rollbackErr)
			}
		}
	}
	journal.Phase = "committed"
	if err := writeRestoreJournal(journal); err != nil {
		rollbackErr := rollbackRestore(journalPath, replacements)
		return BackupInspection{}, errors.Join(fmt.Errorf("record committed restore: %w", err), rollbackErr)
	}
	if err := finalizeRestore(journalPath, replacements); err != nil {
		return BackupInspection{}, err
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
	temp, err := createSecureTempFile(filepath.Dir(path), ".llmgw-backup-*.tmp")
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
		file, openErr := os.Open(filepath.Join(stage, filepath.FromSlash(entry.Name)))
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
	if err = durableRename(tempPath, path); err != nil {
		return err
	}
	return restrictPath(path, false)
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
	var totalSize int64
	for _, entry := range manifest.Files {
		if !allowedBackupName(entry.Name) || entry.Size < 0 || entry.Size > maxBackupFileBytes || entry.Size > maxBackupTotalBytes-totalSize || len(entry.SHA256) != 64 {
			return "", BackupManifest{}, fmt.Errorf("invalid backup entry metadata")
		}
		totalSize += entry.Size
		if _, duplicate := expected[entry.Name]; duplicate {
			return "", BackupManifest{}, fmt.Errorf("duplicate backup entry metadata")
		}
		expected[entry.Name] = entry
	}
	if _, ok := expected["gateway.db"]; !ok {
		return "", BackupManifest{}, fmt.Errorf("backup has no gateway database")
	}
	tempDir, err := secureTempDir("llmgw-inspect-*")
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
		target := filepath.Join(tempDir, filepath.FromSlash(header.Name))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return fail(err)
		}
		out, err := createSecureFile(target)
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
	cachePresent := false
	for _, entry := range manifest.Files {
		if strings.HasPrefix(entry.Name, "cache/") {
			cachePresent = true
		} else {
			files = append(files, entry.Name)
		}
		if sqliteBackupFiles[entry.Name] {
			if err := validateSQLite(filepath.Join(dir, filepath.FromSlash(entry.Name))); err != nil {
				return BackupInspection{}, fmt.Errorf("validate %s: %w", entry.Name, err)
			}
		}
	}
	if cachePresent {
		files = append(files, "copilot-cache")
	}
	sort.Strings(files)
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

func copyFile(source, target string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := createSecureFile(target)
	if err != nil {
		return err
	}
	return copyAndClose(in, out)
}

func copyToOpenFile(source string, output *os.File) error {
	input, err := os.Open(source)
	if err != nil {
		_ = output.Close()
		return err
	}
	defer input.Close()
	return copyAndClose(input, output)
}

func copyAndClose(input io.Reader, output *os.File) error {
	_, copyErr := io.Copy(output, input)
	syncErr := output.Sync()
	closeErr := output.Close()
	return errors.Join(copyErr, syncErr, closeErr)
}
