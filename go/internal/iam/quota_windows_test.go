package iam

import (
	"testing"
	"time"
)

// A refused request's quota window ends at the next UTC minute, day or month,
// whatever the zone of the time it was refused at.
func TestQuotaWindowsEndAtTheirUTCBoundaries(t *testing.T) {
	ends := quotaWindowEnds(time.Date(2026, time.December, 31, 23, 59, 30, 0, time.FixedZone("fixture", 3600)))
	for name, check := range map[string]struct{ got, want time.Time }{
		"minute": {ends.minute, time.Date(2026, time.December, 31, 23, 0, 0, 0, time.UTC)},
		"day":    {ends.day, time.Date(2027, time.January, 1, 0, 0, 0, 0, time.UTC)},
		"month":  {ends.month, time.Date(2027, time.January, 1, 0, 0, 0, 0, time.UTC)},
	} {
		if !check.got.Equal(check.want) {
			t.Fatalf("%s window ends %v, want %v", name, check.got, check.want)
		}
	}
	exceeded := checkPolicyCounters("key", KeyPolicy{RPM: 1, DailyRequests: 9}, quotaCounters{minute: quotaCounter{Requests: 1}, day: quotaCounter{Requests: 1}}, ends)
	if refusal, ok := exceeded.(*QuotaExceeded); !ok || refusal.Metric != "requests/minute" || refusal.Code() != "quota:key:rpm" || !refusal.Reset.Equal(ends.minute) {
		t.Fatalf("refusal = %#v, want the minute limit resetting at the minute's end", exceeded)
	}
}
