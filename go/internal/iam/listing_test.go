package iam

import (
	"reflect"
	"testing"
)

func listingStore(t *testing.T) {
	t.Helper()
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	ResetForTests()
	t.Cleanup(ResetForTests)
	if _, err := Initialize(); err != nil {
		t.Fatal(err)
	}
}

// listingKey issues a key for a new principal in a new project and returns
// the principal's and the key's IDs, which rows must reference.
func listingKey(t *testing.T, name string) (principalID, keyID string) {
	t.Helper()
	principal, err := CreatePrincipal("human", "fixture:"+name, "", name)
	if err != nil {
		t.Fatal(err)
	}
	project, err := CreateProject(name, name)
	if err != nil {
		t.Fatal(err)
	}
	if err := SetMembership(project.ID, principal.ID, "member"); err != nil {
		t.Fatal(err)
	}
	issued, err := IssueKey(KeyCreate{ProjectID: project.ID, PrincipalID: principal.ID, Name: name})
	if err != nil {
		t.Fatal(err)
	}
	return principal.ID, issued.ID
}

// Recorded requests are listed newest first, filtered, and paged by a
// cursor that never repeats or skips a row.
func TestListUsageEventsFiltersAndPages(t *testing.T) {
	listingStore(t)
	_, k1 := listingKey(t, "one")
	_, k2 := listingKey(t, "two")
	for i, event := range []UsageEvent{
		{RequestID: "req-1", Timestamp: 1000, Endpoint: "chat", StatusCode: 200, Provider: "openai", RoutedModel: "a", KeyID: k1, InputTokens: 5},
		{RequestID: "req-2", Timestamp: 1010, Endpoint: "chat", StatusCode: 429, Provider: "openai", RoutedModel: "a", KeyID: k2, ErrorCode: "rate_limit"},
		{RequestID: "req-3", Timestamp: 1020, Endpoint: "messages", StatusCode: 200, Provider: "anthropic", RoutedModel: "b", KeyID: k1},
		{RequestID: "req-4", Timestamp: 1030, Endpoint: "chat", StatusCode: 502, Provider: "openai", RoutedModel: "b", KeyID: k1},
		{RequestID: "req-5", Timestamp: 1040, Endpoint: "chat", StatusCode: 200, Provider: "echo", RoutedModel: "echo", IsStub: true},
	} {
		if err := RecordUsageEvent(event); err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
	}
	ids := func(events []RecordedRequest) []string {
		out := []string{}
		for _, event := range events {
			out = append(out, event.RequestID)
		}
		return out
	}
	for name, check := range map[string]struct {
		filter UsageEventFilter
		want   []string
	}{
		"everything":        {UsageEventFilter{}, []string{"req-5", "req-4", "req-3", "req-2", "req-1"}},
		"a provider":        {UsageEventFilter{Provider: "openai"}, []string{"req-4", "req-2", "req-1"}},
		"a model and key":   {UsageEventFilter{Model: "b", KeyID: k1}, []string{"req-4", "req-3"}},
		"errors":            {UsageEventFilter{Status: "error"}, []string{"req-4", "req-2"}},
		"successes":         {UsageEventFilter{Status: "ok", Provider: "openai"}, []string{"req-1"}},
		"one code":          {UsageEventFilter{Status: "429"}, []string{"req-2"}},
		"a time range":      {UsageEventFilter{From: 1010, To: 1030}, []string{"req-3", "req-2"}},
		"one request":       {UsageEventFilter{RequestID: "req-3"}, []string{"req-3"}},
		"older than a page": {UsageEventFilter{BeforeID: 3}, []string{"req-2", "req-1"}},
	} {
		events, next, err := ListUsageEvents(check.filter)
		if err != nil || next != 0 || !reflect.DeepEqual(ids(events), check.want) {
			t.Fatalf("%s: ids=%v next=%d err=%v, want %v", name, ids(events), next, err, check.want)
		}
	}
	first, next, err := ListUsageEvents(UsageEventFilter{Limit: 2})
	if err != nil || !reflect.DeepEqual(ids(first), []string{"req-5", "req-4"}) || next == 0 {
		t.Fatalf("first page: %v next=%d err=%v", ids(first), next, err)
	}
	second, next, err := ListUsageEvents(UsageEventFilter{Limit: 2, BeforeID: next})
	if err != nil || !reflect.DeepEqual(ids(second), []string{"req-3", "req-2"}) || next == 0 {
		t.Fatalf("second page: %v next=%d err=%v", ids(second), next, err)
	}
	last, next, err := ListUsageEvents(UsageEventFilter{Limit: 2, BeforeID: next})
	if err != nil || !reflect.DeepEqual(ids(last), []string{"req-1"}) || next != 0 {
		t.Fatalf("last page: %v next=%d err=%v", ids(last), next, err)
	}
	if !first[0].Stub || first[1].Stub || first[1].StatusCode != 502 || first[1].Provider != "openai" {
		t.Fatalf("rows: %+v", first)
	}
	if _, _, err := ListUsageEvents(UsageEventFilter{Status: "bad"}); err == nil {
		t.Fatal("an unknown status filter was accepted")
	}

	stats, err := UsageStatsFor(UsageTimeSeriesFilter{From: 1000, To: 1030, Provider: "openai"})
	if err != nil {
		t.Fatal(err)
	}
	if totals := stats["totals"].(UsageTotals); totals.Requests != 2 || totals.Errors != 1 || totals.InputTokens != 5 {
		t.Fatalf("filtered totals = %+v", totals)
	}
}

func TestListAuditFiltersAndPages(t *testing.T) {
	listingStore(t)
	p1, _ := listingKey(t, "p1")
	p2, k9 := listingKey(t, "p2")
	for _, event := range []AuditEvent{
		{Timestamp: 100, ActorPrincipalID: p1, Action: "provider.update", TargetType: "provider", TargetID: "openai"},
		{Timestamp: 110, ActorKeyID: k9, Action: "provider.delete", TargetType: "provider", TargetID: "old", Result: "denied"},
		{Timestamp: 120, ActorPrincipalID: p2, Action: "key.issue", TargetType: "key", TargetID: "k1"},
		{Timestamp: 130, ActorPrincipalID: p1, Action: "provider_x.update", TargetType: "provider", TargetID: "x"},
	} {
		if err := RecordAudit(event); err != nil {
			t.Fatal(err)
		}
	}
	actions := func(events []AuditEvent) []string {
		out := []string{}
		for _, event := range events {
			out = append(out, event.Action)
		}
		return out
	}
	for name, check := range map[string]struct {
		filter AuditFilter
		want   []string
	}{
		// The prefix is literal: "_" matches only an underscore.
		"an action prefix": {AuditFilter{Action: "provider."}, []string{"provider.delete", "provider.update"}},
		"an underscore":    {AuditFilter{Action: "provider_"}, []string{"provider_x.update"}},
		"an actor":         {AuditFilter{Actor: p1}, []string{"provider_x.update", "provider.update"}},
		"an actor key":     {AuditFilter{Actor: k9}, []string{"provider.delete"}},
		"a result":         {AuditFilter{Result: "denied"}, []string{"provider.delete"}},
		"a target":         {AuditFilter{TargetType: "key", TargetID: "k1"}, []string{"key.issue"}},
		"a time range":     {AuditFilter{From: 110, To: 130}, []string{"key.issue", "provider.delete"}},
	} {
		events, next, err := ListAuditFiltered(check.filter)
		if err != nil || next != 0 || !reflect.DeepEqual(actions(events), check.want) {
			t.Fatalf("%s: %v next=%d err=%v, want %v", name, actions(events), next, err, check.want)
		}
	}
	page, next, err := ListAuditFiltered(AuditFilter{Limit: 3})
	if err != nil || len(page) != 3 || next != page[2].ID {
		t.Fatalf("first page: %v next=%d err=%v", actions(page), next, err)
	}
	rest, next, err := ListAuditFiltered(AuditFilter{Limit: 3, BeforeID: next})
	if err != nil || !reflect.DeepEqual(actions(rest), []string{"provider.update"}) || next != 0 {
		t.Fatalf("second page: %v next=%d err=%v", actions(rest), next, err)
	}
}
