package router

import (
	"database/sql"
	"log"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"

	_ "modernc.org/sqlite"
)

// The savings ledger records asked-vs-served model + tokens + cost, attributed
// to the calling project + named key. Best-effort; never breaks a request.

var (
	savingsMu   sync.Mutex
	savingsDBs  = map[string]*sql.DB{}
	savingsInit = map[string]bool{}
)

const savingsSchema = `
CREATE TABLE IF NOT EXISTS usage_ledger (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    ts INTEGER NOT NULL,
    endpoint TEXT,
    requested_model TEXT,
    routed_model TEXT,
    provider TEXT,
    project TEXT,
    key_name TEXT,
    input_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    cost_usd REAL,
    baseline_cost_usd REAL,
    is_stub INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_usage_ts ON usage_ledger(ts);
`

func savingsDBPath() string {
	raw := config.Get().Savings.DBPath
	if raw == "" {
		return filepath.Join(config.StateDir(), "usage.db")
	}
	return raw
}

func savingsConn() (*sql.DB, error) {
	path := savingsDBPath()
	savingsMu.Lock()
	defer savingsMu.Unlock()
	if db, ok := savingsDBs[path]; ok {
		return db, nil
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if !savingsInit[path] {
		if _, err := db.Exec(savingsSchema); err != nil {
			return nil, err
		}
		savingsInit[path] = true
	}
	savingsDBs[path] = db
	return db, nil
}

// UsageRecord is the input to RecordUsage.
type UsageRecord struct {
	Endpoint       string
	RequestedModel string
	RoutedModel    string
	Provider       string
	Project        string
	Key            string
	ProjectID      string
	PrincipalID    string
	KeyID          string
	InputTokens    int
	OutputTokens   int
	StatusCode     int
	LatencyMS      int64
	ErrorCode      string
	CreditsMilli   int64
	IsStub         bool
}

// RecordUsage appends one usage row. Best-effort.
func RecordUsage(r UsageRecord) {
	s := config.Get()
	overrides := s.Savings.PriceCatalog
	cost := computeCost(r.RoutedModel, r.InputTokens, r.OutputTokens, overrides)
	status := r.StatusCode
	if status == 0 {
		status = 200
	}
	credits := r.CreditsMilli
	if credits == 0 && !r.IsStub && status < 400 {
		credits = 1000 // one neutral model-credit; weighted policies can override later
	}
	if err := iam.RecordUsageEvent(iam.UsageEvent{
		Endpoint: r.Endpoint, StatusCode: status, LatencyMS: r.LatencyMS,
		RequestedModel: r.RequestedModel, RoutedModel: r.RoutedModel,
		Provider: r.Provider, ProjectID: r.ProjectID, PrincipalID: r.PrincipalID,
		KeyID: r.KeyID, InputTokens: r.InputTokens, OutputTokens: r.OutputTokens,
		CostMicroUSD: int64(math.Round(cost * 1_000_000)), CreditsMilli: credits,
		ErrorCode: r.ErrorCode, IsStub: r.IsStub,
	}); err != nil {
		log.Printf("record IAM usage: %v", err)
	}
	if !s.Savings.Enabled {
		return
	}
	var baseline any
	if bm := s.Savings.BaselineModel; bm != "" {
		baseline = computeCost(bm, r.InputTokens, r.OutputTokens, overrides)
	}
	stub := 0
	if r.IsStub {
		stub = 1
	}
	db, err := savingsConn()
	if err != nil {
		return
	}
	_, _ = db.Exec(
		`INSERT INTO usage_ledger (ts, endpoint, requested_model, routed_model, provider, project,
		 key_name, input_tokens, output_tokens, cost_usd, baseline_cost_usd, is_stub)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		time.Now().Unix(), r.Endpoint, r.RequestedModel, r.RoutedModel, r.Provider, nullStr(r.Project),
		nullStr(r.Key), r.InputTokens, r.OutputTokens, cost, baseline, stub,
	)
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func round6(f float64) float64 {
	return float64(int64(f*1e6+0.5)) / 1e6
}

// Totals is the headline usage rollup.
func Totals(includeStubs bool) map[string]any {
	if !config.Get().Savings.Enabled {
		return zeroTotals()
	}
	db, err := savingsConn()
	if err != nil {
		return zeroTotals()
	}
	where := "WHERE is_stub = 0"
	if includeStubs {
		where = ""
	}
	row := db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(cost_usd),0), COALESCE(SUM(baseline_cost_usd),0),
		COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0) FROM usage_ledger ` + where)
	var count, inTok, outTok int64
	var cost, baseline float64
	if row.Scan(&count, &cost, &baseline, &inTok, &outTok) != nil {
		return zeroTotals()
	}
	return map[string]any{
		"requests": count, "cost_usd": round6(cost), "baseline_cost_usd": round6(baseline),
		"savings_usd": round6(baseline - cost), "input_tokens": inTok, "output_tokens": outTok,
	}
}

func zeroTotals() map[string]any {
	return map[string]any{
		"requests": 0, "cost_usd": 0.0, "baseline_cost_usd": 0.0,
		"savings_usd": 0.0, "input_tokens": 0, "output_tokens": 0,
	}
}

// ByProject returns per project+key rollups.
func ByProject(includeStubs bool) []map[string]any {
	if !config.Get().Savings.Enabled {
		return []map[string]any{}
	}
	db, err := savingsConn()
	if err != nil {
		return []map[string]any{}
	}
	where := "WHERE is_stub = 0"
	if includeStubs {
		where = ""
	}
	rows, err := db.Query(`SELECT COALESCE(project,'-'), COALESCE(key_name,'-'), COUNT(*),
		COALESCE(SUM(cost_usd),0), COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0)
		FROM usage_ledger ` + where + ` GROUP BY project, key_name ORDER BY 3 DESC LIMIT 100`)
	if err != nil {
		return []map[string]any{}
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var project, key string
		var count, inTok, outTok int64
		var cost float64
		if rows.Scan(&project, &key, &count, &cost, &inTok, &outTok) != nil {
			continue
		}
		out = append(out, map[string]any{
			"project": project, "key": key, "requests": count,
			"cost_usd": round6(cost), "input_tokens": inTok, "output_tokens": outTok,
		})
	}
	return out
}

// RecentUsage returns the most recent usage rows.
func RecentUsage(limit int) []map[string]any {
	if !config.Get().Savings.Enabled {
		return []map[string]any{}
	}
	db, err := savingsConn()
	if err != nil {
		return []map[string]any{}
	}
	rows, err := db.Query(`SELECT ts, endpoint, requested_model, routed_model, provider, project,
		key_name, input_tokens, output_tokens, cost_usd FROM usage_ledger ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return []map[string]any{}
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var ts, inTok, outTok int64
		var endpoint, reqModel, model, provider, project, key sql.NullString
		var cost sql.NullFloat64
		if rows.Scan(&ts, &endpoint, &reqModel, &model, &provider, &project, &key, &inTok, &outTok, &cost) != nil {
			continue
		}
		out = append(out, map[string]any{
			"ts": ts, "endpoint": endpoint.String, "requested_model": reqModel.String,
			"model": model.String, "provider": provider.String, "project": nullOrString(project),
			"key": nullOrString(key), "input_tokens": inTok, "output_tokens": outTok,
			"cost_usd": round6(cost.Float64),
		})
	}
	return out
}

func nullOrString(ns sql.NullString) any {
	if !ns.Valid {
		return nil
	}
	return ns.String
}

func PruneSavingsBefore(cutoff int64) (int64, error) {
	if !config.Get().Savings.Enabled {
		return 0, nil
	}
	path := savingsDBPath()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	db, err := savingsConn()
	if err != nil {
		return 0, err
	}
	result, err := db.Exec("DELETE FROM usage_ledger WHERE ts < ?", cutoff)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// ResetSavingsState drops cached DB handles (test helper).
func ResetSavingsState() {
	savingsMu.Lock()
	defer savingsMu.Unlock()
	for _, db := range savingsDBs {
		_ = db.Close()
	}
	savingsDBs = map[string]*sql.DB{}
	savingsInit = map[string]bool{}
}
