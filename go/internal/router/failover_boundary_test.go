package router

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/providers"
)

// A stream commits to its target once the upstream answers, before any
// content: a request moves to the next target only before the first response
// byte (docs/ROUTING.md, "Failover boundary"). A later failure reaches the
// caller through the stream, and no other target is tried.
func TestStreamCommitsWhenTheUpstreamAnswers(t *testing.T) {
	for name, frames := range map[string]string{
		"headers only":    "",
		"role-only chunk": `data: {"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}` + "\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			setupEcho(t)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, frames)
				w.(http.Flusher).Flush()
				panic(http.ErrAbortHandler)
			}))
			defer upstream.Close()
			config.Update(func(s *config.Settings) {
				s.Providers["answers"] = &config.ProviderConfig{Type: "openai_compatible", BaseURL: upstream.URL}
				s.Policies.Defaults = config.ProviderPolicy{RetryMaxAttempts: 1}
			})
			providers.ResetProviders()
			t.Cleanup(providers.ResetProviders)
			targets := []Target{{Provider: "answers", Model: "model"}, {Provider: "echo", Model: "echo-default"}}
			it, served, err := ExecuteStream(targets, []providers.Message{{"role": "user", "content": "hi"}}, "route", anonymous, nil)
			if err != nil || served == nil || served.Provider != "answers" {
				t.Fatalf("served=%+v err=%v, want the target that answered", served, err)
			}
			defer it.Close()
			for {
				if _, more := it.Next(); !more {
					break
				}
			}
			if it.Err() == nil {
				t.Fatal("the failure after the upstream answered did not reach the caller")
			}
		})
	}
}

// A chain that runs out of targets reports the last target's own failure,
// even when the fallback deadline passed during that attempt. The deadline's
// 504 is for a chain the deadline stopped before its next target.
func TestChainReportsItsLastFailureWhenTheDeadlinePassesDuringIt(t *testing.T) {
	setupEcho(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// The server notices the caller leave only once it has read the body.
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer upstream.Close()
	config.Update(func(s *config.Settings) {
		s.Providers["stalls"] = &config.ProviderConfig{Type: "openai_compatible", BaseURL: upstream.URL}
		s.Policies.Defaults = config.ProviderPolicy{RetryMaxAttempts: 1}
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)
	ctx := WithFallbackOptions(context.Background(), 20*time.Millisecond, "")
	messages := []providers.Message{{"role": "user", "content": "hi"}}
	stalls := Target{Provider: "stalls", Model: "model"}
	_, _, err := ExecuteCompleteContext(ctx, []Target{stalls}, messages, "route", anonymous, nil)
	var failed *AllTargetsFailed
	if !errors.As(err, &failed) || failed.Status != 0 || !strings.Contains(failed.Msg, "transport error") {
		t.Fatalf("err=%v, want the target's transport failure without a status", err)
	}
	_, _, err = ExecuteCompleteContext(ctx, []Target{stalls, {Provider: "echo", Model: "echo-default"}}, messages, "route", anonymous, nil)
	if !errors.As(err, &failed) || failed.Status != http.StatusGatewayTimeout {
		t.Fatalf("err=%v, want the deadline's 504 before the next target", err)
	}
}
