package iam

import (
	"errors"
	"slices"
	"testing"
	"time"
)

// The deliveries listing shows every outbox event, newest first, with how
// many attempts it made of how many it may, when a worker may next claim it,
// and its last error, filtered by status and kind and paged with a cursor.
func TestDeliveriesListEveryOutboxEventWithItsNextAttempt(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	ResetForTests()
	t.Cleanup(ResetForTests)
	db, err := DB()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	insert := func(kind string) int64 {
		t.Helper()
		result, err := db.Exec(`INSERT INTO outbox_events(ts,kind,payload_json,status,attempts,available_at)
			VALUES(?,?,'{"rule_id":"fixture"}','pending',0,?)`, now, kind, now)
		if err != nil {
			t.Fatal(err)
		}
		id, err := result.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	claim := func(want int64) {
		t.Helper()
		claimed, err := ClaimOutbox("worker-one", 1, time.Minute)
		if err != nil || len(claimed) != 1 || claimed[0].ID != want {
			t.Fatalf("claimed=%+v err=%v, want event %d", claimed, err, want)
		}
	}
	// Each event is claimed and settled before the next is added, since a
	// claim takes the oldest event available.
	pending := insert("quota_warning")
	if _, err := db.Exec("UPDATE outbox_events SET available_at=? WHERE id=?", now+3600, pending); err != nil {
		t.Fatal(err)
	}
	delivered := insert("quota_warning")
	claim(delivered)
	if err := MarkOutboxDelivered(delivered, "worker-one"); err != nil {
		t.Fatal(err)
	}
	failed := insert("key_expiring")
	claim(failed)
	retryAt := now + 600
	if err := MarkOutboxFailed(failed, "worker-one", "webhook answered 503", retryAt); err != nil {
		t.Fatal(err)
	}
	exhausted := insert("quota_exhausted")
	for range maxOutboxAttempts {
		claim(exhausted)
		if err := MarkOutboxFailed(exhausted, "worker-one", "webhook refused", now-1); err != nil {
			t.Fatal(err)
		}
	}
	leased := insert("quota_warning")
	claim(leased)

	all, next, err := ListOutboxDeliveries(OutboxFilter{})
	if err != nil || next != 0 {
		t.Fatalf("listing: next=%d err=%v", next, err)
	}
	ids := func(deliveries []OutboxDelivery) []int64 {
		out := []int64{}
		for _, delivery := range deliveries {
			out = append(out, delivery.ID)
		}
		return out
	}
	if got, want := ids(all), []int64{leased, exhausted, failed, delivered, pending}; !slices.Equal(got, want) {
		t.Fatalf("listed %v, want %v", got, want)
	}
	byID := map[int64]OutboxDelivery{}
	for _, delivery := range all {
		byID[delivery.ID] = delivery
		if delivery.MaxAttempts != maxOutboxAttempts || delivery.Payload["rule_id"] != "fixture" {
			t.Fatalf("delivery %d = %+v", delivery.ID, delivery)
		}
	}
	for _, tc := range []struct {
		id          int64
		status      string
		attempts    int
		exhausted   bool
		nextAttempt int64
		lastError   string
	}{
		{pending, "pending", 0, false, now + 3600, ""},
		{delivered, "delivered", 1, false, 0, ""},
		{failed, "failed", 1, false, retryAt, "webhook answered 503"},
		{exhausted, "failed", maxOutboxAttempts, true, 0, "webhook refused"},
		{leased, "pending", 0, false, byID[leased].LeaseUntil, ""},
	} {
		got := byID[tc.id]
		if got.Status != tc.status || got.Attempts != tc.attempts || got.Exhausted != tc.exhausted ||
			got.NextAttemptAt != tc.nextAttempt || got.LastError != tc.lastError {
			t.Errorf("delivery %d = %+v, want status %s, %d attempts, exhausted %v, next attempt %d, last error %q",
				tc.id, got, tc.status, tc.attempts, tc.exhausted, tc.nextAttempt, tc.lastError)
		}
	}
	if byID[leased].LeaseUntil <= now || byID[leased].ClaimedBy != "worker-one" {
		t.Fatalf("leased delivery = %+v, want the worker's lease", byID[leased])
	}

	for status, want := range map[string][]int64{
		"pending": {leased, pending}, "delivered": {delivered},
		"failed": {failed}, "exhausted": {exhausted},
	} {
		got, _, err := ListOutboxDeliveries(OutboxFilter{Status: status})
		if err != nil || !slices.Equal(ids(got), want) {
			t.Errorf("status %s: listed %v, err %v, want %v", status, ids(got), err, want)
		}
	}
	if got, _, err := ListOutboxDeliveries(OutboxFilter{Kind: "key_expiring"}); err != nil || !slices.Equal(ids(got), []int64{failed}) {
		t.Errorf("kind key_expiring: listed %v, err %v", ids(got), err)
	}
	var invalid *InvalidFilterError
	if _, _, err := ListOutboxDeliveries(OutboxFilter{Status: "stuck"}); !errors.As(err, &invalid) {
		t.Errorf("an unknown status: err=%v, want an invalid filter", err)
	}

	first, cursor, err := ListOutboxDeliveries(OutboxFilter{Limit: 2})
	if err != nil || !slices.Equal(ids(first), []int64{leased, exhausted}) || cursor != exhausted {
		t.Fatalf("first page %v, cursor %d, err %v", ids(first), cursor, err)
	}
	second, cursor, err := ListOutboxDeliveries(OutboxFilter{Limit: 2, BeforeID: cursor})
	if err != nil || !slices.Equal(ids(second), []int64{failed, delivered}) || cursor != delivered {
		t.Fatalf("second page %v, cursor %d, err %v", ids(second), cursor, err)
	}
	last, cursor, err := ListOutboxDeliveries(OutboxFilter{Limit: 2, BeforeID: cursor})
	if err != nil || !slices.Equal(ids(last), []int64{pending}) || cursor != 0 {
		t.Fatalf("last page %v, cursor %d, err %v", ids(last), cursor, err)
	}
}
