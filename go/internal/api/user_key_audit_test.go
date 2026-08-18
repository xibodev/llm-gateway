package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
)

func TestOwnerUpdatesOwnKeyAndReadsSecretFreeAudit(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	oldSSO, oldSecret, oldAuto := config.Get().SSOEnabled, config.Get().SSOSharedSecret, config.Get().SSOAutoProvision
	t.Cleanup(func() {
		config.Update(func(s *config.Settings) {
			s.SSOEnabled, s.SSOSharedSecret, s.SSOAutoProvision = oldSSO, oldSecret, oldAuto
		})
	})
	config.Update(func(s *config.Settings) {
		s.SSOEnabled, s.SSOSharedSecret, s.SSOAutoProvision = true, "proxy-secret", true
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	owner, err := iam.EnsurePrincipalBySubject("human", "authentik:key-owner", "", "Key Owner")
	if err != nil {
		t.Fatal(err)
	}
	other, err := iam.EnsurePrincipalBySubject("human", "authentik:key-other", "", "Key Other")
	if err != nil {
		t.Fatal(err)
	}
	project, err := iam.CreateProject("key-owner", "Key Owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := iam.SetMembership(project.ID, owner.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	if err := iam.SetMembership(project.ID, other.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	issued, err := iam.IssueKey(iam.KeyCreate{ProjectID: project.ID, PrincipalID: owner.ID, Name: "laptop"})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(NewServer())
	defer server.Close()

	status, updated := ssoConnectionRequest(t, server.URL, "key-owner", http.MethodPost, "/user/api/keys/"+issued.ID+"/update", map[string]any{"daily_requests": 3})
	if status != http.StatusOK || updated["ok"] != true {
		t.Fatalf("update status=%d payload=%+v", status, updated)
	}
	key, found, err := iam.APIKeyByID(issued.ID)
	if err != nil || !found || key.Policy.DailyRequests != 3 {
		t.Fatalf("updated key=%+v found=%v err=%v", key, found, err)
	}
	status, _ = ssoConnectionRequest(t, server.URL, "key-other", http.MethodPost, "/user/api/keys/"+issued.ID+"/update", map[string]any{"daily_requests": 4})
	if status != http.StatusNotFound {
		t.Fatalf("other owner update status=%d", status)
	}
	status, audit := ssoConnectionRequest(t, server.URL, "key-owner", http.MethodGet, "/user/api/audit", nil)
	if status != http.StatusOK {
		t.Fatalf("audit status=%d payload=%+v", status, audit)
	}
	raw, _ := json.Marshal(audit)
	if strings.Contains(string(raw), issued.Token) {
		t.Fatalf("audit leaked one-time key token: %s", raw)
	}
	events := audit["events"].([]any)
	if len(events) == 0 || events[0].(map[string]any)["action"] != "api_key.update" {
		t.Fatalf("audit events=%+v", events)
	}
}

func TestOwnerCannotChangeRevokedKeyStatusThroughPortal(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	oldSSO, oldSecret, oldAuto := config.Get().SSOEnabled, config.Get().SSOSharedSecret, config.Get().SSOAutoProvision
	t.Cleanup(func() {
		config.Update(func(s *config.Settings) {
			s.SSOEnabled, s.SSOSharedSecret, s.SSOAutoProvision = oldSSO, oldSecret, oldAuto
		})
	})
	config.Update(func(s *config.Settings) {
		s.SSOEnabled, s.SSOSharedSecret, s.SSOAutoProvision = true, "proxy-secret", true
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	owner, err := iam.EnsurePrincipalBySubject("human", "authentik:revoked-owner", "", "Revoked Owner")
	if err != nil {
		t.Fatal(err)
	}
	project, err := iam.CreateProject("revoked-owner", "Revoked Owner")
	if err != nil {
		t.Fatal(err)
	}
	if err := iam.SetMembership(project.ID, owner.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	issued, err := iam.IssueKey(iam.KeyCreate{ProjectID: project.ID, PrincipalID: owner.ID, Name: "laptop"})
	if err != nil {
		t.Fatal(err)
	}
	if err := iam.RevokeAPIKey(issued.ID); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(NewServer())
	defer server.Close()
	status, response := ssoConnectionRequest(t, server.URL, "revoked-owner", http.MethodPost, "/user/api/keys/"+issued.ID+"/update", map[string]any{"disabled": false})
	if status != http.StatusBadRequest || response["error"] == nil {
		t.Fatalf("revoked portal update status=%d response=%+v", status, response)
	}
	key, found, err := iam.APIKeyByID(issued.ID)
	if err != nil || !found || key.Status != "revoked" {
		t.Fatalf("key=%+v found=%v err=%v", key, found, err)
	}
}
