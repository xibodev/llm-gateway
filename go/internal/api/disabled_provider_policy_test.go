package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/router"
)

func disabledPolicyRequest(t *testing.T, handler http.Handler, path, model, token string, stream bool) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	contentType := "application/json"
	if path == "/v1/audio/transcriptions" {
		writer := multipart.NewWriter(&body)
		if err := writer.WriteField("model", model); err != nil {
			t.Fatal(err)
		}
		file, err := writer.CreateFormFile("file", "fixture.wav")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = file.Write([]byte("fixture audio"))
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		contentType = writer.FormDataContentType()
	} else if err := json.NewEncoder(&body).Encode(map[string]any{
		"model": model, "messages": []any{}, "input": "hi", "prompt": "hi", "max_tokens": 16, "stream": stream,
	}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, &body)
	req.Header.Set("Content-Type", contentType)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

func TestDisabledProviderPolicyPrecedenceAllPublicSurfaces(t *testing.T) {
	var calls atomic.Int32
	handler := setupAPISmokeFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "unexpected upstream call", 500)
	}))
	config.Update(func(s *config.Settings) {
		disabled := *s.Providers["fixture"]
		disabled.Disabled = true
		s.Providers["disabled"] = &disabled
		s.Providers["private"] = &config.ProviderConfig{Type: "github_copilot", Disabled: true}
		s.Providers["private-enabled"] = &config.ProviderConfig{Type: "github_copilot"}
		s.Endpoints["all-disabled"] = &config.EndpointConfig{Failover: []config.EndpointMember{{Provider: "disabled", Model: "model"}}}
		s.Endpoints["private-disabled"] = &config.EndpointConfig{Failover: []config.EndpointMember{{Provider: "private", Model: "model"}}}
		s.Endpoints["mixed"] = &config.EndpointConfig{Failover: []config.EndpointMember{
			{Provider: "disabled", Model: "model"}, {Provider: "fixture", Model: "model"},
		}}
		s.Endpoints["mixed-private"] = &config.EndpointConfig{Failover: []config.EndpointMember{
			{Provider: "disabled", Model: "model"}, {Provider: "private-enabled", Model: "model"},
		}}
	})
	owner, err := iam.CreatePrincipal("human", "disabled-policy-subject", "", "Fixture")
	if err != nil {
		t.Fatal(err)
	}
	project, err := iam.CreateProject("disabled-policy", "Fixture")
	if err != nil {
		t.Fatal(err)
	}
	if err := iam.SetMembership(project.ID, owner.ID, "member"); err != nil {
		t.Fatal(err)
	}
	paths := []string{
		"/v1/chat/completions", "/v1/responses", "/v1/messages", "/v1/messages/count_tokens",
		"/v1/embeddings", "/v1/audio/speech", "/v1/audio/transcriptions", "/v1/images/generations", "/v1/videos/generations",
	}
	for _, tc := range []struct {
		name    string
		models  []string
		policy  iam.KeyPolicy
		project bool
		status  int
		message string
	}{
		{"model-denied", []string{"disabled/model", "all-disabled", "mixed"}, iam.KeyPolicy{AllowedModels: []string{"other"}}, false, 403, "This key is not allowed to use model"},
		{"provider-denied", []string{"disabled/model", "all-disabled", "mixed"}, iam.KeyPolicy{AllowedProviders: []string{"other"}}, false, 403, "not allowed to use any provider"},
		{"project-model-denied", []string{"disabled/model", "all-disabled", "mixed"}, iam.KeyPolicy{AllowedModels: []string{"other"}}, true, 403, "This project is not allowed to use model"},
		{"project-provider-denied", []string{"disabled/model", "all-disabled", "mixed"}, iam.KeyPolicy{AllowedProviders: []string{"other"}}, true, 403, "not allowed to use any provider"},
		{"credential-denied", []string{"private/model", "private-disabled", "mixed-private"}, iam.KeyPolicy{}, false, 403, "no active provider credential"},
		{"allowed-unavailable", []string{"disabled/model", "all-disabled"}, iam.KeyPolicy{AllowedModels: []string{"disabled/model", "all-disabled"}, AllowedProviders: []string{"disabled"}, RPM: 1}, false, 404, "No enabled provider"},
		{"disabled-cannot-mask-enabled-denial", []string{"mixed"}, iam.KeyPolicy{AllowedProviders: []string{"disabled"}}, false, 403, "not allowed to use any provider"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			keyPolicy, projectPolicy := tc.policy, iam.KeyPolicy{}
			if tc.project {
				keyPolicy, projectPolicy = projectPolicy, keyPolicy
			}
			if _, err := iam.SetProjectPolicy(project.ID, projectPolicy); err != nil {
				t.Fatal(err)
			}
			key, err := iam.IssueKey(iam.KeyCreate{ProjectID: project.ID, PrincipalID: owner.ID, Name: tc.name, Policy: keyPolicy})
			if err != nil {
				t.Fatal(err)
			}
			for _, model := range tc.models {
				for _, path := range paths {
					for _, stream := range []bool{false, true} {
						w := disabledPolicyRequest(t, handler, path, model, key.Token, stream)
						if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.message) || calls.Load() != 0 || strings.Contains(w.Header().Get("Content-Type"), "event-stream") {
							t.Fatalf("%s %s stream=%t: status=%d calls=%d body=%s", path, model, stream, w.Code, calls.Load(), w.Body.String())
						}
						if strings.Contains(w.Body.String(), key.Token) || strings.Contains(w.Body.String(), "fixture-upstream-token") {
							t.Fatal("credential leaked in error response")
						}
					}
				}
			}
		})
	}
	for _, path := range paths {
		for _, token := range []string{"", "invalid-fixture-key"} {
			w := disabledPolicyRequest(t, handler, path, "disabled/model", token, false)
			if w.Code != 401 || calls.Load() != 0 {
				t.Fatalf("authentication bypass at %s: %d %s", path, w.Code, w.Body.String())
			}
		}
	}
}

func TestDisabledProviderMixedPolicyAndOrderedFallback(t *testing.T) {
	var attempted []string
	handler := setupAPISmokeFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		model, _ := payload["model"].(string)
		attempted = append(attempted, model)
		if model == "first" {
			http.Error(w, "fixture unavailable", 503)
			return
		}
		if stream, _ := payload["stream"].(bool); stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		writeJSON(w, 200, map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop"}}})
	}))
	config.Update(func(s *config.Settings) {
		disabled, denied := *s.Providers["fixture"], *s.Providers["fixture"]
		disabled.Disabled = true
		s.Providers["disabled"], s.Providers["denied"] = &disabled, &denied
		s.Providers["private"] = &config.ProviderConfig{Type: "github_copilot"}
		s.Endpoints["mixed"] = &config.EndpointConfig{Failover: []config.EndpointMember{
			{Provider: "disabled", Model: "disabled"}, {Provider: "denied", Model: "denied"},
			{Provider: "private", Model: "private"}, {Provider: "fixture", Model: "first"},
			{Provider: "disabled", Model: "disabled"}, {Provider: "fixture", Model: "second"},
		}}
	})
	owner, err := iam.CreatePrincipal("human", "mixed-policy-subject", "", "Fixture")
	if err != nil {
		t.Fatal(err)
	}
	project, err := iam.CreateProject("mixed-policy", "Fixture")
	if err != nil {
		t.Fatal(err)
	}
	if err := iam.SetMembership(project.ID, owner.ID, "member"); err != nil {
		t.Fatal(err)
	}
	key, err := iam.IssueKey(iam.KeyCreate{ProjectID: project.ID, PrincipalID: owner.ID, Name: "mixed", Policy: iam.KeyPolicy{
		AllowedModels: []string{"mixed", "disabled/second"}, AllowedProviders: []string{"disabled", "private", "fixture"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for _, stream := range []bool{false, true} {
		attempted = nil
		w := disabledPolicyRequest(t, handler, "/v1/chat/completions", "mixed", key.Token, stream)
		if w.Code != 200 || !reflect.DeepEqual(attempted, []string{"first", "second"}) {
			t.Fatalf("ordered authorized fallback: status=%d attempted=%v body=%s", w.Code, attempted, w.Body.String())
		}
	}
	for _, disabled := range []bool{false, true, false} {
		config.Update(func(s *config.Settings) { s.Providers["disabled"].Disabled = disabled })
		attempted = nil
		w := disabledPolicyRequest(t, handler, "/v1/chat/completions", "disabled/second", key.Token, false)
		if disabled {
			if w.Code != 404 || len(attempted) != 0 {
				t.Fatalf("disabled cached provider used: %d %v", w.Code, attempted)
			}
		} else if w.Code != 200 || !reflect.DeepEqual(attempted, []string{"second"}) {
			t.Fatalf("re-enabled provider failed: %d %v %s", w.Code, attempted, w.Body.String())
		}
	}
}

func TestDisabledProviderPlaygroundAvailability(t *testing.T) {
	setupAPISmokeFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected upstream call") }))
	config.Update(func(s *config.Settings) { s.Providers["fixture"].Disabled = true })
	project, err := iam.CreateProject("disabled-playground", "Fixture")
	if err != nil {
		t.Fatal(err)
	}
	principal := &config.Principal{ProjectID: project.ID}
	resolution, err := router.ResolveForPrincipal("fixture/model", principal)
	if err != nil {
		t.Fatal(err)
	}
	for _, allowed := range []string{"other", "fixture/model"} {
		if _, err := iam.SetProjectPolicy(project.ID, iam.KeyPolicy{AllowedModels: []string{allowed}, RPM: 1}); err != nil {
			t.Fatal(err)
		}
		want := 403
		if allowed == "fixture/model" {
			want = 404
		}
		for i := 0; i < 2; i++ {
			if _, status, message := enforcePlaygroundPolicy(principal, "fixture/model", "", resolution.Targets); status != want {
				t.Fatalf("playground policy status=%d want=%d: %s", status, want, message)
			}
			if _, _, status, message := audioPlaygroundTarget(principal, "fixture/model"); status != want {
				t.Fatalf("audio playground policy status=%d want=%d: %s", status, want, message)
			}
		}
	}
}
