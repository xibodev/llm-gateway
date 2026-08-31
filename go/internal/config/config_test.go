package config

import (
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestBedrockBaseURL(t *testing.T) {
	cases := map[string]string{
		"us-west-2": "https://bedrock-runtime.us-west-2.amazonaws.com/v1",
		"":          "https://bedrock-runtime.us-east-1.amazonaws.com/v1",
		"  ":        "https://bedrock-runtime.us-east-1.amazonaws.com/v1",
	}
	for region, want := range cases {
		if got := BedrockBaseURL(region); got != want {
			t.Errorf("BedrockBaseURL(%q)=%q want %q", region, got, want)
		}
	}
}

func TestKeyMintResolveRevoke(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	token := MintKey("proj", "cli")
	if len(token) < 10 || token[:6] != "llmgw_" {
		t.Fatalf("bad token: %q", token)
	}
	p := ResolvePrincipal(token)
	if p == nil || p.Project != "proj" || p.Key != "cli" {
		t.Fatalf("resolve wrong: %+v", p)
	}
	if ResolvePrincipal("nope") != nil {
		t.Error("unknown token should resolve nil")
	}
	found := false
	for _, k := range ListKeys() {
		if k.Token == token && k.Project == "proj" {
			found = true
		}
	}
	if !found {
		t.Error("minted key not listed")
	}
	if !RevokeKey(token) {
		t.Error("revoke should return true")
	}
	if ResolvePrincipal(token) != nil {
		t.Error("revoked token should resolve nil")
	}
	if RevokeKey(token) {
		t.Error("second revoke should return false")
	}
}

func TestKeyGovernance(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())

	// disabled key resolves nil
	tok := MintKey("p", "k")
	if !UpdateKey(tok, KeyUpdate{Disabled: ptr(true)}) {
		t.Fatal("update should find the key")
	}
	if ResolvePrincipal(tok) != nil {
		t.Error("disabled key should resolve nil")
	}
	// re-enable + set limits, they surface on the principal
	UpdateKey(tok, KeyUpdate{Disabled: ptr(false), RPM: ptrInt(30), AllowedModels: &[]string{"smart"}})
	p := ResolvePrincipal(tok)
	if p == nil || p.RPM != 30 || len(p.AllowedModels) != 1 || p.AllowedModels[0] != "smart" {
		t.Fatalf("governance not surfaced on principal: %+v", p)
	}
	// expired key resolves nil
	tok2 := MintKey("p", "k2")
	UpdateKey(tok2, KeyUpdate{ExpiresAt: ptrInt64(1)}) // 1970 -> long expired
	if ResolvePrincipal(tok2) != nil {
		t.Error("expired key should resolve nil")
	}
	for _, ki := range ListKeys() {
		if ki.Token == tok2 && !ki.Expired {
			t.Error("expired flag should be set in ListKeys")
		}
	}
	if UpdateKey("nope", KeyUpdate{Disabled: ptr(true)}) {
		t.Error("update of unknown token should return false")
	}
}

func ptr(b bool) *bool        { return &b }
func ptrInt(i int) *int       { return &i }
func ptrInt64(i int64) *int64 { return &i }

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LLMGW_STATE_DIR", dir)
	t.Setenv("LLMGW_CONFIG", filepath.Join(dir, "config.yaml"))
	Update(func(s *Settings) {
		s.Providers = map[string]*ProviderConfig{
			"br":  {Type: "bedrock", Region: "eu-central-1"},
			"cop": {Type: "github_copilot", RegistryID: "github_copilot", ForceApiSupport: true},
			"vertex": {
				Type: "vertex_ai", RegistryID: "vertex_ai",
				Project: "project-a", Location: "us-central1",
				DefaultVoice: "voice-a", Disabled: true,
			},
		}
		s.OpenAICodexClientID = "codex-client"
		s.Endpoints = map[string]*EndpointConfig{
			"smart": {Failover: []EndpointMember{{Provider: "br", Model: "m1"}}},
		}
	})
	if err := Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	reloaded := Load()
	br, ok := reloaded.Providers["br"]
	if !ok || br.Type != "bedrock" || br.Region != "eu-central-1" {
		t.Fatalf("provider round-trip wrong: %+v", br)
	}
	if br.ForceApiSupport {
		t.Fatalf("force_api_support should default false: %+v", br)
	}
	cop, ok := reloaded.Providers["cop"]
	if !ok || cop.RegistryID != "github_copilot" || !cop.ForceApiSupport {
		t.Fatalf("force_api_support did not round-trip: %+v", cop)
	}
	vertex, ok := reloaded.Providers["vertex"]
	if !ok || vertex.Project != "project-a" || vertex.Location != "us-central1" ||
		vertex.DefaultVoice != "voice-a" || !vertex.Disabled {
		t.Fatalf("provider setup fields did not round-trip: %+v", vertex)
	}
	if reloaded.OpenAICodexClientID != "codex-client" {
		t.Fatalf("codex client id did not round-trip: %q", reloaded.OpenAICodexClientID)
	}
	ep, ok := reloaded.Endpoints["smart"]
	if !ok || len(ep.Failover) != 1 || ep.Failover[0].Provider != "br" {
		t.Fatalf("endpoint round-trip wrong: %+v", ep)
	}
}

func TestSavingsLedgerDefaultsOffAndExplicitConfigIsPreserved(t *testing.T) {
	if Defaults().Savings.Enabled {
		t.Fatal("legacy savings ledger should default off")
	}
	s := parseSettingsForTest(t, `
savings:
  enabled: true
  db_path: custom/usage.db
  baseline_model: provider/baseline
`)
	if !s.Savings.Enabled || s.Savings.DBPath != "custom/usage.db" || s.Savings.BaselineModel != "provider/baseline" {
		t.Fatalf("explicit savings config not preserved: %+v", s.Savings)
	}
}

func TestSecretsIsolation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LLMGW_STATE_DIR", dir)
	SaveSecret("prov", "sk-secret")
	if LoadSecrets()["prov"] != "sk-secret" {
		t.Error("secret not stored")
	}
	DeleteSecret("prov")
	if LoadSecrets()["prov"] != "" {
		t.Error("secret not deleted")
	}
}

// parseSettingsForTest applies config.go's own parse path (applyConfig) to a
// YAML snippet, rather than reimplementing yaml.Unmarshal + field mapping.
func parseSettingsForTest(t *testing.T, yamlStr string) *Settings {
	t.Helper()
	var payload map[string]any
	if err := yaml.Unmarshal([]byte(yamlStr), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	s := Defaults()
	applyConfig(s, payload)
	return s
}

// serialiseSettingsForTest applies config.go's own serialise path
// (configPayload) so the test observes exactly what Save() would write.
func serialiseSettingsForTest(t *testing.T, s *Settings) string {
	t.Helper()
	b, err := yaml.Marshal(configPayload(s))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// A config written before the rename must keep loading. This is a config-file
// first product; silently dropping a user's routing chains on upgrade is not an
// acceptable migration.
func TestLegacyCategoriesKeyStillLoads(t *testing.T) {
	settings := parseSettingsForTest(t, `
providers:
  openai:
    type: openai
categories:
  smart:
    failover:
      - { provider: openai, model: gpt-4o-mini }
`)
	chain, ok := settings.Endpoints["smart"]
	if !ok {
		t.Fatalf("legacy categories: key did not populate Endpoints: %+v", settings.Endpoints)
	}
	if len(chain.Failover) != 1 || chain.Failover[0].Provider != "openai" {
		t.Fatalf("failover=%+v", chain.Failover)
	}
}

func TestEndpointsKeyLoads(t *testing.T) {
	settings := parseSettingsForTest(t, `
providers:
  openai:
    type: openai
endpoints:
  smart:
    failover:
      - { provider: openai, model: gpt-4o-mini }
`)
	if _, ok := settings.Endpoints["smart"]; !ok {
		t.Fatalf("endpoints: key did not load: %+v", settings.Endpoints)
	}
}

// When both keys are present the new one wins, and the old one must not
// silently merge — an operator mid-migration should get a predictable result.
//
// "smart" alone would pass under merge semantics too, since a merge that
// applies endpoints: on top of categories: converges to the same value for a
// key present in both. legacy-only is the case that tells ignore and merge
// apart: a merge would carry it through from categories:, an ignore drops it
// entirely because categories: is never consulted once endpoints: is present.
func TestEndpointsWinsOverLegacyCategories(t *testing.T) {
	settings := parseSettingsForTest(t, `
providers:
  openai:
    type: openai
categories:
  smart:
    failover:
      - { provider: openai, model: legacy-model }
  legacy-only:
    failover:
      - { provider: openai, model: legacy-model }
endpoints:
  smart:
    failover:
      - { provider: openai, model: current-model }
`)
	chain := settings.Endpoints["smart"]
	if len(chain.Failover) != 1 || chain.Failover[0].Model != "current-model" {
		t.Fatalf("endpoints: did not take precedence: %+v", chain.Failover)
	}
	if _, ok := settings.Endpoints["legacy-only"]; ok {
		t.Fatalf("categories:-only key leaked through — endpoints: should ignore categories: entirely, not merge: %+v", settings.Endpoints)
	}
}

// Round-tripping writes the new key, so a save quietly migrates the file: the
// legacy key must both appear as endpoints: and disappear as categories:.
func TestSaveWritesEndpointsKey(t *testing.T) {
	settings := parseSettingsForTest(t, `
providers:
  openai:
    type: openai
categories:
  smart:
    failover:
      - { provider: openai, model: gpt-4o-mini }
`)
	out := serialiseSettingsForTest(t, settings)
	if !strings.Contains(out, "endpoints:") {
		t.Fatalf("serialised config has no endpoints: key:\n%s", out)
	}
	if strings.Contains(out, "categories:") {
		t.Fatalf("serialised config still has the legacy categories: key, save did not migrate it:\n%s", out)
	}
}
