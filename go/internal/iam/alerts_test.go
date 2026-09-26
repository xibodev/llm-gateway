package iam

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"llmgw/internal/diagnostics"
)

func TestQuotaAlertsEnqueueWarningAndExhaustionOnce(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	ResetForTests()
	t.Cleanup(ResetForTests)
	p, _ := quotaPrincipal(t, KeyPolicy{DailyRequests: 10})
	if _, err := SetProjectPolicy(p.ProjectID, KeyPolicy{DailyRequests: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateAlertRule(AlertRule{
		ProjectID: p.ProjectID, Kind: "quota_usage", Metric: "requests",
		Threshold: 80, Period: "day",
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 10, 2, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		at := now.Add(time.Duration(i) * time.Minute)
		if err := CheckAndConsumeRequest(p, at); err != nil {
			t.Fatalf("request %d: %v", i+1, err)
		}
		if err := RecordUsageEvent(UsageEvent{
			Timestamp: at.Unix(), Endpoint: "openai.chat", StatusCode: 200,
			ProjectID: p.ProjectID, PrincipalID: p.PrincipalID, KeyID: p.KeyID,
			InputTokens: 1, OutputTokens: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	events, err := ClaimOutbox("worker-one", 20, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Kind != "quota_warning" ||
		events[1].Kind != "quota_exhausted" {
		t.Fatalf("events=%+v", events)
	}
	// Re-recording at the same exhausted period cannot duplicate either event.
	if err := RecordUsageEvent(UsageEvent{
		Timestamp: now.Add(11 * time.Minute).Unix(), Endpoint: "openai.chat",
		StatusCode: 429, ProjectID: p.ProjectID, PrincipalID: p.PrincipalID,
		KeyID: p.KeyID,
	}); err != nil {
		t.Fatal(err)
	}
	if duplicate, err := ClaimOutbox("worker-two", 20, time.Minute); err != nil ||
		len(duplicate) != 0 {
		t.Fatalf("second worker claimed leased events: %+v err=%v", duplicate, err)
	}
	if err := MarkOutboxDelivered(events[0].ID, "wrong-worker"); err == nil {
		t.Fatal("wrong worker acknowledged claimed event")
	}
	if err := MarkOutboxDelivered(events[0].ID, "worker-one"); err != nil {
		t.Fatal(err)
	}
	remaining, _ := PendingOutbox(20)
	if len(remaining) != 1 || remaining[0].ID != events[1].ID {
		t.Fatalf("remaining outbox=%+v, want second event", remaining)
	}
}

func TestKeyExpiryScheduledAlert(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	ResetForTests()
	t.Cleanup(ResetForTests)
	now := time.Date(2026, 7, 10, 2, 0, 0, 0, time.UTC)
	principal, _ := CreatePrincipal("service", "service:expiring", "", "Expiring")
	project, _ := CreateProject("expiry", "Expiry")
	_ = SetMembership(project.ID, principal.ID, "member")
	issued, err := IssueKey(KeyCreate{
		ProjectID: project.ID, PrincipalID: principal.ID, Name: "soon",
		ExpiresAt: now.Add(5 * 24 * time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CreateAlertRule(AlertRule{
		ProjectID: project.ID, Kind: "key_expiry", Threshold: 7, Period: "day",
	}); err != nil {
		t.Fatal(err)
	}
	count, err := EvaluateScheduledAlerts(now)
	if err != nil || count != 1 {
		t.Fatalf("evaluate count=%d err=%v", count, err)
	}
	events, _ := PendingOutbox(10)
	if len(events) != 1 || events[0].Kind != "key_expiring" ||
		events[0].Payload["key_id"] != issued.ID {
		t.Fatalf("events=%+v", events)
	}
}

func TestOutboxErrorsSanitizePersistenceAndHistoricalRows(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	ResetForTests()
	t.Cleanup(ResetForTests)
	if _, err := Initialize(); err != nil {
		t.Fatal(err)
	}
	db, err := DB()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	result, err := db.Exec(`INSERT INTO outbox_events(
		ts,kind,payload_json,status,attempts,available_at
	) VALUES(?,?,?,'pending',0,?)`, now, "fixture", `{"key_name":"functional@example.test"}`, now)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := ClaimOutbox("worker-one", 1, time.Minute)
	if err != nil || len(claimed) != 1 || claimed[0].ID != id {
		t.Fatalf("claim=%+v err=%v", claimed, err)
	}
	secret := "llmgw_" + strings.Repeat("c", 32)
	message := "界" + strings.Repeat("x", 486) + " " + secret + strings.Repeat("界", 20) + " owner@example.test"
	retryAt := now + 60
	if err := MarkOutboxFailed(id, "worker-one", message, retryAt); err != nil {
		t.Fatal(err)
	}
	var rawError, rawPayload, status string
	var attempts int
	var availableAt int64
	var claimedBy *string
	var leaseUntil *int64
	if err := db.QueryRow(`SELECT last_error,payload_json,status,attempts,available_at,
		claimed_by,lease_until FROM outbox_events WHERE id=?`, id).Scan(
		&rawError, &rawPayload, &status, &attempts, &availableAt, &claimedBy, &leaseUntil,
	); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rawError, "llmgw_") || strings.Contains(rawError, "owner@example.test") ||
		!strings.Contains(rawError, diagnostics.Redacted) ||
		utf8.RuneCountInString(rawError) > 500 || !utf8.ValidString(rawError) {
		t.Fatalf("unsafe outbox error persisted: %q", rawError)
	}
	if rawPayload != `{"key_name":"functional@example.test"}` {
		t.Fatalf("functional payload changed: %s", rawPayload)
	}
	if status != "failed" || attempts != 1 || availableAt != retryAt ||
		claimedBy != nil || leaseUntil != nil {
		t.Fatalf("retry/lease semantics changed: status=%s attempts=%d available=%d claimed=%v lease=%v",
			status, attempts, availableAt, claimedBy, leaseUntil)
	}

	historical := "Bearer historical-token historical@example.test"
	if _, err := db.Exec(`UPDATE outbox_events SET last_error=?,available_at=? WHERE id=?`,
		historical, now-1, id); err != nil {
		t.Fatal(err)
	}
	pending, err := PendingOutbox(1)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending=%+v err=%v", pending, err)
	}
	if strings.Contains(pending[0].LastError, "historical-token") ||
		strings.Contains(pending[0].LastError, "historical@example.test") ||
		pending[0].Status != "failed" || pending[0].Attempts != 1 ||
		pending[0].Payload["key_name"] != "functional@example.test" {
		t.Fatalf("historical pending event unsafe or changed: %+v", pending[0])
	}
	claimed, err = ClaimOutbox("worker-two", 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("historical claim=%+v err=%v", claimed, err)
	}
	if strings.Contains(claimed[0].LastError, "historical-token") ||
		strings.Contains(claimed[0].LastError, "historical@example.test") ||
		claimed[0].ClaimedBy != "worker-two" || claimed[0].LeaseUntil <= now ||
		claimed[0].Attempts != 1 || claimed[0].AvailableAt != now-1 {
		t.Fatalf("historical claimed event unsafe or changed: %+v", claimed[0])
	}
}
