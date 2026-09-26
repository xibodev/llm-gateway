package api

import (
	"context"
	"errors"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"

	"github.com/xibodev/llm-provider-auth/tokenstore"
	core "github.com/xibodev/llmgw-core"
	"github.com/xibodev/llmgw-core/oauthflow"
)

// maxOAuthFlowsPerPrincipal caps a principal's pending OAuth flows; starting
// one more evicts the oldest.
const maxOAuthFlowsPerPrincipal = 5

// Start parameters the gateway keeps with a flow for its completion. A
// manual flow's OAuth client travels with them, in the parameters
// providers.ProviderAuthManualConfig.OAuthParams names.
const (
	oauthParamConnectionName = "connection_name"
	oauthParamSource         = "source"
	oauthParamKind           = "credential_kind"
	oauthParamPersistProfile = "persist_profile"
)

// oauthCaller is the core.Caller of the principal an OAuth route acts for:
// the SSO user, or the principal an administrator's route names. A flow
// belongs to it, so only routes acting for that principal reach the flow.
func oauthCaller(principal iam.Principal) core.Caller {
	return requestCaller(sourcePrincipal, &config.Principal{PrincipalID: principal.ID, PrincipalKind: principal.Kind})
}

// oauthConnectionParams are the start parameters of a browser or manual
// flow, whose start names the connection its completion stores.
func oauthConnectionParams(connectionName, source, kind string) map[string]string {
	return map[string]string{oauthParamConnectionName: connectionName, oauthParamSource: source, oauthParamKind: kind}
}

// oauthExpiresIn is the lifetime a start response reports, in the whole
// seconds the driver named: the flow's expiry less when the start began.
func oauthExpiresIn(view oauthflow.View, startedAt time.Time) int {
	return int(view.ExpiresAt.Sub(startedAt) / time.Second)
}

// oauthCompletion is what a route that can finish a flow tells the key hook
// about its request, and what the hook tells the route back. The Service
// hands the hook the route's context and nothing else of the request.
type oauthCompletion struct {
	// providerRef is the provider the route names. A manual flow's profile
	// and provider settings follow from it.
	providerRef string
	// name and source name a device flow's connection: the console names
	// it when it polls, not when it starts.
	name, source string
	// connection is the connection the hook stored; failure is what the
	// owner is told when storing it failed.
	connection *iam.ProviderConnection
	failure    string
}

type oauthCompletionKey struct{}

// withOAuthCompletion returns ctx carrying completion for the key hook.
func withOAuthCompletion(ctx context.Context, completion *oauthCompletion) context.Context {
	return context.WithValue(ctx, oauthCompletionKey{}, completion)
}

func oauthCompletionFrom(ctx context.Context) *oauthCompletion {
	completion, _ := ctx.Value(oauthCompletionKey{}).(*oauthCompletion)
	return completion
}

// fail records what the owner is told about a completion that could not be
// stored, and returns it as the hook's error.
func (c *oauthCompletion) fail(detail string) error {
	c.failure = detail
	return errors.New(detail)
}

// oauthDriver resolves the driver of instance, the provider ID
// oauthAdapterFor resolved when the flow started, through the instance's
// registry auth adapter.
func (s *server) oauthDriver(instance string, method oauthflow.Method) (oauthflow.Driver, error) {
	_, _, adapter, err := oauthAdapterFor(instance)
	if err != nil {
		return nil, err
	}
	return s.providers().OAuthDriver(adapter.ID(), instance, method)
}

// takeOAuthOutcome hands the connection a browser flow stored to the one
// poll that claims it. The callback that completed the flow carried no
// session, so its owner learns the outcome from a poll, and, as before, from
// exactly one: taking the key out of the flow claims it, and the store's
// revision check lets one of concurrent polls win.
func (s *server) takeOAuthOutcome(ctx context.Context, caller core.Caller, id string) (string, bool) {
	flow, err := s.oauthStore.Get(ctx, caller, id)
	if err != nil || !flow.Consumed() || flow.Progress.CredentialKey == "" {
		return "", false
	}
	key := flow.Progress.CredentialKey
	flow.Progress.CredentialKey = ""
	if _, err := s.oauthStore.Update(ctx, caller, flow); err != nil {
		return "", false
	}
	return key, true
}

// oauthConnectionStore is the credential store the Service saves completed
// flows through. storeOAuthConnection has already stored the connection, so
// Save only confirms that the key is the connection that hook stored for
// this completion: saving the record again would change what the gateway
// writes. The Service calls nothing else.
type oauthConnectionStore struct{}

var _ core.CredentialStore = oauthConnectionStore{}

var errOAuthConnectionStore = errors.New("the OAuth flow credential store only confirms completed connections")

// Save implements tokenstore.Store.
func (oauthConnectionStore) Save(ctx context.Context, key string, record tokenstore.Record) (tokenstore.Record, error) {
	if completion := oauthCompletionFrom(ctx); completion != nil && completion.connection != nil && completion.connection.ID == key {
		return record, nil
	}
	return tokenstore.Record{}, errOAuthConnectionStore
}

// Load implements tokenstore.Store.
func (oauthConnectionStore) Load(context.Context, string) (tokenstore.Record, error) {
	return tokenstore.Record{}, errOAuthConnectionStore
}

// ReplaceIfCurrent implements tokenstore.Store.
func (oauthConnectionStore) ReplaceIfCurrent(context.Context, string, string, tokenstore.Record) (tokenstore.Record, error) {
	return tokenstore.Record{}, errOAuthConnectionStore
}

// RevokeIfCurrent implements tokenstore.Store.
func (oauthConnectionStore) RevokeIfCurrent(context.Context, string, string) error {
	return errOAuthConnectionStore
}

// Lease implements tokenstore.Store.
func (oauthConnectionStore) Lease(context.Context, string) (func(), error) {
	return nil, errOAuthConnectionStore
}

// Resolve implements core.CredentialStore.
func (oauthConnectionStore) Resolve(context.Context, core.Caller, string) (string, error) {
	return "", errOAuthConnectionStore
}
