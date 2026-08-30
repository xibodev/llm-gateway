package router

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
)

func setupEcho(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	config.Update(func(s *config.Settings) {
		s.Savings.Enabled = false
		s.Providers = map[string]*config.ProviderConfig{
			"echo":  {Type: "echo"},
			"echo2": {Type: "echo"},
			"bad":   {Type: "openai_compatible", BaseURL: "http://127.0.0.1:1/v1"},
		}
		s.Endpoints = map[string]*config.EndpointConfig{
			"smart":    {Failover: []config.EndpointMember{{Provider: "echo", Model: "echo-default"}}},
			"failover": {Failover: []config.EndpointMember{{Provider: "bad", Model: "x"}, {Provider: "echo", Model: "echo-default"}}},
			"empty":    {Failover: nil},
		}
	})
	providers.ResetProviders()
	ResetTelemetryState()
	ResetSavingsState()
	t.Cleanup(func() {
		ResetTelemetryState()
		ResetSavingsState()
	})
}

func TestResolveTargets(t *testing.T) {
	setupEcho(t)

	// category
	resolution, err := ResolveForPrincipal("SMART", nil)
	if err != nil || resolution.Category != "smart" || len(resolution.Targets) != 1 || resolution.Targets[0].Provider != "echo" {
		t.Fatalf("category resolve wrong: %+v %v", resolution, err)
	}
	targets := resolution.Targets
	// provider/model
	targets, err = ResolveTargets("echo/echo-strong")
	if err != nil || len(targets) != 1 || targets[0].Model != "echo-strong" {
		t.Fatalf("provider/model resolve wrong: %v %v", targets, err)
	}
	// unknown -> 404
	if _, err := ResolveTargets("does-not-exist"); err == nil {
		t.Error("unknown model should error")
	} else if _, ok := err.(*ModelNotFoundError); !ok {
		t.Errorf("want ModelNotFoundError, got %T", err)
	}
	// empty category -> 404
	if _, err := ResolveTargets("empty"); err == nil {
		t.Error("empty category should error")
	}
}

func TestResolveCategoryRejectsAmbiguousCaseVariants(t *testing.T) {
	setupEcho(t)
	config.Update(func(s *config.Settings) {
		s.Endpoints["SMART"] = &config.EndpointConfig{
			Failover: []config.EndpointMember{{Provider: "echo2", Model: "echo-deep"}},
		}
	})

	lower, err := ResolveForPrincipal("smart", nil)
	if err != nil || lower.Category != "smart" || lower.Targets[0].Provider != "echo" {
		t.Fatalf("exact lower-case route resolve wrong: %+v %v", lower, err)
	}
	upper, err := ResolveForPrincipal("SMART", nil)
	if err != nil || upper.Category != "SMART" || upper.Targets[0].Provider != "echo2" {
		t.Fatalf("exact upper-case route resolve wrong: %+v %v", upper, err)
	}
	if _, err := ResolveForPrincipal("SmArT", nil); err == nil {
		t.Fatal("ambiguous case-insensitive route lookup should fail")
	} else if _, ok := err.(*AmbiguousCategoryError); !ok {
		t.Fatalf("want AmbiguousCategoryError, got %T: %v", err, err)
	}
}

// The type name is internal; the message is not. Endpoint lookup hands this
// error to the API layer, so its prose must use the product's current
// vocabulary — the same word ModelNotFoundError and owned_by: "endpoint" use —
// rather than the pre-rename "category".
func TestAmbiguousEndpointErrorSpeaksOfEndpoints(t *testing.T) {
	message := (&AmbiguousCategoryError{
		Requested: "SmArT", Matches: []string{"SMART", "smart"},
	}).Error()
	if strings.Contains(strings.ToLower(message), "categor") {
		t.Fatalf("message %q still uses the pre-rename vocabulary", message)
	}
	if !strings.Contains(message, "Endpoint") || !strings.Contains(message, "endpoints") {
		t.Fatalf("message %q does not name the endpoint it is about", message)
	}
	for _, match := range []string{"SMART", "smart"} {
		if !strings.Contains(message, match) {
			t.Fatalf("message %q omits colliding name %q", message, match)
		}
	}
}

func TestNativeKey(t *testing.T) {
	// picker's dashed native form and the catalog's dotted form must match
	if nativeKey("claude-opus-4.8") != nativeKey("claude-opus-4-8") {
		t.Errorf("dotted vs dashed should match: %q vs %q", nativeKey("claude-opus-4.8"), nativeKey("claude-opus-4-8"))
	}
	if nativeKey("Claude-Sonnet-4.5") != nativeKey("claude-sonnet-4-5") {
		t.Error("case + dot/dash should match")
	}
	// but a retired opus-4 must NOT collide with opus-4.8
	if nativeKey("claude-opus-4.8") == nativeKey("claude-opus-4") {
		t.Error("opus-4.8 must not equal retired opus-4")
	}
}

func TestResolveNativeAlias(t *testing.T) {
	setupEcho(t)
	// a bare catalog name (no provider prefix, not a category) resolves; with
	// two echo providers the sorted-order winner is deterministic ("echo").
	targets, err := ResolveTargets("echo-strong")
	if err != nil || len(targets) != 1 || targets[0].Provider != "echo" || targets[0].Model != "echo-strong" {
		t.Fatalf("bare-name resolve wrong: %v %v", targets, err)
	}
	// a "[1m]"-style context-variant tag is stripped before matching
	targets, err = ResolveTargets("echo-strong[1m]")
	if err != nil || len(targets) != 1 || targets[0].Model != "echo-strong" {
		t.Fatalf("tagged-name resolve wrong: %v %v", targets, err)
	}
	// case-insensitive
	if targets, err := ResolveTargets("ECHO-DEEP"); err != nil || len(targets) != 1 || targets[0].Model != "echo-deep" {
		t.Fatalf("case-insensitive resolve wrong: %v %v", targets, err)
	}
	// discovery-alias prefix strip: "claude-<non-claude>" falls back to the real
	// model after a direct match fails (surfaces non-Claude models in the picker).
	if targets, err := ResolveTargets("claude-echo-strong"); err != nil || len(targets) != 1 || targets[0].Model != "echo-strong" {
		t.Fatalf("prefix-strip resolve wrong: %v %v", targets, err)
	}
	// genuinely unknown still 404s
	if _, err := ResolveTargets("totally-unknown-9"); err == nil {
		t.Error("unknown should still 404")
	}
}

func TestExecuteCompleteEcho(t *testing.T) {
	setupEcho(t)
	targets, _ := ResolveTargets("smart")
	resp, served, err := ExecuteComplete(targets, []providers.Message{{"role": "user", "content": "hi"}}, "smart", nil, providers.Kwargs{})
	if err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	if served.Provider != "echo" {
		t.Errorf("served wrong: %v", served)
	}
	choices := resp["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "echo:hi" {
		t.Errorf("content wrong: %v", msg["content"])
	}
}

func TestExecuteCompleteFailover(t *testing.T) {
	setupEcho(t)
	targets, _ := ResolveTargets("failover")
	resp, served, err := ExecuteComplete(targets, []providers.Message{{"role": "user", "content": "hi"}}, "failover", &config.Principal{Project: "p", Key: "k"}, providers.Kwargs{})
	if err != nil {
		t.Fatalf("failover should succeed via echo: %v", err)
	}
	if served.Provider != "echo" {
		t.Errorf("should have failed over to echo, served %v", served)
	}
	_ = resp
	// telemetry should have recorded the failover chain
	stats := TelemetryStats()
	if stats["events"].(int64) != 1 {
		t.Errorf("want 1 telemetry event, got %v", stats["events"])
	}
	recent := RecentTelemetry(10)
	if len(recent) != 1 {
		t.Fatalf("want 1 recent event, got %d", len(recent))
	}
	attempts := recent[0]["attempts"].([]map[string]any)
	if len(attempts) != 2 {
		t.Errorf("want 2 attempts, got %d", len(attempts))
	}
	if recent[0]["served"] != "echo/echo-default" {
		t.Errorf("served wrong in telemetry: %v", recent[0]["served"])
	}
}

func TestFailoverErrorsAndTelemetryAreSanitized(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	secret := "llmgw_" + strings.Repeat("A", 32)
	email := "routing-owner@example.test"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"message": "failed for " + email + " using " + secret},
		})
	}))
	defer upstream.Close()

	config.Update(func(s *config.Settings) {
		s.Savings.Enabled = false
		s.Providers = map[string]*config.ProviderConfig{
			"bad":  {Type: "openai_compatible", BaseURL: upstream.URL},
			"echo": {Type: "echo"},
		}
		s.Policies.Defaults = config.ProviderPolicy{RetryMaxAttempts: 1}
		s.Policies.Overrides = map[string]config.ProviderPolicy{}
	})
	providers.ResetProviders()
	ResetTelemetryState()
	ResetSavingsState()
	t.Cleanup(func() {
		providers.ResetProviders()
		ResetTelemetryState()
		ResetSavingsState()
	})

	messages := []providers.Message{{"role": "user", "content": "hi"}}
	_, served, err := ExecuteComplete(
		[]Target{{Provider: "bad", Model: "bad-model"}, {Provider: "echo", Model: "echo-default"}},
		messages, "fallback", nil, providers.Kwargs{},
	)
	if err != nil || served == nil || served.Provider != "echo" {
		t.Fatalf("fallback result served=%+v err=%v", served, err)
	}

	_, _, err = ExecuteComplete(
		[]Target{{Provider: "bad", Model: "bad-model"}},
		messages, "all-targets", nil, providers.Kwargs{},
	)
	var allTargets *AllTargetsFailed
	if !errors.As(err, &allTargets) {
		t.Fatalf("all-target failure type = %T, want *AllTargetsFailed", err)
	}
	if allTargets.Status != http.StatusServiceUnavailable {
		t.Fatalf("all-target status = %d", allTargets.Status)
	}
	if message := err.Error(); strings.Contains(message, secret) || strings.Contains(message, email) {
		t.Fatalf("all-target error exposed diagnostics: %q", message)
	}
	if message := (&AllTargetsFailed{Msg: "raw " + email + " " + secret}).Error(); strings.Contains(message, secret) || strings.Contains(message, email) {
		t.Fatalf("direct all-target error exposed diagnostics: %q", message)
	}

	recordTelemetryEvent("sink-defense", []eventAttempt{{
		Provider: "raw", Model: "model", Error: "raw " + email + " " + secret,
	}}, "", "", "", "")

	db, err := telConn()
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`SELECT attempts_json FROM failover_events ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	rawRows := 0
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		rawRows++
		if strings.Contains(raw, secret) || strings.Contains(raw, email) {
			rows.Close()
			t.Fatalf("raw telemetry exposed diagnostics: %s", raw)
		}
		if !strings.Contains(raw, "[redacted]") {
			rows.Close()
			t.Fatalf("raw telemetry has no redaction marker: %s", raw)
		}
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if rawRows != 3 {
		t.Fatalf("raw telemetry rows = %d, want fallback, all-target, and sink-defense rows", rawRows)
	}

	legacy, _ := json.Marshal([]map[string]any{{
		"provider": "legacy", "model": "model", "ok": false,
		"error": "legacy " + email + " used " + secret,
	}})
	if _, err := db.Exec(
		`INSERT INTO failover_events (ts, requested, served_provider, served_model, throttled, attempts_json, project, key_name)
		 VALUES (?, ?, NULL, NULL, 0, ?, NULL, NULL)`,
		1, "legacy", string(legacy),
	); err != nil {
		t.Fatal(err)
	}
	recent := RecentTelemetry(1)
	if len(recent) != 1 {
		t.Fatalf("recent telemetry rows = %d", len(recent))
	}
	attempts := recent[0]["attempts"].([]map[string]any)
	message, _ := attempts[0]["error"].(string)
	if strings.Contains(message, secret) || strings.Contains(message, email) || !strings.Contains(message, "[redacted]") {
		t.Fatalf("historical telemetry was not sanitized on read: %q", message)
	}
}

func TestAttemptTruncationSanitizesBeforeLimiting(t *testing.T) {
	secret := "llmgw_" + strings.Repeat("B", 32)
	got := truncate(strings.Repeat("safe ", 35) + secret)
	if len(got) > 200 {
		t.Fatalf("attempt text length = %d", len(got))
	}
	if strings.Contains(got, secret[:24]) || !strings.Contains(got, "[redacted]") {
		t.Fatalf("attempt text was truncated before sanitization: %q", got)
	}
}

func TestExecuteStreamEcho(t *testing.T) {
	setupEcho(t)
	targets, _ := ResolveTargets("smart")
	it, served, err := ExecuteStream(targets, []providers.Message{{"role": "user", "content": "hi"}}, "smart", nil, providers.Kwargs{})
	if err != nil {
		t.Fatalf("stream failed: %v", err)
	}

	if served.Provider != "echo" {
		t.Errorf("served wrong: %v", served)
	}
	count := 0
	for {
		_, more := it.Next()
		if !more {
			break
		}
		count++
	}
	if count == 0 {
		t.Error("expected stream chunks")
	}
}

func TestResponsesFallbackRejectsUnsupportedToolConstraints(t *testing.T) {
	setupEcho(t)
	config.Update(func(settings *config.Settings) {
		settings.Providers["anthropic"] = &config.ProviderConfig{Type: "anthropic"}
	})
	target := Target{Provider: "anthropic", Model: "claude"}
	messages := []providers.Message{{"role": "user", "content": "hi"}}
	for name, kw := range map[string]providers.Kwargs{
		"tool choice": {
			"tools": []any{map[string]any{
				"type": "function", "function": map[string]any{
					"name": "lookup", "parameters": map[string]any{"type": "object"},
				},
			}},
			"tool_choice": "required",
		},
		"strict tool": {
			"tools": []any{map[string]any{
				"type": "function", "function": map[string]any{
					"name": "lookup", "strict": true,
					"parameters": map[string]any{"type": "object"},
				},
			}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := responsesFallbackCompatibility(target, nil, messages, kw); err == nil {
				t.Fatal("unsupported tool constraint was accepted")
			}
		})
	}
}

func TestRecordUsageWritesControlPlaneLedger(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	ResetSavingsState()
	t.Cleanup(func() {
		iam.ResetForTests()
		ResetSavingsState()
	})
	config.Update(func(s *config.Settings) {
		s.Savings.Enabled = false
	})
	RecordUsage(UsageRecord{
		Endpoint: "openai.chat", RequestedModel: "smart", RoutedModel: "echo-default",
		Provider: "echo", InputTokens: 3, OutputTokens: 2, LatencyMS: 5,
	})
	stats, err := iam.UsageStats(0)
	if err != nil {
		t.Fatal(err)
	}
	if got := stats["totals"].(iam.UsageTotals).Requests; got != 1 {
		t.Fatalf("control-plane requests=%d, want 1", got)
	}
}
