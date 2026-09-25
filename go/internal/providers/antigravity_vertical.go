package providers

import (
	"context"

	"llmgw/internal/config"

	core "github.com/xibodev/llmgw-core"
	coreproviders "github.com/xibodev/llmgw-core/providers"
)

// antigravityCoreVertical is Antigravity as the core Runtime serves it:
// core's Antigravity against this Runtime's Cloud Code Assist endpoint, the
// caller's own OAuth connection from the OAuth store, and core's Antigravity
// refresh behind antigravityGrantRefusal.
func (rt *Runtime) antigravityCoreVertical() coreVertical {
	return coreVertical{
		serves: func(_ *config.Settings, _ string, cfg *config.ProviderConfig) bool {
			return antigravityInstance(cfg)
		},
		provider: func(*config.Settings, string) (core.Provider, error) {
			antigravity, err := rt.newCoreAntigravity()
			if err != nil {
				return nil, err
			}
			return antigravityCoreProvider{Antigravity: antigravity}, nil
		},
		refresh:     rt.antigravityRefresh,
		credentials: rt.credentials,
	}
}

// antigravityCoreProvider is core's Antigravity as the core Runtime serves
// it. Each request the Runtime sends upstream, its replay included, counts
// on the operation's oauthCall, so a 401 the Runtime did not replay tells
// that the refresh after it failed. The facade never streams through the
// Runtime, which Antigravity refuses anyway.
type antigravityCoreProvider struct{ *coreproviders.Antigravity }

func (p antigravityCoreProvider) Invoke(ctx context.Context, request core.Request) (core.Response, error) {
	oauthCallFrom(ctx).attempt()
	return p.Antigravity.Invoke(ctx, request)
}

// newCoreAntigravity builds core's Antigravity against this Runtime's Cloud
// Code Assist endpoint. It stores each project it discovers with the
// connection that discovered it.
func (rt *Runtime) newCoreAntigravity() (*coreproviders.Antigravity, error) {
	endpoint := rt.antigravityEndpoint.get()
	antigravity, err := coreproviders.NewAntigravity(coreproviders.AntigravityConfig{
		BaseURL: endpoint.BaseURL, Client: endpoint.HTTPClient, ProjectResolved: rt.storeAntigravityProject,
	})
	if err != nil {
		return nil, &ConfigError{Msg: "google_antigravity: initialize transport: " + err.Error()}
	}
	return antigravity, nil
}
