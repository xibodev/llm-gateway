package providers

import (
	"context"
	"strings"
	"sync"

	"github.com/xibodev/llm-provider-auth/tokenstore"
)

// codexCall is one Codex operation as the credential store and the refresh
// see it when a tokenstore.Coordinator, the core Runtime's or the gateway's
// own, runs them on the operation's behalf. It travels in the operation's
// context, which the Coordinator passes to both.
type codexCall struct {
	principalID, providerID string

	mu sync.Mutex
	// account pins a scope to the ChatGPT account of the first record it
	// loads; see pin.
	account string
	pinned  bool
	// rejection is the failure of the refresh the core Runtime runs after
	// an upstream 401. The Runtime then returns the 401, not the failure.
	rejection error
}

type codexCallKey struct{}

// withCodexCall returns ctx carrying a new call of principalID on providerID.
func withCodexCall(ctx context.Context, principalID, providerID string) (context.Context, *codexCall) {
	call := &codexCall{principalID: principalID, providerID: providerID}
	return context.WithValue(ctx, codexCallKey{}, call), call
}

// codexCallFrom returns the call ctx carries, or nil. Every method accepts a
// nil call and then does nothing.
func codexCallFrom(ctx context.Context) *codexCall {
	call, _ := ctx.Value(codexCallKey{}).(*codexCall)
	return call
}

// attempt starts a new pin scope. The operation calls it before each request
// it sends upstream, the core Runtime's replay included.
//
// A scope is therefore the credential fetch before one request: the
// Coordinator's Token before the first, its Rejected before the replay. That
// is exactly the pin the Codex path had. It checked the account within one
// prepare and within one refresh, but not across the replay, so a replay
// after the connection was signed in again to another account used the new
// sign-in, and still does.
func (c *codexCall) attempt() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pinned, c.account, c.rejection = false, "", nil
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
func (c *codexCall) pin(record tokenstore.Record) error {
	if c == nil {
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
func (c *codexCall) reject(err error) {
	if c == nil || err == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rejection = err
}

func (c *codexCall) rejected() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rejection
}

// forget drops the caches a write to the caller's credential invalidates, as
// the Codex path did after each refresh it stored and each revocation.
func (c *codexCall) forget(runtime *Runtime) {
	if c == nil {
		return
	}
	runtime.ForgetProviderForPrincipal(c.providerID, c.principalID)
	runtime.ForgetCatalogForPrincipal(c.providerID, c.principalID)
}
