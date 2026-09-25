package api

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"

	core "github.com/xibodev/llmgw-core"
)

func TestRequestCallerMapsEveryProducer(t *testing.T) {
	key := func(id, kind string) *config.Principal {
		return &config.Principal{
			PrincipalID: id, PrincipalKind: kind, ProjectID: "prj_one", Project: "one",
			KeyID: "key_" + id, Key: "laptop", Token: "fixture-token",
		}
	}
	external := &config.Principal{ProjectID: "prj_one", Project: "one", Key: "ci", Token: "fixture-external"}
	for _, c := range []struct {
		name      string
		source    callerSource
		principal *config.Principal
		want      core.Caller
	}{
		{"IAM key of a human", sourcePrincipal, key("prn_h", "human"), core.Caller{ID: "prn_h", Kind: core.CallerHuman, ProjectID: "prj_one"}},
		{"IAM key of a service", sourcePrincipal, key("prn_s", "service"), core.Caller{ID: "prn_s", Kind: core.CallerService, ProjectID: "prj_one"}},
		{"IAM key of a system principal", sourcePrincipal, key("prn_y", "system"), core.Caller{ID: "prn_y", Kind: core.CallerService, ProjectID: "prj_one"}},
		{"external key", sourceExternalKey, external, core.Caller{ID: "external:prj_one/ci", Kind: core.CallerService, ProjectID: "prj_one"}},
		{"static admin key", sourceAdminKey, &config.Principal{Project: "admin", Key: "admin"}, core.Caller{ID: "gateway:admin", Kind: core.CallerService}},
		{"unauthenticated local", sourceLocal, &config.Principal{Project: "local", Key: "local"}, core.LocalCaller()},
		{"admin key named like local", sourceAdminKey, &config.Principal{Project: "local", Key: "local"}, core.Caller{ID: "gateway:admin", Kind: core.CallerService}},
		{"local named like admin", sourceLocal, &config.Principal{Project: "admin", Key: "admin"}, core.LocalCaller()},
		{"console human", sourcePrincipal, &config.Principal{PrincipalID: "prn_c", PrincipalKind: "human"}, core.Caller{ID: "prn_c", Kind: core.CallerHuman}},
		{"playground human in a project", sourcePrincipal, &config.Principal{PrincipalID: "prn_c", PrincipalKind: "human", ProjectID: "prj_one", Key: "playground"}, core.Caller{ID: "prn_c", Kind: core.CallerHuman, ProjectID: "prj_one"}},
		{"principal-less usage", sourcePrincipal, &config.Principal{}, core.Caller{Kind: core.CallerAnonymous}},
		{"gateway-internal", sourcePrincipal, nil, core.Caller{Kind: core.CallerAnonymous}},
	} {
		got := requestCaller(c.source, c.principal)
		if got != c.want {
			t.Errorf("%s: caller=%+v, want %+v", c.name, got, c.want)
		}
		if err := got.Validate(); err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
		// Only a Principal with a PrincipalID may reach private credentials.
		wantPrivate := ""
		if c.principal != nil {
			wantPrivate = c.principal.PrincipalID
		}
		if private := iam.CallerPrincipalID(got); private != wantPrivate {
			t.Errorf("%s: private principal=%q, want %q", c.name, private, wantPrivate)
		}
		if c.source == sourcePrincipal && callerOf(c.principal) != c.want {
			t.Errorf("%s: callerOf=%+v, want the mapped caller", c.name, callerOf(c.principal))
		}
	}
}

func TestRequireAPIKeyRecordsTheProducerCaller(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	old := *config.Get()
	t.Cleanup(func() {
		config.Update(func(s *config.Settings) { *s = old })
		iam.ResetForTests()
	})
	config.Update(func(s *config.Settings) {
		s.APIKey, s.APIKeys, s.AllowUnauthenticatedAPI = "fixture-admin", nil, false
	})
	project, err := iam.CreateProject("tools", "Tools")
	if err != nil {
		t.Fatal(err)
	}
	issue := func(kind string) (string, string) {
		principal, err := iam.CreatePrincipal(kind, "fixture:"+kind, "", kind)
		if err != nil {
			t.Fatal(err)
		}
		if err := iam.SetMembership(project.ID, principal.ID, "member"); err != nil {
			t.Fatal(err)
		}
		issued, err := iam.IssueKey(iam.KeyCreate{ProjectID: project.ID, PrincipalID: principal.ID, Name: kind})
		if err != nil {
			t.Fatal(err)
		}
		return issued.Token, principal.ID
	}
	humanToken, humanID := issue("human")
	serviceToken, serviceID := issue("service")
	keys := filepath.Join(t.TempDir(), "keys.json")
	if err := os.WriteFile(keys, []byte(`{"version":1,"keys":[{"name":"build","project":"tools","key":"fixture-external"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LLMGW_EXTERNAL_KEYS_FILE", keys)
	stop, err := iam.StartExternalKeysFromEnv(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)

	for _, c := range []struct {
		name, token string
		want        core.Caller
	}{
		{"static admin key", "fixture-admin", core.Caller{ID: iam.AdminCallerID, Kind: core.CallerService}},
		{"IAM key of a human", humanToken, core.Caller{ID: humanID, Kind: core.CallerHuman, ProjectID: project.ID}},
		{"IAM key of a service", serviceToken, core.Caller{ID: serviceID, Kind: core.CallerService, ProjectID: project.ID}},
		{"external key", "fixture-external", core.Caller{ID: iam.ExternalKeyCallerID(project.ID, "build"), Kind: core.CallerService, ProjectID: project.ID}},
	} {
		request := httptest.NewRequest("GET", "/v1/models", nil)
		request.Header.Set("Authorization", "Bearer "+c.token)
		principal, status, message := requireAPIKey(request)
		if status != 0 || principal.Caller != c.want || callerOf(principal) != c.want {
			t.Errorf("%s: status=%d %s caller=%+v, want %+v", c.name, status, message, principal, c.want)
		}
	}
	config.Update(func(s *config.Settings) { s.AllowUnauthenticatedAPI = true })
	principal, status, _ := requireAPIKey(httptest.NewRequest("GET", "/v1/models", nil))
	if status != 0 || callerOf(principal) != core.LocalCaller() {
		t.Fatalf("local mode: status=%d principal=%+v", status, principal)
	}
}
