package providers

import (
	"context"
	"testing"

	"llmgw/internal/iam"
)

type fixtureQuotaAdapter struct{ id string }

func (adapter fixtureQuotaAdapter) ID() string { return adapter.id }

func (fixtureQuotaAdapter) Fetch(
	context.Context, QuotaFetchRequest,
) (QuotaFetchResult, error) {
	return QuotaFetchResult{
		Snapshots: []iam.ProviderQuotaSnapshot{{Metric: "requests", Unit: "requests", Window: "day"}},
	}, nil
}

func TestQuotaAdapterRegistry(t *testing.T) {
	resetQuotaAdaptersForTests()
	t.Cleanup(resetQuotaAdaptersForTests)
	adapter := fixtureQuotaAdapter{id: " fixture "}
	if err := RegisterQuotaAdapter(adapter); err != nil {
		t.Fatal(err)
	}
	got, ok := QuotaAdapterByID("FIXTURE")
	if !ok || got.ID() != adapter.ID() {
		t.Fatalf("adapter=%v ok=%v", got, ok)
	}
	if err := RegisterQuotaAdapter(adapter); err == nil {
		t.Fatal("duplicate quota adapter should be rejected")
	}
	if err := RegisterQuotaAdapter(fixtureQuotaAdapter{id: "bad id"}); err == nil {
		t.Fatal("invalid quota adapter id should be rejected")
	}
}
