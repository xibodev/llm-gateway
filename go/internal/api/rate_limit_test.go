package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"
)

// The limiter counts each caller's requests in one UTC minute and starts
// afresh with the next; the wait it reports ends at that minute.
func TestCallerRateLimiterCountsEachCallerPerMinute(t *testing.T) {
	now := time.Date(2026, time.October, 5, 12, 0, 45, 500_000_000, time.UTC)
	limiter := &callerRateLimiter{now: func() time.Time { return now }}
	for range 2 {
		if _, ok := limiter.admit("key:a", 2); !ok {
			t.Fatal("a request under the limit was refused")
		}
	}
	wait, ok := limiter.admit("key:a", 2)
	if ok || wait != 14500*time.Millisecond {
		t.Fatalf("third request: ok=%v wait=%v, want a refusal until the next minute", ok, wait)
	}
	if _, ok := limiter.admit("key:b", 2); !ok {
		t.Fatal("another caller shares the first caller's count")
	}
	if _, ok := limiter.admit("key:a", 0); !ok {
		t.Fatal("a limit of zero refused a request")
	}
	now = now.Add(15 * time.Second)
	if _, ok := limiter.admit("key:a", 2); !ok {
		t.Fatal("the next minute did not start afresh")
	}
	if len(limiter.counts) != 1 {
		t.Fatalf("counts kept from the previous minute: %v", limiter.counts)
	}
}

// LLMGW_RATE_LIMIT_PER_MINUTE covers callers no quota meters, such as the
// static administrator key, and refuses in each surface's own error shape
// with the wait until the caller's next minute.
func TestRateLimitPerMinuteRefusesWithRetryAfter(t *testing.T) {
	handler := setupAPISmokeFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"choices": []any{map[string]any{
			"message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop",
		}}})
	}))
	now := time.Date(2026, time.October, 5, 12, 0, 30, 0, time.UTC)
	previous := callerRates
	callerRates = &callerRateLimiter{now: func() time.Time { return now }}
	t.Cleanup(func() { callerRates = previous })
	config.Update(func(s *config.Settings) { s.RateLimitPerMinute = 2 })

	hello := []any{map[string]any{"role": "user", "content": "hi"}}
	chat := map[string]any{"model": "fixture/model", "messages": hello}
	for i := range 2 {
		if w := apiSmokeRequest(handler, "/v1/chat/completions", chat); w.Code != http.StatusOK {
			t.Fatalf("request %d: status=%d body=%s", i+1, w.Code, w.Body.String())
		}
	}
	w := apiSmokeRequest(handler, "/v1/chat/completions", chat)
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "30" ||
		!strings.Contains(w.Body.String(), `"type":"invalid_request_error"`) || !strings.Contains(w.Body.String(), "2 requests per minute") {
		t.Fatalf("chat over the limit: status=%d retry-after=%q body=%s", w.Code, w.Header().Get("Retry-After"), w.Body.String())
	}
	w = apiSmokeRequest(handler, "/v1/messages", map[string]any{"model": "fixture/model", "max_tokens": 8, "messages": hello})
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "30" || !strings.Contains(w.Body.String(), `"type":"rate_limit_error"`) {
		t.Fatalf("messages over the limit: status=%d retry-after=%q body=%s", w.Code, w.Header().Get("Retry-After"), w.Body.String())
	}
	db, err := iam.DB()
	if err != nil {
		t.Fatal(err)
	}
	var refused int
	if err := db.QueryRow("SELECT COUNT(*) FROM usage_events WHERE status_code=429 AND error_code='rate_limit'").Scan(&refused); err != nil || refused != 2 {
		t.Fatalf("recorded refusals=%d err=%v", refused, err)
	}
	now = now.Add(30 * time.Second)
	if w := apiSmokeRequest(handler, "/v1/chat/completions", chat); w.Code != http.StatusOK {
		t.Fatalf("the next minute: status=%d body=%s", w.Code, w.Body.String())
	}
}

// A request a key's quota refuses says when the quota's window ends.
func TestQuotaRefusalsCarryRetryAfter(t *testing.T) {
	handler := setupAPISmokeFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"choices": []any{map[string]any{
			"message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop",
		}}})
	}))
	token := admissionFixtureKey(t, "retry-after", iam.KeyPolicy{DailyRequests: 1})
	chat := map[string]any{"model": "fixture/model", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	if w := admissionRequest(handler, "/v1/chat/completions", token, nil, chat); w.Code != http.StatusOK {
		t.Fatalf("first request: status=%d body=%s", w.Code, w.Body.String())
	}
	w := admissionRequest(handler, "/v1/chat/completions", token, nil, chat)
	seconds, err := strconv.Atoi(w.Header().Get("Retry-After"))
	if w.Code != http.StatusTooManyRequests || err != nil || seconds < 1 || seconds > 24*60*60 {
		t.Fatalf("second request: status=%d retry-after=%q body=%s", w.Code, w.Header().Get("Retry-After"), w.Body.String())
	}
	var envelope struct {
		Error struct{ Message string } `json:"error"`
	}
	if json.Unmarshal(w.Body.Bytes(), &envelope) != nil || !strings.Contains(envelope.Error.Message, "requests/day quota exceeded") {
		t.Fatalf("body=%s", w.Body.String())
	}
	midnight := time.Now().UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
	if want := int(time.Until(midnight).Seconds()); seconds < want-5 || seconds > want+5 {
		t.Fatalf("retry-after=%d, want the seconds until UTC midnight (%d)", seconds, want)
	}
}
