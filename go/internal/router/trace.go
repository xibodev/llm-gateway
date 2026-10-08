package router

import (
	"context"
	"sync"
)

type traceSinkKey struct{}

// AttemptTraceSink keeps the routing trace of a chain run under the context
// it travels in: which targets the chain tried, in order, and how each
// attempt ended. Every chain reports to it, streaming or not. A stream's
// chain ends when its stream opens, so its trace is complete before the
// stream's first byte.
type AttemptTraceSink struct {
	mu       sync.Mutex
	attempts []AttemptTrace
}

// WithAttemptTrace returns ctx carrying a new sink, and the sink.
func WithAttemptTrace(ctx context.Context) (context.Context, *AttemptTraceSink) {
	sink := &AttemptTraceSink{}
	return context.WithValue(ctx, traceSinkKey{}, sink), sink
}

// Attempts returns the trace of the last chain run under the sink's
// context, or nothing before one has run.
func (s *AttemptTraceSink) Attempts() []AttemptTrace {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]AttemptTrace{}, s.attempts...)
}

// keep replaces the sink's trace with that of a chain's attempts. A chain
// times its attempts only where it says so, so these carry no duration.
func (s *AttemptTraceSink) keep(attempts []attempt) {
	trace := make([]AttemptTrace, 0, len(attempts))
	for _, attempt := range attempts {
		status := "failed"
		if attempt.OK {
			status = "served"
		}
		trace = append(trace, AttemptTrace{
			Provider: attempt.Provider, Model: attempt.Model, Status: status, Throttled: attempt.Throttled,
		})
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts = trace
}

func traceSinkFrom(ctx context.Context) *AttemptTraceSink {
	sink, _ := ctx.Value(traceSinkKey{}).(*AttemptTraceSink)
	return sink
}
