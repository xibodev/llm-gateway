package iam

import (
	"context"
	"errors"
	"testing"

	"llmgw/internal/config"

	"github.com/xibodev/llm-provider-auth/tokenstore"
	core "github.com/xibodev/llmgw-core"
)

func TestReservedCredentialKeysAreReadOnly(t *testing.T) {
	f := newResolveFixture(t)
	ctx := context.Background()
	store := f.stores[ConnectionPrecedence]
	configured := ConfiguredCredentialKeyPrefix + "fixture-configured"
	shared := ProviderCredentialKeyPrefix + f.shared.ID
	for _, key := range []string{configured, shared} {
		record, err := store.Load(ctx, key)
		if err != nil || record.Revision != readOnlyCredentialRevision {
			t.Fatalf("Load %s: revision=%q err=%v", key, record.Revision, err)
		}
		if _, err := store.Save(ctx, key, record); !errors.Is(err, ErrReadOnlyCredential) {
			t.Fatalf("Save %s: err=%v, want ErrReadOnlyCredential", key, err)
		}
		if _, err := store.ReplaceIfCurrent(ctx, key, record.Revision, record); !errors.Is(err, ErrReadOnlyCredential) {
			t.Fatalf("ReplaceIfCurrent %s: err=%v, want ErrReadOnlyCredential", key, err)
		}
		if err := store.RevokeIfCurrent(ctx, key, record.Revision); !errors.Is(err, ErrReadOnlyCredential) {
			t.Fatalf("RevokeIfCurrent %s: err=%v, want ErrReadOnlyCredential", key, err)
		}
	}
	if record, _ := store.Load(ctx, configured); record.TokenType != core.TokenTypeAPIKey ||
		record.AccessToken != "fixture-configured-key" {
		t.Fatalf("configured key record=%s", record)
	}
	// A configuration reload applies on the next Load, without a new key.
	setProviderConfig(t, "fixture-configured", &config.ProviderConfig{APIKey: "fixture-configured-key-2"})
	if record, _ := store.Load(ctx, configured); record.AccessToken != "fixture-configured-key-2" {
		t.Fatal("Load kept a configured key the configuration replaced")
	}
	setProviderConfig(t, "fixture-configured", &config.ProviderConfig{})
	if _, err := store.Load(ctx, configured); !errors.Is(err, tokenstore.ErrNotFound) {
		t.Fatalf("removed configured key: err=%v, want ErrNotFound", err)
	}
	if record, _ := store.Load(ctx, shared); record.AccessToken != "fixture-shared-token" ||
		record.TokenType == core.TokenTypeAPIKey {
		t.Fatalf("shared credential record=%s", record)
	}
	if err := SetProviderCredentialStatus(f.shared.ID, "disabled"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(ctx, shared); !errors.Is(err, tokenstore.ErrNotFound) {
		t.Fatalf("disabled shared credential: err=%v, want ErrNotFound", err)
	}
}

func TestResolveSelectsPrecedencePerInstanceAndValidates(t *testing.T) {
	f := newResolveFixture(t)
	store, err := NewCredentialStore(f.stores[ConnectionPrecedence].db, CredentialStoreOptions{
		Precedence: func(instance string) CredentialPrecedence {
			if instance == "copilot" {
				return OAuthPrecedence
			}
			return ConnectionPrecedence
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if key, err := store.Resolve(ctx, f.workerCaller, " copilot "); err != nil ||
		key != ProviderCredentialKeyPrefix+f.shared.ID {
		t.Fatalf("copilot for a service: key=%q err=%v", key, err)
	}
	if key, err := store.Resolve(ctx, f.workerCaller, "fixture-openai"); err != nil || key != f.systemKey.ID {
		t.Fatalf("API-key provider for a service: key=%q err=%v", key, err)
	}
	if _, err := store.Resolve(ctx, anonymousCaller, " "); !errors.Is(err, core.ErrNoCredential) {
		t.Fatalf("empty instance: err=%v", err)
	}
	for name, caller := range map[string]core.Caller{
		"human without id": {Kind: core.CallerHuman},
		"unknown kind":     {ID: f.worker.ID, Kind: "system"},
	} {
		if _, err := store.Resolve(ctx, caller, "fixture-openai"); err == nil || errors.Is(err, core.ErrNoCredential) {
			t.Fatalf("%s: err=%v, want a validation error", name, err)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := store.Resolve(canceled, f.aliceCaller, "copilot"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled resolve: err=%v", err)
	}
	unknown, _ := NewCredentialStore(store.db, CredentialStoreOptions{
		Precedence: func(string) CredentialPrecedence { return CredentialPrecedence(99) },
	})
	if _, err := unknown.Resolve(ctx, f.aliceCaller, "copilot"); err == nil || errors.Is(err, core.ErrNoCredential) {
		t.Fatalf("unknown precedence: err=%v", err)
	}
}
