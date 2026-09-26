package iam

import (
	"testing"
	"time"

	"llmgw/internal/config"

	core "github.com/xibodev/llmgw-core"
)

func TestCallerPrincipalIDExcludesSharedOnlyCallers(t *testing.T) {
	system := SystemPrincipalCallerID("prn_system")
	for _, c := range []struct {
		name     string
		caller   core.Caller
		id, kind string
	}{
		{"human", core.Caller{ID: "prn_human", Kind: core.CallerHuman}, "prn_human", "human"},
		{"service in a project", core.Caller{ID: "prn_service", Kind: core.CallerService, ProjectID: "prj_one"}, "prn_service", "service"},
		{"system principal", core.Caller{ID: system, Kind: core.CallerService, ProjectID: "prj_one"}, "prn_system", "system"},
		{"system ID as a human", core.Caller{ID: system, Kind: core.CallerHuman}, "", ""},
		{"system ID without a principal", core.Caller{ID: SystemPrincipalCallerID(""), Kind: core.CallerService}, "", ""},
		{"anonymous", core.Caller{Kind: core.CallerAnonymous}, "", ""},
		{"anonymous in a project", core.Caller{Kind: core.CallerAnonymous, ProjectID: "prj_one"}, "", ""},
		{"local", core.LocalCaller(), "", ""},
		{"static admin key", core.Caller{ID: AdminCallerID, Kind: core.CallerService}, "", ""},
		{"external key", core.Caller{ID: ExternalKeyCallerID("prj_one", "ci"), Kind: core.CallerService, ProjectID: "prj_one"}, "", ""},
		{"reserved gateway namespace", core.Caller{ID: "gateway:probe", Kind: core.CallerService}, "", ""},
		{"reserved ID as a human", core.Caller{ID: AdminCallerID, Kind: core.CallerHuman}, "", ""},
		{"kind core does not know", core.Caller{ID: "prn_system", Kind: "system"}, "", ""},
		{"zero caller", core.Caller{}, "", ""},
	} {
		if id, kind := CallerPrincipalID(c.caller); id != c.id || kind != c.kind {
			t.Errorf("%s: CallerPrincipalID=%q/%q, want %q/%q", c.name, id, kind, c.id, c.kind)
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

// TestSystemCallerResolvesAsTheSystemPrincipal compares a system-owned key's
// caller with the Principal iam.ResolveAPIKey builds for that key, which is
// what providers resolved with before they took a Caller. The project binds
// one shared Copilot credential for services and another for system
// principals, and a second system principal owns its own connection, so a
// caller that lost the principal or its kind would resolve differently.
func TestSystemCallerResolvesAsTheSystemPrincipal(t *testing.T) {
	f := newResolveFixture(t)
	system := must(EnsureSystemPrincipal())
	ops := must(CreatePrincipal("system", "fixture:ops", "", "Ops"))
	opsKey := must(PutProviderConnection(ProviderConnectionCreate{
		PrincipalID: ops.ID, ProviderID: "fixture-openai", Name: "ops",
		Kind: "api_key", Secret: "fixture-ops-key", Source: ConnectionSourceAdmin,
	}))
	systemShared := must(PutGatewayProviderCredential("copilot", "github_oauth", "fixture-system-token"))
	// The writer issues only service bindings, but the schema and the resolver
	// honour a system row, so the fixture writes one directly.
	db, err := DB()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
INSERT INTO provider_credential_bindings(project_id,provider_id,principal_kind,credential_id,status,created_at,updated_at)
VALUES(?,?,?,?,'active',?,?)`, f.project.ID, "copilot", "system", systemShared.ID, time.Now().Unix(), time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	bound := ProviderCredentialKeyPrefix + systemShared.ID
	for _, owner := range []struct {
		principal  Principal
		connection string
	}{{system, f.systemKey.ID}, {ops, opsKey.ID}} {
		name := owner.principal.DisplayName
		if err := SetMembership(f.project.ID, owner.principal.ID, "member"); err != nil {
			t.Fatal(err)
		}
		caller := core.Caller{ID: SystemPrincipalCallerID(owner.principal.ID), Kind: core.CallerService, ProjectID: f.project.ID}
		legacy := &config.Principal{PrincipalID: owner.principal.ID, PrincipalKind: "system", ProjectID: f.project.ID}
		assertResolvesLike(t, f.stores[conn], resolveCase{name + " connection", caller, "fixture-openai", conn, owner.connection}, legacy)
		assertResolvesLike(t, f.stores[oauth], resolveCase{name + " Copilot binding", caller, "copilot", oauth, bound}, legacy)
		// Providers authorize Copilot through the caller resolver itself.
		secret, _, found, err := ResolveCallerOAuthCredentialSecretWithObservation(caller, "copilot")
		wantSecret, _, wantFound, wantErr := ResolveProviderOAuthCredentialSecretWithObservation(legacy, "copilot")
		if err != nil || wantErr != nil || !found || !wantFound || secret != wantSecret {
			t.Fatalf("%s: Copilot credential found=%v err=%v differs from the system principal's (found=%v err=%v)",
				name, found, err, wantFound, wantErr)
		}
	}
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
