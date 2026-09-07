package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
)

func TestDisabledProviderRouteAPIContract(t *testing.T) {
	var calls atomic.Int32
	handler := setupAPISmokeFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if payload["model"] != "model" {
			t.Errorf("model=%v", payload["model"])
		}
		if stream, _ := payload["stream"].(bool); stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		writeJSON(w, 200, map[string]any{"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop"}}})
	}))
	config.Update(func(s *config.Settings) {
		disabled := *s.Providers["fixture"]
		disabled.Disabled = true
		s.Providers["disabled"] = &disabled
		s.Endpoints = map[string]*config.EndpointConfig{
			"mixed":        {Failover: []config.EndpointMember{{Provider: "disabled", Model: "model"}, {Provider: "fixture", Model: "model"}}},
			"all-disabled": {Failover: []config.EndpointMember{{Provider: "disabled", Model: "model"}, {Provider: "disabled", Model: "other"}}},
		}
	})
	for _, stream := range []bool{false, true} {
		for _, model := range []string{"disabled/model", "all-disabled", "mixed"} {
			t.Run(fmt.Sprintf("%s/stream=%t", model, stream), func(t *testing.T) {
				before := calls.Load()
				w := apiSmokeRequest(handler, "/v1/chat/completions", map[string]any{"model": model, "messages": []any{}, "stream": stream})
				if model == "mixed" {
					if w.Code != 200 || calls.Load()-before != 1 {
						t.Fatalf("mixed route status=%d calls=%d body=%s", w.Code, calls.Load()-before, w.Body.String())
					}
					return
				}
				if w.Code != 404 || calls.Load() != before || !strings.Contains(w.Body.String(), "No enabled provider") || strings.Contains(w.Header().Get("Content-Type"), "event-stream") {
					t.Fatalf("unavailable route status=%d calls=%d body=%s", w.Code, calls.Load()-before, w.Body.String())
				}
				var body map[string]any
				if json.Unmarshal(w.Body.Bytes(), &body) != nil || body["error"] == nil || strings.Contains(w.Body.String(), "fixture-upstream-token") {
					t.Fatalf("unsafe/invalid error: %s", w.Body.String())
				}
			})
		}
	}
	// Re-enable, warm the provider cache, then disable without clearing it:
	// route availability must not depend on a previously cached instance.
	for _, disabled := range []bool{false, true, false} {
		config.Update(func(s *config.Settings) { s.Providers["disabled"].Disabled = disabled })
		before := calls.Load()
		w := apiSmokeRequest(handler, "/v1/chat/completions", map[string]any{"model": "disabled/model", "messages": []any{}})
		if disabled {
			if w.Code != 404 || calls.Load() != before {
				t.Fatalf("cached disabled provider used: status=%d calls=%d", w.Code, calls.Load()-before)
			}
		} else if w.Code != 200 || calls.Load()-before != 1 {
			t.Fatalf("re-enabled provider unavailable: status=%d calls=%d body=%s", w.Code, calls.Load()-before, w.Body.String())
		}
	}
	// The common resolver also protects the native API entry points, before
	// adapters, credentials, or upstream requests can run.
	config.Update(func(s *config.Settings) { s.Providers["disabled"].Disabled = true })
	for _, path := range []string{"/v1/responses", "/v1/messages"} {
		before := calls.Load()
		w := apiSmokeRequest(handler, path, map[string]any{"model": "disabled/model", "input": "hi", "messages": []any{}, "max_tokens": 16})
		if w.Code != 404 || calls.Load() != before {
			t.Fatalf("%s status=%d calls=%d body=%s", path, w.Code, calls.Load()-before, w.Body.String())
		}
	}
	config.Update(func(s *config.Settings) { s.Providers["fixture"].Disabled = true })
	for _, path := range []string{"/v1/models", "/admin/api/state"} {
		before := calls.Load()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer fixture-gateway-token")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		var body map[string]any
		if w.Code != 200 || calls.Load() != before || json.Unmarshal(w.Body.Bytes(), &body) != nil {
			t.Fatalf("%s status=%d calls=%d body=%s", path, w.Code, calls.Load()-before, w.Body.String())
		}
		if path == "/v1/models" {
			rows, ok := body["data"].([]any)
			if !ok || len(rows) != 0 {
				t.Fatalf("all-disabled discovery must be an empty array, got: %s", w.Body.String())
			}
		} else {
			rows, _ := body["providers"].([]any)
			found := false
			for _, raw := range rows {
				row, _ := raw.(map[string]any)
				if row["id"] == "disabled" && row["disabled"] == true {
					found = true
				}
			}
			if !found {
				t.Fatal("disabled provider configuration disappeared from admin metadata")
			}
		}
	}
}

func TestDisabledProviderDiscoveryEligibility(t *testing.T) {
	handler := setupAPISmokeFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("discovery should use the fixture catalog, not an upstream request")
		http.Error(w, "unexpected request", 500)
	}))
	config.Update(func(s *config.Settings) {
		s.AnthropicDiscoveryAliases = false
		s.Providers["disabled"] = &config.ProviderConfig{Type: "echo", Disabled: true}
		s.Endpoints = map[string]*config.EndpointConfig{
			"mixed": {Failover: []config.EndpointMember{
				{Provider: "disabled", Model: "model"}, {Provider: "fixture", Model: "model"},
				{Provider: "disabled", Model: "other"}, {Provider: "fixture", Model: "other"},
			}},
			"all-disabled": {Failover: []config.EndpointMember{{Provider: "disabled", Model: "model"}}},
		}
	})
	originalCatalog := catalogModelsForPrincipal
	catalogCalls := map[string]int{}
	catalogModelsForPrincipal = func(id string, _ *config.Principal) []providers.ModelInfo {
		catalogCalls[id]++
		return []providers.ModelInfo{{ID: "model"}, {ID: "other"}}
	}
	t.Cleanup(func() { catalogModelsForPrincipal = originalCatalog })
	for _, allDisabled := range []bool{false, true} {
		config.Update(func(s *config.Settings) { s.Providers["fixture"].Disabled = allDisabled })
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer fixture-gateway-token")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		var body struct {
			Data []struct {
				ID       string                  `json:"id"`
				Failover []config.EndpointMember `json:"failover"`
			} `json:"data"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Data == nil {
			t.Fatalf("invalid discovery response: %d %s", w.Code, w.Body.String())
		}
		if allDisabled {
			if len(body.Data) != 0 {
				t.Fatalf("disabled exact models or endpoints still advertised: %s", w.Body.String())
			}
			continue
		}
		if len(body.Data) != 3 || body.Data[0].ID != "fixture/model" || body.Data[1].ID != "fixture/other" || body.Data[2].ID != "mixed" {
			t.Fatalf("unexpected eligible models/endpoints: %s", w.Body.String())
		}
		members := body.Data[2].Failover
		if len(members) != 2 || members[0] != (config.EndpointMember{Provider: "fixture", Model: "model"}) ||
			members[1] != (config.EndpointMember{Provider: "fixture", Model: "other"}) {
			t.Fatalf("mixed endpoint did not retain only enabled members in order: %+v", members)
		}
	}
	if catalogCalls["disabled"] != 0 || catalogCalls["fixture"] != 1 {
		t.Fatalf("disabled providers were queried for discovery: %+v", catalogCalls)
	}
}

func TestDisabledProviderRoutePreservesOperationalStatus(t *testing.T) {
	var calls atomic.Int32
	status := 503
	handler := setupAPISmokeFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeJSON(w, status, map[string]any{"error": map[string]any{"message": "fixture upstream failure"}})
	}))
	config.Update(func(s *config.Settings) {
		s.Providers["disabled"] = &config.ProviderConfig{Type: "echo", Disabled: true}
		s.Endpoints["mixed"] = &config.EndpointConfig{Failover: []config.EndpointMember{
			{Provider: "disabled", Model: "model"}, {Provider: "fixture", Model: "model"}, {Provider: "disabled", Model: "model"},
		}}
	})
	for _, code := range []int{401, 403, 429, 500, 503} {
		status = code
		for _, stream := range []bool{false, true} {
			before := calls.Load()
			w := apiSmokeRequest(handler, "/v1/chat/completions", map[string]any{"model": "mixed", "messages": []any{}, "stream": stream})
			if w.Code != code || calls.Load()-before != 1 {
				t.Fatalf("status=%d want=%d calls=%d body=%s", w.Code, code, calls.Load()-before, w.Body.String())
			}
		}
	}
	// Initialization failures remain operational errors, not unavailable models.
	config.Update(func(s *config.Settings) { s.Providers["fixture"].Type = "invalid-fixture-type" })
	providers.ResetProviders()
	w := apiSmokeRequest(handler, "/v1/chat/completions", map[string]any{"model": "mixed", "messages": []any{}})
	if w.Code != 502 {
		t.Fatalf("configuration failure status changed: %d %s", w.Code, w.Body.String())
	}
}

func TestDisabledProviderRoutePreservesCredentialPolicy(t *testing.T) {
	var calls atomic.Int32
	handler := setupAPISmokeFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "unexpected upstream call", 500)
	}))
	config.Update(func(s *config.Settings) {
		s.Providers["disabled"] = &config.ProviderConfig{Type: "echo", Disabled: true}
		s.Providers["private"] = &config.ProviderConfig{Type: "github_copilot"}
		s.Endpoints["mixed"] = &config.EndpointConfig{Failover: []config.EndpointMember{
			{Provider: "disabled", Model: "model"}, {Provider: "private", Model: "model"},
		}}
	})
	principal, err := iam.CreatePrincipal("human", "fixture-subject", "", "Fixture")
	if err != nil {
		t.Fatal(err)
	}
	project, err := iam.CreateProject("fixture-project", "Fixture")
	if err != nil {
		t.Fatal(err)
	}
	if err := iam.SetMembership(project.ID, principal.ID, "member"); err != nil {
		t.Fatal(err)
	}
	key, err := iam.IssueKey(iam.KeyCreate{ProjectID: project.ID, PrincipalID: principal.ID, Name: "fixture-key"})
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range []string{"private/model", "mixed"} {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"model":%q,"messages":[]}`, model)))
		req.Header.Set("Authorization", "Bearer "+key.Token)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != 403 || !strings.Contains(w.Body.String(), "no active provider credential") || calls.Load() != 0 {
			t.Fatalf("credential failure masked: status=%d calls=%d body=%s", w.Code, calls.Load(), w.Body.String())
		}
	}
}
