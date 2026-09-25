package api

import (
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
)

// legacyScope is the provider and catalog cache key the gateway derived from a
// Principal before providers took a core.Caller.
func legacyScope(providerID string, p *config.Principal) string {
	if p == nil || p.PrincipalID == "" {
		return providerID
	}
	key := providerID + "@" + p.PrincipalID
	if p.PrincipalKind == "service" && p.ProjectID != "" {
		key += "#" + p.ProjectID
	}
	return key
}

// scopeProducers builds each Principal the way its producer does, so the
// Caller comes from the same path requests take.
func scopeProducers() []struct {
	name      string
	principal *config.Principal
} {
	key := func(id, kind, project string) *config.Principal {
		return withCaller(sourcePrincipal, &config.Principal{
			PrincipalID: id, PrincipalKind: kind, ProjectID: project, Key: "fixture", Token: "fixture-token",
		})
	}
	external := func(project string) *config.Principal {
		return withCaller(sourceExternalKey, &config.Principal{ProjectID: project, Key: "ci", Token: "fixture-external"})
	}
	return []struct {
		name      string
		principal *config.Principal
	}{
		{"IAM key of a human", key("prn_human", "human", "prj_one")},
		{"IAM key of the same human elsewhere", key("prn_human", "human", "prj_two")},
		{"IAM key of a service", key("prn_service", "service", "prj_one")},
		{"IAM key of the same service elsewhere", key("prn_service", "service", "prj_two")},
		{"service without a project", key("prn_service", "service", "")},
		{"IAM key of a system principal", key("prn_system", "system", "prj_one")},
		{"IAM key of the same system principal elsewhere", key("prn_system", "system", "prj_two")},
		{"external key", external("prj_one")},
		{"external key of another project", external("prj_two")},
		{"static admin key", withCaller(sourceAdminKey, &config.Principal{Project: "admin", Key: "admin"})},
		{"unauthenticated local", withCaller(sourceLocal, &config.Principal{Project: "local", Key: "local"})},
		{"console human", &config.Principal{PrincipalID: "prn_console", PrincipalKind: "human"}},
		{"playground human", &config.Principal{PrincipalID: "prn_console", PrincipalKind: "human", ProjectID: "prj_one", Key: "playground"}},
		{"principal-less usage", &config.Principal{}},
		{"gateway-internal", nil},
	}
}

// TestCallerKeepsEveryProducersScope proves the Caller partitions producers
// exactly as the Principal did: two producers share a provider instance, and
// see each other's catalog refresh, only when their legacy keys match.
func TestCallerKeepsEveryProducersScope(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	old := *config.Get()
	t.Cleanup(func() {
		config.Update(func(s *config.Settings) { *s = old })
		providers.ForgetCatalog("scope")
		providers.ResetProviders()
		iam.ResetForTests()
	})
	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{"scope": {Type: "echo"}}
		// A circuit wraps the instance in a pointer, so identity is observable.
		s.Policies.Overrides = map[string]config.ProviderPolicy{"scope": {CircuitFailureThreshold: 5, CircuitCooldownSeconds: 30}}
	})
	providers.ResetProviders()
	producers := scopeProducers()
	instances := make([]providers.Provider, len(producers))
	for i, producer := range producers {
		instance, err := providers.GetProviderForPrincipal("scope", callerOf(producer.principal))
		if err != nil {
			t.Fatalf("%s: %v", producer.name, err)
		}
		instances[i] = instance
	}
	for i, a := range producers {
		for j, b := range producers {
			want := legacyScope("scope", a.principal) == legacyScope("scope", b.principal)
			if shared := instances[i] == instances[j]; shared != want {
				t.Errorf("%s / %s: shared instance=%v, want %v", a.name, b.name, shared, want)
			}
		}
	}
	for _, a := range producers {
		providers.ForgetCatalog("scope")
		if rows := providers.RefreshCatalogForPrincipal("scope", callerOf(a.principal)); len(rows) == 0 {
			t.Fatalf("%s: refresh returned no models", a.name)
		}
		for _, b := range producers {
			want := legacyScope("scope", a.principal) == legacyScope("scope", b.principal)
			if _, refreshed := providers.CatalogCachedForPrincipal("scope", callerOf(b.principal)); refreshed.IsZero() == want {
				t.Errorf("%s refresh visible to %s=%v, want %v", a.name, b.name, !refreshed.IsZero(), want)
			}
		}
	}
}
