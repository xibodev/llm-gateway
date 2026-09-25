package router

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	"llmgw/internal/iam"
	"llmgw/internal/providers"
)

func TestGovernanceHidesWhatTheKeyMayNotAddress(t *testing.T) {
	setupEcho(t)
	background := context.Background()
	routes := WithGovernance(background, Governance{AllowedRoutes: []string{"smart"}})
	routesOnly := WithGovernance(background, Governance{RoutesOnly: true})
	for _, c := range []struct {
		name   string
		ctx    context.Context
		model  string
		hidden bool
	}{
		{"allowed route", routes, "SMART", false},
		{"route outside the allowlist", routes, "failover", true},
		{"direct model beside a route allowlist", routes, "echo/echo-default", false},
		{"route of a routes-only key without routes", routesOnly, "smart", true},
		{"direct model of a routes-only key", routesOnly, "echo/echo-default", true},
		{"ungoverned route", background, "failover", false},
		{"ungoverned direct model", background, "echo/echo-default", false},
	} {
		_, err := ResolveForPrincipal(c.ctx, c.model, anonymous)
		var missing *ModelNotFoundError
		if hidden := errors.As(err, &missing); hidden != c.hidden || (!c.hidden && err != nil) {
			t.Errorf("%s: err=%v, want hidden=%v", c.name, err, c.hidden)
		}
	}
	if candidates, err := NativeAliasCandidates(routesOnly, anonymous); err != nil || len(candidates) != 0 {
		t.Fatalf("routes-only native aliases=%v err=%v", candidates, err)
	}
	if candidates, err := NativeAliasCandidates(background, anonymous); err != nil || len(candidates) == 0 {
		t.Fatalf("ungoverned native aliases=%v err=%v", candidates, err)
	}
}

func TestGovernanceFiltersNativeAliasesByKeyAndProject(t *testing.T) {
	setupEcho(t)
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	project, err := iam.CreateProject("alias-project", "Alias project")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := iam.SetProjectPolicy(project.ID, iam.KeyPolicy{AllowedProviders: []string{"echo2"}}); err != nil {
		t.Fatal(err)
	}
	inProject := anonymous
	inProject.ProjectID = project.ID
	background := context.Background()
	check := func(name string, ctx context.Context, want map[string]bool) {
		t.Helper()
		candidates, err := NativeAliasCandidates(ctx, inProject)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got := map[string]bool{}
		for _, targets := range candidates {
			for _, target := range targets {
				got[target.Provider+"/"+target.Model] = true
			}
		}
		for target := range want {
			if !got[target] {
				t.Errorf("%s: missing candidate %s in %v", name, target, got)
			}
		}
		for target := range got {
			if !want[target] {
				t.Errorf("%s: unexpected candidate %s", name, target)
			}
		}
	}
	all := map[string]bool{}
	for _, provider := range []string{"echo", "echo2"} {
		for _, model := range providers.CatalogModels(provider) {
			all[provider+"/"+model.ID] = true
		}
	}
	check("ungoverned ignores the project policy", background, all)
	projectOnly := map[string]bool{}
	for target := range all {
		if strings.HasPrefix(target, "echo2/") {
			projectOnly[target] = true
		}
	}
	check("governed applies the project provider allowlist", WithGovernance(background, Governance{}), projectOnly)
	check("key model allowlist narrows further",
		WithGovernance(background, Governance{AllowedModels: []string{"echo2/echo-deep"}}),
		map[string]bool{"echo2/echo-deep": true})
	check("key and project provider allowlists intersect",
		WithGovernance(background, Governance{AllowedProviders: []string{"echo"}}), map[string]bool{})
}

func TestGovernanceAttributesTelemetry(t *testing.T) {
	setupEcho(t)
	targets, err := ResolveTargets("failover")
	if err != nil {
		t.Fatal(err)
	}
	messages := []providers.Message{{"role": "user", "content": "hi"}}
	attributed := WithGovernance(context.Background(), Governance{Project: "fixture-project", Key: "fixture-key"})
	for _, ctx := range []context.Context{attributed, context.Background()} {
		if _, _, err := ExecuteCompleteContext(ctx, targets, messages, "failover", anonymous, nil); err != nil {
			t.Fatal(err)
		}
	}
	db, err := telConn()
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`SELECT project, key_name FROM failover_events ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var project, key sql.NullString
		if err := rows.Scan(&project, &key); err != nil {
			t.Fatal(err)
		}
		got = append(got, project.String+"|"+key.String)
	}
	if len(got) != 2 || got[0] != "fixture-project|fixture-key" || got[1] != "|" {
		t.Fatalf("telemetry attribution=%q, want the governed names then none", got)
	}
}
