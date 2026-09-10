package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
	"llmgw/internal/router"
)

func setupKeyScope(t *testing.T) (iam.Principal, iam.Project) {
	t.Helper()
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	old := *config.Get()
	oldCatalog := catalogModelsForPrincipal
	t.Cleanup(func() {
		config.Update(func(s *config.Settings) { *s = old })
		catalogModelsForPrincipal = oldCatalog
		providers.ResetProviders()
		iam.ResetForTests()
	})
	config.Update(func(s *config.Settings) {
		s.APIKey = "scope-admin"
		s.APIKeys = nil
		s.SSOEnabled, s.SSOAutoProvision = true, true
		s.SSOSharedSecret = "scope-proxy"
		s.CredentialEncryptionKey = ""
		s.Providers = map[string]*config.ProviderConfig{"one": {Type: "echo"}, "two": {Type: "echo"}, "three": {Type: "echo"}}
		s.Endpoints = map[string]*config.EndpointConfig{
			"echo-default": {Failover: []config.EndpointMember{{Provider: "one", Model: "echo-default"}, {Provider: "two", Model: "echo-default"}, {Provider: "three", Model: "echo-default"}}},
			"other":        {Failover: []config.EndpointMember{{Provider: "one", Model: "echo-default"}}},
		}
		s.AnthropicDiscoveryAliases, s.AnthropicDiscoveryAllModels = true, true
	})
	providers.ResetProviders()
	catalogModelsForPrincipal = func(string, *config.Principal) []providers.ModelInfo {
		return []providers.ModelInfo{{ID: "echo-default"}, {ID: "echo-new"}}
	}
	principal, err := iam.CreatePrincipal("human", "authentik:scope-owner", "", "Scope owner")
	if err != nil {
		t.Fatal(err)
	}
	project, err := iam.CreateProject("scope-project", "Scope project")
	if err != nil {
		t.Fatal(err)
	}
	if err := iam.SetMembership(project.ID, principal.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	return principal, project
}

func TestKeyScopeRoutingAndCatalog(t *testing.T) {
	owner, project := setupKeyScope(t)
	_, err := iam.SetProjectPolicy(project.ID, iam.KeyPolicy{AllowedModels: []string{"echo-default"}, AllowedProviders: []string{"two", "three"}})
	if err != nil {
		t.Fatal(err)
	}
	issued, err := iam.IssueKey(iam.KeyCreate{ProjectID: project.ID, PrincipalID: owner.ID, Policy: iam.KeyPolicy{AllowedRoutes: []string{"echo-default"}, RoutesOnly: true, AllowedProviders: []string{"one", "two"}}})
	if err != nil {
		t.Fatal(err)
	}
	p, found, err := iam.ResolveAPIKey(issued.Token)
	if err != nil || !found {
		t.Fatalf("resolve: %v", err)
	}
	for _, model := range []string{"one/echo-default", "two/echo-default", "claude-echo-default", "echo-default[1m]", "other"} {
		if _, err := router.ResolveForPrincipal(model, p); err == nil {
			t.Errorf("direct/other selector accepted: %s", model)
		}
	}
	for _, model := range []string{"echo-default", " ECHO-DEFAULT "} {
		resolution, err := router.ResolveForPrincipal(model, p)
		if err != nil {
			t.Fatal(err)
		}
		targets, status, msg := authorizeKeyPolicy(p, model, resolution.Category, resolution.Targets)
		if status != 0 || !reflect.DeepEqual(targets, []router.Target{{Provider: "two", Model: "echo-default"}}) {
			t.Fatalf("intersection: %v %d %s", targets, status, msg)
		}
	}
	list, err := buildModelList(p)
	if err != nil {
		t.Fatal(err)
	}
	rows := list["data"].([]any)
	if len(rows) != 1 {
		t.Fatalf("catalog exposed direct/other rows: %+v", rows)
	}
	row := rows[0].(map[string]any)
	if row["id"] != "echo-default" || row["owned_by"] != "endpoint" || len(row["failover"].([]any)) != 1 {
		t.Fatalf("route catalog: %+v", row)
	}
	config.Update(func(s *config.Settings) {
		s.Endpoints["echo-default"] = &config.EndpointConfig{Failover: []config.EndpointMember{{Provider: "two", Model: "echo-new"}}}
	})
	resolution, err := router.ResolveForPrincipal("echo-default", p)
	if err != nil {
		t.Fatal(err)
	}
	targets, status, _ := authorizeKeyPolicy(p, "echo-default", resolution.Category, resolution.Targets)
	if status != 0 || len(targets) != 1 || targets[0].Model != "echo-new" {
		t.Fatalf("route grant did not follow edit: %+v %d", targets, status)
	}
	for _, deny := range []iam.KeyPolicy{
		{AllowedModels: []string{"other"}},
		{AllowedProviders: []string{"three"}},
	} {
		if _, err := iam.SetProjectPolicy(project.ID, deny); err != nil {
			t.Fatal(err)
		}
		if _, status, _ := authorizeKeyPolicy(p, "echo-default", resolution.Category, resolution.Targets); status != 403 {
			t.Fatalf("project restriction bypassed: %+v", deny)
		}
		list, err := buildModelList(p)
		if err != nil || len(list["data"].([]any)) != 0 {
			t.Fatalf("project restriction catalog: %+v %v", list, err)
		}
	}
	if _, err := iam.SetProjectPolicy(project.ID, iam.KeyPolicy{}); err != nil {
		t.Fatal(err)
	}
	// The endpoint name is also a native model. Removing it must not turn the grant into direct access.
	config.Update(func(s *config.Settings) { delete(s.Endpoints, "echo-default") })
	if _, err := router.ResolveForPrincipal("echo-default", p); err == nil {
		t.Fatal("deleted route fell back to native model")
	}
	if _, status, _ := authorizeKeyPolicy(p, "echo-default", "", []router.Target{{Provider: "two", Model: "echo-default"}}); status != 403 {
		t.Fatal("direct collision bypassed policy")
	}
	p.AllowedRoutes = nil
	if _, err := router.ResolveForPrincipal("other", p); err == nil {
		t.Fatal("empty route-only scope was unrestricted")
	}
	list, err = buildModelList(p)
	if err != nil || len(list["data"].([]any)) != 0 {
		t.Fatalf("empty route-only catalog: %+v %v", list, err)
	}
	aliases, err := router.NativeAliasCandidates(p)
	if err != nil || len(aliases) != 0 {
		t.Fatalf("route-only aliases: %+v %v", aliases, err)
	}
}

func TestKeyScopeRoutesAndDirectModelsAreIndependent(t *testing.T) {
	_, _ = setupKeyScope(t)
	p := &config.Principal{Token: "scope-test", AllowedRoutes: []string{"echo-default"}}
	resolution, err := router.ResolveForPrincipal("one/echo-new", p)
	if err != nil {
		t.Fatal(err)
	}
	if _, status, _ := authorizeKeyPolicy(p, "one/echo-new", resolution.Category, resolution.Targets); status != 0 {
		t.Fatalf("non-route-only key lost direct access: %d", status)
	}
	if _, err := router.ResolveForPrincipal("other", p); err == nil {
		t.Fatal("route allowlist did not restrict routes")
	}
	for _, name := range []string{"one/echo-new", "ONE/ECHO-NEW"} {
		t.Run(name, func(t *testing.T) {
			config.Update(func(s *config.Settings) {
				s.Endpoints[name] = &config.EndpointConfig{Failover: []config.EndpointMember{{Provider: "one", Model: "echo-new"}}}
			})
			t.Cleanup(func() { config.Update(func(s *config.Settings) { delete(s.Endpoints, name) }) })
			if _, err := router.ResolveForPrincipal("one/echo-new", p); err == nil {
				t.Fatal("router accepted forbidden colliding endpoint")
			}
			list, err := buildModelList(p)
			if err != nil {
				t.Fatal(err)
			}
			for _, raw := range list["data"].([]any) {
				if id := raw.(map[string]any)["id"]; id == "one/echo-new" || id == name {
					t.Fatal("catalog exposed forbidden route through namespaced collision")
				}
			}
			allowed := &config.Principal{Token: "scope-test", AllowedRoutes: []string{name}}
			list, err = buildModelList(allowed)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, raw := range list["data"].([]any) {
				row := raw.(map[string]any)
				if row["id"] == name && row["owned_by"] == "endpoint" {
					found = true
				}
				if row["id"] == "one/echo-new" && row["owned_by"] != "endpoint" {
					t.Fatal("authorized route also exposed as direct model")
				}
			}
			if !found {
				t.Fatal("authorized canonical endpoint missing")
			}
		})
	}
}

func TestKeyScopeProjectPolicyRejectsKeyOnlyFields(t *testing.T) {
	_, project := setupKeyScope(t)
	existing := iam.KeyPolicy{AllowedModels: []string{"echo-default"}, AllowedProviders: []string{"two"}, RPM: 7}
	if _, err := iam.SetProjectPolicy(project.ID, existing); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(NewServer())
	defer server.Close()
	path := server.URL + "/admin/api/projects/" + project.ID + "/policy"
	for _, body := range []map[string]any{
		{"allowed_routes": []string{"echo-default"}},
		{"routes_only": true},
		{"admin_managed": true},
	} {
		status, response := jsonRequest(t, path, "POST", "scope-admin", body)
		if status != http.StatusBadRequest {
			t.Fatalf("key-only project fields accepted: %d %+v", status, response)
		}
		stored, err := iam.GetProjectPolicy(project.ID)
		if err != nil || !reflect.DeepEqual(stored.KeyPolicy, existing) {
			t.Fatalf("rejected update changed project policy: %+v %v", stored, err)
		}
	}
	// Neutral values are accepted by the shared DTO, but are never persisted as project scopes.
	status, response := jsonRequest(t, path, "POST", "scope-admin", map[string]any{"allowed_routes": []string{}, "routes_only": false, "admin_managed": false})
	if status != http.StatusOK {
		t.Fatalf("neutral key-only fields: %d %+v", status, response)
	}
	stored, err := iam.GetProjectPolicy(project.ID)
	if err != nil || !reflect.DeepEqual(stored.KeyPolicy, iam.KeyPolicy{}) {
		t.Fatalf("neutral project policy: %+v %v", stored, err)
	}
}

func TestKeyScopeAdminPortalContract(t *testing.T) {
	owner, project := setupKeyScope(t)
	server := httptest.NewServer(NewServer())
	defer server.Close()
	portal := func(method, path string, body any) (int, map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequest(method, server.URL+path, bytes.NewReader(raw))
		req.Header.Set("Origin", server.URL)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(ssoSecretHeader, "scope-proxy")
		req.Header.Set(ssoSubjectHeader, "scope-owner")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out := map[string]any{}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, out
	}
	for _, adminIssued := range []bool{false, true} {
		body := map[string]any{"project_id": project.ID, "principal_id": owner.ID, "allowed_routes": []string{"echo-default"}, "routes_only": true, "admin_managed": !adminIssued}
		var status int
		var response map[string]any
		if adminIssued {
			status, response = jsonRequest(t, server.URL+"/admin/api/keys", "POST", "scope-admin", body)
		} else {
			status, response = portal("POST", "/user/api/keys", body)
		}
		if status != 200 {
			t.Fatalf("create admin=%v: %d %+v", adminIssued, status, response)
		}
		key := response["key"].(map[string]any)
		id := key["id"].(string)
		stored, _, err := iam.APIKeyByID(id)
		if err != nil || stored.Policy.AdminManaged != adminIssued || !stored.Policy.RoutesOnly || !reflect.DeepEqual(stored.Policy.AllowedRoutes, []string{"echo-default"}) {
			t.Fatalf("create policy: %+v %v", stored.Policy, err)
		}
		path := "/user/api/keys/" + id
		if !adminIssued {
			status, response = portal("POST", path+"/update", map[string]any{"allowed_routes": []string{"other"}, "routes_only": true})
			if status != 200 {
				t.Fatalf("owner update: %d %+v", status, response)
			}
			status, response = jsonRequest(t, server.URL+"/admin/api/keys/update", "POST", "scope-admin", map[string]any{"id": id, "expires_at": 12345, "disabled": true, "allowed_routes": []string{"echo-default"}})
			if status != 200 {
				t.Fatalf("admin update: %d %+v", status, response)
			}
		}
		for _, patch := range []map[string]any{{"routes_only": false, "admin_managed": false}, {"allowed_routes": []string{"other"}}, {"allowed_providers": []string{}}, {"rpm": 0}, {"expires_at": 0}, {"disabled": false}} {
			status, response = portal("POST", path+"/update", patch)
			if status != 403 {
				t.Fatalf("locked portal patch %+v: %d %+v", patch, status, response)
			}
		}
		status, response = portal("DELETE", path, nil)
		if status != 200 {
			t.Fatalf("owner revoke: %d %+v", status, response)
		}
	}
	status, response := portal("POST", "/user/api/keys", map[string]any{"project_id": project.ID, "routes_only": true})
	if status != 400 {
		t.Fatalf("empty routes accepted: %d %+v", status, response)
	}
}
