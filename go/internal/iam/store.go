// Package iam owns the gateway's durable multi-user control plane.
//
// It intentionally uses the same embedded SQLite dependency as the existing
// usage/telemetry stores: one Go binary, one state volume, no Postgres or Redis.
package iam

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"llmgw/internal/config"

	_ "modernc.org/sqlite"
)

var (
	storeMu   sync.Mutex
	storeDB   *sql.DB
	storePath string
)

// DB returns the initialized IAM/control-plane database.
func DB() (*sql.DB, error) {
	path := filepath.Join(config.StateDir(), "gateway.db")
	storeMu.Lock()
	defer storeMu.Unlock()

	if storeDB != nil && storePath == path {
		return storeDB, nil
	}
	if storeDB != nil {
		_ = storeDB.Close()
		storeDB = nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create IAM state directory: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open IAM database: %w", err)
	}
	// A single writer keeps quota checks and key lifecycle operations simple and
	// deterministic. WAL still allows concurrent readers without another service.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	for _, pragma := range []string{
		"PRAGMA foreign_keys = ON",
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = NORMAL",
		"PRAGMA busy_timeout = 5000",
	} {
		if _, err := db.Exec(pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}
	if err := applyMigrations(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	_ = os.Chmod(path, 0o600)
	storeDB = db
	storePath = path
	return storeDB, nil
}

// ResetForTests closes the cached database handle.
func ResetForTests() {
	storeMu.Lock()
	defer storeMu.Unlock()
	if storeDB != nil {
		_ = storeDB.Close()
	}
	storeDB = nil
	storePath = ""
}
