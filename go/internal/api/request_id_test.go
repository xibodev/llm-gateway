package api

import (
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
)

// generatedRequestIDRE matches an ID the gateway assigned.
var generatedRequestIDRE = regexp.MustCompile(`^req_[0-9a-f]{32}$`)

// namedAsSent reports whether id names a request that sent the ID sent: the
// client's own when it sent one, an ID the gateway assigned otherwise.
func namedAsSent(id, sent string) bool {
	if sent == "" {
		return generatedRequestIDRE.MatchString(id)
	}
	return id == sent
}

// requestIDTestKey issues a project key, whose requests are metered.
func requestIDTestKey(t *testing.T) string {
	t.Helper()
	principal, err := iam.CreatePrincipal("service", "service:request-ids", "", "Request IDs")
	if err != nil {
		t.Fatal(err)
	}
	project, err := iam.CreateProject("request-ids", "Request IDs")
	if err != nil {
		t.Fatal(err)
	}
	if err := iam.SetMembership(project.ID, principal.ID, "member"); err != nil {
		t.Fatal(err)
	}
	issued, err := iam.IssueKey(iam.KeyCreate{ProjectID: project.ID, PrincipalID: principal.ID, Name: "request-ids"})
	if err != nil {
		t.Fatal(err)
	}
	return issued.Token
}

// postWithRequestID posts body to url, as token unless it is empty, naming
// the request sent unless that is empty, and returns the response and its
// body.
func postWithRequestID(t *testing.T, url, token, sent, body string) (*http.Response, []byte) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if sent != "" {
		request.Header.Set("X-Request-Id", sent)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response, raw
}

// usageRequestIDs returns the request ID of every usage record, oldest first.
func usageRequestIDs(t *testing.T) []string {
	t.Helper()
	db, err := iam.DB()
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`SELECT request_id FROM usage_events ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}

// useEchoGateway serves fresh state with the static administrator key
// fixture-admin, an echo provider and a project key, which it returns.
func useEchoGateway(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	useAuditTestSettings(t, func(s *config.Settings) {
		s.APIKey = "fixture-admin"
		s.Providers = map[string]*config.ProviderConfig{"echo": {Type: "echo"}}
	})
	token := requestIDTestKey(t)
	server := httptest.NewServer(NewServer(Runtime{}))
	t.Cleanup(server.Close)
	return server, token
}

const echoChat = `{"model":"echo/echo-default","messages":[{"role":"user","content":"hello"}]}`

// The gateway adopts a client's ID when it is a short token of letters,
// digits and . _ : -, which no header, log line or lookup can be broken
// with. Anything else, or nothing, gets an ID of the gateway's own, never one
// an earlier request had.
func TestRequestIDsAreAdoptedOrAssigned(t *testing.T) {
	handler := useHeaderProbe(t)
	requestID := func(sent string) string {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/health", nil)
		if sent != "" {
			request.Header.Set("X-Request-Id", sent)
		}
		return serve(handler, request).Header().Get("X-Request-Id")
	}
	first, second := requestID(""), requestID("")
	if !generatedRequestIDRE.MatchString(first) || !generatedRequestIDRE.MatchString(second) || first == second {
		t.Fatalf("assigned %q and %q, want two different gateway IDs", first, second)
	}
	for _, sent := range []string{"client-trace_01:retry.2", "7d0f3c1e-5b8a-4f0e-9a43-2c6e1d7b9f10", strings.Repeat("a", 128)} {
		if got := requestID(sent); got != sent {
			t.Errorf("client ID %q came back as %q", sent, got)
		}
	}
	for _, sent := range []string{strings.Repeat("a", 129), "two words", "semi;colon", "path/like", `quoted"id`, "naïve", "line\nbreak"} {
		if got := requestID(sent); !generatedRequestIDRE.MatchString(got) {
			t.Errorf("client ID %q came back as %q, want a gateway ID", sent, got)
		}
	}
}

// A data-plane request's usage record is found by the ID its response named,
// whether the gateway assigned it or the client chose it.
func TestUsageRecordsCarryTheRequestID(t *testing.T) {
	server, token := useEchoGateway(t)
	var named []string
	for _, sent := range []string{"", "client-request.1"} {
		response, body := postWithRequestID(t, server.URL+"/v1/chat/completions", token, sent, echoChat)
		id := response.Header.Get("X-Request-Id")
		if response.StatusCode != http.StatusOK || !namedAsSent(id, sent) {
			t.Fatalf("sent %q: status=%d X-Request-Id=%q body=%s", sent, response.StatusCode, id, body)
		}
		named = append(named, id)
	}
	if ids := usageRequestIDs(t); !slices.Equal(ids, named) {
		t.Fatalf("usage records %q, want %q", ids, named)
	}
}

// A client that sends one ID twice has both requests recorded: the first
// under the ID, the repeat under one of its own, since an ID names one usage
// record and dropping the repeat would drop what it consumed.
func TestARepeatedClientIDKeepsEveryUsageRecord(t *testing.T) {
	server, token := useEchoGateway(t)
	for range 2 {
		response, body := postWithRequestID(t, server.URL+"/v1/chat/completions", token, "client-retry.1", echoChat)
		if response.StatusCode != http.StatusOK || response.Header.Get("X-Request-Id") != "client-retry.1" {
			t.Fatalf("status=%d X-Request-Id=%q body=%s", response.StatusCode, response.Header.Get("X-Request-Id"), body)
		}
	}
	if ids := usageRequestIDs(t); len(ids) != 2 || ids[0] != "client-retry.1" || ids[1] == "" || ids[1] == ids[0] {
		t.Fatalf("usage records %q, want the first under the client's ID and the repeat under its own", ids)
	}
}

// Every response names its request, an error's too, and the gateway's own
// error envelopes repeat the ID where each surface's clients read it: inside
// the standard error, and beside Anthropic's error, where Anthropic's own
// envelope carries it. A failure the gateway records is recorded under the
// same ID.
func TestErrorResponsesNameTheirRequest(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"fixture outage"}}`)
	}))
	t.Cleanup(upstream.Close)
	useAuditTestSettings(t, func(s *config.Settings) {
		s.APIKey = "fixture-admin"
		s.Providers = map[string]*config.ProviderConfig{"down": {Type: "openai_compatible", BaseURL: upstream.URL, APIKey: "fixture"}}
		s.Policies.Defaults = config.ProviderPolicy{RetryMaxAttempts: 1}
		s.Policies.Overrides = map[string]config.ProviderPolicy{}
	})
	token := requestIDTestKey(t)
	server := httptest.NewServer(NewServer(Runtime{}))
	t.Cleanup(server.Close)
	chat := func(model string) string {
		return `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
	}
	for _, tc := range []struct {
		name, path, token, body string
		status                  int
		anthropic, recorded     bool
	}{
		{"invalid key", "/v1/chat/completions", "wrong", chat("down/model"), http.StatusUnauthorized, false, false},
		{"unknown model", "/v1/chat/completions", token, chat("missing/model"), http.StatusNotFound, false, true},
		{"failed upstream", "/v1/chat/completions", token, chat("down/model"), http.StatusServiceUnavailable, false, true},
		{"Messages invalid key", "/v1/messages", "wrong", chat("down/model"), http.StatusUnauthorized, true, false},
		{"Messages unknown model", "/v1/messages", token, chat("missing/model"), http.StatusNotFound, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response, raw := postWithRequestID(t, server.URL+tc.path, tc.token, "", tc.body)
			id := response.Header.Get("X-Request-Id")
			var envelope map[string]any
			if err := json.Unmarshal(raw, &envelope); err != nil || response.StatusCode != tc.status || !generatedRequestIDRE.MatchString(id) {
				t.Fatalf("status=%d X-Request-Id=%q body=%s", response.StatusCode, id, raw)
			}
			errorBody, _ := envelope["error"].(map[string]any)
			named, keys, errorKeys := errorBody["request_id"], []string{"error"}, []string{"code", "message", "request_id", "type"}
			if tc.anthropic {
				named, keys, errorKeys = envelope["request_id"], []string{"error", "request_id", "type"}, []string{"message", "type"}
			}
			if named != id || !slices.Equal(slices.Sorted(maps.Keys(envelope)), keys) || !slices.Equal(slices.Sorted(maps.Keys(errorBody)), errorKeys) {
				t.Fatalf("X-Request-Id=%q envelope=%s", id, raw)
			}
			if recorded := slices.Contains(usageRequestIDs(t), id); recorded != tc.recorded {
				t.Fatalf("usage recorded under %q: %v, want %v", id, recorded, tc.recorded)
			}
		})
	}
	// A response no handler wrote names its request as well.
	response, err := http.Get(server.URL + "/v1/missing")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound || !generatedRequestIDRE.MatchString(response.Header.Get("X-Request-Id")) {
		t.Fatalf("unknown route: status=%d X-Request-Id=%q", response.StatusCode, response.Header.Get("X-Request-Id"))
	}
}

// A stream names its request in its headers, before its first event, and
// its usage record carries the same ID however the stream ends.
func TestStreamsNameTheirRequest(t *testing.T) {
	upstream := newStreamOutcomeUpstream(t)
	for _, tc := range []struct{ name, path, model, sent string }{
		{"Chat with the client's ID", "/v1/chat/completions", "chat/complete", "client-stream.1"},
		{"native Messages", "/v1/messages", "claude/complete", ""},
		{"Responses cut short", "/v1/responses", "native/cut", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gateway := setupStreamOutcomeTest(t, upstream.URL)
			response, body := postWithRequestID(t, gateway.URL+tc.path, "", tc.sent, streamRequestBody(tc.path, tc.model))
			id := response.Header.Get("X-Request-Id")
			if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") ||
				!strings.Contains(string(body), "Hello there") || !namedAsSent(id, tc.sent) {
				t.Fatalf("status=%d content-type=%q X-Request-Id=%q body=%s", response.StatusCode, response.Header.Get("Content-Type"), id, body)
			}
			if ids := usageRequestIDs(t); !slices.Equal(ids, []string{id}) {
				t.Fatalf("usage records %q, want one under %q", ids, id)
			}
		})
	}
}

// An administrator's change is recorded with the ID of the request that
// made it, so the audit history meets what the administrator's client saw.
func TestAdminAuditNamesTheRequest(t *testing.T) {
	const adminKey = "fixture-admin"
	useAuditTestSettings(t, func(s *config.Settings) { s.APIKey = adminKey })
	server := httptest.NewServer(NewServer(Runtime{}))
	t.Cleanup(server.Close)
	for _, tc := range []struct{ slug, sent string }{{"assigned", ""}, {"chosen", "admin-change.7"}} {
		var response *http.Response
		var body []byte
		event := expectOneAdminEvent(t, "project.create", adminKey, func() {
			response, body = postWithRequestID(t, server.URL+"/admin/api/projects", adminKey, tc.sent,
				`{"slug":"`+tc.slug+`","name":"`+tc.slug+`"}`)
		})
		id := response.Header.Get("X-Request-Id")
		if response.StatusCode != http.StatusCreated || !namedAsSent(id, tc.sent) || event.Detail["request_id"] != id {
			t.Fatalf("sent %q: status=%d X-Request-Id=%q detail=%+v body=%s", tc.sent, response.StatusCode, id, event.Detail, body)
		}
	}
}
