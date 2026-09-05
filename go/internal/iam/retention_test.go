package iam

import (
	"errors"
	"testing"
	"time"
)

func TestPruneOperationalHistoryPreservesActiveState(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	ResetForTests()
	t.Cleanup(ResetForTests)
	db, err := DB()
	if err != nil {
		t.Fatal(err)
	}
	principal, _ := CreatePrincipal("service", "service:retention", "", "Retention")
	project, _ := CreateProject("retention", "Retention")
	_ = SetMembership(project.ID, principal.ID, "member")
	issued, _ := IssueKey(KeyCreate{
		ProjectID: project.ID, PrincipalID: principal.ID, Name: "limited",
		Policy: KeyPolicy{RPM: 1},
	})
	resolved, found, err := ResolveAPIKey(issued.Token)
	if err != nil || !found {
		t.Fatal("issued key did not resolve")
	}

	now := time.Date(2026, 9, 5, 12, 34, 0, 0, time.UTC)
	old := now.AddDate(-2, 0, 0).Unix()
	newer := now.AddDate(0, 0, -1).Unix()
	minute, day, month := quotaPeriods(now)
	for _, ts := range []int64{old, newer} {
		_, err = db.Exec(`INSERT INTO usage_events(request_id,ts,endpoint,status_code) VALUES(?,?,?,200)`, newIDForTest(t), ts, "test")
		if err != nil {
			t.Fatal(err)
		}
		_, err = db.Exec(`INSERT INTO audit_events(ts,action,result,detail_json) VALUES(?,?,?,?)`, ts, "test", "success", "{}")
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, period := range []struct {
		name    string
		old     int64
		current int64
	}{
		{"minute", minute - 60, minute},
		{"day", day - 86400, day},
		{"month", time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC).Unix(), month},
	} {
		for _, start := range []int64{period.old, period.current} {
			_, err = db.Exec(`INSERT INTO quota_counters(key_id,period,period_start,requests) VALUES(?,?,?,1)`, issued.ID, period.name, start)
			if err != nil {
				t.Fatal(err)
			}
			_, err = db.Exec(`INSERT INTO project_quota_counters(project_id,period,period_start,requests) VALUES(?,?,?,1)`, project.ID, period.name, start)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, row := range []struct {
		status      string
		deliveredAt any
	}{
		{"delivered", old},
		{"delivered", newer},
		{"pending", nil},
		{"failed", nil},
	} {
		_, err = db.Exec(`INSERT INTO outbox_events(ts,kind,payload_json,status,available_at,delivered_at) VALUES(?,?,?,?,?,?)`, old, "test", "{}", row.status, old, row.deliveredAt)
		if err != nil {
			t.Fatal(err)
		}
	}

	result, err := PruneOperationalHistory(now, RetentionPolicy{UsageDays: 90, AuditDays: 365, DeliveredOutboxDays: 400})
	if err != nil {
		t.Fatal(err)
	}
	if result.UsageEvents != 1 || result.AuditEvents != 1 || result.KeyQuotaCounters != 3 || result.ProjectQuotaCounters != 3 || result.DeliveredOutbox != 1 {
		t.Fatalf("prune result = %+v", result)
	}
	for _, table := range []string{"principals", "projects", "project_memberships", "api_keys"} {
		var count int
		err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count)
		if err != nil || count != 1 {
			t.Fatalf("active table %s count=%d err=%v", table, count, err)
		}
	}
	var count int
	_ = db.QueryRow("SELECT COUNT(*) FROM outbox_events").Scan(&count)
	if count != 3 {
		t.Fatalf("outbox rows=%d, want 3", count)
	}
	_ = db.QueryRow("SELECT COUNT(*) FROM quota_counters").Scan(&count)
	if count != 3 {
		t.Fatalf("key quota rows=%d, want 3", count)
	}
	if err := CheckAndConsumeRequest(resolved, now); !errors.As(err, new(*QuotaExceeded)) {
		t.Fatalf("current quota was not preserved: %v", err)
	}
	second, err := PruneOperationalHistory(now, RetentionPolicy{UsageDays: 90, AuditDays: 365, DeliveredOutboxDays: 400})
	if err != nil || second != (RetentionResult{}) {
		t.Fatalf("second prune = %+v err=%v", second, err)
	}
}

func TestRetentionPolicyUsesSafeDefaults(t *testing.T) {
	t.Setenv("LLMGW_RETENTION_USAGE_DAYS", "0")
	t.Setenv("LLMGW_RETENTION_AUDIT_DAYS", "invalid")
	t.Setenv("LLMGW_RETENTION_DELIVERED_OUTBOX_DAYS", "10")
	policy := DefaultRetentionPolicy()
	if policy.UsageDays != 90 || policy.AuditDays != 365 || policy.DeliveredOutboxDays != 400 {
		t.Fatalf("policy = %+v", policy)
	}
}

func newIDForTest(t *testing.T) string {
	t.Helper()
	id, err := newID("ret")
	if err != nil {
		t.Fatal(err)
	}
	return id
}
