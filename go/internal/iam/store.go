// Package iam owns the gateway's durable multi-user control plane.
//
// It intentionally uses the same embedded SQLite dependency as the existing
// usage/telemetry stores: one Go binary, one state volume, no Postgres or Redis.
package iam

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
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
	db, err := openDB(path)
	if err != nil {
		return nil, err
	}
	storeDB = db
	storePath = path
	return storeDB, nil
}

// openDB opens and migrates one handle on the database at path. DB caches a
// single handle per state directory; tests open more, the way separate
// processes would, to prove cross-process guarantees such as credential leases.
func openDB(path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create IAM state directory: %w", err)
	}
	// The driver runs these on every connection it opens. database/sql
	// replaces a connection that fails, and a pragma run once through Exec
	// would leave the replacement without foreign keys or a busy timeout.
	dsn, err := sqliteDSN(path,
		"foreign_keys(1)", "journal_mode(WAL)", "synchronous(NORMAL)", "busy_timeout(5000)",
	)
	if err != nil {
		return nil, fmt.Errorf("resolve IAM database path: %w", err)
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open IAM database: %w", err)
	}
	// A single writer keeps quota checks and key lifecycle operations simple and
	// deterministic. WAL still allows concurrent readers without another service.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open IAM database: %w", err)
	}
	if err := applyMigrations(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	_ = os.Chmod(path, 0o600)
	return db, nil
}

// sqliteDSN names the database at path as a SQLite URI whose _pragma
// parameters the driver runs on each new connection. SQLite ends a URI path
// at ? or # and decodes %HH escapes in it, so the path travels URL-escaped.
// It is made absolute, since a relative path's first segment would read as
// the URI's authority, and SQLite reads "/C:/..." as a Windows drive.
func sqliteDSN(path string, pragmas ...string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	slashed := filepath.ToSlash(absolute)
	if !strings.HasPrefix(slashed, "/") {
		slashed = "/" + slashed
	}
	uri := url.URL{Scheme: "file", Path: slashed, RawQuery: url.Values{"_pragma": pragmas}.Encode()}
	return uri.String(), nil
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
