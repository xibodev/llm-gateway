package router

import (
	"context"
	"reflect"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/providers"
)

// A chain reports which targets it tried, and how each attempt ended, to the
// sink its context carries; a stream's chain does so as its stream opens.
func TestChainsReportTheirAttemptsToTheTraceSink(t *testing.T) {
	setupEcho(t)
	config.Update(func(s *config.Settings) { s.Policies.Defaults = config.ProviderPolicy{RetryMaxAttempts: 1} })
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)
	targets := []Target{{Provider: "bad", Model: "x"}, {Provider: "echo", Model: "echo-default"}}
	messages := []providers.Message{{"role": "user", "content": "hi"}}
	want := []AttemptTrace{
		{Provider: "bad", Model: "x", Status: "failed"},
		{Provider: "echo", Model: "echo-default", Status: "served"},
	}

	ctx, sink := WithAttemptTrace(context.Background())
	if got := sink.Attempts(); len(got) != 0 {
		t.Fatalf("a sink holds %+v before any chain ran", got)
	}
	it, _, err := ExecuteStreamContext(ctx, targets, messages, "route", anonymous, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := sink.Attempts(); !reflect.DeepEqual(got, want) {
		t.Fatalf("the open stream's trace %+v, want %+v", got, want)
	}
	_ = it.Close()

	ctx, sink = WithAttemptTrace(context.Background())
	if _, _, err := ExecuteCompleteContext(ctx, targets, messages, "route", anonymous, nil); err != nil {
		t.Fatal(err)
	}
	if got := sink.Attempts(); !reflect.DeepEqual(got, want) {
		t.Fatalf("the completion's trace %+v, want %+v", got, want)
	}
	// A context without a sink keeps nothing, and a chain run without one is
	// no different.
	if _, _, err := ExecuteCompleteContext(context.Background(), targets, messages, "route", anonymous, nil); err != nil {
		t.Fatal(err)
	}
}
