package router

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/providers"

	_ "modernc.org/sqlite"
)

// The telemetry store is separate from the savings ledger. For every request
// that goes through a failover chain we record which targets failed (throttle
// vs other) and which one served — the "learn as we go" surface.

var (
	telMu   sync.Mutex
	telDB   *sql.DB
	telPath string
)

const telSchema = `
CREATE TABLE IF NOT EXISTS failover_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    ts INTEGER NOT NULL,
    requested TEXT,
    served_provider TEXT,
    served_model TEXT,
    throttled INTEGER NOT NULL DEFAULT 0,
    attempts_json TEXT,
    project TEXT,
    key_name TEXT
);
CREATE INDEX IF NOT EXISTS idx_fe_ts ON failover_events(ts);
`

func telConn() (*sql.DB, error) {
	path := filepath.Join(config.StateDir(), "telemetry.db")
	telMu.Lock()
	defer telMu.Unlock()
	if telDB != nil && telPath == path {
		return telDB, nil
	}
	if telDB != nil {
		_ = telDB.Close()
		telDB = nil
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(telSchema); err != nil {
		return nil, err
	}
	telDB = db
	telPath = path
	return db, nil
}

// eventAttempt is the JSON shape stored per attempt.
type eventAttempt struct {
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
	Throttled bool   `json:"throttled,omitempty"`
}

func toEventAttempts(attempts []attempt) []eventAttempt {
	out := make([]eventAttempt, len(attempts))
	for i, a := range attempts {
		out[i] = eventAttempt{Provider: a.Provider, Model: a.Model, OK: a.OK, Error: a.Error, Throttled: a.Throttled}
	}
	return out
}

func recordTelemetryEvent(requested string, attempts []eventAttempt, servedProvider, servedModel, project, key string) {
	throttled := 0
	for i := range attempts {
		attempts[i].Error = providers.SanitizeDiagnosticTextLimit(attempts[i].Error, 200)
		if attempts[i].Throttled {
			throttled = 1
		}
	}
	db, err := telConn()
	if err != nil {
		return
	}
	blob, _ := json.Marshal(attempts)
	_, _ = db.Exec(
		`INSERT INTO failover_events (ts, requested, served_provider, served_model, throttled,
		 attempts_json, project, key_name) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		time.Now().Unix(), requested, nullStr(servedProvider), nullStr(servedModel), throttled,
		string(blob), nullStr(project), nullStr(key),
	)
}

// RecentTelemetry returns recent failover events.
func RecentTelemetry(limit int) []map[string]any {
	db, err := telConn()
	if err != nil {
		return []map[string]any{}
	}
	rows, err := db.Query(`SELECT ts, requested, served_provider, served_model, throttled, attempts_json
		FROM failover_events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return []map[string]any{}
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var ts, throttled int64
		var requested, sp, sm, aj sql.NullString
		if rows.Scan(&ts, &requested, &sp, &sm, &throttled, &aj) != nil {
			continue
		}
		var attempts []map[string]any
		if aj.Valid {
			_ = json.Unmarshal([]byte(aj.String), &attempts)
		}
		for _, attempt := range attempts {
			if message, ok := attempt["error"].(string); ok {
				attempt["error"] = providers.SanitizeDiagnosticTextLimit(message, 200)
			}
		}
		var served any
		if sp.Valid && sm.Valid {
			served = sp.String + "/" + sm.String
		}
		out = append(out, map[string]any{
			"ts": ts, "requested": requested.String, "served": served,
			"throttled": throttled != 0, "attempts": attempts,
		})
	}
	return out
}

// TelemetryStats returns aggregate counts.
func TelemetryStats() map[string]any {
	db, err := telConn()
	if err != nil {
		return map[string]any{"events": 0, "throttled": 0, "by_requested": []any{}}
	}
	var events, throttled int64
	_ = db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(throttled),0) FROM failover_events`).Scan(&events, &throttled)
	byReq := []any{}
	rows, err := db.Query(`SELECT requested, COUNT(*) c, COALESCE(SUM(throttled),0) t
		FROM failover_events GROUP BY requested ORDER BY c DESC LIMIT 20`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var requested sql.NullString
			var c, t int64
			if rows.Scan(&requested, &c, &t) == nil {
				byReq = append(byReq, map[string]any{"requested": requested.String, "events": c, "throttled": t})
			}
		}
	}
	return map[string]any{"events": events, "throttled": throttled, "by_requested": byReq}
}

func PruneTelemetryBefore(cutoff int64) (int64, error) {
	path := filepath.Join(config.StateDir(), "telemetry.db")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	db, err := telConn()
	if err != nil {
		return 0, err
	}
	result, err := db.Exec("DELETE FROM failover_events WHERE ts < ?", cutoff)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// ResetTelemetryState drops the cached DB handle (test helper).
func ResetTelemetryState() {
	telMu.Lock()
	defer telMu.Unlock()
	if telDB != nil {
		_ = telDB.Close()
		telDB = nil
	}
	telPath = ""
}
