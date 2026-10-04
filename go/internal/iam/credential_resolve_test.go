package iam

import (
	"context"
	"errors"
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

// A credential the v7 migration copied lives in both tables under one ID.
// Whichever half is revoked, and by whichever path, neither may resolve after.
func TestRevokingAMigratedCredentialRevokesBothRows(t *testing.T) {
	for _, c := range []struct {
		name   string
		revoke func(f *resolveFixture, owner Principal, id string) error
	}{
		{"connection", func(_ *resolveFixture, owner Principal, id string) error {
			return RevokeProviderConnection(owner.ID, id)
		}},
		{"credential store", func(f *resolveFixture, _ Principal, id string) error {
			return f.stores[OAuthPrecedence].RevokeIfCurrent(context.Background(), id, "1")
		}},
		{"failed refresh", func(_ *resolveFixture, owner Principal, _ string) error {
			envelope, connection, found, err := OAuthProviderConnectionSecret(owner.ID, "copilot", "")
			if err != nil || !found {
				return errors.Join(err, errors.New("migrated connection missing"))
			}
			revoked, err := RevokeOAuthProviderConnectionIfCurrent(connection, envelope)
			if err == nil && !revoked {
				err = errors.New("migrated connection was not revoked")
			}
			return err
		}},
		{"legacy credential", func(_ *resolveFixture, owner Principal, _ string) error {
			return RevokeProviderCredential(owner.ID, "copilot")
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newResolveFixture(t)
			dave := must(CreatePrincipal("human", "fixture:dave", "", "Dave"))
			legacy := must(PutProviderCredential(dave.ID, "copilot", "github_oauth", "fixture-dave-legacy"))
			copyAsMigratedConnection(t, legacy.ID)
			caller := core.Caller{ID: dave.ID, Kind: core.CallerHuman}
			f.check(t, []resolveCase{{"migrated connection", caller, "copilot", oauth, legacy.ID}})
			if err := c.revoke(f, dave, legacy.ID); err != nil {
				t.Fatal(err)
			}
			f.check(t, []resolveCase{{"revoked migrated credential", caller, "copilot", oauth, ""}})
			var legacyStatus, connectionStatus string
			if err := must(DB()).QueryRow(`
SELECT (SELECT status FROM provider_credentials WHERE id=?),
       (SELECT status FROM provider_connections WHERE id=?)`, legacy.ID, legacy.ID,
			).Scan(&legacyStatus, &connectionStatus); err != nil {
				t.Fatal(err)
			}
			if legacyStatus != "revoked" || connectionStatus != "revoked" {
				t.Fatalf("legacy=%s connection=%s, want both revoked", legacyStatus, connectionStatus)
			}
		})
	}
}

// A gateway-owned credential is shared through project bindings rather than
// served as a fallback, so revoking its v7 copy must leave the binding intact.
func TestRevokingTheCopyOfAGatewayCredentialKeepsItsBinding(t *testing.T) {
	f := newResolveFixture(t)
	copyAsMigratedConnection(t, f.shared.ID)
	if err := RevokeProviderConnection(f.shared.PrincipalID, f.shared.ID); err != nil {
		t.Fatal(err)
	}
	f.check(t, []resolveCase{{
		"binding of a gateway credential", f.workerCaller, "copilot", oauth,
		ProviderCredentialKeyPrefix + f.shared.ID,
	}})
}

// copyAsMigratedConnection copies one provider_credentials row into
// provider_connections under the same ID, with its account state, exactly as
// migrations 7 and 8 did.
func copyAsMigratedConnection(t *testing.T, id string) {
	t.Helper()
	for _, statement := range []string{`
INSERT INTO provider_connections(
    id,principal_id,provider_id,connection_name,credential_kind,source,
    private_to_principal,is_default,ciphertext,nonce,key_version,aad_version,
    status,created_at,updated_at,last_used_at
)
SELECT
    id,principal_id,provider_id,'default',credential_kind,'migration',
    1,1,ciphertext,nonce,key_version,1,status,created_at,updated_at,last_used_at
FROM provider_credentials WHERE id=?`, `
INSERT INTO provider_account_state(connection_id,updated_at)
SELECT id,updated_at FROM provider_connections WHERE id=?`,
	} {
		if _, err := must(DB()).Exec(statement, id); err != nil {
			t.Fatal(err)
		}
	}
}

func callerWithoutProject(f *resolveFixture) core.Caller {
	caller := f.workerCaller
	caller.ProjectID = ""
	return caller
}
