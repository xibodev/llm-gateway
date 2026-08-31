package api

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
	"llmgw/internal/router"
)

type loadSetup struct {
	handler               http.Handler
	token, keyID, project string
}

func validateBenchmarkResponse(status int, header http.Header, body []byte, stream bool) error {
	if status != http.StatusOK {
		return fmt.Errorf("status=%d body=%q", status, body)
	}
	if !stream {
		var out struct {
			Object, Model string
			Choices       []struct {
				Message      struct{ Role, Content string } `json:"message"`
				FinishReason string                         `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			return err
		}
		if out.Object != "chat.completion" || out.Model != "echo-default" || len(out.Choices) != 1 ||
			out.Choices[0].Message.Role != "assistant" || out.Choices[0].Message.Content != "echo:hello" ||
			out.Choices[0].FinishReason != "stop" || bytes.Contains(body, []byte(`"error"`)) {
			return fmt.Errorf("unexpected chat response: %s", body)
		}
		return nil
	}
	if !strings.HasPrefix(header.Get("Content-Type"), "text/event-stream") {
		return fmt.Errorf("content-type=%q", header.Get("Content-Type"))
	}
	var content string
	finish, done := false, false
	for _, line := range bytes.Split(body, []byte("\n")) {
		data := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if !bytes.HasPrefix(line, []byte("data:")) || len(data) == 0 {
			continue
		}
		if bytes.Equal(data, []byte("[DONE]")) {
			done = true
			continue
		}
		if done {
			return fmt.Errorf("SSE data after [DONE]")
		}
		var event struct {
			Choices []struct {
				Delta        struct{ Content string } `json:"delta"`
				FinishReason *string                  `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(data, &event); err != nil {
			return err
		}
		if bytes.Contains(data, []byte(`"error"`)) || len(event.Choices) != 1 ||
			event.Choices[0].FinishReason != nil && *event.Choices[0].FinishReason != "stop" {
			return fmt.Errorf("unexpected SSE event: %s", data)
		}
		content += event.Choices[0].Delta.Content
		finish = finish || event.Choices[0].FinishReason != nil && *event.Choices[0].FinishReason == "stop"
	}
	if content != "echo:hello" || !finish || !done {
		return fmt.Errorf("incomplete SSE: content=%q finish=%v done=%v", content, finish, done)
	}
	return nil
}
func setupGatewayLoad(tb testing.TB, unauthenticated, minted bool) loadSetup {
	previous := *config.Get()
	tb.Setenv("LLMGW_STATE_DIR", tb.TempDir())
	reset := func() {
		iam.ResetForTests()
		router.ResetSavingsState()
		router.ResetTelemetryState()
		providers.ResetProviders()
	}
	reset()
	tb.Cleanup(func() {
		reset()
		config.Update(func(s *config.Settings) { *s = previous })
	})
	if _, err := iam.Initialize(); err != nil {
		tb.Fatal(err)
	}
	config.Update(func(s *config.Settings) {
		*s = *config.Defaults()
		s.APIKey = "benchmark-admin"
		s.APIKeys = nil
		s.AllowUnauthenticatedAPI = unauthenticated
		s.GatewayPreamble = ""
		s.Providers = map[string]*config.ProviderConfig{"echo": {Type: "echo"}}
		s.Endpoints = map[string]*config.EndpointConfig{}
	})
	providers.ResetProviders()
	setup := loadSetup{handler: NewServer(), token: "benchmark-admin"}
	if !minted {
		return setup
	}
	principal, err := iam.CreatePrincipal("service", "service:benchmark", "", "Benchmark")
	if err != nil {
		tb.Fatal(err)
	}
	project, err := iam.CreateProject("benchmark", "Benchmark")
	if err != nil {
		tb.Fatal(err)
	}
	policy := iam.KeyPolicy{
		AllowedModels: []string{"echo/echo-default"}, AllowedProviders: []string{"echo"},
		RPM: 1_000_000_000, DailyRequests: 1_000_000_000, MonthlyRequests: 1_000_000_000,
		DailyInputTokens: 1_000_000_000, DailyOutputTokens: 1_000_000_000, MonthlyTotalTokens: 2_000_000_000,
	}
	for _, err := range []error{
		iam.SetMembership(project.ID, principal.ID, "member"),
		func() error { _, err := iam.SetProjectPolicy(project.ID, policy); return err }(),
	} {
		if err != nil {
			tb.Fatal(err)
		}
	}
	issued, err := iam.IssueKey(iam.KeyCreate{
		ProjectID: project.ID, PrincipalID: principal.ID, Name: "benchmark", Policy: policy,
	})
	if err != nil {
		tb.Fatal(err)
	}
	setup.token, setup.keyID, setup.project = issued.Token, issued.ID, project.ID
	return setup
}
func runGatewayBenchmark(b *testing.B, unauthenticated, minted, stream bool) {
	setup := setupGatewayLoad(b, unauthenticated, minted)
	body := []byte(`{"model":"echo/echo-default","messages":[{"role":"user","content":"hello"}]}`)
	if stream {
		body = []byte(`{"model":"echo/echo-default","messages":[{"role":"user","content":"hello"}],"stream":true}`)
	}
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
			if !unauthenticated {
				req.Header.Set("Authorization", "Bearer "+setup.token)
			}
			recorder := httptest.NewRecorder()
			setup.handler.ServeHTTP(recorder, req)
			if err := validateBenchmarkResponse(recorder.Code, recorder.Header(), recorder.Body.Bytes(), stream); err != nil {
				b.Error(err)
				return
			}
		}
	})
}
func BenchmarkGatewayUnauthenticatedChat(b *testing.B) { runGatewayBenchmark(b, true, false, false) }
func BenchmarkGatewayStaticKeyChat(b *testing.B)       { runGatewayBenchmark(b, false, false, false) }
func BenchmarkGatewayMintedKeyChat(b *testing.B)       { runGatewayBenchmark(b, false, true, false) }
func BenchmarkGatewayStaticKeyChatSSE(b *testing.B)    { runGatewayBenchmark(b, false, false, true) }
func TestGatewayMintedUsageCountersReconcile(t *testing.T) {
	setup := setupGatewayLoad(t, false, true)
	const requests = 8
	body := []byte(`{"model":"echo/echo-default","messages":[{"role":"user","content":"hello"}]}`)
	for i := 0; i < requests; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+setup.token)
		recorder := httptest.NewRecorder()
		setup.handler.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusOK {
			t.Fatalf("request %d status=%d body=%q", i, recorder.Code, recorder.Body.String())
		}
	}
	db, err := iam.DB()
	if err != nil {
		t.Fatal(err)
	}
	var events, inputTokens, outputTokens int
	if err := db.QueryRow(`SELECT COUNT(*),SUM(input_tokens),SUM(output_tokens) FROM usage_events WHERE key_id=?`, setup.keyID).
		Scan(&events, &inputTokens, &outputTokens); err != nil {
		t.Fatal(err)
	}
	if events != requests || inputTokens != requests || outputTokens != requests {
		t.Fatalf("usage events=(%d,%d,%d), want (%d,%d,%d)", events, inputTokens, outputTokens, requests, requests, requests)
	}
	for table, id := range map[string]string{"quota_counters": setup.keyID, "project_quota_counters": setup.project} {
		query := "SELECT COALESCE(SUM(requests),0),COALESCE(SUM(input_tokens),0),COALESCE(SUM(output_tokens),0) FROM " + table + " WHERE period=? AND "
		if table == "quota_counters" {
			query += "key_id=?"
		} else {
			query += "project_id=?"
		}
		for _, period := range []string{"minute", "day", "month"} {
			var counted, in, out int
			if err := db.QueryRow(query, period, id).Scan(&counted, &in, &out); err != nil {
				t.Fatal(err)
			}
			wantIn, wantOut := inputTokens, outputTokens
			if period == "minute" {
				wantIn, wantOut = 0, 0
			}
			if counted != events || in != wantIn || out != wantOut {
				t.Fatalf("%s %s=(%d,%d,%d), want (%d,%d,%d)", table, period, counted, in, out, events, wantIn, wantOut)
			}
		}
	}
}

func TestGatewayLegacySavingsLedgerIsOptIn(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		setup := setupGatewayLoad(t, false, false)
		body := []byte(`{"model":"echo/echo-default","messages":[{"role":"user","content":"hello"}]}`)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+setup.token)
		recorder := httptest.NewRecorder()
		setup.handler.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status=%d body=%q", recorder.Code, recorder.Body.String())
		}
		db, err := iam.DB()
		if err != nil {
			t.Fatal(err)
		}
		var events int
		if err := db.QueryRow("SELECT COUNT(*) FROM usage_events").Scan(&events); err != nil || events != 1 {
			t.Fatalf("gateway usage events=%d err=%v, want 1", events, err)
		}
		server := httptest.NewServer(setup.handler)
		defer server.Close()
		status, state := jsonRequest(t, server.URL+"/admin/api/state", http.MethodGet, setup.token, nil)
		if status != http.StatusOK {
			t.Fatalf("admin state status=%d payload=%v", status, state)
		}
		savings, ok := state["savings"].(map[string]any)
		if !ok || savings["requests"] != float64(0) {
			t.Fatalf("admin state savings=%v", state["savings"])
		}
		status, usage := jsonRequest(t, server.URL+"/admin/api/usage", http.MethodGet, setup.token, nil)
		if status != http.StatusOK {
			t.Fatalf("admin usage status=%d payload=%v", status, usage)
		}
		totals, totalsOK := usage["totals"].(map[string]any)
		byProject, projectsOK := usage["by_project"].([]any)
		recent, recentOK := usage["recent"].([]any)
		if !totalsOK || totals["requests"] != float64(0) || !projectsOK || len(byProject) != 0 || !recentOK || len(recent) != 0 {
			t.Fatalf("legacy compatibility usage totals=%v by_project=%v recent=%v", usage["totals"], usage["by_project"], usage["recent"])
		}
		if _, err := os.Stat(filepath.Join(config.StateDir(), "usage.db")); !os.IsNotExist(err) {
			t.Fatalf("default legacy ledger exists: %v", err)
		}
	})

	t.Run("explicit enabled", func(t *testing.T) {
		setup := setupGatewayLoad(t, false, true)
		legacyPath := filepath.Join(config.StateDir(), "legacy", "custom-usage.db")
		config.Update(func(s *config.Settings) {
			s.Savings.Enabled = true
			s.Savings.DBPath = legacyPath
			s.Savings.BaselineModel = "baseline/model"
			s.Savings.PriceCatalog = map[string]map[string]float64{
				"baseline/model": {"input": 1, "output": 2},
			}
		})
		body := []byte(`{"model":"echo/echo-default","messages":[{"role":"user","content":"hello"}]}`)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+setup.token)
		recorder := httptest.NewRecorder()
		setup.handler.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status=%d body=%q", recorder.Code, recorder.Body.String())
		}
		db, err := sql.Open("sqlite", legacyPath)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var project, key string
		var baseline float64
		if err := db.QueryRow("SELECT project,key_name,baseline_cost_usd FROM usage_ledger").Scan(&project, &key, &baseline); err != nil {
			t.Fatal(err)
		}
		if project != "benchmark" || key != "benchmark" || baseline <= 0 {
			t.Fatalf("legacy labels/baseline=(%q,%q,%v)", project, key, baseline)
		}
		totals, projects, recent := router.Totals(true), router.ByProject(true), router.RecentUsage(1)
		if totals["requests"] != int64(1) || len(projects) != 1 || len(recent) != 1 {
			t.Fatalf("legacy savings reads totals=%v projects=%v recent=%v", totals, projects, recent)
		}
	})
}
