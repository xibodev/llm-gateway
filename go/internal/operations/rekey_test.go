package operations

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
)

func TestRekeyRefusesWhileTheGatewayHoldsTheState(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	old := *config.Get()
	t.Cleanup(func() {
		iam.ResetForTests()
		config.Update(func(s *config.Settings) { *s = old })
	})
	current := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	next := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32))
	config.Update(func(s *config.Settings) {
		s.CredentialEncryptionKey = current
		s.Providers = map[string]*config.ProviderConfig{}
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	iam.ResetForTests()

	gateway, err := AcquireStateLock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RekeyCredentials(current, next); !errors.Is(err, ErrStateInUse) {
		_ = gateway.Release()
		t.Fatalf("rekey while the state is in use: err=%v, want %v", err, ErrStateInUse)
	}
	if err := gateway.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := RekeyCredentials(current, next); err != nil {
		t.Fatalf("rekey once the state is free: %v", err)
	}
	config.Update(func(s *config.Settings) { s.CredentialEncryptionKey = next })
	if _, err := iam.Initialize(); err != nil {
		t.Fatalf("start with the new key: %v", err)
	}
}

func TestRekeyRequiresAnExistingState(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	current := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32))
	next := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{4}, 32))
	if _, err := RekeyCredentials(current, next); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("rekey without a state database: err=%v", err)
	}
}
