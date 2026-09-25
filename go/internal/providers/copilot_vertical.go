package providers

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	"llmgw/internal/config"

	"github.com/xibodev/llm-provider-auth/tokenstore"
	"github.com/xibodev/llm-translate"
	core "github.com/xibodev/llmgw-core"
	coreproviders "github.com/xibodev/llmgw-core/providers"
)

// copilotCoreType is GitHub Copilot's key in coreVerticals. The facade names
// it on each operation, so the core Runtime's credential store loads Copilot's
// credentials from Copilot's store.
const copilotCoreType = "github_copilot"

// The product the gateway names to Copilot beside the configured editor.
const (
	copilotPluginVersion = "llm-gateway/0.1"
	copilotUserAgent     = "GithubCopilotChat/llm-gateway"
)

// copilotCoreVertical is GitHub Copilot as the core Runtime serves it: core's
// Copilot on this Runtime's shared Copilot client, and the caller's GitHub
// token from Copilot's store, or none for the gateway-wide token. A GitHub
// token never refreshes; the sessions it buys expire, and core replaces them,
// so there is no refresh.
func (rt *Runtime) copilotCoreVertical() coreVertical {
	return coreVertical{
		serves: func(_ *config.Settings, _ string, cfg *config.ProviderConfig) bool {
			return copilotInstance(cfg)
		},
		provider: rt.coreCopilot,
		credentials: copilotStore{open: func() (core.CredentialStore, error) {
			return rt.openCredentials(true)
		}},
	}
}

// copilotInstance reports whether a configured provider is GitHub Copilot.
func copilotInstance(cfg *config.ProviderConfig) bool {
	return strings.EqualFold(strings.TrimSpace(cfg.Type), "github_copilot")
}

// copilotForceAdapt reports whether an instance may serve Chat over a
// model's Responses endpoint: always, unless the operator turned Copilot's
// API adaptation off and the instance does not force it back on.
func copilotForceAdapt(cfg *config.ProviderConfig) bool {
	return cfg.ForceApiSupport || os.Getenv("LLMGW_DISABLE_COPILOT_API_ADAPTATION") != "1"
}

// coreCopilot builds core's Copilot for a Copilot instance, with the
// configured editor identity and requests that time out as the OpenAI
// transport's did.
func (rt *Runtime) coreCopilot(settings *config.Settings, instance string) (core.Provider, error) {
	cfg := settings.Providers[instance]
	copilot, err := coreproviders.NewCopilot(coreproviders.CopilotConfig{
		Auth: rt.copilot, IntegrationID: settings.GithubCopilotIntegrationID,
		EditorVersion: settings.GithubCopilotEditorVersion, EditorPluginVersion: copilotPluginVersion,
		UserAgent: copilotUserAgent, Client: httpClient(cfg.TimeoutOr(settings.GithubCopilotTimeoutSeconds)),
		DisableAdaptation: !copilotForceAdapt(cfg),
	})
	if err != nil {
		return nil, &ConfigError{Msg: "github_copilot: initialize transport: " + err.Error()}
	}
	return &copilotCoreProvider{Copilot: copilot, listed: map[string]time.Time{}, endpoints: map[string][]string{}}, nil
}

// copilotCoreProvider is core's Copilot as the core Runtime serves it. Core
// routes a model by the catalog it last listed: Responses is native for a
// model whose row lists it, and Chat goes over Responses for one whose row
// lists only Responses. The gateway fetched its catalog on the request path
// when it had none, so the provider lists the catalog with a request's
// credential when that credential has not listed it within the gateway
// catalog's lifetime. Before core serves Chat over Responses, the provider
// refuses what the gateway's conversion refused: core drops the fields it
// does not carry before it converts a request.
type copilotCoreProvider struct {
	*coreproviders.Copilot

	mu sync.Mutex
	// listed is when each credential last listed the catalog, by key; the
	// gateway-wide token's key is empty.
	listed map[string]time.Time
	// endpoints are the endpoints each model's last listed row names, which
	// core routes by.
	endpoints map[string][]string
}

func (p *copilotCoreProvider) Invoke(ctx context.Context, request core.Request) (core.Response, error) {
	if err := p.prepare(ctx, request); err != nil {
		return core.Response{}, err
	}
	return p.Copilot.Invoke(ctx, request)
}

func (p *copilotCoreProvider) Stream(ctx context.Context, request core.Request) (core.StreamIter, error) {
	if err := p.prepare(ctx, request); err != nil {
		return nil, err
	}
	return p.Copilot.Stream(ctx, request)
}

// ListModels implements core.Provider and keeps what the rows say.
func (p *copilotCoreProvider) ListModels(ctx context.Context, credential *core.Credential) ([]core.ModelInfo, error) {
	models, err := p.Copilot.ListModels(ctx, credential)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.listed[copilotCredentialKey(credential)] = time.Now()
	for _, model := range models {
		p.endpoints[model.ID] = model.SupportedAPIs
	}
	return models, nil
}

// prepare lists the catalog for a request that routes by it: Responses, and
// Chat that may be served over Responses, which the gateway planned from its
// catalog. A listing that fails leaves the routes as they were, as a catalog
// the gateway could not fetch left its plan.
func (p *copilotCoreProvider) prepare(ctx context.Context, request core.Request) error {
	chat := copilotChatFrom(ctx)
	adapted := request.Surface == core.ModelSurfaceChatCompletions && chat.adapt
	if !adapted && request.Surface != core.ModelSurfaceResponses {
		return nil
	}
	p.mu.Lock()
	listed, ok := p.listed[copilotCredentialKey(request.Credential)]
	p.mu.Unlock()
	if !ok || time.Since(listed) > catalogTTL {
		_, _ = p.ListModels(ctx, request.Credential)
	}
	if adapted && chat.refuse != nil && p.overResponses(request.Model) {
		return chat.refuse()
	}
	return nil
}

// overResponses reports a model core serves Chat for over Responses when it
// adapts: one whose row lists Responses but not Chat Completions.
func (p *copilotCoreProvider) overResponses(model string) bool {
	p.mu.Lock()
	endpoints, known := p.endpoints[model]
	p.mu.Unlock()
	return known && translate.PreferredEndpoint(endpoints) == "responses"
}

func copilotCredentialKey(credential *core.Credential) string {
	if credential == nil {
		return ""
	}
	return credential.ConnectionID
}

// copilotChat is what the facade tells the vertical's provider about one
// Chat request that its body does not say.
type copilotChat struct {
	// adapt is the request's force_api_support: whether core may serve it
	// over Responses.
	adapt bool
	// refuse is the gateway's conversion of the request to Responses, which
	// refuses what Responses cannot carry.
	refuse func() error
}

type copilotChatKey struct{}

func withCopilotChat(ctx context.Context, chat copilotChat) context.Context {
	return context.WithValue(ctx, copilotChatKey{}, chat)
}

// copilotChatFrom returns the Chat request ctx names, or the zero one.
func copilotChatFrom(ctx context.Context) copilotChat {
	chat, _ := ctx.Value(copilotChatKey{}).(copilotChat)
	return chat
}

// copilotStore is the credential store of Copilot instances: the IAM store
// the OAuth store wraps, whose precedence is the resolver's. A human gets
// their own connection, then their legacy credential; a service or a system
// principal gets its project's binding for its kind; and a caller without a
// principal gets ErrNoCredential, on which the core Runtime sends no
// credential and core's Copilot uses the gateway-wide token. A principal
// without a credential is refused, as the resolver refused it, rather than
// served with the gateway's token. Each read marks the credential used, as
// the resolver's did.
type copilotStore struct {
	open func() (core.CredentialStore, error)
}

var _ core.CredentialStore = copilotStore{}

func errNoCopilotCredential() error {
	return &ConfigError{Msg: "github_copilot: this principal has no active Copilot credential"}
}

// copilotCredentialFailure reports a credential that could not be read as
// the resolver's failure did.
func copilotCredentialFailure(err error) error {
	if isContextError(err) {
		return err
	}
	return invocation("github_copilot: load BYOC credential: " + err.Error())
}

// Resolve implements core.CredentialStore. A caller without a principal
// resolves nothing, as the resolver never looked one up for it.
func (s copilotStore) Resolve(ctx context.Context, caller core.Caller, instance string) (string, error) {
	if callerPrincipalID(caller) == "" {
		return "", core.ErrNoCredential
	}
	store, err := s.open()
	if err != nil {
		return "", copilotCredentialFailure(err)
	}
	key, err := store.Resolve(ctx, caller, instance)
	switch {
	case errors.Is(err, core.ErrNoCredential):
		return "", errNoCopilotCredential()
	case err != nil:
		return "", copilotCredentialFailure(err)
	}
	return key, nil
}

// Load implements tokenstore.Store. The record carries no expiry, so the
// Coordinator hands the token out as the resolver did: GitHub's token names
// none the gateway keeps, and the session it buys, which expires, is core's
// to replace. The operation's collector sees the credential it used.
func (s copilotStore) Load(ctx context.Context, key string) (tokenstore.Record, error) {
	store, err := s.open()
	var record tokenstore.Record
	if err == nil {
		record, err = store.Load(ctx, key)
	}
	switch {
	case errors.Is(err, tokenstore.ErrNotFound):
		return tokenstore.Record{}, errNoCopilotCredential()
	case err != nil:
		return tokenstore.Record{}, copilotCredentialFailure(err)
	}
	record.Expiry = time.Time{}
	credentialCollectorFrom(ctx).observe(key, record.Revision)
	return record, nil
}

// Save, ReplaceIfCurrent, RevokeIfCurrent and Lease implement
// tokenstore.Store. Nothing refreshes a Copilot token, so only the
// Coordinator's lease after a rejected session reaches them; they pass to
// the IAM store all the same.

func (s copilotStore) Save(ctx context.Context, key string, record tokenstore.Record) (tokenstore.Record, error) {
	store, err := s.open()
	if err != nil {
		return tokenstore.Record{}, err
	}
	return store.Save(ctx, key, record)
}

func (s copilotStore) ReplaceIfCurrent(ctx context.Context, key, revision string, record tokenstore.Record) (tokenstore.Record, error) {
	store, err := s.open()
	if err != nil {
		return tokenstore.Record{}, err
	}
	return store.ReplaceIfCurrent(ctx, key, revision, record)
}

func (s copilotStore) RevokeIfCurrent(ctx context.Context, key, revision string) error {
	store, err := s.open()
	if err != nil {
		return err
	}
	return store.RevokeIfCurrent(ctx, key, revision)
}

func (s copilotStore) Lease(ctx context.Context, key string) (func(), error) {
	store, err := s.open()
	if err != nil {
		return nil, err
	}
	return store.Lease(ctx, key)
}
