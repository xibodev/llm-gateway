package providers

import (
	"testing"

	"llmgw/internal/iam"

	core "github.com/xibodev/llmgw-core"
)

// TestCallerScopesKeepTheLegacyKeyStrings pins the provider and catalog cache
// keys of each producer's Caller to the strings its Principal produced: a
// human or system principal alone, a service in its project, and the shared
// key for everyone without a PrincipalID.
func TestCallerScopesKeepTheLegacyKeyStrings(t *testing.T) {
	system := iam.SystemPrincipalCallerID("prn_system")
	for _, c := range []struct {
		name   string
		caller core.Caller
		want   string
	}{
		{"human", core.Caller{ID: "prn_human", Kind: core.CallerHuman, ProjectID: "prj_one"}, "scope@prn_human"},
		{"service in a project", core.Caller{ID: "prn_service", Kind: core.CallerService, ProjectID: "prj_one"}, "scope@prn_service#prj_one"},
		{"service without a project", core.Caller{ID: "prn_service", Kind: core.CallerService}, "scope@prn_service"},
		{"system principal in a project", core.Caller{ID: system, Kind: core.CallerService, ProjectID: "prj_one"}, "scope@prn_system"},
		{"external key", core.Caller{ID: iam.ExternalKeyCallerID("prj_one", "ci"), Kind: core.CallerService, ProjectID: "prj_one"}, "scope"},
		{"static admin key", core.Caller{ID: iam.AdminCallerID, Kind: core.CallerService}, "scope"},
		{"local", core.LocalCaller(), "scope"},
		{"gateway-internal", gatewayCaller(), "scope"},
	} {
		if got := providerCacheKey("scope", c.caller); got != c.want {
			t.Errorf("%s: provider key=%q, want %q", c.name, got, c.want)
		}
		if got := catalogCacheKey("scope", c.caller); got != c.want {
			t.Errorf("%s: catalog key=%q, want %q", c.name, got, c.want)
		}
	}
}
