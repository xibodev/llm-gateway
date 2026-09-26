package iam

import (
	"errors"
	"reflect"
	"testing"

	"github.com/xibodev/llm-provider-auth/tokenstore"
)

// TestConditionalWritesMatchTheGatewayRefreshPaths applies each gateway OAuth
// refresh write to one database and the store's replacement to an identical
// copy, then requires the same rows, for connections owned by each kind of
// principal. tokenstore.Coordinator will take over these writes, so any
// difference would change behavior the day it does.
func TestConditionalWritesMatchTheGatewayRefreshPaths(t *testing.T) {
	type operation struct {
		name    string
		gateway func(*parityWorld, Principal) error
		store   func(*parityWorld, Principal) error
		changes bool
	}
	revoke := func(stale bool) func(*parityWorld, Principal) error {
		return func(w *parityWorld, owner Principal) error {
			revoked, err := w.gatewayRevoke(owner, stale)
			if err == nil && revoked == stale {
				return errors.New("gateway revoke outcome is not the fixture's")
			}
			return err
		}
	}
	operations := []operation{
		{"replace", func(w *parityWorld, o Principal) error { return w.gatewayReplace(o, false) },
			func(w *parityWorld, o Principal) error { return w.storeReplace(o, false) }, true},
		{"revoke", revoke(false),
			func(w *parityWorld, o Principal) error { return w.storeRevoke(o, false) }, true},
		{"replace conflict", func(w *parityWorld, o Principal) error {
			if err := w.gatewayReplace(o, true); !errors.Is(err, ErrOAuthProviderConnectionChanged) {
				return errors.Join(errors.New("gateway accepted a stale replace"), err)
			}
			return nil
		}, func(w *parityWorld, o Principal) error {
			if err := w.storeReplace(o, true); !errors.Is(err, tokenstore.ErrConflict) {
				return errors.Join(errors.New("store accepted a stale replace"), err)
			}
			return nil
		}, false},
		{"revoke conflict", revoke(true), func(w *parityWorld, o Principal) error {
			if err := w.storeRevoke(o, true); !errors.Is(err, tokenstore.ErrConflict) {
				return errors.Join(errors.New("store accepted a stale revoke"), err)
			}
			return nil
		}, false},
	}
	for _, kind := range []string{"human", "service", "system"} {
		for _, op := range operations {
			t.Run(kind+" "+op.name, func(t *testing.T) {
				w := newParityWorld(t)
				owner := w.owners[kind]
				before := paritySnapshot(t, w.store.db)
				if err := op.gateway(w, owner); err != nil {
					t.Fatalf("gateway %s: %v", op.name, err)
				}
				if err := op.store(w, owner); err != nil {
					t.Fatalf("store %s: %v", op.name, err)
				}
				after := paritySnapshot(t, w.store.db)
				if changed := !reflect.DeepEqual(before, after); changed != op.changes {
					t.Fatalf("%s changed rows=%v, want %v", op.name, changed, op.changes)
				}
				assertSameSnapshot(t, kind+" "+op.name, paritySnapshot(t, w.gateway), after)
			})
		}
	}
}

// TestSaveMatchesTheGatewayLogin compares Save with the re-authorization
// write it mirrors, for the default and a non-default connection, and
// requires both to refuse owners the gateway never lets hold OAuth.
func TestSaveMatchesTheGatewayLogin(t *testing.T) {
	for _, c := range []struct {
		kind, name string
		refused    bool
	}{
		{"human", "target", false}, {"human", "newer", false},
		{"service", "target", true}, {"system", "target", true},
	} {
		t.Run(c.kind+" "+c.name, func(t *testing.T) {
			w := newParityWorld(t)
			owner := w.owners[c.kind]
			before := paritySnapshot(t, w.store.db)
			gatewayErr, storeErr := w.gatewaySave(owner, c.name), w.storeSave(owner, c.name)
			if (gatewayErr != nil) != c.refused || (storeErr != nil) != c.refused {
				t.Fatalf("gateway err=%v store err=%v, want refused=%v", gatewayErr, storeErr, c.refused)
			}
			after := paritySnapshot(t, w.store.db)
			if changed := !reflect.DeepEqual(before, after); changed == c.refused {
				t.Fatalf("save changed rows=%v, refused=%v", changed, c.refused)
			}
			assertSameSnapshot(t, c.kind+" save", paritySnapshot(t, w.gateway), after)
		})
	}
}
