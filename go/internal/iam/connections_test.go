package iam

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"llmgw/internal/config"
)

func TestProviderConnectionsSupportMultipleNamedDefaults(t *testing.T) {
	setupConnectionTest(t)
	human, err := CreatePrincipal("human", "authentik:connections", "", "Connections")
	if err != nil {
		t.Fatal(err)
	}
	first, err := PutProviderConnection(ProviderConnectionCreate{
		PrincipalID: human.ID, ProviderID: "gemini", Name: "personal",
		Kind: "api_key", Secret: "first-secret", Source: ConnectionSourceUser,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !first.IsDefault || !first.PrivateToPrincipal {
		t.Fatalf("first connection=%+v", first)
	}
	second, err := PutProviderConnection(ProviderConnectionCreate{
		PrincipalID: human.ID, ProviderID: "gemini", Name: "work",
		Kind: "api_key", Secret: "second-secret", Source: ConnectionSourceUser,
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.IsDefault {
		t.Fatalf("second connection unexpectedly default: %+v", second)
	}
	updated, err := PutProviderConnection(ProviderConnectionCreate{
		PrincipalID: human.ID, ProviderID: "gemini", Name: "personal",
		Kind: "api_key", Secret: "rotated-secret", Source: ConnectionSourceUser,
	})
	if err != nil || !updated.IsDefault {
		t.Fatalf("updating default connection lost default: %+v err=%v", updated, err)
	}
	secret, selected, ok, err := ProviderConnectionSecret(human.ID, "gemini", "")
	if err != nil || !ok || secret != "rotated-secret" || selected.Name != "personal" {
		t.Fatalf("selected=%+v secret=%q ok=%v err=%v", selected, secret, ok, err)
	}
	if err := RevokeProviderConnection(human.ID, first.ID); err != nil {
		t.Fatal(err)
	}
	secret, selected, ok, err = ProviderConnectionSecret(human.ID, "gemini", "")
	if err != nil || !ok || secret != "second-secret" || selected.Name != "work" ||
		!selected.IsDefault {
		t.Fatalf("promoted=%+v secret=%q ok=%v err=%v", selected, secret, ok, err)
	}
}

func TestProviderConnectionsEnforceOwnershipAndEncryption(t *testing.T) {
	setupConnectionTest(t)
	human, _ := CreatePrincipal("human", "authentik:owner", "", "Owner")
	service, _ := CreatePrincipal("service", "service:worker", "", "Worker")
	system, _ := EnsureSystemPrincipal()
	if _, err := PutProviderConnection(ProviderConnectionCreate{
		PrincipalID: service.ID, ProviderID: "gemini", Kind: "api_key", Secret: "secret",
	}); err == nil {
		t.Fatal("service principal unexpectedly owned a provider connection")
	}

	if err := RevokeProviderConnection(human.ID, "missing"); !errors.Is(
		err, ErrProviderConnectionNotFound,
	) {
		t.Fatalf("missing connection error=%v", err)
	}
	if _, err := PutProviderConnection(ProviderConnectionCreate{
		PrincipalID: system.ID, ProviderID: "copilot", Kind: "github_oauth",
		Secret: "secret", Source: ConnectionSourceConfig,
	}); err == nil {
		t.Fatal("system principal unexpectedly owned an OAuth subscription")
	}
	if _, err := PutProviderConnection(ProviderConnectionCreate{
		PrincipalID: human.ID, ProviderID: "gemini", Kind: "api_key", Secret: "secret",
	}); err != nil {
		t.Fatal(err)
	}
	wrong := make([]byte, 32)
	wrong[0] = 99
	config.Update(func(s *config.Settings) {
		s.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(wrong)
	})
	if _, _, _, err := ProviderConnectionSecret(human.ID, "gemini", "default"); err == nil {
		t.Fatal("wrong encryption key unexpectedly decrypted a provider connection")
	}
}

func TestResolvablePrivateConnectionFailsClosedWithoutEncryptionKey(t *testing.T) {
	setupConnectionTest(t)
	human, err := CreatePrincipal("human", "authentik:state-owner", "", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PutProviderConnection(ProviderConnectionCreate{
		PrincipalID: human.ID, ProviderID: "copilot", Name: "personal",
		Kind: "github_oauth", Secret: "private-token",
		Source: ConnectionSourceUser, MakeDefault: true,
	}); err != nil {
		t.Fatal(err)
	}
	resolvable, err := HasResolvablePrivateProviderConnection(human.ID, "copilot")
	if err != nil || !resolvable {
		t.Fatalf("configured resolvable=%v err=%v", resolvable, err)
	}
	config.Update(func(settings *config.Settings) {
		settings.CredentialEncryptionKey = ""
	})
	resolvable, err = HasResolvablePrivateProviderConnection(human.ID, "copilot")
	if err != nil || resolvable {
		t.Fatalf("missing-key resolvable=%v err=%v", resolvable, err)
	}
}

func TestConfigSeedsSystemConnectionOnlyWhenAbsent(t *testing.T) {
	setupConnectionTest(t)
	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{
			"gemini": {Type: "openai_compatible", APIKey: "config-first"},
		}
	})
	seeded, err := SeedSystemProviderConnectionsFromConfig()
	if err != nil || seeded != 1 {
		t.Fatalf("first seed=%d err=%v", seeded, err)
	}
	config.Update(func(s *config.Settings) {
		s.Providers["gemini"].APIKey = "config-second"
	})
	seeded, err = SeedSystemProviderConnectionsFromConfig()
	if err != nil || seeded != 0 {
		t.Fatalf("second seed=%d err=%v", seeded, err)
	}
	secret, connection, ok, err := SystemProviderConnectionSecret("gemini")
	if err != nil || !ok || secret != "config-first" ||
		connection.Source != ConnectionSourceConfig {
		t.Fatalf("seeded connection=%+v secret=%q ok=%v err=%v", connection, secret, ok, err)
	}
	if updated, err := PutSystemProviderConnection("gemini", "api_key", "admin-rotated"); err != nil || !updated {
		t.Fatalf("explicit rotation updated=%v err=%v", updated, err)
	}
	secret, connection, ok, err = SystemProviderConnectionSecret("gemini")
	if err != nil || !ok || secret != "admin-rotated" ||
		connection.Source != ConnectionSourceAdmin {
		t.Fatalf("rotated connection=%+v secret=%q ok=%v err=%v", connection, secret, ok, err)
	}
}

func TestStartupMovesPlaintextProviderSecretsIntoEncryptedStore(t *testing.T) {
	setupConnectionTest(t)
	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{
			"configured": {Type: "openai_compatible"},
		}
	})
	config.SaveSecret("configured", "fixture-configured-key")
	config.SaveSecret("unlisted", "fixture-unlisted-key")
	if _, err := Initialize(); err != nil {
		t.Fatal(err)
	}
	for providerID, want := range map[string]string{
		"configured": "fixture-configured-key", "unlisted": "fixture-unlisted-key",
	} {
		secret, connection, ok, err := SystemProviderConnectionSecret(providerID)
		if err != nil || !ok || secret != want || connection.Source != ConnectionSourceConfig {
			t.Fatalf("%s: connection=%+v matches=%v ok=%v err=%v", providerID, connection, secret == want, ok, err)
		}
	}
	if _, err := os.Stat(filepath.Join(config.StateDir(), "secrets.json")); !os.IsNotExist(err) {
		t.Fatalf("secrets.json remains after every entry moved: %v", err)
	}
}

// Moving plaintext keys must not change which key a provider resolves: a key
// saved through the console stays, a revoked connection is not revived, and a
// key configured in YAML keeps precedence. Only an entry the active system
// connection already holds is removed.
func TestStartupMoveKeepsResolutionPrecedence(t *testing.T) {
	setupConnectionTest(t)
	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{
			"yaml": {Type: "openai_compatible", APIKey: "fixture-yaml-key"},
		}
	})
	for providerID, secret := range map[string]string{
		"console": "fixture-console-key", "duplicate": "fixture-duplicate-key",
		"revoked": "fixture-revoked-key",
	} {
		if _, err := PutSystemProviderConnection(providerID, "api_key", secret); err != nil {
			t.Fatal(err)
		}
	}
	if err := RevokeSystemProviderConnection("revoked"); err != nil {
		t.Fatal(err)
	}
	plaintext := map[string]string{
		"console": "fixture-earlier-key", "duplicate": "fixture-duplicate-key",
		"revoked": "fixture-revoked-key", "yaml": "fixture-plaintext-key",
	}
	for providerID, secret := range plaintext {
		config.SaveSecret(providerID, secret)
	}
	if _, err := Initialize(); err != nil {
		t.Fatal(err)
	}
	delete(plaintext, "duplicate")
	if remaining := config.LoadSecrets(); !reflect.DeepEqual(remaining, plaintext) {
		t.Fatalf("secrets.json kept %d entries, want console, revoked and yaml", len(remaining))
	}
	for providerID, want := range map[string]string{
		"console": "fixture-console-key", "duplicate": "fixture-duplicate-key", "yaml": "fixture-yaml-key",
	} {
		secret, _, ok, err := SystemProviderConnectionSecret(providerID)
		if err != nil || !ok || secret != want {
			t.Fatalf("%s: matches=%v ok=%v err=%v", providerID, secret == want, ok, err)
		}
	}
	if _, _, ok, err := SystemProviderConnectionSecret("revoked"); err != nil || ok {
		t.Fatalf("revoked connection was revived: ok=%v err=%v", ok, err)
	}
}

func TestStartupKeepsPlaintextProviderSecretsWithoutEncryption(t *testing.T) {
	setupConnectionTest(t)
	config.Update(func(s *config.Settings) {
		s.CredentialEncryptionKey = ""
		s.Providers = map[string]*config.ProviderConfig{"plain": {Type: "openai_compatible"}}
	})
	config.SaveSecret("plain", "fixture-plain-key")
	if _, err := Initialize(); err != nil {
		t.Fatal(err)
	}
	if got := config.ResolveProviderAPIKey("plain", config.Get().Providers["plain"]); got != "fixture-plain-key" {
		t.Fatal("plaintext key no longer resolves without encryption")
	}
	if exists, err := SystemProviderConnectionExists("plain"); err != nil || exists {
		t.Fatalf("system connection exists=%v err=%v", exists, err)
	}
}

func TestMigrationCopiesLegacyCredentialWithLegacyAAD(t *testing.T) {
	setupConnectionTest(t)
	human, _ := CreatePrincipal("human", "authentik:legacy-connection", "", "Legacy")
	key, err := credentialKey()
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, nonce, err := encryptCredential(
		key, []byte("legacy-secret"), connectionAAD(human.ID, "copilot", "default", 1),
	)
	if err != nil {
		t.Fatal(err)
	}
	db, err := DB()
	if err != nil {
		t.Fatal(err)
	}
	now := int64(1234)
	if _, err := db.Exec(`
INSERT INTO provider_credentials(
    id,principal_id,provider_id,credential_kind,ciphertext,nonce,key_version,
    status,created_at,updated_at
) VALUES('cred_legacy',?,?,?,?,?,1,'active',?,?)`,
		human.ID, "copilot", "github_oauth", ciphertext, nonce, now, now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("DROP TABLE provider_connections"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("DELETE FROM schema_migrations WHERE version=7"); err != nil {
		t.Fatal(err)
	}
	ResetForTests()
	if _, err := DB(); err != nil {
		t.Fatal(err)
	}
	secret, connection, ok, err := ProviderConnectionSecret(human.ID, "copilot", "")
	if err != nil || !ok || secret != "legacy-secret" ||
		connection.Source != ConnectionSourceMigration {
		t.Fatalf("migrated connection=%+v secret=%q ok=%v err=%v", connection, secret, ok, err)
	}
}

func TestMigrationRevokesLegacyRowsOfRevokedConnections(t *testing.T) {
	setupConnectionTest(t)
	human, err := CreatePrincipal("human", "authentik:legacy-repair", "", "Legacy")
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := CreatePrincipal("system", "fixture:gateway-credentials", "", "Gateway")
	if err != nil {
		t.Fatal(err)
	}
	db, err := DB()
	if err != nil {
		t.Fatal(err)
	}
	// Each legacy row has its v7 copy under the same ID; the timestamps say
	// whether the row was written before or after its copy was revoked.
	rows := []struct {
		owner, provider, connectionStatus string
		legacyUpdated, connectionUpdated  int64
		want                              string
	}{
		{human.ID, "left-behind", "revoked", 100, 200, "revoked"},
		{human.ID, "connected", "active", 100, 100, "active"},
		{human.ID, "re-enabled", "revoked", 300, 200, "active"},
		{gateway.ID, "shared", "revoked", 100, 200, "active"},
	}
	for _, row := range rows {
		if _, err := db.Exec(`
INSERT INTO provider_credentials(
    id,principal_id,provider_id,credential_kind,ciphertext,nonce,status,created_at,updated_at
) VALUES(?,?,?,'github_oauth',X'00',X'00','active',1,?)`,
			"cred_"+row.provider, row.owner, row.provider, row.legacyUpdated,
		); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`
INSERT INTO provider_connections(
    id,principal_id,provider_id,connection_name,credential_kind,source,
    private_to_principal,is_default,ciphertext,nonce,aad_version,status,created_at,updated_at
) VALUES(?,?,?,'default','github_oauth','migration',1,?,X'00',X'00',1,?,1,?)`,
			"cred_"+row.provider, row.owner, row.provider,
			boolInt(row.connectionStatus == "active"), row.connectionStatus, row.connectionUpdated,
		); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec("DELETE FROM schema_migrations WHERE version=21"); err != nil {
		t.Fatal(err)
	}
	if err := applyMigrations(db); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		info, found, err := ProviderCredentialByID("cred_" + row.provider)
		if err != nil || !found || info.Status != row.want {
			t.Errorf("%s: status=%q found=%v err=%v, want %s", row.provider, info.Status, found, err, row.want)
		}
	}
}

func setupConnectionTest(t *testing.T) {
	t.Helper()
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	ResetForTests()
	oldKey := config.Get().CredentialEncryptionKey
	oldProviders := config.Get().Providers
	t.Cleanup(func() {
		ResetForTests()
		config.Update(func(s *config.Settings) {
			s.CredentialEncryptionKey = oldKey
			s.Providers = oldProviders
		})
	})
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	config.Update(func(s *config.Settings) {
		s.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(key)
		s.Providers = map[string]*config.ProviderConfig{}
	})
}
