package iam

import (
	"fmt"
	"testing"

	"llmgw/internal/config"

	core "github.com/xibodev/llmgw-core"
)

type resolveFixture struct {
	stores                      map[CredentialPrecedence]*CredentialStore
	alice, bob, carol           Principal
	worker, outsider            Principal
	project, otherProject       Project
	aliceWork, alicePersonal    ProviderConnection
	carolKey, systemKey         ProviderConnection
	aliceLegacy, bobLegacy      ProviderCredentialInfo
	shared                      ProviderCredentialInfo
	aliceCaller, bobCaller      core.Caller
	carolCaller, workerCaller   core.Caller
	outsiderCaller, strayWorker core.Caller
}

// must keeps fixture setup readable; a failing fixture is a broken test.
func must[T any](value T, err error) T {
	if err != nil {
		panic(fmt.Sprintf("fixture setup: %v", err))
	}
	return value
}

func newResolveFixture(t *testing.T) *resolveFixture {
	t.Helper()
	path := credentialStatePath(t)
	f := &resolveFixture{stores: map[CredentialPrecedence]*CredentialStore{}}
	for _, precedence := range []CredentialPrecedence{ConnectionPrecedence, OAuthPrecedence} {
		f.stores[precedence] = openCredentialStore(t, path, CredentialStoreOptions{
			Precedence: func(string) CredentialPrecedence { return precedence },
		})
	}
	f.alice = must(CreatePrincipal("human", "fixture:alice", "", "Alice"))
	f.bob = must(CreatePrincipal("human", "fixture:bob", "", "Bob"))
	f.carol = must(CreatePrincipal("human", "fixture:carol", "", "Carol"))
	f.worker = must(CreatePrincipal("service", "fixture:worker", "", "Worker"))
	f.outsider = must(CreatePrincipal("service", "fixture:outsider", "", "Outsider"))
	f.project = must(CreateProject("fixture-project", "Fixture"))
	f.otherProject = must(CreateProject("fixture-other", "Other"))
	if err := SetMembership(f.project.ID, f.worker.ID, "member"); err != nil {
		t.Fatal(err)
	}
	if err := SetMembership(f.otherProject.ID, f.outsider.ID, "member"); err != nil {
		t.Fatal(err)
	}
	oauth := func(owner, name, token string) ProviderConnection {
		return must(PutOAuthProviderConnection(OAuthConnectionCreate{
			PrincipalID: owner, ProviderID: "copilot", Name: name, Kind: "github_oauth",
			Source: ConnectionSourceUser, MakeDefault: true, AccessToken: token,
		}))
	}
	f.alicePersonal = oauth(f.alice.ID, "personal", "fixture-alice-personal")
	f.aliceWork = oauth(f.alice.ID, "work", "fixture-alice-work")
	f.aliceLegacy = must(PutProviderCredential(f.alice.ID, "copilot", "github_oauth", "fixture-alice-legacy"))
	f.bobLegacy = must(PutProviderCredential(f.bob.ID, "copilot", "github_oauth", "fixture-bob-legacy"))
	f.carolKey = must(PutProviderConnection(ProviderConnectionCreate{
		PrincipalID: f.carol.ID, ProviderID: "fixture-openai", Name: "personal",
		Kind: "api_key", Secret: "fixture-carol-key", Source: ConnectionSourceUser,
	}))
	if _, err := PutSystemProviderConnection("fixture-openai", "api_key", "fixture-system-key"); err != nil {
		t.Fatal(err)
	}
	system := must(EnsureSystemPrincipal())
	f.systemKey, _, _ = ActiveProviderConnection(system.ID, "fixture-openai")
	setProviderConfig(t, "fixture-configured", &config.ProviderConfig{APIKey: "fixture-configured-key"})
	f.shared = must(PutGatewayProviderCredential("copilot", "github_oauth", "fixture-shared-token"))
	if _, err := SetProviderCredentialBinding(f.project.ID, "copilot", "service", f.shared.ID); err != nil {
		t.Fatal(err)
	}
	f.aliceCaller = core.Caller{ID: f.alice.ID, Kind: core.CallerHuman}
	f.bobCaller = core.Caller{ID: f.bob.ID, Kind: core.CallerHuman}
	f.carolCaller = core.Caller{ID: f.carol.ID, Kind: core.CallerHuman, ProjectID: f.project.ID}
	f.workerCaller = core.Caller{ID: f.worker.ID, Kind: core.CallerService, ProjectID: f.project.ID}
	f.outsiderCaller = core.Caller{ID: f.outsider.ID, Kind: core.CallerService, ProjectID: f.otherProject.ID}
	f.strayWorker = core.Caller{ID: f.outsider.ID, Kind: core.CallerService, ProjectID: f.project.ID}
	return f
}

func (f *resolveFixture) check(t *testing.T, cases []resolveCase) {
	t.Helper()
	for _, c := range cases {
		assertResolvesLikeGateway(t, f.stores[c.precedence], c)
	}
}

var (
	anonymousCaller = core.Caller{Kind: core.CallerAnonymous}
	localCaller     = core.LocalCaller()
)
