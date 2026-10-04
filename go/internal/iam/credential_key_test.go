package iam

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"llmgw/internal/config"
)

// useCredentialKeyTestState gives a test its own state directory and settings
// without configured provider keys, which a start would otherwise seal into
// the database.
func useCredentialKeyTestState(t *testing.T) {
	t.Helper()
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	ResetForTests()
	old := *config.Get()
	t.Cleanup(func() {
		ResetForTests()
		config.Update(func(s *config.Settings) { *s = old })
	})
	config.Update(func(s *config.Settings) { s.Providers = map[string]*config.ProviderConfig{} })
}

func fixtureCredentialKey(seed byte) []byte { return bytes.Repeat([]byte{seed}, 32) }

func configureCredentialKey(key []byte) {
	config.Update(func(s *config.Settings) {
		s.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(key)
	})
}

// restartWithCredentialKey closes the database and initializes it again
// with key configured, as a gateway restart does.
func restartWithCredentialKey(key []byte) error {
	ResetForTests()
	configureCredentialKey(key)
	_, err := Initialize()
	return err
}

func storeFixtureConnection(t *testing.T, subject, secret string) Principal {
	t.Helper()
	human, err := CreatePrincipal("human", subject, "", "Fixture Human")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PutProviderConnection(ProviderConnectionCreate{
		PrincipalID: human.ID, ProviderID: "fixture", Kind: "api_key", Secret: secret,
	}); err != nil {
		t.Fatal(err)
	}
	return human
}

func credentialKeyState(t *testing.T) string {
	t.Helper()
	db, err := DB()
	if err != nil {
		t.Fatal(err)
	}
	state, err := InspectCredentialKey(db)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func TestStartRefusesACredentialKeyTheDatabaseWasNotEncryptedWith(t *testing.T) {
	useCredentialKeyTestState(t)
	original, other := fixtureCredentialKey(1), fixtureCredentialKey(2)
	if err := restartWithCredentialKey(original); err != nil {
		t.Fatal(err)
	}
	human := storeFixtureConnection(t, "fixture:key-owner", "fixture-connection-secret")

	err := restartWithCredentialKey(other)
	if !errors.Is(err, ErrCredentialKeyMismatch) ||
		!strings.Contains(err.Error(), "does not match the key this database was encrypted with") {
		t.Fatalf("start with another key: err=%v, want the key mismatch", err)
	}
	if state := credentialKeyState(t); state != CredentialKeyMismatch {
		t.Fatalf("credential key state=%q, want %q", state, CredentialKeyMismatch)
	}
	// The refused start changed nothing the original key relies on.
	if err := restartWithCredentialKey(original); err != nil {
		t.Fatal(err)
	}
	secret, _, ok, err := ProviderConnectionSecret(human.ID, "fixture", "")
	if err != nil || !ok || secret != "fixture-connection-secret" {
		t.Fatalf("connection after the refused start: ok=%v err=%v", ok, err)
	}
}

// A key that changes before anything is encrypted protects nothing, so the
// start records the new one instead of refusing it.
func TestStartAcceptsANewKeyForADatabaseWithoutCredentials(t *testing.T) {
	useCredentialKeyTestState(t)
	original, other := fixtureCredentialKey(3), fixtureCredentialKey(4)
	if err := restartWithCredentialKey(original); err != nil {
		t.Fatal(err)
	}
	if state := credentialKeyState(t); state != CredentialKeyMatches {
		t.Fatalf("first start recorded state %q, want %q", state, CredentialKeyMatches)
	}
	if err := restartWithCredentialKey(other); err != nil {
		t.Fatalf("start with a new key and no credentials: %v", err)
	}
	if state := credentialKeyState(t); state != CredentialKeyMatches {
		t.Fatalf("new key state %q, want %q", state, CredentialKeyMatches)
	}
	configureCredentialKey(original)
	if state := credentialKeyState(t); state != CredentialKeyMismatch {
		t.Fatalf("replaced key state %q, want %q", state, CredentialKeyMismatch)
	}
}

// A database an earlier release encrypted has no check value. It starts as
// before, and records the key once the key opens one of its credentials.
func TestStartRecordsTheKeyOfADatabaseWithoutACheckValue(t *testing.T) {
	useCredentialKeyTestState(t)
	original, other := fixtureCredentialKey(5), fixtureCredentialKey(6)
	if err := restartWithCredentialKey(original); err != nil {
		t.Fatal(err)
	}
	storeFixtureConnection(t, "fixture:earlier-owner", "fixture-earlier-secret")
	db, err := DB()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("DELETE FROM control_metadata WHERE key=?", credentialKeyCheckName); err != nil {
		t.Fatal(err)
	}

	if err := restartWithCredentialKey(other); err != nil {
		t.Fatalf("start of an unchecked database with a key that opens nothing: %v", err)
	}
	if state := credentialKeyState(t); state != CredentialKeyNotRecorded {
		t.Fatalf("a key that opened nothing was recorded: state %q", state)
	}
	if err := restartWithCredentialKey(original); err != nil {
		t.Fatal(err)
	}
	if state := credentialKeyState(t); state != CredentialKeyMatches {
		t.Fatalf("the key that opened a credential was not recorded: state %q", state)
	}
	if err := restartWithCredentialKey(other); !errors.Is(err, ErrCredentialKeyMismatch) {
		t.Fatalf("start with another key after recording: err=%v, want the key mismatch", err)
	}
}

func TestInspectCredentialKeyReportsAMissingOrInvalidKey(t *testing.T) {
	useCredentialKeyTestState(t)
	if err := restartWithCredentialKey(fixtureCredentialKey(7)); err != nil {
		t.Fatal(err)
	}
	config.Update(func(s *config.Settings) { s.CredentialEncryptionKey = "" })
	if state := credentialKeyState(t); state != CredentialKeyNotConfigured {
		t.Fatalf("state without a key %q, want %q", state, CredentialKeyNotConfigured)
	}
	config.Update(func(s *config.Settings) { s.CredentialEncryptionKey = "too-short" })
	if state := credentialKeyState(t); state != CredentialKeyInvalid {
		t.Fatalf("state with an invalid key %q, want %q", state, CredentialKeyInvalid)
	}
}

func TestDecryptRefusesANonceOfTheWrongSize(t *testing.T) {
	key := fixtureCredentialKey(8)
	ciphertext, nonce, err := encryptCredential(key, []byte("fixture"), []byte("aad"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decryptCredential(key, ciphertext, nonce[:4], []byte("aad")); err == nil {
		t.Fatal("a short nonce decrypted")
	}
}
