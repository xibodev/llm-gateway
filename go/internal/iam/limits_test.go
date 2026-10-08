package iam

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

// A key's report lists every limit its own policy and its project's set, and
// no other, each with what the window it counts has used and when that
// window ends; a project's lists the project's alone.
func TestLimitUsageReportsEverySetLimitWithItsWindow(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	ResetForTests()
	t.Cleanup(ResetForTests)
	p, _ := quotaPrincipal(t, KeyPolicy{RPM: 4, DailyInputTokens: 1000})
	if _, err := SetProjectPolicy(p.ProjectID, KeyPolicy{DailyRequests: 10, MonthlyCostMicroUSD: 5000}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 10, 1, 2, 3, 0, time.UTC)
	for range 2 {
		if err := CheckAndConsumeRequest(p, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := RecordUsageEvent(UsageEvent{
		Timestamp: now.Unix(), Endpoint: "openai.chat", StatusCode: 200,
		ProjectID: p.ProjectID, PrincipalID: p.PrincipalID, KeyID: p.KeyID,
		InputTokens: 400, OutputTokens: 9, CostMicroUSD: 1000,
	}); err != nil {
		t.Fatal(err)
	}
	key, found, err := APIKeyByID(p.KeyID)
	if err != nil || !found {
		t.Fatalf("key found=%v err=%v", found, err)
	}
	minute := time.Date(2026, 7, 10, 1, 3, 0, 0, time.UTC).Unix()
	day := time.Date(2026, 7, 11, 0, 0, 0, 0, time.UTC).Unix()
	month := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC).Unix()
	projectLimits := []LimitUsage{
		{Scope: "project", Field: "daily_requests", Metric: "requests", Period: "day", Limit: 10, Used: 2, ResetsAt: day},
		{Scope: "project", Field: "monthly_cost_microusd", Metric: "cost_microusd", Period: "month", Limit: 5000, Used: 1000, ResetsAt: month},
	}
	want := append([]LimitUsage{
		{Scope: "key", Field: "rpm", Metric: "requests", Period: "minute", Limit: 4, Used: 2, ResetsAt: minute},
		{Scope: "key", Field: "daily_input_tokens", Metric: "input_tokens", Period: "day", Limit: 1000, Used: 400, ResetsAt: day},
	}, projectLimits...)
	got, err := KeyLimitUsage(key, now)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("key limits err=%v\n got %+v\nwant %+v", err, got, want)
	}
	// The next minute counts afresh; the day and month go on.
	later, err := KeyLimitUsage(key, now.Add(time.Minute))
	if err != nil || later[0].Used != 0 || later[0].ResetsAt != minute+60 || later[1].Used != 400 {
		t.Fatalf("next minute's limits err=%v: %+v", err, later)
	}
	got, err = ProjectLimitUsage(p.ProjectID, now)
	if err != nil || !reflect.DeepEqual(got, projectLimits) {
		t.Fatalf("project limits err=%v\n got %+v\nwant %+v", err, got, projectLimits)
	}
	unlimited, err := ProjectLimitUsage("project-without-policy", now)
	if err != nil || len(unlimited) != 0 {
		t.Fatalf("a project without a policy reports %+v, err=%v", unlimited, err)
	}
}

// A refusal names the limit that refused it, as the request's usage records.
func TestQuotaRefusalsNameTheirLimit(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	ResetForTests()
	t.Cleanup(ResetForTests)
	p, _ := quotaPrincipal(t, KeyPolicy{RPM: 1})
	if _, err := SetProjectPolicy(p.ProjectID, KeyPolicy{DailyRequests: 2}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 10, 1, 2, 3, 0, time.UTC)
	if err := CheckAndConsumeRequest(p, now); err != nil {
		t.Fatal(err)
	}
	var exceeded *QuotaExceeded
	if err := CheckAndConsumeRequest(p, now); !errors.As(err, &exceeded) || exceeded.Code() != "quota:key:rpm" {
		t.Fatalf("key refusal err=%v", err)
	}
	if err := CheckAndConsumeRequest(p, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := CheckAndConsumeRequest(p, now.Add(2*time.Minute)); !errors.As(err, &exceeded) ||
		exceeded.Code() != "quota:project:daily_requests" || exceeded.Metric != "project requests/day" {
		t.Fatalf("project refusal err=%v", err)
	}
}

func TestClosestLimitIsTheMostUsedAndRefusesLongest(t *testing.T) {
	day, month := int64(100), int64(200)
	for name, check := range map[string]struct {
		limits  []LimitUsage
		closest int
	}{
		"nothing used marks nothing": {[]LimitUsage{
			{Limit: 5, ResetsAt: day}, {Limit: 9, ResetsAt: month},
		}, -1},
		"the largest share of its limit": {[]LimitUsage{
			{Limit: 10, Used: 4, ResetsAt: month}, {Limit: 1000, Used: 500, ResetsAt: day},
		}, 1},
		"of limits used up, the one refusing longest": {[]LimitUsage{
			{Limit: 100, Used: 150, ResetsAt: day}, {Limit: 7, Used: 7, ResetsAt: month}, {Limit: 3, Used: 2, ResetsAt: month},
		}, 1},
	} {
		MarkClosestLimit(check.limits)
		for index, limit := range check.limits {
			if limit.Closest != (index == check.closest) {
				t.Fatalf("%s: limit %d closest=%v, want limit %d", name, index, limit.Closest, check.closest)
			}
		}
	}
}
