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
// instances whose vertical core owns. Codex is the only one so far: the
// Runtime holds its provider and the coordinators that refresh its
// credentials, while the gateway supplies the settings, the connections, the
// refresh and the evidence sink.
func newCoreRuntime(rt *Runtime) (*coreruntime.Runtime[*config.Settings], error) {
	return coreruntime.New(coreruntime.Options[*config.Settings]{
		Settings:    config.Source{},
		Providers:   rt.coreProvider,
		Credentials: rt.codexCredentials,
		Refresh:     rt.coreRefresh,
		Evidence:    credentialEvidence{},
		// Catalogs stays nil. The gateway lists Codex models on its own
		// catalog path and never asks the Runtime, so the Runtime's
		// in-memory catalog stays empty.
	})
}

// iamCredentialStore opens the IAM credential store over the database IAM
// serves now. That handle follows the state directory, which tests change,
// so each operation opens the store rather than keeping one; opening costs
// no more than the store value. Only Codex instances reach the core Runtime
// so far, and they resolve owner-private OAuth connections. Each read marks
// the connection used, as the Codex path's reads did.
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
// and this Runtime's Codex endpoints; other instances are not routed through
// the Runtime yet.
func (rt *Runtime) coreProvider(settings *config.Settings, instance string) (core.Provider, error) {
	cfg := settings.Providers[instance]
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
// account pin scope on the operation's codexCall.
type codexCoreProvider struct{ *coreproviders.Codex }

func (p codexCoreProvider) Invoke(ctx context.Context, request core.Request) (core.Response, error) {
	codexCallFrom(ctx).attempt()
	return p.Codex.Invoke(ctx, request)
}

func (p codexCoreProvider) Stream(ctx context.Context, request core.Request) (core.StreamIter, error) {
	codexCallFrom(ctx).attempt()
	return p.Codex.Stream(ctx, request)
}

// coreRefresh is the Runtime's RefreshFactory. A grant that names no client
// would refresh with the effective client ID, but refreshCodexRecord refuses
// such a grant first.
func (rt *Runtime) coreRefresh(settings *config.Settings, instance string) tokenstore.RefreshFunc {
	if cfg := settings.Providers[instance]; cfg == nil || !codexInstance(instance, cfg) {
		return nil
	}
	return rt.codexRefresh(effectiveCodexClientID(settings))
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
