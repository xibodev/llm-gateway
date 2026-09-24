package iam

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xibodev/llm-provider-auth/tokenstore"
)

// TestCredentialStoreKeepsTokensEncryptedAtRest proves nothing the store
// writes reaches the database in plaintext: not the stored row, and not the
// database, WAL or shared-memory files, where SQLite may hold recent pages.
func TestCredentialStoreKeepsTokensEncryptedAtRest(t *testing.T) {
	path := credentialStatePath(t)
	ctx := context.Background()
	human, _ := CreatePrincipal("human", "fixture:at-rest", "", "At rest")
	connection := fullOAuthConnection(t, human.ID)
	store := openCredentialStore(t, path, CredentialStoreOptions{})
	current, err := store.Load(ctx, connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	secrets := []string{
		"fixture-access-rotated-plain", "fixture-refresh-rotated-plain", "fixture-id-rotated-plain",
		"fixture-client-secret-plain", "fixture-extra-secret-plain",
	}
	next := current.Clone()
	next.AccessToken, next.RefreshToken, next.IDToken = secrets[0], secrets[1], secrets[2]
	next.Expiry = time.Now().Add(time.Hour)
	next.Metadata[CredentialMetadataOAuthClientSecret] = secrets[3]
	next.Metadata["fixture-extra"] = secrets[4]
	replaced, err := store.ReplaceIfCurrent(ctx, connection.ID, current.Revision, next)
	if err != nil {
		t.Fatal(err)
	}
	saved := next.Clone()
	saved.AccessToken = "fixture-access-saved-plain"
	secrets = append(secrets, saved.AccessToken)
	if _, err := store.Save(ctx, connection.ID, saved); err != nil {
		t.Fatal(err)
	}
	if loaded, err := store.Load(ctx, connection.ID); err != nil || loaded.AccessToken != saved.AccessToken ||
		loaded.Revision == replaced.Revision {
		t.Fatalf("Load after writes: %s err=%v", loaded, err)
	}

	var row strings.Builder
	var ciphertext, nonce []byte
	var kind, name string
	if err := store.db.QueryRow(`
SELECT ciphertext,nonce,credential_kind,connection_name FROM provider_connections WHERE id=?`,
		connection.ID).Scan(&ciphertext, &nonce, &kind, &name); err != nil {
		t.Fatal(err)
	}
	row.Write(ciphertext)
	row.Write(nonce)
	row.WriteString(kind + name)
	assertNoPlaintext(t, "stored row", row.String(), secrets)

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	// The provider ID is stored in plaintext; finding it proves the scan
	// reads the pages the credential was written to.
	sawPlaintextColumn := false
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), filepath.Base(path)) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(filepath.Dir(path), entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		sawPlaintextColumn = sawPlaintextColumn || strings.Contains(string(raw), "fixture-codex")
		assertNoPlaintext(t, entry.Name(), string(raw), secrets)
	}
	if !sawPlaintextColumn {
		t.Fatal("the database files did not show a plaintext column; the scan proves nothing")
	}
}

func assertNoPlaintext(t *testing.T, where, content string, secrets []string) {
	t.Helper()
	for _, secret := range secrets {
		if strings.Contains(content, secret) {
			t.Fatalf("%s contains a credential in plaintext", where)
		}
	}
}

// TestCredentialStoreErrorsNeverCarryTokens pins that write failures do not
// echo token material, since errors are routinely logged and reported.
func TestCredentialStoreErrorsNeverCarryTokens(t *testing.T) {
	path := credentialStatePath(t)
	ctx := context.Background()
	store := openCredentialStore(t, path, CredentialStoreOptions{})
	if _, err := PutSystemProviderConnection("fixture-openai", "api_key", "fixture-api-key-plain"); err != nil {
		t.Fatal(err)
	}
	system, _ := EnsureSystemPrincipal()
	connection, _, _ := ActiveProviderConnection(system.ID, "fixture-openai")
	current, err := store.Load(ctx, connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	mismatch := tokenstore.Record{AccessToken: "fixture-access-plain", RefreshToken: "fixture-refresh-plain"}
	for _, err := range []error{
		func() error {
			_, err := store.ReplaceIfCurrent(ctx, connection.ID, current.Revision, mismatch)
			return err
		}(),
		func() error { _, err := store.ReplaceIfCurrent(ctx, connection.ID, "999", mismatch); return err }(),
		func() error { _, err := store.Save(ctx, "conn_missing", mismatch); return err }(),
	} {
		if err == nil {
			t.Fatal("an invalid write succeeded")
		}
		assertNoPlaintext(t, "error", err.Error(), []string{"fixture-access-plain", "fixture-refresh-plain", "fixture-api-key-plain"})
	}
}
