package providers

import (
	"context"
	"errors"
	"testing"

	"llmgw/internal/config"

	"github.com/xibodev/llm-provider-auth/tokenstore"
	core "github.com/xibodev/llmgw-core"
	coreruntime "github.com/xibodev/llmgw-core/runtime"
)

// coreVerticalCases pins which registered type serves each configuration,
// "" meaning none. The types must never both claim an instance: the table has
// no order to break a tie with.
var coreVerticalCases = map[string]struct {
	cfg  *config.ProviderConfig
	want string
}{
	"codex":            {&config.ProviderConfig{Type: "openai_compatible", RegistryID: "openai_codex"}, "openai_codex"},
	"codex litellm":    {&config.ProviderConfig{Type: "litellm", RegistryID: "openai_codex"}, "openai_codex"},
	"codex at zen URL": {&config.ProviderConfig{Type: "openai_compatible", RegistryID: "openai_codex", BaseURL: "https://opencode.ai/zen/v1"}, "openai_codex"},
	"antigravity":      {&config.ProviderConfig{Type: "google_antigravity"}, "google_antigravity"},
	"zen":              {&config.ProviderConfig{Type: "openai_compatible", RegistryID: "opencode_zen"}, zenCoreType},
	"zen by URL":       {&config.ProviderConfig{Type: "litellm", BaseURL: "https://opencode.ai/zen/v1"}, zenCoreType},
	"bedrock at zen":   {&config.ProviderConfig{Type: "bedrock", BaseURL: "https://opencode.ai/zen/v1"}, ""},
	"plain":            {&config.ProviderConfig{Type: "openai_compatible", BaseURL: "https://api.example.test/v1"}, ""},
	"copilot":          {&config.ProviderConfig{Type: "github_copilot"}, copilotCoreType},
	"copilot by type":  {&config.ProviderConfig{Type: " GitHub_Copilot "}, copilotCoreType},
	"copilot registry": {&config.ProviderConfig{Type: "openai_compatible", RegistryID: "github_copilot"}, ""},
	"anthropic":        {&config.ProviderConfig{Type: "anthropic"}, anthropicCoreType},
	"anthropic spaced": {&config.ProviderConfig{Type: " Anthropic ", RegistryID: "custom_anthropic"}, anthropicCoreType},
}

func TestCoreVerticalsServeDisjointInstances(t *testing.T) {
	runtime := newRuntime(func(bool) (core.CredentialStore, error) { return core.NewMemoryCredentialStore(), nil })
	settings := config.Defaults()
	settings.Providers = map[string]*config.ProviderConfig{}
	for instance, fixture := range coreVerticalCases {
		settings.Providers[instance] = fixture.cfg
	}
	for instance, fixture := range coreVerticalCases {
		var serving []string
		for name, vertical := range runtime.verticals {
			if vertical.serves(settings, instance, fixture.cfg) {
				serving = append(serving, name)
			}
		}
		if len(serving) > 1 {
			t.Errorf("%s: served by %v, want at most one type", instance, serving)
		}
		if name, _, _ := runtime.vertical(settings, instance); name != fixture.want {
			t.Errorf("%s: served by %q, want %q", instance, name, fixture.want)
		}
	}
	if _, err := runtime.coreProvider(settings, "plain"); !IsConfig(err) {
		t.Fatalf("an instance no type serves built a core provider: err=%v", err)
	}
	if refresh := runtime.coreRefresh(settings, "plain"); refresh != nil {
		t.Fatal("an instance no type serves has a refresh")
	}
	if refresh := runtime.coreRefresh(settings, "zen"); refresh != nil {
		t.Fatal("a Zen API key would refresh")
	}
	// An instance without a base URL of its own is Zen when the default is.
	settings.OpenAICompatibleBaseURL = "https://opencode.ai/zen/v1"
	if name, _, _ := runtime.vertical(settings, "plain-default"); name != "" {
		t.Fatalf("an unconfigured instance is served by %q", name)
	}
	settings.Providers["plain-default"] = &config.ProviderConfig{Type: "openai_compatible"}
	if name, _, _ := runtime.vertical(settings, "plain-default"); name != zenCoreType {
		t.Fatalf("an instance at the Zen default is served by %q", name)
	}
}

// Resolve reaches the store of the instance's type. The token-store methods
// carry only a key, so they reach the store of the vertical the operation's
// context names, and the OAuth store without one.
func TestCoreCredentialsDispatchesByType(t *testing.T) {
	ctx := context.Background()
	oauth, fixture := core.NewMemoryCredentialStore(), core.NewMemoryCredentialStore()
	for store, token := range map[*core.MemoryCredentialStore]string{oauth: "oauth-token", fixture: "fixture-token"} {
		if _, err := store.Save(ctx, "key", tokenstore.Record{AccessToken: token}); err != nil {
			t.Fatal(err)
		}
	}
	oauth.BindShared("codex", "key")
	fixture.BindShared("fixture", "key")
	runtime := newRuntime(func(oauthStore bool) (core.CredentialStore, error) {
		if !oauthStore {
			t.Error("the OAuth store opened another store")
		}
		return oauth, nil
	})
	runtime.verticals["fixture"] = coreVertical{
		serves:      func(_ *config.Settings, instance string, _ *config.ProviderConfig) bool { return instance == "fixture" },
		credentials: fixture,
	}
	settings := config.Defaults()
	settings.Providers = map[string]*config.ProviderConfig{
		"codex":   {Type: "openai_compatible", RegistryID: "openai_codex"},
		"fixture": {Type: "openai_compatible"},
	}
	credentials := coreCredentials{runtime: runtime, settings: coreruntime.NewMemorySettings(settings)}

	for _, instance := range []string{"codex", "fixture"} {
		if key, err := credentials.Resolve(ctx, gatewayCaller(), instance); err != nil || key != "key" {
			t.Fatalf("%s: key=%q err=%v", instance, key, err)
		}
	}
	if _, err := credentials.Resolve(ctx, gatewayCaller(), "missing"); !IsConfig(err) || errors.Is(err, core.ErrNoCredential) {
		t.Fatalf("an instance no type serves resolved: err=%v", err)
	}
	for vertical, want := range map[string]string{"": "oauth-token", "openai_codex": "oauth-token", "fixture": "fixture-token"} {
		record, err := credentials.Load(withCoreOperation(ctx, vertical, gatewayCaller()), "key")
		if err != nil || record.AccessToken != want {
			t.Fatalf("operation of %q: token=%q err=%v, want %q", vertical, record.AccessToken, err, want)
		}
	}
}
