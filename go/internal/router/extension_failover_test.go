package router

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/providers"
)

// A provider the companion daemon serves leaves an endpoint to its next
// member as any other provider does: past a daemon that is rate limited,
// unavailable or unreachable, but not past one that refuses the request.
func TestEndpointFailsOverPastCompanionDaemonFailures(t *testing.T) {
	unreachable := httptest.NewServer(http.NotFoundHandler())
	unreachable.Close()
	for name, test := range map[string]struct {
		// status is the daemon's answer, 0 for a daemon that is unreachable.
		status  int
		advance bool
	}{
		"rate limited": {http.StatusTooManyRequests, true},
		"unavailable":  {http.StatusServiceUnavailable, true},
		"unreachable":  {0, true},
		"bad request":  {http.StatusBadRequest, false},
	} {
		t.Run(name, func(t *testing.T) {
			setupEcho(t)
			var calls atomic.Int32
			address := unreachable.URL
			if test.status != 0 {
				daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if r.URL.Path != "/extension/v1/openai_codex/invoke" && r.URL.Path != "/extension/v1/openai_codex/stream" {
						t.Errorf("the daemon was asked for %s", r.URL.Path)
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(test.status)
					_, _ = io.WriteString(w, `{"error":{"message":"fixture"}}`)
				}))
				defer daemon.Close()
				address = daemon.URL
			}
			t.Setenv("LLMGW_EXTENSION_URL", address)
			t.Setenv("LLMGW_EXTENSION_SECRET", "")
			policy := config.Get().Policies.Defaults
			config.Update(func(s *config.Settings) {
				s.Providers["codex"] = &config.ProviderConfig{Type: "openai_compatible", RegistryID: "openai_codex"}
				s.Policies.Defaults = config.ProviderPolicy{RetryMaxAttempts: 1}
			})
			providers.ResetProviders()
			t.Cleanup(func() {
				config.Update(func(s *config.Settings) { s.Policies.Defaults = policy })
				providers.ResetProviders()
			})
			targets := []Target{{Provider: "codex", Model: "gpt-test"}, {Provider: "echo", Model: "echo-default"}}
			messages := []providers.Message{{"role": "user", "content": "hi"}}

			_, served, completeErr := ExecuteComplete(targets, messages, "route", anonymous, nil)
			stream, streamed, streamErr := ExecuteStream(targets, messages, "route", anonymous, nil)
			if stream != nil {
				_ = stream.Close()
			}
			for chain, outcome := range map[string]struct {
				served *Target
				err    error
			}{"complete": {served, completeErr}, "stream": {streamed, streamErr}} {
				if test.advance {
					if outcome.err != nil || outcome.served == nil || outcome.served.Provider != "echo" {
						t.Errorf("%s chain: served=%+v err=%v, want the next member", chain, outcome.served, outcome.err)
					}
					continue
				}
				var failed *AllTargetsFailed
				if outcome.served != nil || !errors.As(outcome.err, &failed) || failed.Status != test.status {
					t.Errorf("%s chain: served=%+v err=%v, want the daemon's %d", chain, outcome.served, outcome.err, test.status)
				}
			}
			if want := int32(2); test.status != 0 && calls.Load() != want {
				t.Errorf("the daemon answered %d requests, want %d", calls.Load(), want)
			}
		})
	}
}
