package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"

	core "github.com/xibodev/llmgw-core"
)

func TestResolveMediaTargetSkipsIneligibleRouteMembers(t *testing.T) {
	old := *config.Get()
	t.Cleanup(func() {
		config.Update(func(settings *config.Settings) { *settings = old })
		providers.ResetProviders()
	})
	config.Update(func(settings *config.Settings) {
		settings.AllowUnauthenticatedAPI = true
		settings.Providers = map[string]*config.ProviderConfig{
			"text":  {Type: "echo"},
			"media": {Type: "ai_studio", APIKey: "synthetic-key"},
		}
		settings.Endpoints = map[string]*config.EndpointConfig{
			"images": {Failover: []config.EndpointMember{{Provider: "text", Model: "text-model"}, {Provider: "media", Model: "image-model"}}},
			"videos": {Failover: []config.EndpointMember{{Provider: "text", Model: "text-model"}, {Provider: "media", Model: "video-model"}}},
		}
	})
	providers.ResetProviders()
	providerID, model, status, message := resolveMediaTarget(nil, "images", core.ModelOperationImage)
	if status != 0 || providerID != "media" || model != "image-model" {
		t.Fatalf("image target=%s/%s status=%d message=%q", providerID, model, status, message)
	}
	providerID, model, status, message = resolveMediaTarget(nil, "videos", core.ModelOperationVideo)
	if status != 0 || providerID != "media" || model != "video-model" {
		t.Fatalf("video target=%s/%s status=%d message=%q", providerID, model, status, message)
	}
}

func TestVideoPollingEnforcesResolvedRouteTarget(t *testing.T) {
	for _, surface := range []string{"ai_studio", "vertex_ai"} {
		t.Run(surface, func(t *testing.T) {
			resetState(t)
			old := *config.Get()
			t.Cleanup(func() { config.Update(func(s *config.Settings) { *s = old }) })
			var requests atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Header.Get("x-goog-api-key") != "synthetic-key" {
					t.Error("missing synthetic provider credential")
				}
				if surface == "vertex_ai" {
					if r.Method != http.MethodPost || r.URL.Path != "/v1/projects/test-project/locations/global/publishers/google/models/veo-a:fetchPredictOperation" {
						t.Errorf("unexpected poll: %s %s", r.Method, r.URL.Path)
					}
				} else if r.Method != http.MethodGet || r.URL.Path != "/v1/operations/job-1" {
					t.Errorf("unexpected poll: %s %s", r.Method, r.URL.Path)
				}
				_, _ = w.Write([]byte(`{"done":false}`))
			}))
			defer upstream.Close()
			config.Update(func(s *config.Settings) {
				s.APIKey = "synthetic-admin"
				s.APIKeys = nil
				s.AllowUnauthenticatedAPI = false
				s.Providers = map[string]*config.ProviderConfig{
					"video": {Type: surface, BaseURL: upstream.URL + "/v1", APIKey: "synthetic-key", Project: "test-project", Location: "global"},
				}
				s.Endpoints = map[string]*config.EndpointConfig{
					"movies": {Failover: []config.EndpointMember{{Provider: "video", Model: "veo-a"}}},
				}
			})
			owner, err := iam.CreatePrincipal("human", "", "", "Video test owner")
			if err != nil {
				t.Fatal(err)
			}
			project, err := iam.CreateProject("video-test", "Video test")
			if err != nil {
				t.Fatal(err)
			}
			if err := iam.SetMembership(project.ID, owner.ID, "owner"); err != nil {
				t.Fatal(err)
			}
			issued, err := iam.IssueKey(iam.KeyCreate{ProjectID: project.ID, PrincipalID: owner.ID,
				Policy: iam.KeyPolicy{AllowedRoutes: []string{"movies"}, RoutesOnly: true}})
			if err != nil {
				t.Fatal(err)
			}
			prefix := "models/"
			if surface == "vertex_ai" {
				prefix = "projects/test-project/locations/global/publishers/google/models/"
			}
			for _, tc := range []struct {
				operation string
				status    int
			}{
				{prefix + "veo-b/operations/job-1", http.StatusForbidden},
				{upstream.URL + "/stolen", http.StatusBadRequest},
				{encodeVideoOperation("other", "veo-a", prefix+"veo-a/operations/job-1"), http.StatusForbidden},
				{prefix + "veo-a/operations/job-1", http.StatusOK},
				{encodeVideoOperation("video", "veo-a", prefix+"veo-a/operations/job-1"), http.StatusOK},
			} {
				payload, _ := json.Marshal(map[string]string{"model": "movies", "operation": tc.operation})
				req := httptest.NewRequest(http.MethodPost, "/v1/videos/generations", bytes.NewReader(payload))
				req.Header.Set("Authorization", "Bearer "+issued.Token)
				rec := httptest.NewRecorder()
				before := requests.Load()
				NewServer(Runtime{}).ServeHTTP(rec, req)
				if rec.Code != tc.status {
					t.Fatalf("operation=%s status=%d want=%d body=%s", tc.operation, rec.Code, tc.status, rec.Body.String())
				}
				wantRequests := int32(0)
				if tc.status == http.StatusOK {
					wantRequests = 1
				}
				if requests.Load()-before != wantRequests {
					t.Fatalf("operation=%s caused %d requests, want %d", tc.operation, requests.Load()-before, wantRequests)
				}
			}
		})
	}
}

func TestPlaygroundVideoPollingEnforcesResolvedRouteTarget(t *testing.T) {
	resetState(t)
	old := *config.Get()
	t.Cleanup(func() { config.Update(func(s *config.Settings) { *s = old }) })
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("x-goog-api-key") != "synthetic-key" {
			t.Error("missing synthetic provider credential")
		}
		_, _ = w.Write([]byte(`{"done":false}`))
	}))
	defer upstream.Close()
	config.Update(func(s *config.Settings) {
		s.APIKey = "synthetic-admin"
		s.APIKeys = nil
		s.AllowUnauthenticatedAPI = false
		s.Providers = map[string]*config.ProviderConfig{
			"video": {Type: "ai_studio", BaseURL: upstream.URL + "/v1", APIKey: "synthetic-key"},
		}
		s.Endpoints = map[string]*config.EndpointConfig{}
	})
	owner, _ := iam.CreatePrincipal("human", "", "", "Video test owner")
	project, _ := iam.CreateProject("playground-video-test", "Playground video test")
	if err := iam.SetMembership(project.ID, owner.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(NewServer(Runtime{}))
	defer server.Close()
	for _, testCase := range []struct {
		operation string
		status    int
	}{
		{upstream.URL + "/stolen", http.StatusBadRequest},
		{"models/veo-b/operations/job-1", http.StatusForbidden},
		{encodeVideoOperation("other", "veo-a", "models/veo-a/operations/job-1"), http.StatusForbidden},
		{"models/veo-a/operations/job-1", http.StatusOK},
		{encodeVideoOperation("video", "veo-a", "models/veo-a/operations/job-1"), http.StatusOK},
	} {
		before := requests.Load()
		status, _ := jsonRequest(t, server.URL+"/admin/api/playground/video", http.MethodPost, "synthetic-admin", map[string]any{
			"principal_id": owner.ID, "project_id": project.ID,
			"model": "video/veo-a", "operation": testCase.operation,
		})
		if status != testCase.status {
			t.Fatalf("operation=%q status=%d want=%d", testCase.operation, status, testCase.status)
		}
		wantRequests := int32(0)
		if status == http.StatusOK {
			wantRequests = 1
		}
		if requests.Load()-before != wantRequests {
			t.Fatalf("operation=%q requests=%d want=%d", testCase.operation, requests.Load()-before, wantRequests)
		}
	}
}

func TestVideoJobPayloadBindsOperationToProviderAndModel(t *testing.T) {
	payload := videoJobPayload("provider-a", "veo-a", providers.VideoJob{Operation: "models/veo-a/operations/job-1"})
	encoded, _ := payload["operation"].(string)
	handle, ok := decodeVideoOperation(encoded)
	if !ok || handle.Provider != "provider-a" || handle.Model != "veo-a" || handle.Operation != "models/veo-a/operations/job-1" {
		t.Fatalf("encoded handle=%q decoded=%+v ok=%v", encoded, handle, ok)
	}
	if _, status, _ := resolveVideoOperation(encoded, "provider-b", "veo-a"); status != http.StatusForbidden {
		t.Fatalf("cross-provider handle status=%d want=%d", status, http.StatusForbidden)
	}
}
