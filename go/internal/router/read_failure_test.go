package router

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"path/filepath"
	"testing"

	"llmgw/internal/config"
)

// errFailedRead is the failure failingReadConnector injects.
var errFailedRead = errors.New("injected read failure")

// failingReadConnector connects to SQLite through driver, but every result set
// fails after its first row, the way a read error part-way through a query
// reaches database/sql: Next returns false and only Err reports it.
type failingReadConnector struct {
	dsn    string
	driver driver.Driver
}

func (c failingReadConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.driver.Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return failingReadConn{conn}, nil
}

func (c failingReadConnector) Driver() driver.Driver { return c.driver }

type failingReadConn struct{ driver.Conn }

func (c failingReadConn) BeginTx(ctx context.Context, options driver.TxOptions) (driver.Tx, error) {
	return c.Conn.(driver.ConnBeginTx).BeginTx(ctx, options)
}

func (c failingReadConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

func (c failingReadConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	rows, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
	if err != nil {
		return nil, err
	}
	return &failingReadRows{Rows: rows}, nil
}

type failingReadRows struct {
	driver.Rows
	read bool
}

func (r *failingReadRows) Next(dest []driver.Value) error {
	if r.read {
		return errFailedRead
	}
	r.read = true
	return r.Rows.Next(dest)
}

// failingReads returns a handle on the database at path whose result sets
// fail after their first row, and closes the handle it replaces when the test
// ends.
func failingReads(t *testing.T, replaced *sql.DB, path string) *sql.DB {
	t.Helper()
	t.Cleanup(func() { _ = replaced.Close() })
	return sql.OpenDB(failingReadConnector{dsn: path, driver: replaced.Driver()})
}

// A history read that fails part-way through its rows reports no rows, as a
// failed query does, rather than a partial history that reads as complete.
func TestHistoryReadsDiscardRowsAfterIterationErrors(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	oldSavings := config.Get().Savings
	config.Update(func(settings *config.Settings) {
		settings.Savings.Enabled = true
		settings.Savings.DBPath = ""
	})
	rt := NewRuntime(nil)
	t.Cleanup(func() {
		rt.ResetTelemetryState()
		rt.ResetSavingsState()
		config.Update(func(settings *config.Settings) { settings.Savings = oldSavings })
	})
	telemetry, err := rt.telemetry.conn()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := telemetry.Exec(`INSERT INTO failover_events(ts,requested,throttled,attempts_json)
		VALUES(10,'fixture-model',0,'[]')`); err != nil {
		t.Fatal(err)
	}
	savingsPath := savingsDBPath(config.Get())
	savings, err := rt.savings.conn(savingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := savings.Exec(`INSERT INTO usage_ledger(ts,routed_model,project,key_name,input_tokens,output_tokens,cost_usd)
		VALUES(10,'fixture-model','fixture-project','fixture-key',1,1,0.5)`); err != nil {
		t.Fatal(err)
	}
	rt.telemetry.db = failingReads(t, telemetry, filepath.Join(config.StateDir(), "telemetry.db"))
	rt.savings.dbs[savingsPath] = failingReads(t, savings, savingsPath)

	if got := rt.RecentTelemetry(10); len(got) != 0 {
		t.Errorf("recent telemetry=%v, want none", got)
	}
	if got := rt.TelemetryStats()["by_requested"].([]any); len(got) != 0 {
		t.Errorf("telemetry by requested model=%v, want none", got)
	}
	if got := rt.ByProject(false); len(got) != 0 {
		t.Errorf("usage by project=%v, want none", got)
	}
	if got := rt.RecentUsage(10); len(got) != 0 {
		t.Errorf("recent usage=%v, want none", got)
	}
}
