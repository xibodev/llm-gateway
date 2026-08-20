package web

import (
	"strings"
	"testing"

	"llmgw/internal/providers"
)

// The bundled admin page is served straight out of the binary and has no
// runtime harness, so this is the only automated guard that it still talks to
// the routes and payload keys the server actually promises to keep. "endpoints"
// is canonical; "/admin/api/categories" and the "categories" state key are the
// deprecated aliases scheduled for removal, and a page that still depended on
// them would break itself the release they go.
func TestAdminPageUsesTheCanonicalEndpointSurface(t *testing.T) {
	for _, deprecated := range []string{
		"/admin/api/categories",
		"s.categories",
	} {
		if strings.Contains(AdminHTML, deprecated) {
			t.Errorf("admin.html still uses the deprecated %q", deprecated)
		}
	}
	for _, canonical := range []string{
		"'/admin/api/endpoints'",
		"'/admin/api/endpoints/'",
		"s.endpoints",
	} {
		if !strings.Contains(AdminHTML, canonical) {
			t.Errorf("admin.html does not use the canonical %q", canonical)
		}
	}
	// The one read of the alias that must remain: a single normalisation of the
	// state payload, so an older server still renders the page.
	if !strings.Contains(AdminHTML, "d.endpoints=d.categories") {
		t.Error("admin.html dropped the legacy read fallback for the state payload")
	}
}

// scriptFunctionBody returns the body of a one-name function defined in the
// page's inline script, so a guard can assert on what that function reads
// rather than on the whole document.
func scriptFunctionBody(t *testing.T, name string) string {
	t.Helper()
	start := strings.Index(AdminHTML, name+"(){")
	if start < 0 {
		t.Fatalf("admin.html defines no %s() — the drawer no longer derives that input", name)
	}
	rest := AdminHTML[start+len(name)+2:]
	depth := 0
	for i, r := range rest {
		switch r {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return rest[:i+1]
			}
		}
	}
	t.Fatalf("admin.html: %s() has no closing brace", name)
	return ""
}

// The drawer's inputs must come from the registry's onboarding_fields, which
// the state payload already carries, never from a list of provider types
// written into the page. The hard-coded lists that used to stand here named no
// azure_openai, so selecting that preset hid both required inputs and posted an
// empty base URL and key for the server to reject — the provider type this page
// had just gained could not be configured from it at all. A derived page stops
// that repeating for the next provider type, and this guard is what keeps it
// derived: the page is //go:embed-ed and served with no runtime harness, so
// nothing else would notice.
func TestAdminPageDerivesProviderInputsFromTheRegistry(t *testing.T) {
	for _, fn := range []string{
		"needsUrl", "needsKey", "needsRegion", "needsProject", "needsLocation",
	} {
		body := scriptFunctionBody(t, fn)
		if !strings.Contains(body, "this.onboardingFields()") {
			t.Errorf("%s() does not read the registry's onboarding fields: %s", fn, body)
		}
		for _, providerType := range providers.ProviderTypes {
			if strings.Contains(body, "'"+providerType+"'") {
				t.Errorf("%s() hard-codes the provider type %q instead of deriving it: %s",
					fn, providerType, body)
			}
		}
	}
	if !strings.Contains(scriptFunctionBody(t, "onboardingFields"), "onboarding_fields") {
		t.Error("onboardingFields() does not read the onboarding_fields the state payload carries")
	}
}

// Every onboarding field a curated integration declares must be a field the
// drawer can actually collect. A provider type added with a field this page
// never binds would repeat the azure_openai defect one level down: the
// derivation would be honest and the input still missing.
func TestAdminPageBindsEveryOnboardingFieldItCanCollect(t *testing.T) {
	// Onboarding steps this drawer deliberately does not perform: the OAuth
	// device flows are completed from Settings after the provider is added,
	// and a gateway client is configured in its own client, not here.
	elsewhere := map[string]bool{"human_owner": true, "client_id": true, "gateway_url": true}
	for _, entry := range providers.ProviderRegistry() {
		if entry.Availability != providers.ProviderAvailable {
			continue
		}
		for _, field := range entry.OnboardingFields {
			if elsewhere[field] {
				continue
			}
			if !strings.Contains(AdminHTML, "has('"+field+"')") {
				t.Errorf("%s declares onboarding field %q that no needs*() reads", entry.ID, field)
			}
			if !strings.Contains(AdminHTML, `x-model="form.`+field+`"`) {
				t.Errorf("%s declares onboarding field %q that the drawer has no input for",
					entry.ID, field)
			}
		}
	}
}
