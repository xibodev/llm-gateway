package iam

import (
	"testing"
	"time"
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
