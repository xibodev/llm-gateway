package iam

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func leaseTestPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "gateway.db")
}

func mustLease(t *testing.T, store *CredentialStore, key string) func() {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	release, err := store.Lease(ctx, key)
	if err != nil {
		t.Fatalf("Lease %s: %v", key, err)
	}
	return release
}

func assertLeaseHeld(t *testing.T, store *CredentialStore, key string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	release, err := store.Lease(ctx, key)
	if err == nil {
		release()
		t.Fatalf("lease %s was acquired while another holder had it", key)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("held lease wait: err=%v, want context.DeadlineExceeded", err)
	}
}

func TestCredentialLeaseExpiredHolderIsTakenOver(t *testing.T) {
	t.Parallel()
	path := leaseTestPath(t)
	clock := newFakeClock()
	options := CredentialStoreOptions{LeaseTTL: time.Minute, Now: clock.Now}
	first := openCredentialStore(t, path, options)
	second := openCredentialStore(t, path, options)
	third := openCredentialStore(t, path, options)

	stale := mustLease(t, first, "conn_lease")
	assertLeaseHeld(t, second, "conn_lease")
	clock.Advance(time.Minute - time.Nanosecond)
	assertLeaseHeld(t, second, "conn_lease")
	clock.Advance(time.Nanosecond)
	current := mustLease(t, second, "conn_lease")

	// The overtaken holder's release is scoped to its own holder token.
	stale()
	assertLeaseHeld(t, third, "conn_lease")
	current()
	mustLease(t, third, "conn_lease")()
}

func TestCredentialLeaseWaitEndsWithContext(t *testing.T) {
	t.Parallel()
	path := leaseTestPath(t)
	holder := openCredentialStore(t, path, CredentialStoreOptions{})
	waiter := openCredentialStore(t, path, CredentialStoreOptions{})
	release := mustLease(t, holder, "conn_wait")
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		extra, err := waiter.Lease(ctx, "conn_wait")
		if err == nil {
			extra()
		}
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled wait: err=%v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a canceled lease wait did not return")
	}
	var rows int
	if err := holder.db.QueryRow("SELECT COUNT(*) FROM credential_leases").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("lease rows=%d after a canceled wait, want only the holder's", rows)
	}
}

func TestCredentialLeaseOutlivesItsAcquireContext(t *testing.T) {
	t.Parallel()
	path := leaseTestPath(t)
	holder := openCredentialStore(t, path, CredentialStoreOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	release, err := holder.Lease(ctx, "conn_outlives")
	if err != nil {
		t.Fatal(err)
	}
	// tokenstore.Coordinator cancels the wait context once Lease returns.
	cancel()
	assertLeaseHeld(t, openCredentialStore(t, path, CredentialStoreOptions{}), "conn_outlives")
	release()
	release()
	mustLease(t, holder, "conn_outlives")()
}

func TestCredentialLeaseAdmitsOneHolderAcrossHandles(t *testing.T) {
	t.Parallel()
	path := leaseTestPath(t)
	const handles, workers, rounds = 4, 3, 4
	var inside, maxInside atomic.Int32
	var group sync.WaitGroup
	errs := make(chan error, handles*workers)
	for range handles {
		store := openCredentialStore(t, path, CredentialStoreOptions{})
		for range workers {
			group.Add(1)
			go func() {
				defer group.Done()
				for range rounds {
					ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
					release, err := store.Lease(ctx, "conn_contended")
					cancel()
					if err != nil {
						errs <- err
						return
					}
					now := inside.Add(1)
					for {
						seen := maxInside.Load()
						if now <= seen || maxInside.CompareAndSwap(seen, now) {
							break
						}
					}
					time.Sleep(time.Millisecond)
					inside.Add(-1)
					release()
				}
			}()
		}
	}
	group.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("contended lease: %v", err)
	}
	if got := maxInside.Load(); got != 1 {
		t.Fatalf("%d holders shared one lease, want 1", got)
	}
}

func TestCredentialLeaseValidatesRequests(t *testing.T) {
	t.Parallel()
	store := openCredentialStore(t, leaseTestPath(t), CredentialStoreOptions{})
	if _, err := store.Lease(context.Background(), " "); err == nil {
		t.Fatal("Lease accepted an empty key")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Lease(canceled, "conn_x"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Lease on a canceled context: err=%v", err)
	}
	if _, err := NewCredentialStore(nil, CredentialStoreOptions{}); err == nil {
		t.Fatal("NewCredentialStore accepted a nil database")
	}
	if _, err := NewCredentialStore(store.db, CredentialStoreOptions{LeaseTTL: -time.Second}); err == nil {
		t.Fatal("NewCredentialStore accepted a negative lease TTL")
	}
	if store.leaseTTL != DefaultCredentialLeaseTTL {
		t.Fatalf("default lease TTL=%v", store.leaseTTL)
	}
}
