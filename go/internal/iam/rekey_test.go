package iam

import (
	"encoding/base64"
	"errors"
	"slices"
	"strings"
	"testing"
)

func encodedCredentialKey(key []byte) string { return base64.RawURLEncoding.EncodeToString(key) }

// rekeyFixture is a database with a value in every encrypted column, sealed
// with the key configured when it was made.
type rekeyFixture struct {
	owner   Principal
	issued  IssuedKey
	profile OAuthClientProfile
}

func newRekeyFixture(t *testing.T, key []byte) rekeyFixture {
	t.Helper()
	if err := restartWithCredentialKey(key); err != nil {
		t.Fatal(err)
	}
	owner := storeFixtureConnection(t, "fixture:rekey-owner", "fixture-connection-secret")
	// A connection the v7 migration copied keeps the associated data of the
	// legacy row it came from, which a rekey must keep as well.
	if _, err := PutProviderConnection(ProviderConnectionCreate{
		PrincipalID: owner.ID, ProviderID: "migrated", Kind: "api_key", Secret: "unused",
	}); err != nil {
		t.Fatal(err)
	}
	ciphertext, nonce, err := encryptCredential(
		key, []byte("fixture-migrated-secret"), connectionAAD(owner.ID, "migrated", "default", 1),
	)
	if err != nil {
		t.Fatal(err)
	}
	db, err := DB()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
UPDATE provider_connections SET ciphertext=?,nonce=?,aad_version=1
WHERE principal_id=? AND provider_id='migrated'`, ciphertext, nonce, owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := PutProviderCredential(owner.ID, "copilot", "github_oauth", "fixture-legacy-secret"); err != nil {
		t.Fatal(err)
	}
	project, err := CreateProject("rekey", "Rekey")
	if err != nil {
		t.Fatal(err)
	}
	if err := SetMembership(project.ID, owner.ID, "owner"); err != nil {
		t.Fatal(err)
	}
	issued, err := IssueKey(KeyCreate{ProjectID: project.ID, PrincipalID: owner.ID, Name: "rekey"})
	if err != nil {
		t.Fatal(err)
	}
	profile := OAuthClientProfile{
		ProviderID: "google_antigravity", Profile: "consumer_manual", ClientID: "fixture-client",
		ClientSecret: "fixture-client-secret", ClientMode: "confidential",
		RedirectURI: "http://localhost:51121/oauth-callback",
	}
	if err := PutOAuthClientProfile(profile); err != nil {
		t.Fatal(err)
	}
	// The command opens the database itself, as a separate process would.
	ResetForTests()
	return rekeyFixture{owner: owner, issued: issued, profile: profile}
}

// expectEveryValueOpensWith requires every stored encrypted value to open
// with opens and with none of closed.
func expectEveryValueOpensWith(t *testing.T, opens []byte, closed ...[]byte) {
	t.Helper()
	db, err := DB()
	if err != nil {
		t.Fatal(err)
	}
	stored, err := encryptedCredentials(db)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) == 0 {
		t.Fatal("no encrypted values are stored")
	}
	for _, credential := range stored {
		if !credential.opens(opens) {
			t.Fatalf("%s row %s does not open with the expected key", credential.table, credential.id)
		}
		for _, key := range closed {
			if credential.opens(key) {
				t.Fatalf("%s row %s still opens with a replaced key", credential.table, credential.id)
			}
		}
	}
}

func TestRekeyReencryptsEveryCredentialUnderTheNewKey(t *testing.T) {
	useCredentialKeyTestState(t)
	original, next := fixtureCredentialKey(11), fixtureCredentialKey(12)
	fixture := newRekeyFixture(t, original)

	counts, err := RekeyCredentials(encodedCredentialKey(original), encodedCredentialKey(next))
	if err != nil {
		t.Fatal(err)
	}
	want := []RekeyCount{
		{"provider_connections", 2}, {"provider_credentials", 1}, {"api_keys", 1}, {"oauth_client_profiles", 1},
	}
	if !slices.Equal(counts, want) {
		t.Fatalf("counts=%+v, want %+v", counts, want)
	}
	configureCredentialKey(next)
	expectEveryValueOpensWith(t, next, original)
	for provider, want := range map[string]string{"fixture": "fixture-connection-secret", "migrated": "fixture-migrated-secret"} {
		if secret, _, ok, err := ProviderConnectionSecret(fixture.owner.ID, provider, ""); err != nil || !ok || secret != want {
			t.Fatalf("%s connection after rekey: ok=%v err=%v", provider, ok, err)
		}
	}
	if secret, ok, err := ProviderCredentialSecret(fixture.owner.ID, "copilot"); err != nil || !ok || secret != "fixture-legacy-secret" {
		t.Fatalf("legacy credential after rekey: ok=%v err=%v", ok, err)
	}
	if token, _, err := RevealAPIKey(fixture.issued.ID); err != nil || token != fixture.issued.Token {
		t.Fatalf("revealed key after rekey matches=%v err=%v", token == fixture.issued.Token, err)
	}
	if profile, ok, err := OAuthClientProfileByName("google_antigravity", "consumer_manual"); err != nil || !ok || profile != fixture.profile {
		t.Fatalf("OAuth client profile after rekey: ok=%v err=%v", ok, err)
	}
	db, err := DB()
	if err != nil {
		t.Fatal(err)
	}
	var aadVersion int
	if err := db.QueryRow(
		"SELECT aad_version FROM provider_connections WHERE provider_id='migrated'",
	).Scan(&aadVersion); err != nil || aadVersion != 1 {
		t.Fatalf("migrated connection aad_version=%d err=%v, want 1", aadVersion, err)
	}
	if err := restartWithCredentialKey(next); err != nil {
		t.Fatalf("start with the new key: %v", err)
	}
	if err := restartWithCredentialKey(original); !errors.Is(err, ErrCredentialKeyMismatch) {
		t.Fatalf("start with the replaced key: err=%v, want the key mismatch", err)
	}
}

func TestRekeyRefusesACurrentKeyTheDatabaseDoesNotMatch(t *testing.T) {
	useCredentialKeyTestState(t)
	original, wrong, next := fixtureCredentialKey(13), fixtureCredentialKey(14), fixtureCredentialKey(15)
	newRekeyFixture(t, original)

	if _, err := RekeyCredentials(encodedCredentialKey(wrong), encodedCredentialKey(next)); !errors.Is(err, ErrCredentialKeyMismatch) {
		t.Fatalf("rekey with the wrong current key: err=%v, want the key mismatch", err)
	}
	configureCredentialKey(original)
	expectEveryValueOpensWith(t, original, next)
	if state := credentialKeyState(t); state != CredentialKeyMatches {
		t.Fatalf("credential key state after the refused rekey %q, want %q", state, CredentialKeyMatches)
	}
}

// The rekey writes one table after another in a single transaction. A write
// that fails after earlier tables were rewritten must leave none rewritten.
func TestRekeyChangesNothingWhenAWriteFails(t *testing.T) {
	useCredentialKeyTestState(t)
	original, next := fixtureCredentialKey(16), fixtureCredentialKey(17)
	newRekeyFixture(t, original)
	db, err := DB()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
CREATE TRIGGER refuse_profile_rekey BEFORE UPDATE OF ciphertext ON oauth_client_profiles
BEGIN SELECT RAISE(ABORT, 'fixture write failure'); END`); err != nil {
		t.Fatal(err)
	}
	ResetForTests()

	if _, err := RekeyCredentials(encodedCredentialKey(original), encodedCredentialKey(next)); err == nil ||
		!strings.Contains(err.Error(), "fixture write failure") {
		t.Fatalf("rekey with a failing write: err=%v", err)
	}
	configureCredentialKey(original)
	expectEveryValueOpensWith(t, original, next)
	if state := credentialKeyState(t); state != CredentialKeyMatches {
		t.Fatalf("credential key state after the failed rekey %q, want %q", state, CredentialKeyMatches)
	}

	db, err = DB()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("DROP TRIGGER refuse_profile_rekey"); err != nil {
		t.Fatal(err)
	}
	ResetForTests()
	if _, err := RekeyCredentials(encodedCredentialKey(original), encodedCredentialKey(next)); err != nil {
		t.Fatalf("rekey once the write succeeds: %v", err)
	}
	configureCredentialKey(next)
	expectEveryValueOpensWith(t, next, original)
}

func TestRekeyRefusesAValueTheCurrentKeyDoesNotOpen(t *testing.T) {
	useCredentialKeyTestState(t)
	original, foreign, next := fixtureCredentialKey(18), fixtureCredentialKey(19), fixtureCredentialKey(20)
	fixture := newRekeyFixture(t, original)
	ciphertext, nonce, err := encryptCredential(
		foreign, []byte("fixture-foreign-secret"), []byte(fixture.owner.ID+"|foreign"),
	)
	if err != nil {
		t.Fatal(err)
	}
	db, err := DB()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
INSERT INTO provider_credentials(
    id,principal_id,provider_id,credential_kind,ciphertext,nonce,status,created_at,updated_at
) VALUES('cred_foreign',?,'foreign','api_key',?,?,'revoked',1,1)`,
		fixture.owner.ID, ciphertext, nonce,
	); err != nil {
		t.Fatal(err)
	}
	ResetForTests()

	_, err = RekeyCredentials(encodedCredentialKey(original), encodedCredentialKey(next))
	if err == nil || !strings.Contains(err.Error(), "provider_credentials row cred_foreign does not decrypt") ||
		strings.Contains(err.Error(), "fixture-foreign-secret") {
		t.Fatalf("rekey of a database holding a foreign value: err=%v", err)
	}
	configureCredentialKey(original)
	if secret, _, ok, err := ProviderConnectionSecret(fixture.owner.ID, "fixture", ""); err != nil || !ok ||
		secret != "fixture-connection-secret" {
		t.Fatalf("connection after the refused rekey: ok=%v err=%v", ok, err)
	}
}

func TestRekeyRefusesAnUnusableNewKey(t *testing.T) {
	useCredentialKeyTestState(t)
	original := fixtureCredentialKey(21)
	newRekeyFixture(t, original)
	for name, next := range map[string]string{
		"missing": "", "malformed": "fixture-not-a-key", "unchanged": encodedCredentialKey(original),
	} {
		_, err := RekeyCredentials(encodedCredentialKey(original), next)
		if err == nil || !strings.Contains(err.Error(), "LLMGW_NEW_CREDENTIAL_ENCRYPTION_KEY") ||
			(next != "" && strings.Contains(err.Error(), next)) {
			t.Fatalf("%s new key: err=%v", name, err)
		}
	}
}
