package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
	"llmgw/internal/router"

	"github.com/xibodev/llm-provider-auth/tokenstore"
	"github.com/xibodev/llmgw-core/oauthflow"
)

// auditEventsWithAction returns the recorded events whose action is action,
// oldest first.
func auditEventsWithAction(t *testing.T, action string) []iam.AuditEvent {
	t.Helper()
	events, err := iam.ListAudit(1000)
	if err != nil {
		t.Fatal(err)
	}
	matched := []iam.AuditEvent{}
	for index := len(events) - 1; index >= 0; index-- {
		if events[index].Action == action {
			matched = append(matched, events[index])
		}
	}
	return matched
}

// expectOneAdminEvent runs change and requires it to record exactly one
// event with action, authored by the static administrator key adminKey.
func expectOneAdminEvent(t *testing.T, action, adminKey string, change func()) iam.AuditEvent {
	t.Helper()
	before := len(auditEventsWithAction(t, action))
	change()
	events := auditEventsWithAction(t, action)
	if len(events) != before+1 {
		t.Fatalf("%s: recorded %d events, want exactly one", action, len(events)-before)
	}
	event := events[len(events)-1]
	sum := sha256.Sum256([]byte(adminKey))
	if event.ActorPrincipalID != "" || event.Detail["actor_source"] != "static-admin-key" ||
		event.Detail["actor_key_fingerprint"] != hex.EncodeToString(sum[:])[:12] {
		t.Fatalf("%s actor: principal=%q detail=%+v, want the static administrator key",
			action, event.ActorPrincipalID, event.Detail)
	}
	return event
}

func expectNoNewEvent(t *testing.T, action string, change func()) {
	t.Helper()
	before := len(auditEventsWithAction(t, action))
	change()
	if after := len(auditEventsWithAction(t, action)); after != before {
		t.Fatalf("%s: recorded %d events, want none", action, after-before)
	}
}

// changedFields returns the field names an event's changed detail lists.
func changedFields(event iam.AuditEvent) []string {
	raw, _ := event.Detail["changed"].([]any)
	fields := make([]string, 0, len(raw))
	for _, value := range raw {
		field, _ := value.(string)
		fields = append(fields, field)
	}
	return fields
}

func expectChanged(t *testing.T, event iam.AuditEvent, want ...string) {
	t.Helper()
	if got := changedFields(event); !slices.Equal(got, want) {
		t.Fatalf("%s changed=%q, want %q", event.Action, got, want)
	}
}

// expectAuditWithout requires that no audit record stores any of values.
func expectAuditWithout(t *testing.T, values ...string) {
	t.Helper()
	db, err := iam.DB()
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query("SELECT action,COALESCE(target_id,''),detail_json FROM audit_events")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var action, target, detail string
		if err := rows.Scan(&action, &target, &detail); err != nil {
			t.Fatal(err)
		}
		for _, value := range values {
			if strings.Contains(target+detail, value) {
				t.Fatalf("the %s audit record stores a secret value", action)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

// useAuditTestSettings gives a test its own state directory and settings,
// restored when it ends.
func useAuditTestSettings(t *testing.T, apply func(*config.Settings)) {
	t.Helper()
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	router.ResetSavingsState()
	router.ResetTelemetryState()
	old := *config.Get()
	t.Cleanup(func() {
		config.Update(func(s *config.Settings) { *s = old })
		providers.ResetProviders()
		iam.ResetForTests()
		router.ResetSavingsState()
		router.ResetTelemetryState()
	})
	config.Update(func(s *config.Settings) {
		s.APIKeys, s.AllowUnauthenticatedAPI, s.SSOEnabled = nil, false, false
		s.Savings.Enabled = false
		s.Endpoints = map[string]*config.EndpointConfig{}
		apply(s)
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	providers.ResetProviders()
}

func TestProviderAndEndpointChangesRecordTheAdministratorWithoutSecrets(t *testing.T) {
	const adminKey = "fixture-admin-key"
	useAuditTestSettings(t, func(s *config.Settings) {
		s.APIKey = adminKey
		s.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
		s.Providers = map[string]*config.ProviderConfig{"echo": {Type: "echo"}}
	})
	server := httptest.NewServer(NewServer(Runtime{}))
	defer server.Close()
	admin := func(method, path string, body any) func() {
		return func() {
			t.Helper()
			if status, response := jsonRequest(t, server.URL+path, method, adminKey, body); status != http.StatusOK {
				t.Fatalf("%s %s: status=%d body=%+v", method, path, status, response)
			}
		}
	}
	firstKey, secondKey := "fixture-provider-secret-one", "fixture-provider-secret-two"

	created := expectOneAdminEvent(t, "provider.create", adminKey, admin(http.MethodPost, "/admin/api/providers", map[string]any{
		"id": "fixture", "type": "openai_compatible", "base_url": "https://one.example.test/v1", "api_key": firstKey,
	}))
	expectChanged(t, created, "type", "base_url", "credential")
	if created.TargetID != "fixture" || created.Detail["credential_kind"] != "api_key" {
		t.Fatalf("provider.create target=%q detail=%+v", created.TargetID, created.Detail)
	}
	moved := expectOneAdminEvent(t, "provider.update", adminKey, admin(http.MethodPost, "/admin/api/providers", map[string]any{
		"id": "fixture", "type": "openai_compatible", "base_url": "https://two.example.test/v1",
	}))
	expectChanged(t, moved, "base_url")
	if _, named := moved.Detail["credential_kind"]; named {
		t.Fatalf("an update that kept the credential reported one: %+v", moved.Detail)
	}
	rotated := expectOneAdminEvent(t, "provider.update", adminKey, admin(http.MethodPost, "/admin/api/providers", map[string]any{
		"id": "fixture", "type": "openai_compatible", "base_url": "https://two.example.test/v1", "api_key": secondKey,
	}))
	expectChanged(t, rotated, "credential")
	disabled := expectOneAdminEvent(t, "provider.enabled", adminKey, admin(http.MethodPost, "/admin/api/providers/fixture/enabled", map[string]any{
		"enabled": false,
	}))
	if disabled.Detail["enabled"] != false {
		t.Fatalf("provider.enabled detail=%+v", disabled.Detail)
	}

	route := expectOneAdminEvent(t, "endpoint.create", adminKey, admin(http.MethodPost, "/admin/api/endpoints", map[string]any{
		"name": "coding", "failover": []map[string]any{{"provider": "echo", "model": "echo-strong"}},
	}))
	members, _ := route.Detail["members"].([]any)
	if first, _ := members[0].(map[string]any); route.TargetID != "coding" || len(members) != 1 || first["model"] != "echo-strong" {
		t.Fatalf("endpoint.create target=%q detail=%+v", route.TargetID, route.Detail)
	}
	expectOneAdminEvent(t, "endpoint.update", adminKey, admin(http.MethodPost, "/admin/api/endpoints", map[string]any{
		"name": "coding", "failover": []map[string]any{{"provider": "echo", "model": "echo-default"}},
	}))
	removedRoute := expectOneAdminEvent(t, "endpoint.delete", adminKey, admin(http.MethodDelete, "/admin/api/endpoints/coding", nil))
	removedProvider := expectOneAdminEvent(t, "provider.delete", adminKey, admin(http.MethodDelete, "/admin/api/providers/fixture", nil))
	if removedRoute.Detail["existed"] != true || removedProvider.Detail["existed"] != true {
		t.Fatalf("delete details: endpoint=%+v provider=%+v", removedRoute.Detail, removedProvider.Detail)
	}
	expectAuditWithout(t, firstKey, secondKey, adminKey)
}

func TestCodexClientIDChangeRecordsTheAdministrator(t *testing.T) {
	signIn := newCodexSignIn(t, "fixture-codex-client")
	device := codexFlows[0]
	changed := expectOneAdminEvent(t, "oauth_client.update", "admin-secret", func() {
		signIn.startCodex(t, adminCodexStarter, device, map[string]any{"client_id": "fixture-admin-client"})
	})
	if changed.TargetType != "oauth_client" || changed.TargetID != "openai_codex" {
		t.Fatalf("oauth_client.update target=%s/%s", changed.TargetType, changed.TargetID)
	}
	expectChanged(t, changed, "client_id")
	expectNoNewEvent(t, "oauth_client.update", func() {
		signIn.startCodex(t, adminCodexStarter, device, map[string]any{"client_id": "fixture-admin-client"})
		signIn.startCodex(t, selfServiceCodexStarter, device, map[string]any{"client_id": "fixture-user-client"})
	})
}

func TestConsumerManualClientChangeRecordsTheAdministratorWithoutTheSecret(t *testing.T) {
	useAuditTestSettings(t, func(s *config.Settings) {
		s.APIKey = "admin-secret"
		s.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))
		s.Providers = map[string]*config.ProviderConfig{}
		s.GoogleAntigravityOAuthProfile = ""
	})
	owner, err := iam.CreatePrincipal("human", "fixture:antigravity-owner", "", "Antigravity Owner")
	if err != nil {
		t.Fatal(err)
	}
	var starts atomic.Int64
	daemon := http.NewServeMux()
	daemon.HandleFunc("POST /extension/v1/google_antigravity/oauth/start", func(w http.ResponseWriter, r *http.Request) {
		attempt := strconv.FormatInt(starts.Add(1), 10)
		writeJSON(w, http.StatusOK, map[string]any{"authorization": oauthflow.Authorization{
			AuthorizationURL: "https://auth.example.test/authorize",
			Secrets:          oauthflow.Secrets{State: "fixture-state-" + attempt, Verifier: "fixture-verifier"},
		}})
	})
	daemon.HandleFunc("POST /extension/v1/google_antigravity/oauth/exchange", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"record": tokenstore.Record{
			AccessToken: "fixture-access", RefreshToken: "fixture-refresh", Expiry: time.Now().Add(time.Hour),
		}})
	})
	serveExtensionDaemon(t, daemon)
	server := httptest.NewServer(NewServer(Runtime{}))
	defer server.Close()
	route := server.URL + "/admin/api/principals/" + owner.ID + "/connections/google_antigravity/oauth/"
	signIn := func(clientSecret string) func() {
		return func() {
			t.Helper()
			status, started := jsonRequest(t, route+"start", http.MethodPost, "admin-secret", map[string]any{
				"profile": "consumer_manual", "client_id": "fixture-consumer-client", "client_secret": clientSecret,
				"client_mode": "confidential", "redirect_uri": "http://localhost:51121/oauth-callback",
			})
			if status != http.StatusOK || started["flow_id"] == nil {
				t.Fatalf("start: status=%d body=%+v", status, started)
			}
			status, completed := jsonRequest(t, route+"complete", http.MethodPost, "admin-secret", map[string]any{
				"flow_id": started["flow_id"], "authorization_response": "fixture-code",
			})
			if status != http.StatusOK || completed["status"] != "authorized" {
				t.Fatalf("complete: status=%d body=%+v", status, completed)
			}
		}
	}
	first, second := "fixture-client-secret-one", "fixture-client-secret-two"

	stored := expectOneAdminEvent(t, "oauth_client.update", "admin-secret", signIn(first))
	expectChanged(t, stored, "client_id", "client_secret", "client_mode", "redirect_uri")
	if stored.TargetID != "google_antigravity" || stored.Detail["profile"] != "consumer_manual" {
		t.Fatalf("oauth_client.update target=%q detail=%+v", stored.TargetID, stored.Detail)
	}
	expectNoNewEvent(t, "oauth_client.update", signIn(first))
	rotated := expectOneAdminEvent(t, "oauth_client.update", "admin-secret", signIn(second))
	expectChanged(t, rotated, "client_secret")
	expectAuditWithout(t, first, second)
}

func TestAutoConnectRecordsTheAutomationItTurnsOn(t *testing.T) {
	setupAnonymousAutomationAPITest(t)
	server := NewServer(Runtime{})
	// The providers' own checks are not under test, so none is attempted.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	autoConnect := func() {
		t.Helper()
		request := httptest.NewRequest(http.MethodPost, "/admin/api/providers/auto-connect-free", nil).WithContext(ctx)
		request.Header.Set("Authorization", "Bearer admin")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("auto-connect: status=%d body=%s", response.Code, response.Body.String())
		}
	}
	enabled := expectOneAdminEvent(t, "provider_automation.update", "admin", autoConnect)
	if enabled.Detail["override"] != "on" || enabled.Detail["source"] != "auto_connect" {
		t.Fatalf("provider_automation.update detail=%+v", enabled.Detail)
	}
	expectNoNewEvent(t, "provider_automation.update", autoConnect)
}

func TestAdminPlaygroundRecordsTheAdministratorAndThePrincipal(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte{0xff, 0xf3, 0x01, 0x02})
	}))
	defer upstream.Close()
	useAuditTestSettings(t, func(s *config.Settings) {
		s.APIKey = "admin-secret"
		s.SSOEnabled, s.SSOSharedSecret, s.SSOAutoProvision = true, "proxy-secret", true
		s.Providers = map[string]*config.ProviderConfig{
			"echo":    {Type: "echo"},
			"localai": {Type: "openai_compatible", BaseURL: upstream.URL + "/v1", APIKey: "fixture-speech-key"},
		}
	})
	owner, err := iam.EnsurePrincipalBySubject("human", "authentik:playground-subject", "", "Playground Subject")
	if err != nil {
		t.Fatal(err)
	}
	project, err := iam.CreateProject("playground-audit", "Playground Audit")
	if err != nil {
		t.Fatal(err)
	}
	if err := iam.SetMembership(project.ID, owner.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(NewServer(Runtime{}))
	defer server.Close()
	chat := map[string]any{
		"principal_id": owner.ID, "project_id": project.ID, "model": "echo/echo-default",
		"messages": []map[string]any{{"role": "user", "content": "hello"}},
	}
	request := func(path string, body map[string]any) func() {
		return func() {
			t.Helper()
			if status, response := jsonRequest(t, server.URL+path, http.MethodPost, "admin-secret", body); status != http.StatusOK {
				t.Fatalf("%s: status=%d body=%+v", path, status, response)
			}
		}
	}

	executed := expectOneAdminEvent(t, "playground.execute", "admin-secret", request("/admin/api/playground", chat))
	surface := expectOneAdminEvent(t, "playground.execute", "admin-secret", request("/admin/api/playground/v1/chat/completions", chat))
	spoken := expectOneAdminEvent(t, "playground.speech", "admin-secret", request("/admin/api/playground/speech", map[string]any{
		"principal_id": owner.ID, "project_id": project.ID, "model": "localai/fixture-voice", "input": "hello",
	}))
	for _, event := range []iam.AuditEvent{executed, surface, spoken} {
		if event.Detail["on_behalf_of"] != owner.ID || event.TargetID != project.ID {
			t.Fatalf("%s target=%q detail=%+v, want the principal under on_behalf_of", event.Action, event.TargetID, event.Detail)
		}
	}

	// The principal's own request is the principal's.
	before := len(auditEventsWithAction(t, "playground.execute"))
	delete(chat, "principal_id")
	if status, response := ssoConnectionRequest(t, server.URL, "playground-subject", http.MethodPost, "/user/api/playground", chat); status != http.StatusOK {
		t.Fatalf("self-service playground: status=%d body=%+v", status, response)
	}
	events := auditEventsWithAction(t, "playground.execute")
	if len(events) != before+1 {
		t.Fatalf("self-service playground recorded %d events, want one", len(events)-before)
	}
	if own := events[len(events)-1]; own.ActorPrincipalID != owner.ID || own.Detail["on_behalf_of"] != nil {
		t.Fatalf("self-service playground actor=%q detail=%+v", own.ActorPrincipalID, own.Detail)
	}
}
