package providers

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"llmgw/internal/config"

	"github.com/xibodev/llm-provider-auth/tokenstore"
	core "github.com/xibodev/llmgw-core"
	coreproviders "github.com/xibodev/llmgw-core/providers"
)

// googleCoreType is the key of Google AI Studio and Vertex AI in
// coreVerticals. The facade names it on each operation, so the core
// Runtime's credential store loads their credentials from Google's store.
const googleCoreType = "google"

// googleCoreVertical is Google AI Studio and Vertex AI as the core Runtime
// serves them: core's Google for the instance's deployment, and the
// credential the provider factory resolves for either type. An API key
// never refreshes, and core's Google exchanges a service-account key for
// access tokens itself, in a cache each instance owns, so there is no
// refresh.
func (rt *Runtime) googleCoreVertical() coreVertical {
	return coreVertical{
		serves: func(_ *config.Settings, _ string, cfg *config.ProviderConfig) bool {
			_, ok := googleDeployment(cfg)
			return ok
		},
		provider: func(settings *config.Settings, instance string) (core.Provider, error) {
			google, err := newCoreGoogle(instance, settings.Providers[instance])
			if err != nil {
				return nil, err
			}
			return google, nil
		},
		credentials: googleStore{open: func() (core.CredentialStore, error) {
			return rt.openCredentials(false)
		}},
	}
}

// googleDeployment is the Google deployment an instance of cfg's type calls,
// and false for a type that is not Google's.
func googleDeployment(cfg *config.ProviderConfig) (coreproviders.GoogleDeployment, bool) {
	switch strings.ToLower(strings.TrimSpace(cfg.Type)) {
	case "ai_studio":
		return coreproviders.GoogleAIStudio, true
	case "vertex_ai":
		return coreproviders.GoogleVertexAI, true
	}
	return "", false
}

// newCoreGoogle builds core's Google for an instance configured as cfg, as
// the provider factory configured the gateway's transport: the instance's
// base URL and timeout and, on Vertex AI, its project, location and request
// type. An empty project takes the credential's.
func newCoreGoogle(instance string, cfg *config.ProviderConfig) (*coreproviders.Google, error) {
	deployment, _ := googleDeployment(cfg)
	options := coreproviders.GoogleConfig{
		Deployment: deployment, BaseURL: cfg.BaseURL,
		Client: &http.Client{Timeout: googleTimeout(cfg.TimeoutOr(120))},
	}
	if deployment == coreproviders.GoogleVertexAI {
		options.Project, options.Location, options.RequestType = cfg.Project, cfg.Location, cfg.VertexRequestType
	}
	google, err := coreproviders.NewGoogle(options)
	if err != nil {
		return nil, &ConfigError{Msg: fmt.Sprintf("provider '%s': initialize Google: %v", instance, err)}
	}
	return google, nil
}

// googleStore is the credential store of Google instances: the IAM store
// with the provider factory's precedence, the caller's connection, then the
// system connection, then the configured key, read from the config:
// namespace. When none resolves it reports core.ErrNoCredential, on which
// the core Runtime sends the request without a credential: core's Google
// sends it to AI Studio without a key, as the transport sent the factory's
// empty key, and refuses it for Vertex AI, whose facade the factory refused
// to build without one.
type googleStore struct {
	open func() (core.CredentialStore, error)
}

var _ core.CredentialStore = googleStore{}

// Resolve implements core.CredentialStore.
func (s googleStore) Resolve(ctx context.Context, caller core.Caller, instance string) (string, error) {
	store, err := s.open()
	if err != nil {
		return "", err
	}
	return store.Resolve(ctx, caller, instance)
}

// Load implements tokenstore.Store. The factory refuses a connection that is
// neither an API key nor a service-account key, so the store refuses one
// too, should one replace the connection a facade was built with. Core's
// Google refuses a service-account key for AI Studio itself.
func (s googleStore) Load(ctx context.Context, key string) (tokenstore.Record, error) {
	store, err := s.open()
	if err != nil {
		return tokenstore.Record{}, err
	}
	record, err := store.Load(ctx, key)
	if err == nil && record.TokenType != core.TokenTypeAPIKey && record.TokenType != core.TokenTypeGCPServiceAccount {
		return tokenstore.Record{}, &ConfigError{Msg: "google: the resolved connection is neither an API key nor a service account key"}
	}
	return record, err
}

// Save, ReplaceIfCurrent, RevokeIfCurrent and Lease implement
// tokenstore.Store. Neither kind refreshes, so nothing calls them for a
// Google credential; they pass to the IAM store all the same.

func (s googleStore) Save(ctx context.Context, key string, record tokenstore.Record) (tokenstore.Record, error) {
	store, err := s.open()
	if err != nil {
		return tokenstore.Record{}, err
	}
	return store.Save(ctx, key, record)
}

func (s googleStore) ReplaceIfCurrent(ctx context.Context, key, revision string, record tokenstore.Record) (tokenstore.Record, error) {
	store, err := s.open()
	if err != nil {
		return tokenstore.Record{}, err
	}
	return store.ReplaceIfCurrent(ctx, key, revision, record)
}

func (s googleStore) RevokeIfCurrent(ctx context.Context, key, revision string) error {
	store, err := s.open()
	if err != nil {
		return err
	}
	return store.RevokeIfCurrent(ctx, key, revision)
}

func (s googleStore) Lease(ctx context.Context, key string) (func(), error) {
	store, err := s.open()
	if err != nil {
		return nil, err
	}
	return store.Lease(ctx, key)
}
