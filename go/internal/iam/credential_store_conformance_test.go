package iam

import (
	"context"
	"database/sql"
	"testing"

	"github.com/xibodev/llm-provider-auth/tokenstore"
	"github.com/xibodev/llm-provider-auth/tokenstore/storetest"
)

// conformanceStore places every key it saves on a fixture connection before
// the save, the way the gateway creates a connection before it stores any
// credential in it: a key alone does not say which principal owns it. Every
// other method is the store under test, unwrapped.
type conformanceStore struct {
	*CredentialStore
	principalID string
}

func (s conformanceStore) Save(ctx context.Context, key string, record tokenstore.Record) (tokenstore.Record, error) {
	if err := placeFixtureConnection(ctx, s.db, s.principalID, key); err != nil {
		return tokenstore.Record{}, err
	}
	return s.CredentialStore.Save(ctx, key, record)
}

// placeFixtureConnection creates a revoked OAuth connection with ID key. A
// revoked connection holds no usable credential, so Load still reports the key
// missing until the first Save activates it.
func placeFixtureConnection(ctx context.Context, db *sql.DB, principalID, key string) error {
	if _, err := db.ExecContext(ctx, `
INSERT INTO provider_connections(
    id,principal_id,provider_id,connection_name,credential_kind,source,
    private_to_principal,is_default,ciphertext,nonce,status,created_at,updated_at
) VALUES(?,?,'fixture',?,'fixture_oauth','user',1,0,X'00',X'00','revoked',0,0)
ON CONFLICT DO NOTHING`, key, principalID, key); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `
INSERT INTO provider_account_state(connection_id,updated_at) VALUES(?,0)
ON CONFLICT DO NOTHING`, key)
	return err
}

func TestCredentialStoreConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) storetest.Opener {
		path := credentialStatePath(t)
		owner, err := CreatePrincipal("human", "fixture:conformance", "", "Conformance")
		if err != nil {
			t.Fatal(err)
		}
		return func(t *testing.T) tokenstore.Store {
			return conformanceStore{
				CredentialStore: openCredentialStore(t, path, CredentialStoreOptions{}),
				principalID:     owner.ID,
			}
		}
	})
}
