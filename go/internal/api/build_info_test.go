package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"llmgw/internal/buildinfo"
	"llmgw/internal/config"
	"llmgw/internal/iam"
)

func TestBuildInfoIsConsistentAcrossHTTPStatusSurfaces(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	old := buildinfo.Current()
	buildinfo.Version = "1.2.3"
	buildinfo.Commit = "fixture-commit"
	buildinfo.BuildTime = "2026-01-02T03:04:05Z"
	t.Cleanup(func() {
		buildinfo.Version = old.Version
		buildinfo.Commit = old.Commit
		buildinfo.BuildTime = old.BuildTime
	})
	config.Update(func(settings *config.Settings) {
		settings.APIKey = "admin-secret"
		settings.APIKeys = nil
	})
	server := httptest.NewServer(NewServer())
	defer server.Close()
	for _, test := range []struct {
		path  string
		token string
	}{
		{path: "/health"},
		{path: "/admin/api/state", token: "admin-secret"},
	} {
		req, _ := http.NewRequest(http.MethodGet, server.URL+test.path, nil)
		if test.token != "" {
			req.Header.Set("Authorization", "Bearer "+test.token)
		}
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		decodeErr := json.NewDecoder(response.Body).Decode(&payload)
		_ = response.Body.Close()
		if decodeErr != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("%s status=%d decode=%v", test.path, response.StatusCode, decodeErr)
		}
		if payload["version"] != "1.2.3" || payload["commit"] != "fixture-commit" || payload["build_time"] != "2026-01-02T03:04:05Z" {
			t.Fatalf("%s build info=%+v", test.path, payload)
		}
	}
}
