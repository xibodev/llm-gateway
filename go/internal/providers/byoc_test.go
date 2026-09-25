package providers

import (
	"encoding/base64"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"

	core "github.com/xibodev/llmgw-core"
)

func TestCopilotProviderCacheIsPrincipalScoped(t *testing.T) {
	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{
			"copilot": {Type: "github_copilot"},
		}

		s.Policies.Defaults = config.ProviderPolicy{}
		s.Policies.Overrides = map[string]config.ProviderPolicy{}
	})
	ResetProviders()
	t.Cleanup(ResetProviders)

	first, err := GetProviderForPrincipal(
		"copilot", core.Caller{ID: "prn_one", Kind: core.CallerHuman},
	)
	if err != nil {
		t.Fatal(err)
	}
	second, err := GetProviderForPrincipal(
		"copilot", core.Caller{ID: "prn_two", Kind: core.CallerHuman},
	)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("different principals reused one Copilot provider instance")
	}
	assertCopilotPrincipal := func(provider Provider, want string) {
		t.Helper()
		openai, ok := provider.(OpenAIProvider)
		if !ok {
			t.Fatalf("provider type=%T", provider)
		}
		auth, ok := openai.auth.(copilotAuth)
		if !ok || auth.caller.ID != want {
			t.Fatalf("copilot auth=%#v, want principal %q", openai.auth, want)
		}
	}
	assertCopilotPrincipal(first, "prn_one")
	assertCopilotPrincipal(second, "prn_two")
}

func TestCopilotUsesProviderTimeoutOverride(t *testing.T) {
	timeout := 7.5
	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{
			"copilot": {Type: "github_copilot", Timeout: &timeout},
		}
		s.Policies.Defaults = config.ProviderPolicy{}
		s.Policies.Overrides = map[string]config.ProviderPolicy{}
	})
	ResetProviders()
	t.Cleanup(ResetProviders)

	provider, err := GetProviderForPrincipal("copilot", core.Caller{ID: "prn_timeout", Kind: core.CallerHuman})
	if err != nil {
		t.Fatal(err)
	}
	openAI, ok := provider.(OpenAIProvider)
	if !ok || openAI.Timeout != timeout {
		t.Fatalf("provider=%T timeout=%v, want %v", provider, openAI.Timeout, timeout)
	}
}

func TestBedrockCarriesPersonalCredentialObservation(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	ResetProviders()
	t.Cleanup(func() {
		iam.ResetForTests()
		ResetProviders()
	})
	key := make([]byte, 32)
	for index := range key {
		key[index] = byte(index + 1)
	}
	config.Update(func(s *config.Settings) {
		s.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(key)
		s.Providers = map[string]*config.ProviderConfig{
			"bedrock": {Type: "bedrock", Region: "us-east-1"},
		}
		s.Policies.Defaults = config.ProviderPolicy{}
		s.Policies.Overrides = map[string]config.ProviderPolicy{}
	})
	human, err := iam.CreatePrincipal("human", "authentik:bedrock-owner", "", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	connection, err := iam.PutProviderConnection(iam.ProviderConnectionCreate{
		PrincipalID: human.ID, ProviderID: "bedrock", Name: "personal",
		Kind: "api_key", Secret: "fixture", Source: iam.ConnectionSourceUser,
		MakeDefault: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	provider, err := GetProviderForPrincipal(
		"bedrock", core.Caller{ID: human.ID, Kind: core.CallerHuman},
	)
	if err != nil {
		t.Fatal(err)
	}
	openAI, ok := provider.(OpenAIProvider)
	if !ok {
		t.Fatalf("provider type=%T", provider)
	}
	auth, ok := openAI.auth.(bearerAuth)
	if !ok || auth.observation == nil ||
		auth.observation.ConnectionID != connection.ID {
		t.Fatalf("auth observation=%+v", auth.observation)
	}
}

func TestForgetCatalogForPrincipal(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	catMu.Lock()
	catData = map[string]catalogEntry{}
	catGeneration = map[string]uint64{}
	catLoaded = true
	catMu.Unlock()
	t.Cleanup(func() {
		catMu.Lock()
		catData = nil
		catGeneration = nil
		catLoaded = false
		catMu.Unlock()
	})
	storeEntry("copilot@prn_one", []ModelInfo{{ID: "old-model"}})
	storeEntry("copilot@prn_two", []ModelInfo{{ID: "other-model"}})
	ForgetCatalogForPrincipal("copilot", "prn_one")
	if _, ok := cachedEntry("copilot@prn_one"); ok {
		t.Fatal("principal catalog was not removed")
	}
	if entry, ok := cachedEntry("copilot@prn_two"); !ok || len(entry.Models) != 1 {
		t.Fatal("another principal catalog was removed")
	}
}

func TestCopilotProviderCacheIsProjectScoped(t *testing.T) {
	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{
			"copilot": {Type: "github_copilot"},
		}
		s.Policies.Defaults = config.ProviderPolicy{}
		s.Policies.Overrides = map[string]config.ProviderPolicy{}
	})
	ResetProviders()
	t.Cleanup(ResetProviders)
	first, err := GetProviderForPrincipal("copilot", core.Caller{
		ID: "prn_shared", Kind: core.CallerService, ProjectID: "prj_one",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := GetProviderForPrincipal("copilot", core.Caller{
		ID: "prn_shared", Kind: core.CallerService, ProjectID: "prj_two",
	})
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("same service principal reused a Copilot provider across projects")
	}
}

func TestHumanCopilotProviderCacheRemainsPrincipalScoped(t *testing.T) {
	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{
			"copilot": {Type: "github_copilot"},
		}
		s.Policies.Defaults = config.ProviderPolicy{}
		s.Policies.Overrides = map[string]config.ProviderPolicy{}
	})
	ResetProviders()
	t.Cleanup(ResetProviders)
	first, err := GetProviderForPrincipal("copilot", core.Caller{
		ID: "prn_human", Kind: core.CallerHuman, ProjectID: "prj_one",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := GetProviderForPrincipal("copilot", core.Caller{
		ID: "prn_human", Kind: core.CallerHuman, ProjectID: "prj_two",
	})
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("human BYOC provider cache changed from principal to project scope")
	}
}

func TestHumanCopilotCatalogReusesPrincipalScopedEntry(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LLMGW_STATE_DIR", dir)
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	key := make([]byte, 32)
	config.Update(func(s *config.Settings) {
		s.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(key)
		s.Providers = map[string]*config.ProviderConfig{
			"copilot": {Type: "github_copilot"},
		}
	})
	human, _ := iam.CreatePrincipal("human", "authentik:catalog-human", "", "Human")
	project, _ := iam.CreateProject("catalog-human", "Human")
	_ = iam.SetMembership(project.ID, human.ID, "member")
	_, _ = iam.PutProviderCredential(human.ID, "copilot", "github_oauth", "human-secret")
	principal := core.Caller{ID: human.ID, Kind: core.CallerHuman, ProjectID: project.ID}
	storeEntry("copilot@"+human.ID, []ModelInfo{{ID: "existing-model"}})
	models := CatalogModelsForPrincipal("copilot", principal)
	if len(models) != 1 || models[0].ID != "existing-model" {
		t.Fatalf("human principal-scoped catalog was not reused: %+v", models)
	}
}

func TestCopilotCatalogRevalidatesBoundCredential(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LLMGW_STATE_DIR", dir)
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	key := make([]byte, 32)
	config.Update(func(s *config.Settings) {
		s.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(key)
		s.Providers = map[string]*config.ProviderConfig{
			"copilot": {Type: "github_copilot"},
		}
	})
	service, _ := iam.CreatePrincipal("service", "service:catalog", "", "Catalog")
	project, _ := iam.CreateProject("catalog", "Catalog")
	_ = iam.SetMembership(project.ID, service.ID, "member")
	credential, _ := iam.PutGatewayProviderCredential("copilot", "github_oauth", "shared-secret")
	_, _ = iam.SetProviderCredentialBinding(project.ID, "copilot", "service", credential.ID)
	principal := core.Caller{ID: service.ID, Kind: core.CallerService, ProjectID: project.ID}
	storeEntry(catalogCacheKey("copilot", principal), []ModelInfo{{ID: "cached-model"}})
	if models := CatalogModelsForPrincipal("copilot", principal); len(models) != 1 {
		t.Fatalf("active credential models=%+v", models)
	}
	if err := iam.SetProviderCredentialStatus(credential.ID, "revoked"); err != nil {
		t.Fatal(err)
	}
	if models := CatalogModelsForPrincipal("copilot", principal); len(models) != 0 {
		t.Fatalf("revoked credential retained cached models=%+v", models)
	}
}
