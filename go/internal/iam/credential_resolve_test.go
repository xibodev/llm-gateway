package iam

import (
	"testing"

	"llmgw/internal/config"

	core "github.com/xibodev/llmgw-core"
)

const (
	conn  = ConnectionPrecedence
	oauth = OAuthPrecedence
)

func TestResolveMatchesTheGatewayPrecedence(t *testing.T) {
	f := newResolveFixture(t)
	bound := ProviderCredentialKeyPrefix + f.shared.ID
	f.check(t, []resolveCase{
		{"own default connection", f.aliceCaller, "copilot", conn, f.aliceWork.ID},
		{"own API key", f.carolCaller, "fixture-openai", conn, f.carolKey.ID},
		{"system connection for anonymous", anonymousCaller, "fixture-openai", conn, f.systemKey.ID},
		{"system connection for local", localCaller, "fixture-openai", conn, f.systemKey.ID},
		{"system connection for a service", f.workerCaller, "fixture-openai", conn, f.systemKey.ID},
		{"configured key", anonymousCaller, "fixture-configured", conn, ConfiguredCredentialKeyPrefix + "fixture-configured"},
		{"configured key under a principal", f.aliceCaller, "fixture-configured", conn, ConfiguredCredentialKeyPrefix + "fixture-configured"},
		{"nothing for anonymous", anonymousCaller, "copilot", conn, ""},
		{"legacy row is not a connection", f.bobCaller, "copilot", conn, ""},
		{"binding is not a connection", f.workerCaller, "copilot", conn, ""},
		{"OAuth own default", f.aliceCaller, "copilot", oauth, f.aliceWork.ID},
		{"OAuth legacy row", f.bobCaller, "copilot", oauth, ProviderCredentialKeyPrefix + f.bobLegacy.ID},
		{"OAuth project binding", f.workerCaller, "copilot", oauth, bound},
		{"OAuth binding of another project", f.outsiderCaller, "copilot", oauth, ""},
		{"OAuth binding without membership", f.strayWorker, "copilot", oauth, ""},
		{"OAuth anonymous", anonymousCaller, "copilot", oauth, ""},
		{"OAuth local", localCaller, "copilot", oauth, ""},
		{"OAuth rejects an API key", f.carolCaller, "fixture-openai", oauth, ""},
		{"OAuth service without project", callerWithoutProject(f), "copilot", oauth, ""},
		{"OAuth system connection is not shared", anonymousCaller, "fixture-openai", oauth, ""},
	})
}

func TestResolveFollowsDisabledAndRevokedCredentials(t *testing.T) {
	f := newResolveFixture(t)
	bound := ProviderCredentialKeyPrefix + f.shared.ID
	if err := SetProviderCredentialStatus(f.shared.ID, "disabled"); err != nil {
		t.Fatal(err)
	}
	f.check(t, []resolveCase{{"disabled shared credential", f.workerCaller, "copilot", oauth, ""}})
	if err := SetProviderCredentialStatus(f.shared.ID, "active"); err != nil {
		t.Fatal(err)
	}
	f.check(t, []resolveCase{{"re-enabled shared credential", f.workerCaller, "copilot", oauth, bound}})
	if err := SetProviderCredentialBindingStatus(f.project.ID, "copilot", "service", "revoked"); err != nil {
		t.Fatal(err)
	}
	f.check(t, []resolveCase{{"revoked binding", f.workerCaller, "copilot", oauth, ""}})
	if err := SetProjectStatus(f.project.ID, "disabled"); err != nil {
		t.Fatal(err)
	}
	if _, err := SetProviderCredentialBinding(f.otherProject.ID, "copilot", "service", f.shared.ID); err != nil {
		t.Fatal(err)
	}
	f.check(t, []resolveCase{{"binding of an active project", f.outsiderCaller, "copilot", oauth, bound}})

	if err := RevokeProviderConnection(f.alice.ID, f.aliceWork.ID); err != nil {
		t.Fatal(err)
	}
	f.check(t, []resolveCase{
		{"promoted connection", f.aliceCaller, "copilot", conn, f.alicePersonal.ID},
		{"OAuth promoted connection", f.aliceCaller, "copilot", oauth, f.alicePersonal.ID},
	})
	if err := RevokeProviderConnection(f.alice.ID, f.alicePersonal.ID); err != nil {
		t.Fatal(err)
	}
	f.check(t, []resolveCase{
		{"no connection left", f.aliceCaller, "copilot", conn, ""},
		{"OAuth legacy fallback", f.aliceCaller, "copilot", oauth, ProviderCredentialKeyPrefix + f.aliceLegacy.ID},
	})
	if err := RevokeProviderCredential(f.bob.ID, "copilot"); err != nil {
		t.Fatal(err)
	}
	f.check(t, []resolveCase{{"revoked legacy row", f.bobCaller, "copilot", oauth, ""}})

	if err := SetPrincipalStatus(f.carol.ID, "disabled"); err != nil {
		t.Fatal(err)
	}
	f.check(t, []resolveCase{{"disabled owner falls back", f.carolCaller, "fixture-openai", conn, f.systemKey.ID}})
	if err := RevokeSystemProviderConnection("fixture-openai"); err != nil {
		t.Fatal(err)
	}
	f.check(t, []resolveCase{{"revoked system connection", anonymousCaller, "fixture-openai", conn, ""}})
	setProviderConfig(t, "fixture-openai", &config.ProviderConfig{APIKey: "fixture-openai-configured"})
	f.check(t, []resolveCase{{"configured fallback", f.carolCaller, "fixture-openai", conn, ConfiguredCredentialKeyPrefix + "fixture-openai"}})
}

func TestResolveWithoutEncryptionUsesOnlyTheConfiguredKey(t *testing.T) {
	f := newResolveFixture(t)
	key := config.Get().CredentialEncryptionKey
	config.Update(func(s *config.Settings) { s.CredentialEncryptionKey = "" })
	t.Cleanup(func() { config.Update(func(s *config.Settings) { s.CredentialEncryptionKey = key }) })
	f.check(t, []resolveCase{
		{"no key ignores connections", f.carolCaller, "fixture-openai", conn, ""},
		{"no key keeps the configured key", f.aliceCaller, "fixture-configured", conn, ConfiguredCredentialKeyPrefix + "fixture-configured"},
	})
}

func callerWithoutProject(f *resolveFixture) core.Caller {
	caller := f.workerCaller
	caller.ProjectID = ""
	return caller
}
