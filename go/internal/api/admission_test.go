package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"llmgw/internal/iam"
	"llmgw/internal/providers"
)

func admissionFixtureKey(t *testing.T, name string, policy iam.KeyPolicy) string {
	t.Helper()
	owner, err := iam.CreatePrincipal("human", "fixture:"+name, "", name)
	if err != nil {
		t.Fatal(err)
	}
	project, err := iam.CreateProject(name, name)
	if err != nil {
		t.Fatal(err)
	}
	if err := iam.SetMembership(project.ID, owner.ID, "member"); err != nil {
		t.Fatal(err)
	}
	issued, err := iam.IssueKey(iam.KeyCreate{ProjectID: project.ID, PrincipalID: owner.ID, Name: name, Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	return issued.Token
}

func admissionRequest(handler http.Handler, path, token string, header http.Header, payload map[string]any) *httptest.ResponseRecorder {
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	for name, values := range header {
		for _, value := range values {
			req.Header.Add(name, value)
		}
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

// consumedRequests sums the daily request slots taken from every key and every
// project.
func consumedRequests(t *testing.T) (key, project int) {
	t.Helper()
	db, err := iam.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT COALESCE(SUM(requests),0) FROM quota_counters WHERE period='day'").Scan(&key); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT COALESCE(SUM(requests),0) FROM project_quota_counters WHERE period='day'").Scan(&project); err != nil {
		t.Fatal(err)
	}
	return key, project
}

func TestGatewayRejectionsDoNotConsumeQuota(t *testing.T) {
	var inference atomic.Int32
	handler := setupAPISmokeFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, http.StatusOK, map[string]any{"data": []any{map[string]any{
				"id": "model", "capabilities": map[string]any{"supports": map[string]any{"tool_calls": false}},
			}}})
			return
		}
		inference.Add(1)
		writeJSON(w, http.StatusOK, map[string]any{"choices": []any{map[string]any{
			"message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop",
		}}})
	}))
	// The daily limit keeps both outcomes independent of which minute the
	// requests land in: one consumed slot is enough to refuse the next request.
	token := admissionFixtureKey(t, "admission", iam.KeyPolicy{RPM: 1, DailyRequests: 1})
	principal, _, err := iam.ResolveAPIKey(token)
	if err != nil {
		t.Fatal(err)
	}
	if rows := providers.RefreshCatalogForPrincipal("fixture", callerOf(principal)); len(rows) != 1 {
		t.Fatalf("catalog=%+v", rows)
	}
	hello := []any{map[string]any{"role": "user", "content": "hi"}}
	rejected := []struct {
		name, path string
		header     http.Header
		payload    map[string]any
	}{
		{"chat compatibility", "/v1/chat/completions", nil, map[string]any{
			"model": "fixture/model", "messages": hello, "tools": []any{map[string]any{
				"type": "function", "function": map[string]any{"name": "lookup", "parameters": map[string]any{"type": "object"}},
			}},
		}},
		{"responses compatibility", "/v1/responses", nil, map[string]any{
			"model": "fixture/model", "input": "hi", "tools": []any{map[string]any{
				"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"},
			}},
		}},
		{"messages material loss", "/v1/messages", nil, map[string]any{
			"model": "fixture/model", "max_tokens": 8, "stream": true, "messages": []any{map[string]any{
				"role": "user", "content": []any{map[string]any{"type": "document", "source": map[string]any{"type": "base64"}}},
			}},
		}},
		{"transparent contract", "/v1/chat/completions", http.Header{transportModeHeader: {"transparent"}}, map[string]any{
			"model": "fixture/model", "messages": hello, "stream": true,
		}},
		{"unsupported media operation", "/v1/images/generations", nil, map[string]any{
			"model": "fixture/model", "prompt": "hi",
		}},
	}
	for _, tc := range rejected {
		w := admissionRequest(handler, tc.path, token, tc.header, tc.payload)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: status=%d body=%s", tc.name, w.Code, w.Body.String())
		}
	}
	if key, project := consumedRequests(t); key != 0 || project != 0 || inference.Load() != 0 {
		t.Fatalf("rejected requests consumed key=%d project=%d upstream=%d", key, project, inference.Load())
	}
	db, err := iam.DB()
	if err != nil {
		t.Fatal(err)
	}
	var failures int
	if err := db.QueryRow("SELECT COUNT(*) FROM usage_events WHERE status_code=400").Scan(&failures); err != nil || failures != len(rejected) {
		t.Fatalf("failure usage rows=%d err=%v", failures, err)
	}

	valid := map[string]any{"model": "fixture/model", "messages": hello}
	if w := admissionRequest(handler, "/v1/chat/completions", token, nil, valid); w.Code != http.StatusOK {
		t.Fatalf("valid request after rejections: status=%d body=%s", w.Code, w.Body.String())
	}
	for _, tc := range []struct {
		path    string
		payload map[string]any
	}{
		{"/v1/chat/completions", valid},
		{"/v1/chat/completions", map[string]any{"model": "fixture/model", "messages": hello, "stream": true}},
		{"/v1/responses", map[string]any{"model": "fixture/model", "input": "hi"}},
		{"/v1/responses", map[string]any{"model": "fixture/model", "input": "hi", "stream": true}},
		{"/v1/messages", map[string]any{"model": "fixture/model", "max_tokens": 8, "messages": hello}},
		{"/v1/messages", map[string]any{"model": "fixture/model", "max_tokens": 8, "messages": hello, "stream": true}},
		{"/v1/embeddings", map[string]any{"model": "fixture/model", "input": "hi"}},
	} {
		w := admissionRequest(handler, tc.path, token, nil, tc.payload)
		if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), "quota exceeded") {
			t.Fatalf("exhausted quota at %s stream=%v: status=%d body=%s", tc.path, tc.payload["stream"], w.Code, w.Body.String())
		}
	}
	if key, project := consumedRequests(t); key != 1 || project != 1 || inference.Load() != 1 {
		t.Fatalf("admitted request counters key=%d project=%d upstream=%d", key, project, inference.Load())
	}

	// Authorization still answers before validation does.
	denied := admissionFixtureKey(t, "admission-denied", iam.KeyPolicy{AllowedModels: []string{"other"}})
	if w := admissionRequest(handler, rejected[0].path, denied, nil, rejected[0].payload); w.Code != http.StatusForbidden {
		t.Fatalf("denied invalid request: status=%d body=%s", w.Code, w.Body.String())
	}
}
