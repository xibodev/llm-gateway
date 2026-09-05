package operations

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"llmgw/internal/config"
)

const restoreJournalName = ".restore-state.json"

type restoreEntry struct {
	Destination string `json:"destination"`
	Staged      string `json:"staged,omitempty"`
	Rollback    string `json:"rollback,omitempty"`
	HadOriginal bool   `json:"had_original"`
}

type restoreJournal struct {
	Phase   string         `json:"phase"`
	Entries []restoreEntry `json:"entries"`
}

func restoreJournalPath() string {
	return filepath.Join(config.StateDir(), restoreJournalName)
}

// RecoverInterruptedRestore must run while the caller holds the state lock.
// A prepared transaction rolls back; a committed transaction finishes cleanup.
func RecoverInterruptedRestore() error {
	path := restoreJournalPath()
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read restore journal: %w", err)
	}
	var journal restoreJournal
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&journal); err != nil || (journal.Phase != "prepared" && journal.Phase != "committed") {
		return fmt.Errorf("restore journal is invalid; manual recovery is required")
	}
	if journal.Phase == "committed" {
		return finalizeRestore(path, journal.Entries)
	}
	return rollbackRestore(path, journal.Entries)
}

func writeRestoreJournal(journal restoreJournal) error {
	path := restoreJournalPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temp, err := createSecureTempFile(filepath.Dir(path), ".restore-state-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	encoder := json.NewEncoder(temp)
	if err := encoder.Encode(journal); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		return replaceFile(tempPath, path)
	} else if !os.IsNotExist(err) {
		return err
	}
	return durableRename(tempPath, path)
}

func rollbackRestore(journalPath string, entries []restoreEntry) error {
	var result error
	for index := len(entries) - 1; index >= 0; index-- {
		entry := entries[index]
		if entry.HadOriginal {
			if _, err := os.Stat(entry.Rollback); err == nil {
				if removeErr := os.Remove(entry.Destination); removeErr != nil && !os.IsNotExist(removeErr) {
					result = errors.Join(result, removeErr)
					continue
				}
				if renameErr := durableRename(entry.Rollback, entry.Destination); renameErr != nil {
					result = errors.Join(result, renameErr)
				}
			} else if err != nil && !os.IsNotExist(err) {
				result = errors.Join(result, err)
			}
		} else if entry.Staged != "" {
			if _, err := os.Stat(entry.Staged); os.IsNotExist(err) {
				if removeErr := os.Remove(entry.Destination); removeErr != nil && !os.IsNotExist(removeErr) {
					result = errors.Join(result, removeErr)
				}
			} else if err != nil {
				result = errors.Join(result, err)
			}
		}
		if entry.Staged != "" {
			if err := os.Remove(entry.Staged); err != nil && !os.IsNotExist(err) {
				result = errors.Join(result, err)
			}
		}
	}
	if result != nil {
		return fmt.Errorf("roll back interrupted restore: %w", result)
	}
	if err := os.Remove(journalPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func finalizeRestore(journalPath string, entries []restoreEntry) error {
	var result error
	for _, entry := range entries {
		for _, path := range []string{entry.Rollback, entry.Staged} {
			if path == "" {
				continue
			}
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				result = errors.Join(result, err)
			}
		}
	}
	if result != nil {
		return fmt.Errorf("finish committed restore: %w", result)
	}
	if err := os.Remove(journalPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func secureTempDir(pattern string) (string, error) {
	return makeSecureTempDir(pattern)
}

func uniqueRollbackPath(destination string) string {
	return destination + fmt.Sprintf(".restore-old-%d", time.Now().UnixNano())
}
