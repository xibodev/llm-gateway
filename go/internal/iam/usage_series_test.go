package iam

import (
	"testing"
	"time"
)

func TestUsageTimeSeriesUsesUTCBoundariesAndFilters(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	ResetForTests()
	t.Cleanup(ResetForTests)
	principal, err := CreatePrincipal("human", "authentik:series", "", "Series")
	if err != nil {
		t.Fatal(err)
	}
	project, err := CreateProject("series-project", "Series Project")
	if err != nil {
		t.Fatal(err)
	}
	if err := SetMembership(project.ID, principal.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	first := time.Date(2026, 7, 19, 23, 30, 0, 0, time.UTC)
	second := time.Date(2026, 7, 20, 0, 10, 0, 0, time.UTC)
	for _, event := range []UsageEvent{
		{Timestamp: first.Unix(), Endpoint: "openai.chat", StatusCode: 200, Provider: "echo", RoutedModel: "echo-default", ProjectID: project.ID, PrincipalID: principal.ID, InputTokens: 2, OutputTokens: 1},
		{Timestamp: second.Unix(), Endpoint: "openai.chat", StatusCode: 502, Provider: "other", RoutedModel: "other-model", ProjectID: project.ID, PrincipalID: principal.ID, InputTokens: 3, OutputTokens: 4},
	} {
		if err := RecordUsageEvent(event); err != nil {
			t.Fatal(err)
		}
	}
	series, err := UsageTimeSeries(UsageTimeSeriesFilter{
		From:   time.Date(2026, 7, 19, 23, 0, 0, 0, time.UTC).Unix(),
		To:     time.Date(2026, 7, 20, 2, 0, 0, 0, time.UTC).Unix(),
		Bucket: "hour", ProjectID: project.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(series) != 3 {
		t.Fatalf("bucket count=%d series=%+v", len(series), series)
	}
	if series[0].Start != first.Truncate(time.Hour).Unix() || series[0].Requests != 1 || series[1].Requests != 1 || series[1].Errors != 1 || series[2].Requests != 0 {
		t.Fatalf("UTC buckets=%+v", series)
	}
	filtered, err := UsageTimeSeries(UsageTimeSeriesFilter{
		From: first.Add(-time.Hour).Unix(), To: second.Add(time.Hour).Unix(), Bucket: "day", Provider: "echo", Model: "echo-default", ProjectID: project.ID, PrincipalID: principal.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 2 || filtered[0].Requests != 1 || filtered[0].InputTokens != 2 || filtered[1].Requests != 0 {
		t.Fatalf("filtered series=%+v", filtered)
	}
	if _, err := UsageTimeSeries(UsageTimeSeriesFilter{From: first.Unix(), To: second.Unix(), Bucket: "minute"}); err == nil {
		t.Fatal("invalid bucket unexpectedly succeeded")
	}
}
