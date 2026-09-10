package providers

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestGoogleVideoRejectsUnsafeOperationsBeforeRequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	studio := NewAIStudio(server.URL+"/v1beta", "synthetic-key", 5)
	vertex := NewVertexAIWithAccessToken(server.URL+"/v1", "synthetic-token", "test-project", "global", 5)
	vertexOp := "projects/test-project/locations/global/publishers/google/models/veo-a/operations/job-1"
	for _, tc := range []struct {
		name string
		p    GoogleAIProvider
		ops  []string
	}{
		{"studio", studio, []string{
			server.URL + "/operations/stolen", // This server must receive no credential-bearing request.
			"https://foreign.invalid/operations/job-1", "//foreign.invalid/operations/job-1",
			aiStudioDefaultBase + "/operations/job-1", // Even canonical absolute URLs are not resource names.
			"https://generativelanguage.googleapis.com@foreign.invalid/v1beta/operations/job-1",
			"/operations/job-1", "operations/../models", "operations/%2e%2e", "operations/%252f",
			"operations/job-1?key=secret", "operations/job-1#fragment", "operations/job-1?",
			"operations/job-1/extra", "operations/", "operations/.", "operations/..",
			"operations\\job-1", "models/veo-a:predictLongRunning", "models/veo-a/operations/../job-1", "",
		}},
		{"vertex", vertex, []string{
			server.URL + "/v1/" + vertexOp,
			strings.Replace(vertexOp, "test-project", "other-project", 1),
			strings.Replace(vertexOp, "global", "other-location", 1),
			strings.Replace(vertexOp, "google", "other-publisher", 1),
			strings.Replace(vertexOp, "veo-a", "../veo-b", 1),
			strings.Replace(vertexOp, "veo-a", "%2e%2e", 1),
			vertexOp + "?extra=true", vertexOp + "/extra", "operations/job-1", "",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, op := range tc.ops {
				_, err := tc.p.PollVideo(op)
				var invocation *InvocationError
				if !errors.As(err, &invocation) || invocation.Status != http.StatusBadRequest {
					t.Errorf("operation=%q err=%v, want 400", op, err)
				}
			}
		})
	}
	if requests.Load() != 0 {
		t.Fatalf("unsafe operations caused %d outbound requests", requests.Load())
	}
}

func TestGoogleVideoOperationTargetValidation(t *testing.T) {
	vertex := NewVertexAI("", "synthetic-key", "test-project", "global", 5)
	studio := NewAIStudio("", "synthetic-key", 5)
	for _, tc := range []struct {
		p         GoogleAIProvider
		operation string
	}{
		{vertex, "projects/test-project/locations/global/publishers/google/models/veo-a/operations/job-1"},
		{studio, "models/veo-a/operations/job-1"},
	} {
		for _, model := range []string{"veo-a", "models/veo-a"} {
			if err := tc.p.ValidateVideoOperation(model, tc.operation); err != nil {
				t.Fatalf("matching model rejected: %v", err)
			}
		}
		var invocation *InvocationError
		if err := tc.p.ValidateVideoOperation("veo-b", tc.operation); !errors.As(err, &invocation) || invocation.Status != http.StatusForbidden {
			t.Fatalf("model mismatch: %v, want 403", err)
		}
	}
	if err := studio.ValidateVideoOperation("veo-a", "operations/job-1"); err != nil {
		t.Fatalf("opaque operation compatibility: %v", err)
	}
}

func TestGoogleVideoPollingDoesNotFollowRedirects(t *testing.T) {
	var foreignRequests atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignRequests.Add(1)
		_, _ = w.Write([]byte(`{"done":false}`))
	}))
	defer foreign.Close()
	for _, surface := range []string{"studio", "vertex"} {
		for _, status := range []int{301, 302, 303, 307, 308} {
			t.Run(surface+"/"+http.StatusText(status), func(t *testing.T) {
				var requests atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					if r.Header.Get("x-goog-api-key") != "synthetic-key" && r.Header.Get("Authorization") != "Bearer synthetic-token" {
						t.Error("missing synthetic credential on canonical request")
					}
					http.Redirect(w, r, foreign.URL+"/stolen", status)
				}))
				defer upstream.Close()
				p := NewAIStudio(upstream.URL+"/v1beta", "synthetic-key", 5)
				op := "operations/job-1"
				if surface == "vertex" {
					p = NewVertexAIWithAccessToken(upstream.URL+"/v1", "synthetic-token", "test-project", "global", 5)
					op = "projects/test-project/locations/global/publishers/google/models/veo-a/operations/job-1"
				}
				if _, err := p.PollVideo(op); err == nil {
					t.Fatal("redirect reported as a successful poll")
				}
				if requests.Load() != 1 {
					t.Fatalf("canonical requests=%d, want 1", requests.Load())
				}
			})
		}
	}
	if foreignRequests.Load() != 0 {
		t.Fatalf("redirect leaked %d requests to foreign host", foreignRequests.Load())
	}
}
