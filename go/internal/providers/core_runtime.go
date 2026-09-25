package providers

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"llmgw/internal/config"
	"llmgw/internal/iam"

	"github.com/xibodev/llm-provider-auth/tokenstore"
	core "github.com/xibodev/llmgw-core"
	coreproviders "github.com/xibodev/llmgw-core/providers"
	coreruntime "github.com/xibodev/llmgw-core/runtime"
)

// newCoreRuntime builds the llmgw-core Runtime that serves the provider
// instances whose vertical core owns, Codex and Antigravity: the Runtime
// holds their providers and the coordinators that refresh their credentials,
// while the gateway supplies the settings, the connections, the refresh and
// the evidence sink.
func newCoreRuntime(rt *Runtime) (*coreruntime.Runtime[*config.Settings], error) {
	return coreruntime.New(coreruntime.Options[*config.Settings]{
		Settings:    config.Source{},
		Providers:   rt.coreProvider,
		Credentials: rt.credentials,
		Refresh:     rt.coreRefresh,
		Evidence:    credentialEvidence{},
		// Catalogs stays nil. The gateway lists Codex and Antigravity
		// models on its own catalog path and never asks the Runtime, so
		// the Runtime's in-memory catalog stays empty.
	})
}

// iamCredentialStore opens the IAM credential store over the database IAM
// serves now. That handle follows the state directory, which tests change,
// so each operation opens the store rather than keeping one; opening costs
// no more than the store value. Only Codex and Antigravity instances reach
// the core Runtime and the gateway's Coordinators over this store, and both
// resolve owner-private OAuth connections. Each read marks the connection
// used, as both paths' reads did.
func iamCredentialStore() (core.CredentialStore, error) {
	db, err := iam.DB()
	if err != nil {
		return nil, err
	}
	return iam.NewCredentialStore(db, iam.CredentialStoreOptions{Precedence: oauthPrecedence, MarkUsed: true})
}

func oauthPrecedence(string) iam.CredentialPrecedence { return iam.OAuthPrecedence }

// coreProvider is the Runtime's ProviderFactory. It builds core's Codex for a
// Codex instance, with the gateway's instructions and verified client version
// and this Runtime's Codex endpoints, and core's Antigravity for an
// Antigravity instance, with this Runtime's Cloud Code Assist endpoint; other
// instances are not routed through the Runtime yet.
func (rt *Runtime) coreProvider(settings *config.Settings, instance string) (core.Provider, error) {
	cfg := settings.Providers[instance]
	if cfg != nil && antigravityInstance(cfg) {
		antigravity, err := rt.newCoreAntigravity()
		if err != nil {
			return nil, err
		}
		return antigravityCoreProvider{Antigravity: antigravity}, nil
	}
	if cfg == nil || !codexInstance(instance, cfg) {
		return nil, &ConfigError{Msg: fmt.Sprintf("provider '%s' is not served by the core runtime", instance)}
	}
	timeout := settings.OpenAICompatibleTimeoutSeconds
	if cfg.Timeout != nil {
		timeout = *cfg.Timeout
	}
	client := rt.codexEndpoints.get().HTTPClient
	if client == nil {
		client = httpClient(timeout)
	}
	codex, err := rt.newCoreCodex(client, codexCatalogClientVersion)
	if err != nil {
		return nil, err
	}
	return codexCoreProvider{Codex: codex}, nil
}

// codexCoreProvider is core's Codex as the core Runtime serves it. Each
// request the Runtime sends upstream, its replay included, starts a new
// account pin scope on the operation's oauthCall.
type codexCoreProvider struct{ *coreproviders.Codex }

func (p codexCoreProvider) Invoke(ctx context.Context, request core.Request) (core.Response, error) {
	oauthCallFrom(ctx).attempt()
	return p.Codex.Invoke(ctx, request)
}

func (p codexCoreProvider) Stream(ctx context.Context, request core.Request) (core.StreamIter, error) {
	oauthCallFrom(ctx).attempt()
	return p.Codex.Stream(ctx, request)
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

// coreRefresh is the Runtime's RefreshFactory. A Codex grant that names no
// client would refresh with the effective client ID, but refreshCodexRecord
// refuses such a grant first; antigravityGrantRefusal does the same for
// Antigravity.
func (rt *Runtime) coreRefresh(settings *config.Settings, instance string) tokenstore.RefreshFunc {
	cfg := settings.Providers[instance]
	switch {
	case cfg == nil:
		return nil
	case antigravityInstance(cfg):
		return rt.antigravityRefresh(settings, instance)
	case codexInstance(instance, cfg):
		return rt.codexRefresh(effectiveCodexClientID(settings))
	}
	return nil
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

// newCoreCodex builds core's Codex provider against this Runtime's Codex
// endpoints. clientVersion is sent only to the catalog.
func (rt *Runtime) newCoreCodex(client *http.Client, clientVersion string) (*coreproviders.Codex, error) {
	endpoints := rt.codexEndpoints.get().withDefaults()
	codex, err := coreproviders.NewCodex(coreproviders.CodexConfig{
		Instructions: codexInstructions, ClientVersion: clientVersion,
		ResponsesURL: strings.TrimRight(endpoints.ResponsesBaseURL, "/") + "/responses",
		ModelsURL:    endpoints.ModelsURL, Client: client,
	})
	if err != nil {
		return nil, &ConfigError{Msg: "openai_codex: initialize shared transport: " + err.Error()}
	}
	return codex, nil
}
