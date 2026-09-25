package iam

import (
	"testing"
	"time"

	core "github.com/xibodev/llmgw-core"
)

func TestCallerPrincipalIDExcludesSharedOnlyCallers(t *testing.T) {
	for _, c := range []struct {
		name   string
		caller core.Caller
		want   string
	}{
		{"human", core.Caller{ID: "prn_human", Kind: core.CallerHuman}, "prn_human"},
		{"service in a project", core.Caller{ID: "prn_service", Kind: core.CallerService, ProjectID: "prj_one"}, "prn_service"},
		{"anonymous", core.Caller{Kind: core.CallerAnonymous}, ""},
		{"anonymous in a project", core.Caller{Kind: core.CallerAnonymous, ProjectID: "prj_one"}, ""},
		{"local", core.LocalCaller(), ""},
		{"static admin key", core.Caller{ID: AdminCallerID, Kind: core.CallerService}, ""},
		{"external key", core.Caller{ID: ExternalKeyCallerID("prj_one", "ci"), Kind: core.CallerService, ProjectID: "prj_one"}, ""},
		{"reserved gateway namespace", core.Caller{ID: "gateway:probe", Kind: core.CallerService}, ""},
		{"reserved ID as a human", core.Caller{ID: AdminCallerID, Kind: core.CallerHuman}, ""},
		{"kind core does not know", core.Caller{ID: "prn_system", Kind: "system"}, ""},
		{"zero caller", core.Caller{}, ""},
	} {
		if got := CallerPrincipalID(c.caller); got != c.want {
			t.Errorf("%s: CallerPrincipalID=%q, want %q", c.name, got, c.want)
		}
	}
}

// TestReservedCallersResolveOnlySharedCredentials gives principals the
// reserved IDs themselves, so the exclusion is proven rather than holding only
// because no such principal exists. Only humans may own connections, so the
// admin ID owns a personal connection and the external ID is a service member
// of a project that binds a shared Copilot credential.
func TestReservedCallersResolveOnlySharedCredentials(t *testing.T) {
	f := newResolveFixture(t)
	external := ExternalKeyCallerID(f.project.ID, "fixture-key")
	insertPrincipalWithID(t, AdminCallerID, "human")
	must(PutProviderConnection(ProviderConnectionCreate{
		PrincipalID: AdminCallerID, ProviderID: "fixture-openai", Name: "personal",
		Kind: "api_key", Secret: "fixture-reserved-key", Source: ConnectionSourceUser,
	}))
	insertPrincipalWithID(t, external, "service")
	if err := SetMembership(f.project.ID, external, "member"); err != nil {
		t.Fatal(err)
	}
	admin := core.Caller{ID: AdminCallerID, Kind: core.CallerService}
	externalKey := core.Caller{ID: external, Kind: core.CallerService, ProjectID: f.project.ID}
	f.check(t, []resolveCase{
		{"admin key skips the personal connection", admin, "fixture-openai", conn, f.systemKey.ID},
		{"external key gets the system connection", externalKey, "fixture-openai", conn, f.systemKey.ID},
		{"external key skips the project binding", externalKey, "copilot", oauth, ""},
	})
}

func insertPrincipalWithID(t *testing.T, id, kind string) {
	t.Helper()
	db, err := DB()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	if _, err := db.Exec(`
INSERT INTO principals(id,kind,display_name,status,created_at,updated_at)
VALUES(?,?,?,?,?,?)`, id, kind, "Reserved fixture", "active", now, now); err != nil {
		t.Fatal(err)
	}
}
