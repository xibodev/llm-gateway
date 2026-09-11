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
)

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
				} else if r.Method != http.MethodGet || r.URL.Path != "/v1/models/veo-a/operations/job-1" {
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
				{prefix + "veo-a/operations/job-1", http.StatusOK},
			} {
				payload, _ := json.Marshal(map[string]string{"model": "movies", "operation": tc.operation})
				req := httptest.NewRequest(http.MethodPost, "/v1/videos/generations", bytes.NewReader(payload))
				req.Header.Set("Authorization", "Bearer "+issued.Token)
				rec := httptest.NewRecorder()
				before := requests.Load()
				NewServer().ServeHTTP(rec, req)
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
