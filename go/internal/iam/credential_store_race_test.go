package iam

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/xibodev/llm-provider-auth/tokenstore"
)

func TestCredentialStoreConcurrentReplaceHasOneWinner(t *testing.T) {
	path := credentialStatePath(t)
	ctx := context.Background()
	human, _ := CreatePrincipal("human", "fixture:race", "", "Race")
	connection := fullOAuthConnection(t, human.ID)
	first := openCredentialStore(t, path, CredentialStoreOptions{})
	current, err := first.Load(ctx, connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	const writers = 8
	stores := make([]*CredentialStore, writers)
	for index := range stores {
		stores[index] = openCredentialStore(t, path, CredentialStoreOptions{})
	}
	var group sync.WaitGroup
	start := make(chan struct{})
	results := make([]error, writers)
	for index := range writers {
		group.Add(1)
		go func() {
			defer group.Done()
			next := current.Clone()
			next.AccessToken = fmt.Sprintf("fixture-access-writer-%d", index)
			<-start
			_, results[index] = stores[index].ReplaceIfCurrent(ctx, connection.ID, current.Revision, next)
		}()
	}
	close(start)
	group.Wait()
	winner := -1
	for index, err := range results {
		switch {
		case err == nil && winner < 0:
			winner = index
		case err == nil:
			t.Fatalf("writers %d and %d both replaced revision %s", winner, index, current.Revision)
		case !errors.Is(err, tokenstore.ErrConflict):
			t.Fatalf("writer %d: err=%v, want ErrConflict", index, err)
		}
	}
	if winner < 0 {
		t.Fatal("no writer replaced the current revision")
	}
	stored, err := first.Load(ctx, connection.ID)
	if err != nil || stored.AccessToken != fmt.Sprintf("fixture-access-writer-%d", winner) {
		t.Fatalf("stored record=%s err=%v, want writer %d", stored, err, winner)
	}
}

func TestCredentialStoreRevisionFencesStaleWriters(t *testing.T) {
	path := credentialStatePath(t)
	ctx := context.Background()
	human, _ := CreatePrincipal("human", "fixture:fence", "", "Fence")
	connection := fullOAuthConnection(t, human.ID)
	store := openCredentialStore(t, path, CredentialStoreOptions{})
	stale, err := store.Load(ctx, connection.ID)
	if err != nil {
		t.Fatal(err)
	}
	// A reauthorization through the gateway's own path rotates the revision.
	fullOAuthConnection(t, human.ID)
	if _, err := store.ReplaceIfCurrent(ctx, connection.ID, stale.Revision, stale); !errors.Is(err, tokenstore.ErrConflict) {
		t.Fatalf("stale replace after a login: err=%v, want ErrConflict", err)
	}
	if err := store.RevokeIfCurrent(ctx, connection.ID, stale.Revision); !errors.Is(err, tokenstore.ErrConflict) {
		t.Fatalf("stale revoke after a login: err=%v, want ErrConflict", err)
	}
	for _, revision := range []string{"", "0", "-1", "01", " 1", "not-a-revision"} {
		if _, err := store.ReplaceIfCurrent(ctx, connection.ID, revision, stale); !errors.Is(err, tokenstore.ErrConflict) {
			t.Fatalf("replace with revision %q: err=%v, want ErrConflict", revision, err)
		}
	}
	current, _ := store.Load(ctx, connection.ID)
	if err := SetPrincipalStatus(human.ID, "disabled"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(ctx, connection.ID); !errors.Is(err, tokenstore.ErrNotFound) {
		t.Fatalf("Load for a disabled owner: err=%v, want ErrNotFound", err)
	}
	if _, err := store.ReplaceIfCurrent(ctx, connection.ID, current.Revision, current); !errors.Is(err, tokenstore.ErrConflict) {
		t.Fatalf("replace for a disabled owner: err=%v, want ErrConflict", err)
	}
	if err := store.RevokeIfCurrent(ctx, connection.ID, current.Revision); !errors.Is(err, tokenstore.ErrNotFound) {
		t.Fatalf("revoke for a disabled owner: err=%v, want ErrNotFound", err)
	}
}
