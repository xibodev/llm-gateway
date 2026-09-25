package providers

import (
	"context"
	"strings"
	"sync"

	"github.com/xibodev/llm-provider-auth/tokenstore"
)

// oauthCall is one operation on a caller's own OAuth connection, as the
// credential store and the refresh see it when a tokenstore.Coordinator, the
// core Runtime's or the gateway's own, runs them on the operation's behalf.
// It travels in the operation's context, which the Coordinator passes to
// both. Its vertical decides what they do beyond the Coordinator.
type oauthCall struct {
	vertical                oauthVertical
	principalID, providerID string

	mu sync.Mutex
	// account pins a scope to the ChatGPT account of the first record it
	// loads; see pin.
	account string
	pinned  bool
	// attempts counts the requests the operation sent upstream.
	attempts int
	// rejection is the failure of the refresh the core Runtime runs after
	// an upstream 401. The Runtime then returns the 401, not the failure,
	// and Codex reports the failure instead. Antigravity reports every 401
	// the Runtime did not replay alike, whatever failed; see replayed.
	rejection error
}

// oauthVertical is the vertical an operation serves: the gateway path whose
// rules the store and the refresh keep for it.
type oauthVertical int

const (
	// codexVertical is the zero value, so an operation without a call,
	// such as a test's direct read, keeps the rules the store had first.
	codexVertical oauthVertical = iota
	antigravityVertical
)

type oauthCallKey struct{}

// withOAuthCall returns ctx carrying a new call of principalID on providerID,
// which serves vertical.
func withOAuthCall(ctx context.Context, vertical oauthVertical, principalID, providerID string) (context.Context, *oauthCall) {
	call := &oauthCall{vertical: vertical, principalID: principalID, providerID: providerID}
	return context.WithValue(ctx, oauthCallKey{}, call), call
}

// oauthCallFrom returns the call ctx carries, or nil. Every method accepts a
// nil call and then does nothing.
func oauthCallFrom(ctx context.Context) *oauthCall {
	call, _ := ctx.Value(oauthCallKey{}).(*oauthCall)
	return call
}

// attempt counts a request and starts a new pin scope. The operation calls it
// before each request it sends upstream, the core Runtime's replay included.
//
// A scope is therefore the credential fetch before one request: the
// Coordinator's Token before the first, its Rejected before the replay. That
// is exactly the pin the Codex path had. It checked the account within one
// prepare and within one refresh, but not across the replay, so a replay
// after the connection was signed in again to another account used the new
// sign-in, and still does.
func (c *oauthCall) attempt() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pinned, c.account, c.rejection = false, "", nil
	c.attempts++
}

// replayed reports whether the operation sent a request upstream again. The
// core Runtime replays only after a refresh that followed a 401 succeeded.
func (c *oauthCall) replayed() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.attempts > 1
}

// pin fails when record belongs to another account than the first record of
// the scope, when both name one. The Coordinator checks only that a refresh
// keeps the account of the record it refreshed. When its compare-and-swap
// loses to another write, it serves the record that won, and when the record
// under the lease is already fresh, it serves that one; a sign-in to another
// account between the first load and either of those would otherwise switch
// the request's account. The Codex path refused both, so this does too. The
// third check that path made cannot fail any more: it reread the connection
// after storing a refresh, and a sign-in could land in between, but the
// Coordinator hands out the record it stored instead of rereading.
//
// Only Codex pins. The Antigravity path never compared accounts, and
// Google's token endpoint names none.
func (c *oauthCall) pin(record tokenstore.Record) error {
	if c == nil || c.vertical != codexVertical {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.pinned {
		c.pinned, c.account = true, strings.TrimSpace(record.AccountID)
		return nil
	}
	if codexAccountMismatch(c.account, record.AccountID) {
		return errCodexAccountChanged()
	}
	return nil
}

// reject records a failure of the refresh that followed the last attempt.
// The next attempt clears it, so a failure that did not stop the replay is
// never reported.
func (c *oauthCall) reject(err error) {
	if c == nil || err == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rejection = err
}

func (c *oauthCall) rejected() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rejection
}

// forget drops the caches a write to the caller's credential invalidates, as
// each path did after each refresh it stored and each revocation. Codex
// dropped the owner's catalog outright. Antigravity only retired the rows
// cached before the write, because its operations store the project they
// discover, and a catalog the writing operation fetched must still be
// stored; see catalogCache.forgetAfterProviderPersistence.
func (c *oauthCall) forget(runtime *Runtime) {
	if c == nil {
		return
	}
	runtime.ForgetProviderForPrincipal(c.providerID, c.principalID)
	if c.antigravity() {
		runtime.catalogs.forgetAfterProviderPersistence(c.providerID, c.principalID)
		return
	}
	runtime.ForgetCatalogForPrincipal(c.providerID, c.principalID)
}

// The reports below are the failures the store returns, as the path of the
// call's vertical returned them; a nil call gets Codex's. The Antigravity
// facade reports every failure but the upstream's as its own operation's
// (see adaptAntigravityError), so Antigravity passes the store's on as they
// are, and only a missing connection gets a report of its own.

// noConnection is the failure of a caller without a connection. It must not
// match core.ErrNoCredential, on which the Runtime sends the request without
// a credential.
func (c *oauthCall) noConnection() error {
	if c.antigravity() {
		return errNoAntigravityConnection()
	}
	return errNoCodexConnection()
}

// noRefreshToken is why a refresh of a record without a refresh token fails.
func (c *oauthCall) noRefreshToken() error {
	if c.antigravity() {
		return errNoAntigravityRefreshToken()
	}
	return errNoCodexRefreshToken()
}

// loadFailure reports a failure to read the caller's connection.
func (c *oauthCall) loadFailure(err error) error {
	if c.antigravity() {
		return err
	}
	return codexLoadFailure(err)
}

// storeFailure reports a failure to store the caller's refreshed connection.
func (c *oauthCall) storeFailure(err error) error {
	if c.antigravity() {
		return err
	}
	return &reportedError{report: invocation("openai_codex: store refreshed connection: " + err.Error()), cause: err}
}

func (c *oauthCall) antigravity() bool { return c != nil && c.vertical == antigravityVertical }
