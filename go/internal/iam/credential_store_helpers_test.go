package iam

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// openCredentialStore opens an independent handle on path, the way another
// gateway process would, and returns a store over it.
func openCredentialStore(t *testing.T, path string, options CredentialStoreOptions) *CredentialStore {
	t.Helper()
	db, err := openDB(path)
	if err != nil {
		t.Fatalf("open credential store handle: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := NewCredentialStore(db, options)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// credentialStatePath prepares an isolated state directory with credential
// encryption, so fixtures can use the package API through DB while stores
// open their own handles on the same file.
func credentialStatePath(t *testing.T) string {
	t.Helper()
	setupCredentialTest(t)
	if _, err := DB(); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(os.Getenv("LLMGW_STATE_DIR"), "gateway.db")
}

// fakeClock is a settable clock shared by stores that must agree on time.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Unix(1_700_000_000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func formatRevision(revision int64) string { return strconv.FormatInt(revision, 10) }
