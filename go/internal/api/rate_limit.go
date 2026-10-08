package api

import (
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"llmgw/internal/config"
)

// callerRates enforces LLMGW_RATE_LIMIT_PER_MINUTE for every data-plane
// request the process admits.
var callerRates = &callerRateLimiter{now: time.Now}

// callerRateLimiter counts the requests each caller starts in one UTC minute
// on this process. Unlike key and project quotas it keeps no durable state
// and covers every caller, the static administrator key and local mode
// included, so no single client can take a process for itself however its
// key is configured. Only the current minute's counts are kept, so its memory
// is bounded by the callers active in one minute.
type callerRateLimiter struct {
	mu     sync.Mutex
	now    func() time.Time
	minute int64
	counts map[string]int
}

// admit takes one request of caller under limit, or reports how long until
// the next minute when the caller has used this one. A limit that is not
// positive admits everything.
func (l *callerRateLimiter) admit(caller string, limit int) (time.Duration, bool) {
	if limit <= 0 {
		return 0, true
	}
	now := l.now()
	minute := now.Unix() / 60 * 60
	l.mu.Lock()
	defer l.mu.Unlock()
	if minute != l.minute || l.counts == nil {
		l.minute, l.counts = minute, map[string]int{}
	}
	if l.counts[caller] >= limit {
		return time.Unix(minute+60, 0).Sub(now), false
	}
	l.counts[caller]++
	return 0, true
}

// used reports how many requests caller has started in the current minute
// on this process, and when that minute ends.
func (l *callerRateLimiter) used(caller string) (int, time.Time) {
	minute := l.now().Unix() / 60 * 60
	l.mu.Lock()
	defer l.mu.Unlock()
	count := 0
	if minute == l.minute {
		count = l.counts[caller]
	}
	return count, time.Unix(minute+60, 0).UTC()
}

// rateLimitCaller names what a request counts against: its gateway-issued
// key, or for a caller without one, the caller the request was attributed
// to: an external key, the static administrator key or local mode.
func rateLimitCaller(p *config.Principal) string {
	switch {
	case p == nil:
		return ""
	case p.KeyID != "":
		return "key:" + p.KeyID
	case p.Caller.ID != "":
		return string(p.Caller.Kind) + ":" + p.Caller.ID
	}
	return p.Project + "/" + p.Key
}

// admitRate admits one request of p under the process-wide limit, or writes
// a 429 with the wait until the caller's next minute.
func admitRate(w http.ResponseWriter, p *config.Principal) (string, bool) {
	limit := config.Get().RateLimitPerMinute
	wait, ok := callerRates.admit(rateLimitCaller(p), limit)
	if ok {
		return "", true
	}
	setRetryAfter(w, wait)
	return fmt.Sprintf("Rate limit exceeded: this gateway admits %d requests per minute from one caller.", limit), false
}

// setRetryAfter sets Retry-After to wait in whole seconds, rounded up so a
// client never repeats sooner than the gateway admits it.
func setRetryAfter(w http.ResponseWriter, wait time.Duration) {
	if wait <= 0 {
		return
	}
	seconds := int64(wait / time.Second)
	if wait%time.Second != 0 {
		seconds++
	}
	w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
}
