package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// observedFixture serves the smoke fixture through a server whose access log
// writes to the returned buffer.
func observedFixture(t *testing.T) (http.Handler, *bytes.Buffer) {
	t.Helper()
	setupAPISmokeFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 11, "completion_tokens": 3},
		})
	}))
	var log bytes.Buffer
	s := newServer(Runtime{}, time.Now)
	s.accessLog = slog.New(slog.NewJSONHandler(&log, nil))
	return s.handler(), &log
}

func TestAccessLogIsOffUnlessAskedFor(t *testing.T) {
	for value, on := range map[string]bool{"": false, "0": false, "off": false, "json": true, "1": true, "true": true} {
		t.Setenv("LLMGW_ACCESS_LOG", value)
		if got := accessLogger(io.Discard) != nil; got != on {
			t.Fatalf("LLMGW_ACCESS_LOG=%q: on=%v, want %v", value, got, on)
		}
	}
}

// Each request gets one JSON line naming its route, status, caller and the
// target that served it, and never the credential or the prompt.
func TestAccessLogLineDescribesTheRequest(t *testing.T) {
	handler, log := observedFixture(t)
	w := apiSmokeRequest(handler, "/v1/chat/completions", map[string]any{
		"model": "fixture/model", "messages": []any{map[string]any{"role": "user", "content": "a secret prompt"}},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	request := httptest.NewRequest(http.MethodGet, "/no/such/path?token=query-secret", nil)
	handler.ServeHTTP(httptest.NewRecorder(), request)

	lines := strings.Split(strings.TrimSpace(log.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines=%q", lines)
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatal(err)
	}
	for field, want := range map[string]any{
		"msg": "request", "method": "POST", "path": "/v1/chat/completions", "route": "/v1/chat/completions",
		"status": 200.0, "caller": "admin_key", "provider": "fixture", "model": "model",
		"input_tokens": 11.0, "output_tokens": 3.0, "request_id": w.Header().Get("X-Request-Id"),
	} {
		if entry[field] != want {
			t.Fatalf("%s=%#v, want %#v in %s", field, entry[field], want, lines[0])
		}
	}
	if fingerprint, _ := entry["key_fingerprint"].(string); len(fingerprint) != 12 {
		t.Fatalf("key_fingerprint=%#v", entry["key_fingerprint"])
	}
	if entry["bytes_in"].(float64) <= 0 || entry["bytes_out"].(float64) <= 0 || entry["duration_ms"] == nil {
		t.Fatalf("sizes and duration missing: %s", lines[0])
	}
	for _, secret := range []string{"fixture-gateway-token", "a secret prompt", "query-secret"} {
		if strings.Contains(log.String(), secret) {
			t.Fatalf("the access log carries %q: %s", secret, log.String())
		}
	}
	if err := json.Unmarshal([]byte(lines[1]), &entry); err != nil || entry["route"] != "unmatched" || entry["status"] != 404.0 {
		t.Fatalf("unmatched line: %s", lines[1])
	}
}

// /metrics exists only with LLMGW_METRICS_TOKEN, and only for its bearer.
func TestMetricsAnswerOnlyTheirToken(t *testing.T) {
	handler, _ := observedFixture(t)
	scrape := func(token string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, request)
		return w
	}
	if w := scrape("anything"); w.Code != http.StatusNotFound {
		t.Fatalf("without LLMGW_METRICS_TOKEN: status=%d", w.Code)
	}
	t.Setenv("LLMGW_METRICS_TOKEN", "fixture-metrics-token")
	for _, token := range []string{"", "fixture-gateway-token", "fixture-metrics-tokenx"} {
		if w := scrape(token); w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") != "Bearer" {
			t.Fatalf("token %q: status=%d", token, w.Code)
		}
	}
	if w := apiSmokeRequest(handler, "/v1/chat/completions", map[string]any{
		"model": "fixture/model", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}); w.Code != http.StatusOK {
		t.Fatalf("chat status=%d body=%s", w.Code, w.Body.String())
	}
	w := scrape("fixture-metrics-token")
	body := w.Body.String()
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/plain; version=0.0.4") {
		t.Fatalf("status=%d content-type=%q", w.Code, w.Header().Get("Content-Type"))
	}
	for _, series := range []string{
		"# TYPE llmgw_http_requests_total counter",
		`llmgw_http_requests_total{route="/v1/chat/completions",method="POST",code="200"} 1`,
		`llmgw_http_requests_total{route="/metrics",method="GET",code="401"} 3`,
		`llmgw_http_request_duration_seconds_bucket{route="/v1/chat/completions",le="+Inf"} 1`,
		`llmgw_http_request_duration_seconds_count{route="/v1/chat/completions"} 1`,
		`llmgw_upstream_requests_total{provider="fixture",model="model",outcome="success"} 1`,
		`llmgw_tokens_total{provider="fixture",model="model",direction="input"} 11`,
		`llmgw_tokens_total{provider="fixture",model="model",direction="output"} 3`,
		"llmgw_http_requests_in_flight 1",
		"llmgw_build_info{",
		"go_goroutines ",
		"process_start_time_seconds ",
	} {
		if !strings.Contains(body, series) {
			t.Fatalf("missing %q in\n%s", series, body)
		}
	}
	if strings.Contains(body, "fixture-metrics-token") || strings.Contains(body, "fixture-gateway-token") {
		t.Fatal("the metrics carry a credential")
	}
}

func TestMetricLabelsAreEscaped(t *testing.T) {
	if got := quoteLabel("a\"b\\c\nd"); got != `"a\"b\\c\nd"` {
		t.Fatalf("quoteLabel=%s", got)
	}
	m := newGatewayMetrics(time.Unix(0, 0))
	m.observeRequest("/x", "GET", 200, 7*time.Second)
	var out strings.Builder
	m.write(&out)
	for _, line := range []string{
		`llmgw_http_request_duration_seconds_bucket{route="/x",le="5"} 0`,
		`llmgw_http_request_duration_seconds_bucket{route="/x",le="10"} 1`,
		`llmgw_http_request_duration_seconds_sum{route="/x"} 7`,
	} {
		if !strings.Contains(out.String(), line) {
			t.Fatalf("missing %q in\n%s", line, out.String())
		}
	}
}
