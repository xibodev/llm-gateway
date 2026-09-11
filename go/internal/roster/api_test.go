package roster_test

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"llmgw/internal/api"
	"llmgw/internal/config"
	"llmgw/internal/iam"
)

// Keep route integration coverage with the roster implementation so this task
// does not need to change the shared API test fixtures.
func TestRosterRoutesIsolateAdminAndSSOAndLeaveConfigUnchanged(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	t.Setenv("LLMGW_PROVIDER_ROSTER_URL", "")
	t.Setenv("LLMGW_PROVIDER_ROSTER_PUBLIC_KEY", "")
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	config.Update(func(s *config.Settings) {
		s.APIKey = "roster-test-admin"
		s.SSOEnabled = true
		s.SSOSharedSecret = "roster-test-proxy"
		s.SSOAdminGroup = "admins"
		s.SSOAutoProvision = true
	})
	principal, err := iam.CreatePrincipal("service", "service:roster-test", "", "Roster Test")
	if err != nil {
		t.Fatal(err)
	}
	project, err := iam.CreateProject("roster-test", "Roster Test")
	if err != nil {
		t.Fatal(err)
	}
	if err := iam.SetMembership(project.ID, principal.ID, "member"); err != nil {
		t.Fatal(err)
	}
	issued, err := iam.IssueKey(iam.KeyCreate{ProjectID: project.ID, PrincipalID: principal.ID, Name: "test"})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(config.Get())
	server := api.NewServer()
	for _, tc := range []struct {
		method, path, token, groups, origin string
		status                              int
	}{
		{"GET", "/admin/api/provider-roster", "", "", "", 401},
		{"GET", "/user/api/provider-roster", "", "", "", 401},
		{"POST", "/admin/api/provider-roster/refresh", "", "", "", 401},
		{"GET", "/admin/api/provider-roster", issued.Token, "", "", 401},
		{"POST", "/admin/api/provider-roster/refresh", issued.Token, "", "", 401},
		{"GET", "/user/api/provider-roster", issued.Token, "", "", 401},
		{"GET", "/user/api/provider-roster", "roster-test-admin", "", "", 401},
		{"GET", "/admin/api/provider-roster", "roster-test-admin", "", "", 200},
		{"POST", "/admin/api/provider-roster/refresh", "roster-test-admin", "", "", 200},
		{"GET", "/user/api/provider-roster", "", "members", "", 200},
		{"GET", "/admin/api/provider-roster", "", "members", "", 403},
		{"POST", "/admin/api/provider-roster/refresh", "", "members", "http://example.com", 403},
		{"POST", "/admin/api/provider-roster/refresh", "", "admins", "", 403},
		{"POST", "/admin/api/provider-roster/refresh", "", "admins", "http://example.com", 200},
		{"POST", "/user/api/provider-roster/refresh", "", "members", "http://example.com", 404},
	} {
		r := httptest.NewRequest(tc.method, "http://example.com"+tc.path, nil)
		if tc.token != "" {
			r.Header.Set("Authorization", "Bearer "+tc.token)
		}
		if tc.groups != "" {
			r.Header.Set("X-LLMGW-SSO-Secret", "roster-test-proxy")
			r.Header.Set("X-Authentik-Uid", "roster-test-human")
			r.Header.Set("X-Authentik-Groups", tc.groups)
		}
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Errorf("%s %s groups=%s status=%d want=%d body=%s", tc.method, tc.path, tc.groups, w.Code, tc.status, w.Body.String())
		}
		if w.Code == 200 {
			var data map[string]json.RawMessage
			if json.Unmarshal(w.Body.Bytes(), &data) != nil {
				t.Fatal("invalid JSON")
			}
			for _, key := range []string{"entries", "revision", "published_at", "last_checked", "last_success", "auto_refresh", "configured", "error", "stale", "sources"} {
				if _, ok := data[key]; !ok {
					t.Errorf("missing contract field %s", key)
				}
			}
			if string(data["entries"]) != "[]" || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("unexpected seed/cache headers")
			}
		}
	}
	after, _ := json.Marshal(config.Get())
	if !bytes.Equal(before, after) {
		t.Fatal("roster API changed configuration")
	}
}
