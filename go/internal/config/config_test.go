package config

import (
	"path/filepath"
	"testing"
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
		s.Categories = map[string]*CategoryConfig{
			"smart": {Failover: []CategoryMember{{Provider: "br", Model: "m1"}}},
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
	cat, ok := reloaded.Categories["smart"]
	if !ok || len(cat.Failover) != 1 || cat.Failover[0].Provider != "br" {
		t.Fatalf("category round-trip wrong: %+v", cat)
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
