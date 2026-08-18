package iam

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"llmgw/internal/config"
)

func TestProviderCredentialsEncryptedAndHumanOnly(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LLMGW_STATE_DIR", dir)
	ResetForTests()
	t.Cleanup(ResetForTests)
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	config.Update(func(s *config.Settings) {
		s.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(key)
	})
	human, _ := CreatePrincipal("human", "authentik:human", "h@example.com", "Human")
	service, _ := CreatePrincipal("service", "service:worker", "", "Worker")
	secret := "test-provider-credential"
	info, err := PutProviderCredential(human.ID, "copilot", "github_oauth", secret)
	if err != nil {
		t.Fatal(err)
	}
	if info.ProviderID != "copilot" || info.Status != "active" {
		t.Fatalf("credential info=%+v", info)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatal("provider credential stored in plaintext")
	}
	got, ok, err := ProviderCredentialSecret(human.ID, "copilot")
	if err != nil || !ok || got != secret {
		t.Fatalf("get credential: got=%q ok=%v err=%v", got, ok, err)
	}
	if _, err := PutProviderCredential(
		service.ID, "copilot", "github_oauth", secret,
	); err == nil {
		t.Fatal("service principal should not own a Copilot credential")
	}
	if err := RevokeProviderCredential(human.ID, "copilot"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := ProviderCredentialSecret(human.ID, "copilot"); err != nil || ok {
		t.Fatalf("revoked credential available: ok=%v err=%v", ok, err)
	}
}

func TestProviderCredentialRejectsWrongEncryptionKey(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	ResetForTests()
	t.Cleanup(ResetForTests)
	config.Update(func(s *config.Settings) {
		s.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	})
	human, _ := CreatePrincipal("human", "authentik:human2", "", "Human")
	if _, err := PutProviderCredential(
		human.ID, "copilot", "github_oauth", "secret",
	); err != nil {
		t.Fatal(err)
	}
	wrong := make([]byte, 32)
	wrong[0] = 1
	config.Update(func(s *config.Settings) {
		s.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(wrong)
	})
	if _, _, err := ProviderCredentialSecret(human.ID, "copilot"); err == nil {
		t.Fatal("wrong encryption key should fail authentication")
	}
}

func TestServiceCredentialRequiresExactProjectBinding(t *testing.T) {
	setupCredentialTest(t)
	service, _ := CreatePrincipal("service", "service:xibo", "", "Xibo")
	other, _ := CreatePrincipal("service", "service:other", "", "Other")
	project, _ := CreateProject("xibo", "Xibo")
	otherProject, _ := CreateProject("other", "Other")
	_ = SetMembership(project.ID, service.ID, "member")
	_ = SetMembership(otherProject.ID, other.ID, "member")
	shared, err := PutGatewayProviderCredential("copilot", "github_oauth", "shared-secret")
	if err != nil {
		t.Fatal(err)
	}

	xiboPrincipal := requestPrincipal(service, project)
	if _, ok, err := ResolveProviderCredentialSecret(xiboPrincipal, "copilot"); err != nil || ok {
		t.Fatalf("unbound service resolved credential: ok=%v err=%v", ok, err)
	}
	if _, err := SetProviderCredentialBinding(
		otherProject.ID, "copilot", "service", shared.ID,
	); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := ResolveProviderCredentialSecret(xiboPrincipal, "copilot"); err != nil || ok {
		t.Fatalf("cross-project binding resolved credential: ok=%v err=%v", ok, err)
	}
	if _, err := SetProviderCredentialBinding(
		project.ID, "copilot", "service", shared.ID,
	); err != nil {
		t.Fatal(err)
	}
	secret, ok, err := ResolveProviderCredentialSecret(xiboPrincipal, "copilot")
	if err != nil || !ok || secret != "shared-secret" {
		t.Fatalf("bound credential: secret=%q ok=%v err=%v", secret, ok, err)
	}
	nonMember := requestPrincipal(other, project)
	if _, ok, err := ResolveProviderCredentialSecret(nonMember, "copilot"); err != nil || ok {
		t.Fatalf("non-member resolved credential: ok=%v err=%v", ok, err)
	}
}

func TestSharedCredentialStatusAndMetadataAreFailClosed(t *testing.T) {
	setupCredentialTest(t)
	service, _ := CreatePrincipal("service", "service:status", "", "Status")
	project, _ := CreateProject("status", "Status")
	_ = SetMembership(project.ID, service.ID, "member")
	secret := "gateway-managed-provider-secret"
	shared, _ := PutGatewayProviderCredential("copilot", "github_oauth", secret)
	binding, err := SetProviderCredentialBinding(
		project.ID, "copilot", "service", shared.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(struct {
		Credential ProviderCredentialInfo    `json:"credential"`
		Binding    ProviderCredentialBinding `json:"binding"`
	}{shared, binding})
	if strings.Contains(string(encoded), secret) {
		t.Fatal("credential metadata serialized provider secret")
	}

	principal := requestPrincipal(service, project)
	if err := SetProviderCredentialStatus(shared.ID, "disabled"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := ResolveProviderCredentialSecret(principal, "copilot"); err != nil || ok {
		t.Fatalf("disabled credential resolved: ok=%v err=%v", ok, err)
	}
	if err := SetProviderCredentialStatus(shared.ID, "active"); err != nil {
		t.Fatal(err)
	}
	if err := SetProviderCredentialBindingStatus(
		project.ID, "copilot", "service", "revoked",
	); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := ResolveProviderCredentialSecret(principal, "copilot"); err != nil || ok {
		t.Fatalf("revoked binding resolved: ok=%v err=%v", ok, err)
	}
}

func TestHumanCredentialResolutionRemainsPrincipalOwned(t *testing.T) {
	setupCredentialTest(t)
	human, _ := CreatePrincipal("human", "authentik:existing", "", "Existing")
	project, _ := CreateProject("existing", "Existing")
	_ = SetMembership(project.ID, human.ID, "member")
	if _, err := PutProviderCredential(
		human.ID, "copilot", "github_oauth", "human-secret",
	); err != nil {
		t.Fatal(err)
	}
	secret, ok, err := ResolveProviderCredentialSecret(
		requestPrincipal(human, project), "copilot",
	)
	if err != nil || !ok || secret != "human-secret" {
		t.Fatalf("human credential changed: secret=%q ok=%v err=%v", secret, ok, err)
	}
}

func TestSharedCredentialResolutionIsConcurrent(t *testing.T) {
	setupCredentialTest(t)
	service, _ := CreatePrincipal("service", "service:concurrent", "", "Concurrent")
	project, _ := CreateProject("concurrent", "Concurrent")
	_ = SetMembership(project.ID, service.ID, "member")
	shared, _ := PutGatewayProviderCredential("copilot", "github_oauth", "shared-secret")
	_, _ = SetProviderCredentialBinding(project.ID, "copilot", "service", shared.ID)
	principal := requestPrincipal(service, project)

	const workers = 24
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			secret, ok, err := ResolveProviderCredentialSecret(principal, "copilot")
			if err != nil || !ok || secret != "shared-secret" {
				errs <- fmt.Errorf("secret=%q ok=%v err=%v", secret, ok, err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func setupCredentialTest(t *testing.T) {
	t.Helper()
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	ResetForTests()
	t.Cleanup(ResetForTests)
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	config.Update(func(s *config.Settings) {
		s.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(key)
	})
}

func requestPrincipal(principal Principal, project Project) *config.Principal {
	return &config.Principal{
		PrincipalID: principal.ID, PrincipalKind: principal.Kind,
		ProjectID: project.ID, Project: project.Slug, Token: "test-key",
	}
}

// A human's OAuth subscription lives in the connection store, not in
// provider_credentials. The credential resolver must find it there, otherwise
// the authorization gate reports "no credential" for a connected Copilot and
// every catalog refresh returns nothing.
func TestResolveProviderCredentialSecretFindsHumanOAuthConnection(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	ResetForTests()
	t.Cleanup(ResetForTests)
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 7)
	}
	config.Update(func(s *config.Settings) {
		s.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(key)
	})
	human, err := CreatePrincipal("human", "authentik:oauth-owner", "owner@example.local", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PutOAuthProviderConnection(OAuthConnectionCreate{
		PrincipalID: human.ID, ProviderID: "copilot", Name: defaultConnectionName,
		Kind: "github_oauth", Source: ConnectionSourceUser, MakeDefault: true,
		AccessToken: "gho_test_token",
	}); err != nil {
		t.Fatal(err)
	}
	secret, ok, err := ResolveProviderCredentialSecret(
		&config.Principal{PrincipalID: human.ID, PrincipalKind: "human"}, "copilot",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || secret != "gho_test_token" {
		t.Fatalf("resolve = (%q, %v), want the stored OAuth access token", secret, ok)
	}
}

func TestOAuthCredentialResolutionRejectsNonOAuthSecrets(t *testing.T) {
	setupCredentialTest(t)
	human, err := CreatePrincipal("human", "authentik:oauth-only", "", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PutProviderConnection(ProviderConnectionCreate{
		PrincipalID: human.ID, ProviderID: "copilot", Name: "personal",
		Kind: "api_key", Secret: "must-not-reach-github",
		Source: ConnectionSourceUser, MakeDefault: true,
	}); err != nil {
		t.Fatal(err)
	}
	principal := &config.Principal{PrincipalID: human.ID, PrincipalKind: human.Kind}
	if _, _, ok, err := ResolveProviderOAuthCredentialSecretWithObservation(
		principal, "copilot",
	); err == nil || ok {
		t.Fatalf("non-OAuth personal connection resolved: ok=%v err=%v", ok, err)
	}

	service, err := CreatePrincipal("service", "service:oauth-only", "", "Service")
	if err != nil {
		t.Fatal(err)
	}
	project, err := CreateProject("oauth-only", "OAuth Only")
	if err != nil {
		t.Fatal(err)
	}
	if err := SetMembership(project.ID, service.ID, "member"); err != nil {
		t.Fatal(err)
	}
	shared, err := PutGatewayProviderCredential("copilot", "api_key", "shared-api-key")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SetProviderCredentialBinding(
		project.ID, "copilot", "service", shared.ID,
	); err != nil {
		t.Fatal(err)
	}
	servicePrincipal := requestPrincipal(service, project)
	if _, _, ok, err := ResolveProviderOAuthCredentialSecretWithObservation(
		servicePrincipal, "copilot",
	); err == nil || ok {
		t.Fatalf("non-OAuth bound credential resolved: ok=%v err=%v", ok, err)
	}
}

func TestHumanConnectionPrecedesLegacyCredentialAndFallsBackSafely(t *testing.T) {
	setupCredentialTest(t)
	human, err := CreatePrincipal("human", "authentik:connection-first", "", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	other, err := CreatePrincipal("human", "authentik:other-owner", "", "Other")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PutProviderCredential(
		human.ID, "copilot", "github_oauth", "legacy-token",
	); err != nil {
		t.Fatal(err)
	}
	first, err := PutOAuthProviderConnection(OAuthConnectionCreate{
		PrincipalID: human.ID, ProviderID: "copilot", Name: "personal",
		Kind: "github_oauth", Source: ConnectionSourceUser, MakeDefault: true,
		AccessToken: "first-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := PutOAuthProviderConnection(OAuthConnectionCreate{
		PrincipalID: human.ID, ProviderID: "copilot", Name: "work",
		Kind: "github_oauth", Source: ConnectionSourceUser, MakeDefault: true,
		AccessToken: "second-token",
	})
	if err != nil {
		t.Fatal(err)
	}

	resolve := func(principalID string) (string, bool, error) {
		return ResolveProviderCredentialSecret(
			&config.Principal{PrincipalID: principalID, PrincipalKind: "human"},
			"copilot",
		)
	}
	secret, ok, err := resolve(human.ID)
	if err != nil || !ok || secret != "second-token" {
		t.Fatalf("default connection resolve=(%q,%v,%v)", secret, ok, err)
	}
	if _, ok, err := resolve(other.ID); err != nil || ok {
		t.Fatalf("other principal resolved owner credential: ok=%v err=%v", ok, err)
	}

	if err := RevokeProviderConnection(human.ID, second.ID); err != nil {
		t.Fatal(err)
	}
	secret, ok, err = resolve(human.ID)
	if err != nil || !ok || secret != "first-token" {
		t.Fatalf("promoted connection resolve=(%q,%v,%v)", secret, ok, err)
	}
	if err := RevokeProviderConnection(human.ID, first.ID); err != nil {
		t.Fatal(err)
	}
	secret, ok, err = resolve(human.ID)
	if err != nil || !ok || secret != "legacy-token" {
		t.Fatalf("legacy fallback resolve=(%q,%v,%v)", secret, ok, err)
	}
}
