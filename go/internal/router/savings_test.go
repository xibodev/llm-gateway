package router

import (
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
)

// A request that consumed tokens is charged its credit and their cost
// whatever its status, as a stream that failed or lost its client midway
// consumed a model; one that consumed none and failed is charged nothing.
func TestRecordUsageChargesConsumedTokensWhateverTheStatus(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	ResetSavingsState()
	old := *config.Get()
	t.Cleanup(func() {
		iam.ResetForTests()
		ResetSavingsState()
		config.Update(func(s *config.Settings) { *s = old })
	})
	config.Update(func(s *config.Settings) {
		s.Savings.Enabled = false
		s.Savings.PriceCatalog = nil
	})
	db, err := iam.DB()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name                      string
		status, input, output     int
		wantCredits, wantMicroUSD int64
	}{
		// gpt-4o costs 2.50 and 10 US dollars per million tokens.
		{"completed", 200, 10, 5, 1000, 75},
		{"cancelled midway", 499, 10, 5, 1000, 75},
		{"failed midway", 502, 10, 0, 1000, 25},
		{"failed before a model", 502, 0, 0, 0, 0},
		{"completed without usage", 200, 0, 0, 1000, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			RecordUsage(UsageRecord{
				Endpoint: "openai.chat", RequestedModel: tc.name, RoutedModel: "gpt-4o",
				Provider: "fixture", InputTokens: tc.input, OutputTokens: tc.output, StatusCode: tc.status,
			})
			var credits, cost int64
			if err := db.QueryRow(`SELECT credits_milli, cost_microusd FROM usage_events WHERE requested_model = ?`, tc.name).Scan(&credits, &cost); err != nil {
				t.Fatal(err)
			}
			if credits != tc.wantCredits || cost != tc.wantMicroUSD {
				t.Fatalf("credits=%d cost=%d micro-USD, want %d and %d", credits, cost, tc.wantCredits, tc.wantMicroUSD)
			}
		})
	}
}
