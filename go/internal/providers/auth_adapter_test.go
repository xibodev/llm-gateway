package providers

import (
	"context"
	"strings"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
)

type fixtureProviderAuthAdapter struct{}

func (fixtureProviderAuthAdapter) ID() string             { return "fixture_auth" }
func (fixtureProviderAuthAdapter) CredentialKind() string { return "fixture_oauth" }
func (fixtureProviderAuthAdapter) Capabilities() ProviderAuthCapabilities {
	return ProviderAuthCapabilities{TokenImport: true}
}

func TestSafeProviderAuthPollSanitizesBeforeRuneLimit(t *testing.T) {
	secret := "llmgw_" + strings.Repeat("a", 32)
	result := SafeProviderAuthPoll(ProviderAuthPoll{
		Status: "denied", Error: secret + " Bearer top-secret user@example.test?token=query-secret " + strings.Repeat("界", 400),
		AccessToken: "internal-access", RefreshToken: "internal-refresh", IDToken: "internal-id",
	})
	if result.Status != "denied" || result.AccessToken != "internal-access" || result.RefreshToken != "internal-refresh" || result.IDToken != "internal-id" {
		t.Fatalf("safe poll changed semantic or internal token fields: %+v", result)
	}
	if strings.Contains(result.Error, secret) || strings.Contains(result.Error, "top-secret") || strings.Contains(result.Error, "user@example.test") || strings.Contains(result.Error, "query-secret") {
		t.Fatalf("safe poll retained sensitive diagnostics: %q", result.Error)
	}
	if len([]rune(result.Error)) > maxProviderAuthDiagnosticChars {
		t.Fatalf("safe poll error has %d runes", len([]rune(result.Error)))
	}
}
func (fixtureProviderAuthAdapter) Import(
	context.Context, ProviderAuthImport,
) (ProviderAuthPoll, error) {
	return ProviderAuthPoll{Status: "authorized", AccessToken: "fixture"}, nil
}

func TestProviderAuthAdapterRegistryAndCapabilities(t *testing.T) {
	const id = "fixture_auth"
	unregisterProviderAuthAdapterFactoryForTests(id)
	t.Cleanup(func() { unregisterProviderAuthAdapterFactoryForTests(id) })
	if err := RegisterProviderAuthAdapterFactory(id, func(string) (ProviderAuthAdapter, error) {
		return fixtureProviderAuthAdapter{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	adapter, err := NewProviderAuthAdapter(id, "provider")
	if err != nil {
		t.Fatal(err)
	}
	if adapter.CredentialKind() != "fixture_oauth" || !adapter.Capabilities().TokenImport {
		t.Fatalf("adapter=%+v capabilities=%+v", adapter, adapter.Capabilities())
	}
	if _, ok := adapter.(ImportProviderAuthAdapter); !ok {
		t.Fatal("fixture adapter should expose token import")
	}
	if err := RegisterProviderAuthAdapterFactory(id, func(string) (ProviderAuthAdapter, error) {
		return fixtureProviderAuthAdapter{}, nil
	}); err == nil {
		t.Fatal("duplicate auth adapter should be rejected")
	}
}

func TestBuiltInProviderAuthAdapters(t *testing.T) {
	copilot, err := NewProviderAuthAdapter("github_copilot", "copilot")
	if err != nil {
		t.Fatal(err)
	}
	if copilot.CredentialKind() != "github_oauth" || !copilot.Capabilities().DeviceCode {
		t.Fatalf("Copilot adapter=%+v capabilities=%+v", copilot, copilot.Capabilities())
	}
	if _, ok := copilot.(DeviceProviderAuthAdapter); !ok {
		t.Fatal("Copilot adapter should expose device authorization")
	}
	if _, ok := copilot.(RefreshableProviderAuthAdapter); !ok {
		t.Fatal("Copilot adapter should expose refresh")
	}
	if _, ok := copilot.(RevocableProviderAuthAdapter); ok {
		t.Fatal("Copilot adapter must not claim unsupported upstream revocation")
	}
	if _, err := NewProviderAuthAdapter("missing", "provider"); err == nil {
		t.Fatal("unknown auth adapter should be rejected")
	}

	oldClientID := config.Get().OpenAICodexClientID
	t.Cleanup(func() {
		config.Update(func(s *config.Settings) { s.OpenAICodexClientID = oldClientID })
	})
	config.Update(func(s *config.Settings) { s.OpenAICodexClientID = "fixture-client" })
	codex, err := NewProviderAuthAdapter("openai_codex", "codex")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := codex.(GuardedRefreshProviderAuthAdapter); !ok {
		t.Fatal("Codex adapter should expose guarded connection refresh")
	}
	if _, ok := codex.(RefreshableProviderAuthAdapter); ok {
		t.Fatal("Codex adapter must not expose unguarded token-only refresh")
	}
	if _, ok := codex.(RevocableProviderAuthAdapter); !ok {
		t.Fatal("Codex adapter should expose upstream revocation")
	}
}

var _ ImportProviderAuthAdapter = fixtureProviderAuthAdapter{}
var _ RefreshableProviderAuthAdapter = githubCopilotAuthAdapter{}
var _ DeviceProviderAuthAdapter = githubCopilotAuthAdapter{}
var _ = iam.OAuthTokenEnvelope{}
