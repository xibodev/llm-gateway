package providers

import (
	"context"
	"net/http"
	"strings"

	"llmgw/internal/config"

	"github.com/xibodev/llm-provider-auth/tokenstore"
	core "github.com/xibodev/llmgw-core"
	coreproviders "github.com/xibodev/llmgw-core/providers"
)

// codexCoreVertical is Codex as the core Runtime serves it: core's Codex,
// the caller's own OAuth connection from the OAuth store, and core's Codex
// refresh with the effective client ID. A grant that names no client would
// refresh with that client, but refreshCodexRecord refuses such a grant
// first.
func (rt *Runtime) codexCoreVertical() coreVertical {
	return coreVertical{
		serves: func(_ *config.Settings, instance string, cfg *config.ProviderConfig) bool {
			return codexInstance(instance, cfg)
		},
		provider: rt.coreCodex,
		refresh: func(settings *config.Settings, _ string) tokenstore.RefreshFunc {
			return rt.codexRefresh(effectiveCodexClientID(settings))
		},
		credentials: rt.credentials,
	}
}

// coreCodex builds core's Codex for a Codex instance, with the gateway's
// instructions and verified client version and this Runtime's Codex
// endpoints.
func (rt *Runtime) coreCodex(settings *config.Settings, instance string) (core.Provider, error) {
	timeout := settings.OpenAICompatibleTimeoutSeconds
	if cfg := settings.Providers[instance]; cfg != nil && cfg.Timeout != nil {
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
