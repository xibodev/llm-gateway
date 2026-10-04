package iam

import (
	"errors"
	"testing"
	"time"

	"llmgw/internal/config"
)

func quotaPrincipal(t *testing.T, policy KeyPolicy) (*config.Principal, string) {
	t.Helper()
	principal, err := CreatePrincipal("service", "service:quota", "", "Quota Service")
	if err != nil {
		t.Fatal(err)
	}
	project, err := CreateProject("quota-project", "Quota Project")
	if err != nil {
		t.Fatal(err)
	}
	if err := SetMembership(project.ID, principal.ID, "member"); err != nil {
		t.Fatal(err)
	}
	issued, err := IssueKey(KeyCreate{
		ProjectID: project.ID, PrincipalID: principal.ID, Name: "quota", Policy: policy,
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved, ok, err := ResolveAPIKey(issued.Token)
	if err != nil || !ok {
		t.Fatalf("resolve: ok=%v err=%v", ok, err)
	}
	return resolved, issued.Token
}

func TestPersistentRequestQuotasSurviveStoreReset(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	ResetForTests()
	t.Cleanup(ResetForTests)
	p, token := quotaPrincipal(t, KeyPolicy{RPM: 2, DailyRequests: 3})
	now := time.Date(2026, 7, 10, 1, 2, 3, 0, time.UTC)
	if err := CheckAndConsumeRequest(p, now); err != nil {
		t.Fatal(err)
	}
	if err := CheckAndConsumeRequest(p, now); err != nil {
		t.Fatal(err)
	}
	var exceeded *QuotaExceeded
	if err := CheckAndConsumeRequest(p, now); !errors.As(err, &exceeded) ||
		exceeded.Metric != "requests/minute" {
		t.Fatalf("third request err=%v, want RPM quota", err)
	}

	// Reopen the database (simulates a process restart): counters remain.
	ResetForTests()
	p, ok, err := ResolveAPIKey(token)
	if err != nil || !ok {
		t.Fatalf("resolve after reset: ok=%v err=%v", ok, err)
	}
	if err := CheckAndConsumeRequest(p, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := CheckAndConsumeRequest(p, now.Add(time.Minute)); !errors.As(err, &exceeded) ||
		exceeded.Metric != "requests/day" {
		t.Fatalf("fourth daily request err=%v, want daily quota", err)
	}
}

func TestUsageReconciliationEnforcesTokenAndCostBudgets(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	ResetForTests()
	t.Cleanup(ResetForTests)
	p, _ := quotaPrincipal(t, KeyPolicy{
		DailyInputTokens: 100, MonthlyTotalTokens: 150,
		DailyCostMicroUSD: 500, DailyCreditsMilli: 1000,
	})
	now := time.Date(2026, 7, 10, 1, 2, 3, 0, time.UTC)
	if err := CheckAndConsumeRequest(p, now); err != nil {
		t.Fatal(err)
	}
	if err := RecordUsageEvent(UsageEvent{
		Timestamp: now.Unix(), Endpoint: "openai.chat", StatusCode: 200,
		ProjectID: p.ProjectID, PrincipalID: p.PrincipalID, KeyID: p.KeyID,
		InputTokens: 100, OutputTokens: 60, CostMicroUSD: 500, CreditsMilli: 1000,
	}); err != nil {
		t.Fatal(err)
	}
	var exceeded *QuotaExceeded
	err := CheckAndConsumeRequest(p, now.Add(time.Minute))
	if !errors.As(err, &exceeded) {
		t.Fatalf("expected usage quota, got %v", err)
	}
	if exceeded.Metric != "input tokens/day" {
		t.Fatalf("first exceeded metric=%q", exceeded.Metric)
	}
}

func TestUsageStatsGroupsByIdentity(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	ResetForTests()
	t.Cleanup(ResetForTests)
	p, _ := quotaPrincipal(t, KeyPolicy{})
	now := time.Now().Unix()
	if err := RecordUsageEvent(UsageEvent{
		Timestamp: now, Endpoint: "anthropic.messages", StatusCode: 200, LatencyMS: 25,
		RequestedModel: "claude-smart", RoutedModel: "claude-opus-4.8",
		Provider: "copilot", ProjectID: p.ProjectID, PrincipalID: p.PrincipalID,
		KeyID: p.KeyID, InputTokens: 10, OutputTokens: 4, CostMicroUSD: 12,
		CreditsMilli: 1000,
	}); err != nil {
		t.Fatal(err)
	}

	stats, err := UsageStats(now - 1)
	if err != nil {
		t.Fatal(err)
	}
	total := stats["totals"].(UsageTotals)
	if total.Requests != 1 || total.InputTokens != 10 || total.OutputTokens != 4 {
		t.Fatalf("totals=%+v", total)
	}
}

func TestProjectQuotaAggregatesAcrossKeys(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	ResetForTests()
	t.Cleanup(ResetForTests)
	principal, _ := CreatePrincipal("service", "service:project-quota", "", "Service")
	project, _ := CreateProject("shared-budget", "Shared Budget")
	_ = SetMembership(project.ID, principal.ID, "member")
	if _, err := SetProjectPolicy(project.ID, KeyPolicy{DailyRequests: 2}); err != nil {
		t.Fatal(err)
	}
	first, _ := IssueKey(KeyCreate{
		ProjectID: project.ID, PrincipalID: principal.ID, Name: "first",
	})
	second, _ := IssueKey(KeyCreate{
		ProjectID: project.ID, PrincipalID: principal.ID, Name: "second",
	})
	p1, _, _ := ResolveAPIKey(first.Token)
	p2, _, _ := ResolveAPIKey(second.Token)
	now := time.Date(2026, 7, 10, 4, 0, 0, 0, time.UTC)
	if err := CheckAndConsumeRequest(p1, now); err != nil {
		t.Fatal(err)
	}
	if err := CheckAndConsumeRequest(p2, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	var exceeded *QuotaExceeded
	err := CheckAndConsumeRequest(p1, now.Add(2*time.Minute))
	if !errors.As(err, &exceeded) || exceeded.Metric != "project requests/day" {
		t.Fatalf("err=%v, want aggregate project quota", err)
	}
}

// externalKeyPrincipal publishes document as the only external key source and
// resolves token through it.
func externalKeyPrincipal(t *testing.T, document, token string) *config.Principal {
	t.Helper()
	keys, err := decodeExternalKeys([]byte(document), false)
	if err != nil {
		t.Fatal(err)
	}
	store := &externalKeyStore{sources: []*externalKeySource{{name: "file", keys: keys}}, now: time.Now}
	store.publish()
	activeExternalKeys.Store(store)
	t.Cleanup(func() { activeExternalKeys.Store(nil) })
	principal, found, err := ResolveAPIKey(token)
	if err != nil || !found || principal.KeyID != "" || principal.ProjectID == "" {
		t.Fatalf("external principal=%+v found=%v err=%v", principal, found, err)
	}
	return principal
}

func TestExternalKeysConsumeProjectQuota(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	ResetForTests()
	t.Cleanup(ResetForTests)
	project, err := CreateProject("external-budget", "External Budget")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SetProjectPolicy(project.ID, KeyPolicy{RPM: 1, DailyInputTokens: 100}); err != nil {
		t.Fatal(err)
	}
	p := externalKeyPrincipal(t, `{"version":1,"keys":[{"name":"ci","project":"external-budget","key":"ci-secret"}]}`, "ci-secret")
	now := time.Date(2026, 7, 10, 4, 0, 0, 0, time.UTC)
	if err := CheckAndConsumeRequest(p, now); err != nil {
		t.Fatal(err)
	}
	var exceeded *QuotaExceeded
	if err := CheckAndConsumeRequest(p, now); !errors.As(err, &exceeded) ||
		exceeded.Metric != "project requests/minute" {
		t.Fatalf("second request err=%v, want project RPM quota", err)
	}
	if err := RecordUsageEvent(UsageEvent{
		Timestamp: now.Unix(), Endpoint: "openai.chat", StatusCode: 200,
		ProjectID: p.ProjectID, InputTokens: 100, OutputTokens: 5,
	}); err != nil {
		t.Fatal(err)
	}
	err = CheckAndConsumeRequest(p, now.Add(time.Minute))
	if !errors.As(err, &exceeded) || exceeded.Metric != "project input tokens/day" {
		t.Fatalf("err=%v, want the project token budget spent by external-key usage", err)
	}

	for _, unmetered := range []*config.Principal{
		{Project: "admin", Key: "admin"}, {Project: "local", Key: "local"},
	} {
		if err := CheckAndConsumeRequest(unmetered, now); err != nil {
			t.Fatalf("%s: %v", unmetered.Key, err)
		}
	}
	db, err := DB()
	if err != nil {
		t.Fatal(err)
	}
	var foreign int
	if err := db.QueryRow(`
SELECT (SELECT COUNT(*) FROM quota_counters)+
       (SELECT COUNT(*) FROM project_quota_counters WHERE project_id<>?)`, project.ID,
	).Scan(&foreign); err != nil || foreign != 0 {
		t.Fatalf("unmetered principals wrote %d counter rows, err=%v", foreign, err)
	}
}
