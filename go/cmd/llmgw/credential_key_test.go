package main

import (
	"bytes"
	"encoding/base64"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
)

func encodedFixtureKey(seed byte) string {
	return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{seed}, 32))
}

// useStateWithCredential makes a state directory whose database holds one
// provider connection encrypted with the key seed derives, and closes it.
func useStateWithCredential(t *testing.T, seed byte) {
	t.Helper()
	state := t.TempDir()
	t.Setenv("LLMGW_STATE_DIR", state)
	t.Setenv("LLMGW_CONFIG", filepath.Join(state, "config.yaml"))
	t.Setenv("LLMGW_CONFIG_SEED", "")
	t.Setenv("LLMGW_PROVIDER_ROSTER_DISABLE", "1")
	t.Setenv("LLMGW_CREDENTIAL_ENCRYPTION_KEY", encodedFixtureKey(seed))
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	if _, err := config.Load(); err != nil {
		t.Fatal(err)
	}
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	owner, err := iam.CreatePrincipal("human", "fixture:cli-owner", "", "CLI Owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := iam.PutProviderConnection(iam.ProviderConnectionCreate{
		PrincipalID: owner.ID, ProviderID: "fixture", Kind: "api_key", Secret: "fixture-cli-secret",
	}); err != nil {
		t.Fatal(err)
	}
	iam.ResetForTests()
}

func TestServeRefusesACredentialKeyTheDatabaseWasNotEncryptedWith(t *testing.T) {
	useStateWithCredential(t, 1)
	t.Setenv("LLMGW_CREDENTIAL_ENCRYPTION_KEY", encodedFixtureKey(2))
	// A busy port fails a start that got past the key check, rather than
	// leaving it serving.
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	_, port, _ := net.SplitHostPort(busy.Addr().String())
	t.Setenv("LLMGW_HOST", "127.0.0.1")
	t.Setenv("LLMGW_PORT", port)

	err = serve()
	if err == nil || !strings.Contains(err.Error(), "does not match the key this database was encrypted with") {
		t.Fatalf("serve with another credential encryption key returned %v", err)
	}
}

func TestBackupReportsWhetherTheCredentialKeyMatches(t *testing.T) {
	useStateWithCredential(t, 3)
	archive := filepath.Join(t.TempDir(), "state.tar.gz")
	var created bytes.Buffer
	if err := backupCommand([]string{"create", archive}, &created); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(created.String(), "credential_key: matches\n") {
		t.Fatalf("create output %q does not report the matching key", created.String())
	}

	t.Setenv("LLMGW_CREDENTIAL_ENCRYPTION_KEY", encodedFixtureKey(4))
	if _, err := config.Load(); err != nil {
		t.Fatal(err)
	}
	var inspected, restored bytes.Buffer
	if err := backupCommand([]string{"inspect", archive}, &inspected); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(inspected.String(), "credential_key: does not match\n") {
		t.Fatalf("inspect output %q does not report the mismatch", inspected.String())
	}
	if err := backupCommand([]string{"restore", archive, "--force"}, &restored); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(restored.String(), "credential_key: does not match\n") ||
		!strings.Contains(restored.String(), "warning: LLMGW_CREDENTIAL_ENCRYPTION_KEY is not the key") {
		t.Fatalf("restore output %q does not warn about the mismatch", restored.String())
	}
	for _, output := range []string{created.String(), inspected.String(), restored.String()} {
		if strings.Contains(output, "fixture-cli-secret") {
			t.Fatal("backup output exposed a stored credential")
		}
	}
}
