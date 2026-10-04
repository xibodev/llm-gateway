package iam

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"path/filepath"
	"testing"
	"time"

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

// useFailingReads makes DB return a handle on the same database whose result
// sets fail after their first row, until the test ends.
func useFailingReads(t *testing.T) {
	t.Helper()
	if _, err := DB(); err != nil {
		t.Fatal(err)
	}
	dsn, err := sqliteDSN(filepath.Join(config.StateDir(), "gateway.db"), "foreign_keys(1)", "busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	storeMu.Lock()
	previous := storeDB
	failing := sql.OpenDB(failingReadConnector{dsn: dsn, driver: previous.Driver()})
	failing.SetMaxOpenConns(1)
	storeDB = failing
	storeMu.Unlock()
	t.Cleanup(func() {
		storeMu.Lock()
		storeDB = previous
		storeMu.Unlock()
		_ = failing.Close()
	})
}

// A read that fails part-way through its rows must fail the operation rather
// than act on the rows read before the failure.
func TestRowIterationErrorsFailTheRead(t *testing.T) {
	for name, read := range map[string]func(t *testing.T) error{
		"usage report": func(t *testing.T) error {
			if err := RecordUsageEvent(UsageEvent{Endpoint: "openai.chat", StatusCode: 200, InputTokens: 1}); err != nil {
				t.Fatal(err)
			}
			useFailingReads(t)
			_, err := UsageStats(0)
			return err
		},
		"outbox claim": func(t *testing.T) error {
			db, err := DB()
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().Unix()
			if _, err := db.Exec(`INSERT INTO outbox_events(ts,kind,payload_json,status,attempts,available_at)
				VALUES(?,'fixture','{}','pending',0,?)`, now, now); err != nil {
				t.Fatal(err)
			}
			useFailingReads(t)
			_, err = ClaimOutbox("worker-one", 10, time.Minute)
			return err
		},
		"usage alert evaluation": func(t *testing.T) error {
			p, _ := quotaPrincipal(t, KeyPolicy{DailyRequests: 10})
			if _, err := CreateAlertRule(AlertRule{
				Kind: "quota_usage", Metric: "requests", Threshold: 80, Period: "day",
			}); err != nil {
				t.Fatal(err)
			}
			useFailingReads(t)
			return RecordUsageEvent(UsageEvent{
				Endpoint: "openai.chat", StatusCode: 200,
				ProjectID: p.ProjectID, PrincipalID: p.PrincipalID, KeyID: p.KeyID,
			})
		},
		"key expiry evaluation": func(t *testing.T) error {
			now := time.Now()
			principal, _ := CreatePrincipal("service", "service:expiring", "", "Expiring")
			project, _ := CreateProject("expiry", "Expiry")
			if err := SetMembership(project.ID, principal.ID, "member"); err != nil {
				t.Fatal(err)
			}
			if _, err := IssueKey(KeyCreate{
				ProjectID: project.ID, PrincipalID: principal.ID, Name: "soon",
				ExpiresAt: now.Add(5 * 24 * time.Hour).Unix(),
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := CreateAlertRule(AlertRule{Kind: "key_expiry", Threshold: 7, Period: "day"}); err != nil {
				t.Fatal(err)
			}
			useFailingReads(t)
			_, err := EvaluateScheduledAlerts(now)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("LLMGW_STATE_DIR", t.TempDir())
			ResetForTests()
			t.Cleanup(ResetForTests)
			if err := read(t); !errors.Is(err, errFailedRead) {
				t.Fatalf("err=%v, want the read failure", err)
			}
		})
	}
}
