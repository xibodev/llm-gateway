package api

import (
	"context"
	"database/sql"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
	"llmgw/internal/router"

	core "github.com/xibodev/llmgw-core"
)

// resolveAs resolves a model the way a request principal's handler does.
func resolveAs(model string, p *config.Principal) (router.Resolution, error) {
	return resolveModel(context.Background(), model, p)
}

// aliasCandidatesAs lists native-alias candidates under a principal's
// governance.
func aliasCandidatesAs(p *config.Principal) (map[string][]router.Target, error) {
	return router.NativeAliasCandidates(governed(context.Background(), p), callerOf(p))
}

// TestGovernanceKeepsNotFoundAndForbiddenApart sends each request surface
// what a key may not address. The router hides a route outside the key's
// allowlist, and anything but a route for a routes-only key, as not found; key
// policy rejects a model outside the key's allowlist as forbidden. Were the
// envelope lost on the way to the router, the hidden cases would read 403.
func TestGovernanceKeepsNotFoundAndForbiddenApart(t *testing.T) {
	owner, project := setupKeyScope(t)
	issue := func(name string, policy iam.KeyPolicy) string {
		issued, err := iam.IssueKey(iam.KeyCreate{ProjectID: project.ID, PrincipalID: owner.ID, Name: name, Policy: policy})
		if err != nil {
			t.Fatal(err)
		}
		return issued.Token
	}
	routesOnly := issue("routes-only", iam.KeyPolicy{AllowedRoutes: []string{"echo-default"}, RoutesOnly: true})
	models := issue("models", iam.KeyPolicy{AllowedModels: []string{"echo-default"}})
	server := httptest.NewServer(NewServer())
	defer server.Close()
	messages := []map[string]any{{"role": "user", "content": "hi"}}
	surfaces := map[string]func(string) map[string]any{
		"/v1/chat/completions": func(model string) map[string]any { return map[string]any{"model": model, "messages": messages} },
		"/v1/responses":        func(model string) map[string]any { return map[string]any{"model": model, "input": "hi"} },
		"/v1/messages": func(model string) map[string]any {
			return map[string]any{"model": model, "max_tokens": 16, "messages": messages}
		},
		"/v1/messages/count_tokens": func(model string) map[string]any { return map[string]any{"model": model, "messages": messages} },
	}
	for path, body := range surfaces {
		for _, c := range []struct {
			name, token, model string
			want               int
		}{
			{"route outside the allowlist", routesOnly, "other", 404},
			{"direct model of a routes-only key", routesOnly, "one/echo-default", 404},
			{"allowed route", routesOnly, "echo-default", 200},
			{"model outside the key allowlist", models, "one/echo-new", 403},
		} {
			if status, response := jsonRequest(t, server.URL+path, "POST", c.token, body(c.model)); status != c.want {
				t.Errorf("%s %s: status=%d, want %d: %+v", path, c.name, status, c.want, response)
			}
		}
	}

	// Helpers that resolve without a request context carry the same envelope.
	p, found, err := iam.ResolveAPIKey(routesOnly)
	if err != nil || !found {
		t.Fatalf("resolve key: %v", err)
	}
	statuses := map[string]int{}
	_, _, statuses["audio"], _ = resolveAudioTarget(p, "other", core.ModelOperationAudioOut)
	_, _, statuses["media"], _ = resolveMediaTarget(p, "other", core.ModelOperationImage)
	_, _, statuses["playground audio"], _ = audioPlaygroundTarget(p, "other", core.ModelOperationAudioOut)
	_, _, statuses["playground media"], _ = mediaPlaygroundTarget(p, "other", core.ModelOperationImage)
	for name, status := range statuses {
		if status != 404 {
			t.Errorf("%s: hidden route status=%d, want 404", name, status)
		}
	}
}

// TestGovernanceNamesTheKeyInFailoverTelemetry drives a failover chain through
// each execution surface and reads the telemetry the router recorded: every
// chain must carry the project and key names from the envelope.
func TestGovernanceNamesTheKeyInFailoverTelemetry(t *testing.T) {
	owner, project := setupKeyScope(t)
	router.ResetTelemetryState()
	t.Cleanup(router.ResetTelemetryState)
	config.Update(func(s *config.Settings) {
		s.Providers["bad"] = &config.ProviderConfig{Type: "openai_compatible", BaseURL: "http://127.0.0.1:1/v1", APIKey: "none"}
		s.Endpoints["flaky"] = &config.EndpointConfig{Failover: []config.EndpointMember{
			{Provider: "bad", Model: "x"}, {Provider: "one", Model: "echo-default"},
		}}
	})
	providers.ResetProviders()
	issued, err := iam.IssueKey(iam.KeyCreate{ProjectID: project.ID, PrincipalID: owner.ID, Name: "telemetry-key"})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(NewServer())
	defer server.Close()
	messages := []map[string]any{{"role": "user", "content": "hi"}}
	requests := []struct {
		path string
		body map[string]any
	}{
		{"/v1/chat/completions", map[string]any{"model": "flaky", "messages": messages}},
		{"/v1/chat/completions", map[string]any{"model": "flaky", "messages": messages, "stream": true}},
		{"/v1/responses", map[string]any{"model": "flaky", "input": "hi"}},
		{"/v1/responses", map[string]any{"model": "flaky", "input": "hi", "stream": true}},
		{"/v1/messages", map[string]any{"model": "flaky", "max_tokens": 16, "messages": messages}},
		{"/v1/messages", map[string]any{"model": "flaky", "max_tokens": 16, "messages": messages, "stream": true}},
	}
	for _, request := range requests {
		// A stream records its chain before the status line is written.
		if status, response := jsonRequest(t, server.URL+request.path, "POST", issued.Token, request.body); status != 200 {
			t.Fatalf("%s %v: status=%d %+v", request.path, request.body["stream"], status, response)
		}
	}
	db, err := sql.Open("sqlite", filepath.Join(config.StateDir(), "telemetry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT COALESCE(project,''), COALESCE(key_name,'') FROM failover_events`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	chains := 0
	for rows.Next() {
		var projectName, keyName string
		if err := rows.Scan(&projectName, &keyName); err != nil {
			t.Fatal(err)
		}
		chains++
		if projectName != project.Slug || keyName != "telemetry-key" {
			t.Errorf("chain attributed to %q/%q, want %q/telemetry-key", projectName, keyName, project.Slug)
		}
	}
	if chains != len(requests) {
		t.Fatalf("recorded %d failover chains, want %d", chains, len(requests))
	}
}
