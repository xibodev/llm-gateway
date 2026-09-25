package providers

import (
	"context"
	"fmt"

	"llmgw/internal/config"
	"llmgw/internal/iam"

	"github.com/xibodev/llm-provider-auth/tokenstore"
	core "github.com/xibodev/llmgw-core"
	coreruntime "github.com/xibodev/llmgw-core/runtime"
)

// coreVertical is one provider type the llmgw-core Runtime serves: which
// configured instances are of the type, and what the Runtime needs for them.
// Adding a type is a file that builds its coreVertical and a line in
// coreVerticals.
type coreVertical struct {
	// serves reports whether an instance configured as cfg is of the type.
	// No two types serve one instance, so the table has no order.
	serves func(settings *config.Settings, instance string, cfg *config.ProviderConfig) bool
	// provider builds the core provider of an instance.
	provider coreruntime.ProviderFactory[*config.Settings]
	// refresh returns how an instance refreshes an OAuth credential. Nil
	// means credentials of the type never refresh.
	refresh coreruntime.RefreshFactory[*config.Settings]
	// credentials resolves and stores the credentials of the type.
	credentials core.CredentialStore
}

// coreVerticals is the registration table of the provider types the core
// Runtime serves, one line per type. The Runtime holds what it returns.
func (rt *Runtime) coreVerticals() map[string]coreVertical {
	return map[string]coreVertical{
		"openai_codex":       rt.codexCoreVertical(),
		"google_antigravity": rt.antigravityCoreVertical(),
		zenCoreType:          rt.zenCoreVertical(),
		copilotCoreType:      rt.copilotCoreVertical(),
	}
}

// newCoreRuntime builds the llmgw-core Runtime that serves the provider
// instances whose vertical core owns: the Runtime holds their providers and
// the coordinators that refresh their credentials, while the gateway
// supplies the settings, the connections, the refresh and the evidence sink,
// each through the vertical of the instance.
func newCoreRuntime(rt *Runtime) (*coreruntime.Runtime[*config.Settings], error) {
	settings := config.Source{}
	return coreruntime.New(coreruntime.Options[*config.Settings]{
		Settings:    settings,
		Providers:   rt.coreProvider,
		Credentials: coreCredentials{runtime: rt, settings: settings},
		Refresh:     rt.coreRefresh,
		Evidence:    credentialEvidence{},
		// Catalogs stays nil. The gateway lists the models of these
		// instances on its own catalog path and never asks the Runtime,
		// so the Runtime's in-memory catalog stays empty.
	})
}

// vertical returns the name and the entry of the type that serves instance
// under settings.
func (rt *Runtime) vertical(settings *config.Settings, instance string) (string, coreVertical, bool) {
	if cfg := settings.Providers[instance]; cfg != nil {
		for name, vertical := range rt.verticals {
			if vertical.serves(settings, instance, cfg) {
				return name, vertical, true
			}
		}
	}
	return "", coreVertical{}, false
}

func errNotServedByCore(instance string) error {
	return &ConfigError{Msg: fmt.Sprintf("provider '%s' is not served by the core runtime", instance)}
}

// coreProvider is the Runtime's ProviderFactory: the provider of the
// instance's vertical. Other instances are not routed through the Runtime.
func (rt *Runtime) coreProvider(settings *config.Settings, instance string) (core.Provider, error) {
	_, vertical, ok := rt.vertical(settings, instance)
	if !ok {
		return nil, errNotServedByCore(instance)
	}
	return vertical.provider(settings, instance)
}

// coreRefresh is the Runtime's RefreshFactory: the refresh of the instance's
// vertical, if it has one.
func (rt *Runtime) coreRefresh(settings *config.Settings, instance string) tokenstore.RefreshFunc {
	_, vertical, ok := rt.vertical(settings, instance)
	if !ok || vertical.refresh == nil {
		return nil
	}
	return vertical.refresh(settings, instance)
}

// coreCredentials is the Runtime's one credential store, which is each
// vertical's store in turn. Resolve names an instance, so it asks the store
// of the instance's type. The token-store methods name only a key; they use
// the store of the vertical the operation's context names (see
// withCoreOperation), and without one the OAuth store, which every operation
// used before a vertical had a store of its own. The Runtime hands each
// operation's context to Resolve and to the Coordinator it then asks.
type coreCredentials struct {
	runtime  *Runtime
	settings coreruntime.SettingsSource[*config.Settings]
}

var _ core.CredentialStore = coreCredentials{}

// Resolve implements core.CredentialStore.
func (c coreCredentials) Resolve(ctx context.Context, caller core.Caller, instance string) (string, error) {
	settings, _ := c.settings.Snapshot()
	_, vertical, ok := c.runtime.vertical(settings, instance)
	if !ok {
		return "", errNotServedByCore(instance)
	}
	return vertical.credentials.Resolve(ctx, caller, instance)
}

func (c coreCredentials) store(ctx context.Context) core.CredentialStore {
	if vertical, ok := c.runtime.verticals[coreOperationFrom(ctx).vertical]; ok {
		return vertical.credentials
	}
	return c.runtime.credentials
}

// Load implements tokenstore.Store.
func (c coreCredentials) Load(ctx context.Context, key string) (tokenstore.Record, error) {
	return c.store(ctx).Load(ctx, key)
}

// Save implements tokenstore.Store.
func (c coreCredentials) Save(ctx context.Context, key string, record tokenstore.Record) (tokenstore.Record, error) {
	return c.store(ctx).Save(ctx, key, record)
}

// ReplaceIfCurrent implements tokenstore.Store.
func (c coreCredentials) ReplaceIfCurrent(ctx context.Context, key, revision string, record tokenstore.Record) (tokenstore.Record, error) {
	return c.store(ctx).ReplaceIfCurrent(ctx, key, revision, record)
}

// RevokeIfCurrent implements tokenstore.Store.
func (c coreCredentials) RevokeIfCurrent(ctx context.Context, key, revision string) error {
	return c.store(ctx).RevokeIfCurrent(ctx, key, revision)
}

// Lease implements tokenstore.Store.
func (c coreCredentials) Lease(ctx context.Context, key string) (func(), error) {
	return c.store(ctx).Lease(ctx, key)
}

// coreOperation is what an operation of a core-served type tells the stores
// and providers the core Runtime calls for it, which their arguments do not:
// the name of its vertical, and the caller whose catalog a provider reads.
type coreOperation struct {
	vertical string
	caller   core.Caller
}

type coreOperationKey struct{}

// withCoreOperation returns ctx carrying an operation of vertical for caller.
func withCoreOperation(ctx context.Context, vertical string, caller core.Caller) context.Context {
	return context.WithValue(ctx, coreOperationKey{}, coreOperation{vertical: vertical, caller: caller})
}

// coreOperationFrom returns the operation ctx names, or the zero one.
func coreOperationFrom(ctx context.Context) coreOperation {
	operation, _ := ctx.Value(coreOperationKey{}).(coreOperation)
	return operation
}

// iamCredentialStore opens the IAM credential store over the database IAM
// serves now. That handle follows the state directory, which tests change,
// so each operation opens the store rather than keeping one; opening costs
// no more than the store value.
//
// The OAuth store opens it with oauth set, for the Codex and Antigravity
// instances the core Runtime and the gateway's Coordinators serve: both
// types resolve owner-private OAuth connections, and each read marks the
// connection used, as both paths' reads did. Zen's store opens it without:
// a nil Precedence resolves every instance with ConnectionPrecedence, the
// provider factory's order, and reads mark nothing, because the factory
// marks the credential it builds a Zen facade with, which is where the Zen
// path recorded a use.
func iamCredentialStore(oauth bool) (core.CredentialStore, error) {
	db, err := iam.DB()
	if err != nil {
		return nil, err
	}
	options := iam.CredentialStoreOptions{MarkUsed: oauth}
	if oauth {
		options.Precedence = oauthPrecedence
	}
	return iam.NewCredentialStore(db, options)
}

func oauthPrecedence(string) iam.CredentialPrecedence { return iam.OAuthPrecedence }
