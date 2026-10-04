package config

import (
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
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

// mustLoad loads the configuration and fails the test when it cannot.
func mustLoad(t *testing.T) *Settings {
	t.Helper()
	settings, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return settings
}

func TestLoadSeedsWritableConfigOnce(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "state", "config.yaml")
	seed := filepath.Join(dir, "seed.yaml")
	t.Setenv("LLMGW_STATE_DIR", filepath.Join(dir, "state"))
	t.Setenv("LLMGW_CONFIG", target)
	t.Setenv("LLMGW_CONFIG_SEED", seed)
	if err := os.WriteFile(seed, []byte("providers:\n  seeded:\n    type: echo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded := mustLoad(t)
	if loaded.Providers["seeded"] == nil {
		t.Fatal("seeded provider was not loaded")
	}
	if err := os.WriteFile(seed, []byte("providers:\n  replaced:\n    type: echo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded = mustLoad(t)
	if loaded.Providers["seeded"] == nil || loaded.Providers["replaced"] != nil {
		t.Fatalf("existing writable config was overwritten: %+v", loaded.Providers)
	}
}

func TestLoadRefusesAConfigurationItCannotParse(t *testing.T) {
	keepSettings(t)
	useTempConfig(t)
	if _, err := Load(); err != nil {
		t.Fatalf("a missing configuration is the defaults, got %v", err)
	}
	for _, test := range []struct{ name, content, position string }{
		{"syntax", "gateway_preamble: kept\nproviders:\n  a: 1\n b: 2\n", "line 3:"},
		{"duplicate key", "gateway_preamble: one\ngateway_preamble: two\n", "line 2:"},
		{"sequence", "- fixture-secret-value\n", "line 1, column 1:"},
		{"scalar", "\n  fixture-secret-value\n", "line 2, column 3:"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := os.WriteFile(ConfigFilePath(), []byte(test.content), 0o600); err != nil {
				t.Fatal(err)
			}
			before, generation := Snapshot()
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), ConfigFilePath()) || !strings.Contains(err.Error(), test.position) {
				t.Fatalf("err=%v, want the path and %q", err, test.position)
			}
			if strings.Contains(err.Error(), "fixture-secret") {
				t.Fatalf("the error quotes the file: %v", err)
			}
			if current, currentGeneration := Snapshot(); current != before || currentGeneration != generation {
				t.Fatal("a configuration that does not parse was published")
			}
			if _, err := ReadFile(ConfigFilePath()); err == nil {
				t.Fatal("ReadFile accepted a configuration that does not parse")
			}
		})
	}
}

func TestLoadRefusesAConfigurationItCannotRead(t *testing.T) {
	keepSettings(t)
	useTempConfig(t)
	// No platform reads a directory as a file.
	if err := os.Mkdir(ConfigFilePath(), 0o700); err != nil {
		t.Fatal(err)
	}
	generation := Generation()
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), ConfigFilePath()) {
		t.Fatalf("err=%v, want the path", err)
	}
	if Generation() != generation {
		t.Fatal("an unreadable configuration was published")
	}
}

func TestLoadRefusesAConfiguredSeedItCannotUse(t *testing.T) {
	keepSettings(t)
	dir := t.TempDir()
	t.Setenv("LLMGW_STATE_DIR", filepath.Join(dir, "state"))
	target := filepath.Join(dir, "state", "config.yaml")
	valid := filepath.Join(dir, "valid.yaml")
	broken := filepath.Join(dir, "broken.yaml")
	blocker := filepath.Join(dir, "blocker")
	for path, content := range map[string]string{
		valid: "providers:\n  seeded:\n    type: echo\n", broken: "providers: [\n", blocker: "",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct{ name, seed, target, mention string }{
		{"missing", filepath.Join(dir, "missing.yaml"), target, "missing.yaml"},
		{"unparseable", broken, target, broken},
		// A regular file stands where the target's directory belongs.
		{"uncopyable", valid, filepath.Join(blocker, "config.yaml"), "config.yaml"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("LLMGW_CONFIG_SEED", test.seed)
			t.Setenv("LLMGW_CONFIG", test.target)
			generation := Generation()
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), test.mention) {
				t.Fatalf("err=%v, want it to name %q", err, test.mention)
			}
			if Generation() != generation {
				t.Fatal("an unusable seed published settings")
			}
			if _, err := os.Stat(test.target); err == nil {
				t.Fatal("an unusable seed was copied")
			}
		})
	}
}

func TestWritersRefuseToReplaceAConfigurationTheyCannotParse(t *testing.T) {
	keepSettings(t)
	useTempConfig(t)
	Update(func(s *Settings) { s.Providers = map[string]*ProviderConfig{"kept": {Type: "echo"}} })
	broken := "gateway_preamble: an edit in progress\nproviders: [\n"
	if err := os.WriteFile(ConfigFilePath(), []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}
	before, generation := Snapshot()
	if err := Save(); err == nil {
		t.Fatal("Save replaced a configuration that does not parse")
	}
	if _, err := UpdateAndSave(func(s *Settings) error {
		s.Providers["new"] = &ProviderConfig{Type: "echo"}
		return nil
	}); err == nil {
		t.Fatal("UpdateAndSave replaced a configuration that does not parse")
	}
	if added, err := AddProviderIfMissing("added", &ProviderConfig{Type: "echo"}); err == nil || added {
		t.Fatalf("AddProviderIfMissing added=%v err=%v", added, err)
	}
	if current, currentGeneration := Snapshot(); current != before || currentGeneration != generation {
		t.Fatal("a refused save changed the published settings")
	}
	if after, err := os.ReadFile(ConfigFilePath()); err != nil || string(after) != broken {
		t.Fatalf("the configuration changed: err=%v\n%s", err, after)
	}
}

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
				VertexRequestType: "dedicated", DefaultVoice: "voice-a", Disabled: true,
			},
		}
		s.OpenAICodexClientID = "codex-client"
		s.Endpoints = map[string]*EndpointConfig{
			"smart": {Failover: []EndpointMember{{Provider: "br", Model: "m1", AllowUnverified: true}}},
		}
	})
	if err := Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	reloaded := mustLoad(t)
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
		vertex.VertexRequestType != "dedicated" || vertex.DefaultVoice != "voice-a" || !vertex.Disabled {
		t.Fatalf("provider setup fields did not round-trip: %+v", vertex)
	}
	if reloaded.OpenAICodexClientID != "codex-client" {
		t.Fatalf("codex client id did not round-trip: %q", reloaded.OpenAICodexClientID)
	}
	ep, ok := reloaded.Endpoints["smart"]
	if !ok || len(ep.Failover) != 1 || ep.Failover[0].Provider != "br" || !ep.Failover[0].AllowUnverified {
		t.Fatalf("endpoint round-trip wrong: %+v", ep)
	}
}

// operatorConfig sets every key the loader reads, a key it does not know,
// comments, and ${ENV:NAME} references, as an operator's file might.
const operatorConfig = `# Operator notes stay with the file.
api_key: ${ENV:FIXTURE_ADMIN_KEY}
api_keys: [fixture-admin-a, fixture-admin-b]
allow_unauthenticated_api: true
rate_limit_per_minute: 42
gateway_preamble: Answer briefly. # house style
anthropic_discovery_aliases: false
anthropic_discovery_all_models: true
sso_enabled: true
sso_admin_group: gateway-admins
sso_auto_provision: false
openai_compatible_base_url: https://compatible.example.test/v1
openai_compatible_api_key: ${ENV:FIXTURE_COMPATIBLE_KEY}
openai_compatible_timeout_seconds: 120
ollama_base_url: http://ollama.example.test:11434
ollama_timeout_seconds: 45
litellm_timeout_seconds: 90
github_copilot_use_gh_cli: false
github_copilot_cache_dir: /var/cache/fixture
github_copilot_timeout_seconds: 60
github_copilot_editor_version: vscode/1.0.0
github_copilot_integration_id: fixture-integration
allow_copilot_proxy: true
openai_codex_client_id: fixture-codex-client
custom_future_setting:
  nested: [kept, as, written]
providers:
  # The hosted provider reads its URL and key from the environment.
  hosted:
    type: openai_compatible
    registry_id: openrouter
    base_url: ${ENV:FIXTURE_HOSTED_URL}
    api_key: ${ENV:FIXTURE_HOSTED_KEY}
    timeout: 120 # seconds
    custom_provider_setting: kept
  moved:
    type: openai_compatible
    base_url: ${ENV:FIXTURE_MOVED_URL}
  vertex:
    type: vertex_ai
    public_oauth_client_id: fixture-public-client
    region: fixture-region
    project: fixture-project
    location: us-central1
    vertex_request_type: dedicated
    default_voice: fixture-voice
    force_api_support: true
    api_key: fixture-literal-key
  retired:
    type: echo
    api_key: ${ENV:FIXTURE_RETIRED_KEY}
endpoints:
  smart:
    failover:
      - { provider: hosted, model: model-a } # primary
  retired-route:
    failover:
      - { provider: retired, model: model-b }
categories:
  shadowed:
    failover:
      - { provider: hosted, model: model-c }
policies:
  defaults:
    retry_max_attempts: 3
  overrides:
    hosted:
      circuit_failure_threshold: 2
savings:
  enabled: true
  db_path: /var/lib/fixture/usage.db
  baseline_model: hosted/model-a
`

func TestSavesKeepWhatTheyDoNotManage(t *testing.T) {
	keepSettings(t)
	useTempConfig(t)
	t.Setenv("FIXTURE_HOSTED_URL", "https://hosted.example.test/v1")
	t.Setenv("FIXTURE_HOSTED_KEY", "fixture-hosted-secret")
	t.Setenv("FIXTURE_MOVED_URL", "https://moved-old.example.test/v1")
	if err := os.WriteFile(ConfigFilePath(), []byte(operatorConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	before := mustLoad(t)
	if before.Providers["hosted"].BaseURL != "https://hosted.example.test/v1" || before.Providers["hosted"].APIKey != "fixture-hosted-secret" {
		t.Fatalf("fixture references did not resolve: %+v", before.Providers["hosted"])
	}

	console := func(change func(*Settings)) {
		t.Helper()
		if _, err := UpdateAndSave(func(s *Settings) error { change(s); return nil }); err != nil {
			t.Fatalf("save: %v", err)
		}
	}
	// Shaped like the provider upsert, which rebuilds an entry without its key.
	console(func(s *Settings) {
		previous := s.Providers["vertex"]
		s.Providers["vertex"] = &ProviderConfig{
			Type: previous.Type, Project: previous.Project, Location: previous.Location, Region: "changed-region",
			PublicOAuthClientID: previous.PublicOAuthClientID, VertexRequestType: previous.VertexRequestType,
			DefaultVoice: previous.DefaultVoice, Timeout: previous.Timeout, Disabled: previous.Disabled,
		}
		s.Providers["moved"].BaseURL = "https://moved-new.example.test/v1"
	})
	// Shaped like the provider enable toggle.
	console(func(s *Settings) { s.Providers["hosted"].Disabled = true })
	// Shaped like the endpoint and provider deletes.
	console(func(s *Settings) { delete(s.Endpoints, "retired-route") })
	console(func(s *Settings) { delete(s.Providers, "retired") })
	// Shaped like an endpoint save.
	console(func(s *Settings) {
		s.Endpoints["fast"] = &EndpointConfig{Failover: []EndpointMember{{Provider: "vertex", Model: "model-d"}}}
	})

	raw, err := os.ReadFile(ConfigFilePath())
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, kept := range []string{
		"# Operator notes stay with the file.", "# house style", "# seconds", "# primary",
		"# The hosted provider reads its URL and key from the environment.",
		"api_key: ${ENV:FIXTURE_ADMIN_KEY}", "openai_compatible_api_key: ${ENV:FIXTURE_COMPATIBLE_KEY}",
		"base_url: ${ENV:FIXTURE_HOSTED_URL}", "api_key: ${ENV:FIXTURE_HOSTED_KEY}",
		"custom_provider_setting: kept", "api_key: fixture-literal-key",
		"custom_future_setting:", "nested: [kept, as, written]",
		"base_url: https://moved-new.example.test/v1",
	} {
		if !strings.Contains(text, kept) {
			t.Errorf("the saved file lost %q", kept)
		}
	}
	for _, dropped := range []string{
		"retired", "categories:", "shadowed", "FIXTURE_MOVED_URL",
		"https://hosted.example.test/v1", "fixture-hosted-secret", "force_api_support",
	} {
		if strings.Contains(text, dropped) {
			t.Errorf("the saved file holds %q", dropped)
		}
	}
	if t.Failed() {
		t.Fatalf("saved file:\n%s", text)
	}

	after := mustLoad(t)
	unmanaged := func(s *Settings) *Settings {
		rest := cloneSettings(s)
		rest.Providers, rest.Endpoints, rest.OpenAICodexClientID = nil, nil, ""
		rest.Policies, rest.Savings = BackendPolicies{}, SavingsConfig{}
		return rest
	}
	if !reflect.DeepEqual(unmanaged(after), unmanaged(before)) {
		t.Fatalf("settings a save does not manage changed:\n got %+v\nwant %+v", unmanaged(after), unmanaged(before))
	}
	if !reflect.DeepEqual(after.Policies, before.Policies) || !reflect.DeepEqual(after.Savings, before.Savings) ||
		after.OpenAICodexClientID != "fixture-codex-client" {
		t.Fatalf("untouched managed sections changed: %+v %+v %q", after.Policies, after.Savings, after.OpenAICodexClientID)
	}
	hosted, vertex := after.Providers["hosted"], after.Providers["vertex"]
	if hosted == nil || !hosted.Disabled || hosted.APIKey != "fixture-hosted-secret" || hosted.BaseURL != "https://hosted.example.test/v1" {
		t.Fatalf("hosted=%+v", hosted)
	}
	if vertex == nil || vertex.Region != "changed-region" || vertex.ForceApiSupport || vertex.APIKey != "fixture-literal-key" ||
		vertex.Project != "fixture-project" || vertex.PublicOAuthClientID != "fixture-public-client" {
		t.Fatalf("vertex=%+v", vertex)
	}
	if after.Providers["retired"] != nil || after.Endpoints["retired-route"] != nil || after.Endpoints["shadowed"] != nil ||
		after.Endpoints["smart"] == nil || after.Endpoints["fast"] == nil {
		t.Fatalf("providers=%v endpoints=%v", slices.Sorted(maps.Keys(after.Providers)), slices.Sorted(maps.Keys(after.Endpoints)))
	}
}

// A failed delete restores the entry the update removed, with the keys a
// save does not manage.
func TestRestoreReturnsWhatTheUpdateRemoved(t *testing.T) {
	keepSettings(t)
	useTempConfig(t)
	original := "providers:\n  kept:\n    type: echo # restored with its comment\n    api_key: ${ENV:FIXTURE_KEPT_KEY}\n    custom_provider_setting: kept\n"
	if err := os.WriteFile(ConfigFilePath(), []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	mustLoad(t)
	restore, err := UpdateAndSave(func(s *Settings) error {
		delete(s.Providers, "kept")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(ConfigFilePath()); strings.Contains(string(raw), "kept") {
		t.Fatalf("delete left the entry:\n%s", raw)
	}
	if err := restore(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(ConfigFilePath())
	if err != nil {
		t.Fatal(err)
	}
	for _, kept := range []string{"api_key: ${ENV:FIXTURE_KEPT_KEY}", "custom_provider_setting: kept", "# restored with its comment"} {
		if !strings.Contains(string(raw), kept) {
			t.Fatalf("restore lost %q:\n%s", kept, raw)
		}
	}
}

// A save renames a file's pre-rename categories: key where it stands, so its
// comments stay with it.
func TestSaveRenamesCategoriesInPlace(t *testing.T) {
	keepSettings(t)
	useTempConfig(t)
	legacy := "gateway_preamble: first\n# Routes for the team.\ncategories:\n  smart:\n    failover:\n      - { provider: echo, model: model-a }\nsso_admin_group: last\n"
	if err := os.WriteFile(ConfigFilePath(), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	mustLoad(t)
	if err := Save(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(ConfigFilePath())
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	routes, endpoints, last := strings.Index(text, "# Routes for the team."), strings.Index(text, "endpoints:"), strings.Index(text, "sso_admin_group: last")
	if strings.Contains(text, "categories:") || routes < 0 || endpoints < routes || last < endpoints {
		t.Fatalf("categories: was not renamed in place:\n%s", text)
	}
	if mustLoad(t).Endpoints["smart"] == nil {
		t.Fatalf("the renamed endpoints did not load:\n%s", text)
	}
}

// An entry that aliases another is expanded before a save edits it, so it
// keeps the keys it shared and the edit does not reach the entry it named.
func TestSaveExpandsAnAliasedEntryItEdits(t *testing.T) {
	keepSettings(t)
	useTempConfig(t)
	shared := "providers:\n  primary: &shared\n    type: openai_compatible\n    api_key: ${ENV:FIXTURE_SHARED_KEY}\n  secondary: *shared\n"
	if err := os.WriteFile(ConfigFilePath(), []byte(shared), 0o600); err != nil {
		t.Fatal(err)
	}
	mustLoad(t)
	if _, err := UpdateAndSave(func(s *Settings) error {
		s.Providers["secondary"].Disabled = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(ConfigFilePath())
	if err != nil {
		t.Fatal(err)
	}
	after := mustLoad(t)
	if after.Providers["primary"].Disabled || !after.Providers["secondary"].Disabled ||
		strings.Count(string(raw), "api_key: ${ENV:FIXTURE_SHARED_KEY}") != 2 {
		t.Fatalf("aliased entry was not expanded:\n%s", raw)
	}
}

func TestAntigravityOAuthClientConfigurationIsRuntimeOnly(t *testing.T) {
	t.Setenv("LLMGW_GOOGLE_ANTIGRAVITY_CLIENT_ID", "fixture-client-id")
	t.Setenv("LLMGW_GOOGLE_ANTIGRAVITY_CLIENT_SECRET", "fixture-client-secret")
	t.Setenv("LLMGW_GOOGLE_ANTIGRAVITY_OAUTH_PROFILE", "consumer_manual")
	t.Setenv("LLMGW_GOOGLE_ANTIGRAVITY_CLIENT_MODE", "confidential")
	t.Setenv("LLMGW_GOOGLE_ANTIGRAVITY_REDIRECT_URI", "https://callback.example.test/oauth")
	settings := Defaults()
	applyEnv(settings)
	if settings.GoogleAntigravityClientID != "fixture-client-id" || settings.GoogleAntigravityClientSecret != "fixture-client-secret" ||
		settings.GoogleAntigravityOAuthProfile != "consumer_manual" || settings.GoogleAntigravityClientMode != "confidential" ||
		settings.GoogleAntigravityRedirectURI != "https://callback.example.test/oauth" {
		t.Fatalf("runtime OAuth configuration was not loaded")
	}
	payload := configPayload(settings)
	if _, ok := payload["google_antigravity_client_id"]; ok {
		t.Fatal("Antigravity client ID entered persisted config")
	}
	if _, ok := payload["google_antigravity_client_secret"]; ok {
		t.Fatal("Antigravity client secret entered persisted config")
	}
	for _, field := range []string{"google_antigravity_oauth_profile", "google_antigravity_client_mode", "google_antigravity_redirect_uri"} {
		if _, ok := payload[field]; ok {
			t.Fatalf("Antigravity runtime OAuth field %q entered persisted config", field)
		}
	}
}

func TestAntigravityPublicOAuthClientIDPersistsWithProvider(t *testing.T) {
	settings := Defaults()
	settings.Providers["antigravity"] = &ProviderConfig{
		Type: "google_antigravity", PublicOAuthClientID: "fixture-public-client",
	}
	payload := configPayload(settings)
	provider := payload["providers"].(map[string]any)["antigravity"].(map[string]any)
	if provider["public_oauth_client_id"] != "fixture-public-client" {
		t.Fatalf("provider payload=%+v", provider)
	}
}

func TestPublicOAuthClientIDLoadsAndRoundTrips(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LLMGW_STATE_DIR", dir)
	t.Setenv("LLMGW_CONFIG", filepath.Join(dir, "config.yaml"))
	if err := os.WriteFile(ConfigFilePath(), []byte("providers:\n  antigravity:\n    type: google_antigravity\n    public_oauth_client_id: fixture-public-client\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded := mustLoad(t)
	if loaded.Providers["antigravity"].PublicOAuthClientID != "fixture-public-client" {
		t.Fatalf("loaded provider=%+v", loaded.Providers["antigravity"])
	}
	if err := Save(); err != nil {
		t.Fatal(err)
	}
	reloaded := mustLoad(t)
	if reloaded.Providers["antigravity"].PublicOAuthClientID != "fixture-public-client" {
		t.Fatalf("reloaded provider=%+v", reloaded.Providers["antigravity"])
	}
}

func TestAddProviderIfMissingPersistsWithoutOverwrite(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LLMGW_STATE_DIR", dir)
	t.Setenv("LLMGW_CONFIG", filepath.Join(dir, "config.yaml"))
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("# keep this note\ngateway_preamble: keep-me\ncustom_future_setting: keep-too\nproviders:\n  existing:\n    type: openai_compatible\n    api_key: ${ENV:EXISTING_KEY}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mustLoad(t)
	added, err := AddProviderIfMissing("anonymous", &ProviderConfig{Type: "openai_compatible", RegistryID: "fixture", BaseURL: "https://example.com/v1"})
	if err != nil || !added {
		t.Fatalf("added=%v err=%v", added, err)
	}
	added, err = AddProviderIfMissing("anonymous", &ProviderConfig{Type: "echo"})
	if err != nil || added {
		t.Fatalf("overwrite added=%v err=%v", added, err)
	}
	reloaded := mustLoad(t)
	if got := reloaded.Providers["anonymous"]; got == nil || got.RegistryID != "fixture" || got.Type != "openai_compatible" {
		t.Fatalf("provider=%+v", got)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, preserved := range []string{"# keep this note", "gateway_preamble: keep-me", "custom_future_setting: keep-too", "api_key: ${ENV:EXISTING_KEY}"} {
		if !strings.Contains(text, preserved) {
			t.Fatalf("provider patch dropped %q:\n%s", preserved, text)
		}
	}
	if info, err := os.Stat(filepath.Join(dir, "config.yaml")); err != nil ||
		(runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0) {
		t.Fatalf("config permissions=%v err=%v", info.Mode().Perm(), err)
	}
}

func TestAddProviderIfMissingRejectsMalformedExistingConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LLMGW_STATE_DIR", dir)
	t.Setenv("LLMGW_CONFIG", filepath.Join(dir, "config.yaml"))
	Update(func(s *Settings) { s.Providers = map[string]*ProviderConfig{} })
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("providers: [broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	added, err := AddProviderIfMissing("anonymous", &ProviderConfig{Type: "openai_compatible"})
	if err == nil || added || Get().Providers["anonymous"] != nil {
		t.Fatalf("added=%v err=%v providers=%+v", added, err, Get().Providers)
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

func TestPriceCatalogLoadsAndSurvivesASave(t *testing.T) {
	keepSettings(t)
	useTempConfig(t)
	content := "savings:\n  enabled: false\n  price_catalog:\n    fixture-model:\n      input: 0.15 # per million\n      output: 2\n"
	if err := os.WriteFile(ConfigFilePath(), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	want := map[string]map[string]float64{"fixture-model": {"input": 0.15, "output": 2}}
	if got := mustLoad(t).Savings.PriceCatalog; !reflect.DeepEqual(got, want) {
		t.Fatalf("price catalog=%v, want %v", got, want)
	}
	if _, err := UpdateAndSave(func(s *Settings) error {
		s.Providers["added"] = &ProviderConfig{Type: "echo"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(ConfigFilePath())
	if err != nil {
		t.Fatal(err)
	}
	if got := mustLoad(t).Savings.PriceCatalog; !reflect.DeepEqual(got, want) || !strings.Contains(string(raw), "# per million") {
		t.Fatalf("price catalog after a save=%v:\n%s", got, raw)
	}
}

func TestLoadRefusesAnInvalidPriceCatalog(t *testing.T) {
	keepSettings(t)
	useTempConfig(t)
	for name, catalog := range map[string]string{
		"negative":    "{m: {input: -1, output: 1}}",
		"non-numeric": "{m: {input: cheap, output: 1}}",
		"boolean":     "{m: {input: 1, output: true}}",
		"missing":     "{m: {input: 1}}",
		"not a price": "{m: 5}",
		"not a map":   "[m]",
	} {
		t.Run(name, func(t *testing.T) {
			content := "savings:\n  price_catalog: " + catalog + "\n"
			if err := os.WriteFile(ConfigFilePath(), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			generation := Generation()
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), ConfigFilePath()) || !strings.Contains(err.Error(), "savings.price_catalog") {
				t.Fatalf("err=%v, want the path and the setting", err)
			}
			if Generation() != generation {
				t.Fatal("an invalid price catalog was published")
			}
			if err := Save(); err == nil {
				t.Fatal("Save replaced a configuration the loader refuses")
			}
		})
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
	if err := applyConfig(s, payload); err != nil {
		t.Fatalf("apply: %v", err)
	}
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

func TestProviderPoliciesLoadAndRoundTrip(t *testing.T) {
	settings := parseSettingsForTest(t, `
policies:
  defaults:
    retry_max_attempts: 3
    retry_initial_backoff_seconds: 0.25
    retry_max_backoff_seconds: 5
    retry_backoff_multiplier: 1.5
    circuit_failure_threshold: 6
    circuit_cooldown_seconds: 45
  overrides:
    copilot:
      retry_max_attempts: 1
      circuit_failure_threshold: 2
`)
	if settings.Policies.Defaults.RetryMaxAttempts != 3 ||
		settings.Policies.Defaults.CircuitCooldownSeconds != 45 ||
		settings.Policies.Overrides["copilot"].CircuitFailureThreshold != 2 ||
		settings.Policies.Overrides["copilot"].RetryInitialBackoffSeconds != 0.25 ||
		settings.Policies.Overrides["copilot"].RetryBackoffMultiplier != 1.5 {
		t.Fatalf("policies did not load: %+v", settings.Policies)
	}
	reloaded := parseSettingsForTest(t, serialiseSettingsForTest(t, settings))
	if reloaded.Policies.Defaults.RetryInitialBackoffSeconds != 0.25 ||
		reloaded.Policies.Overrides["copilot"].RetryMaxAttempts != 1 {
		t.Fatalf("policies did not round-trip: %+v", reloaded.Policies)
	}
}

func TestPartialProviderPoliciesInheritDefaultsAndPreserveExplicitZero(t *testing.T) {
	settings := parseSettingsForTest(t, `
policies:
  defaults:
    retry_max_attempts: 3
  overrides:
    inherited:
      circuit_failure_threshold: 2
    disabled:
      retry_max_attempts: 0
      circuit_failure_threshold: 0
`)
	if settings.Policies.Defaults.RetryInitialBackoffSeconds != 0.5 ||
		settings.Policies.Defaults.RetryMaxBackoffSeconds != 8 ||
		settings.Policies.Overrides["inherited"].RetryMaxAttempts != 3 ||
		settings.Policies.Overrides["inherited"].RetryInitialBackoffSeconds != 0.5 ||
		settings.Policies.Overrides["disabled"].RetryMaxAttempts != 0 ||
		settings.Policies.Overrides["disabled"].CircuitFailureThreshold != 0 {
		t.Fatalf("partial policy inheritance=%+v", settings.Policies)
	}
	serialized := serialiseSettingsForTest(t, settings)
	var payload map[string]any
	if err := yaml.Unmarshal([]byte(serialized), &payload); err != nil {
		t.Fatal(err)
	}
	policies := payload["policies"].(map[string]any)
	overrides := policies["overrides"].(map[string]any)
	inherited := overrides["inherited"].(map[string]any)
	if len(inherited) != 1 || inherited["circuit_failure_threshold"] == nil {
		t.Fatalf("save materialized inherited policy fields: %+v", inherited)
	}
	defaults := policies["defaults"].(map[string]any)
	defaults["retry_initial_backoff_seconds"] = 1.25
	encoded, err := yaml.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	reloaded := parseSettingsForTest(t, string(encoded))
	if reloaded.Policies.Overrides["inherited"].RetryInitialBackoffSeconds != 1.25 {
		t.Fatalf("saved sparse override stopped inheriting new default: %+v", reloaded.Policies)
	}
	configured := settings.Policies.ConfiguredOverrides()["inherited"].(map[string]any)
	if len(configured) != 1 || configured["circuit_failure_threshold"] == nil {
		t.Fatalf("configured override exposed inherited fields: %+v", configured)
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

func TestUpdateAndSaveFailurePreservesMemory(t *testing.T) {
	keepSettings(t)
	dir := t.TempDir()
	t.Setenv("LLMGW_STATE_DIR", dir)
	t.Setenv("LLMGW_CONFIG", filepath.Join(dir, "missing", "config.yaml"))
	Update(func(s *Settings) {
		*s = *Defaults()
		s.Endpoints["existing"] = &EndpointConfig{Failover: []EndpointMember{{Provider: "echo", Model: "old"}}}
	})
	before, generation := Snapshot()

	restore, err := UpdateAndSave(func(next *Settings) error {
		next.Endpoints["new"] = &EndpointConfig{Failover: []EndpointMember{{Provider: "echo", Model: "new"}}}
		return nil
	})
	if err == nil || restore != nil {
		t.Fatalf("restore=%v err=%v", restore != nil, err)
	}
	current, currentGeneration := Snapshot()
	if current != before || currentGeneration != generation {
		t.Fatalf("failed save published generation %d over %d", currentGeneration, generation)
	}
	if current.Endpoints["new"] != nil || current.Endpoints["existing"].Failover[0].Model != "old" {
		t.Fatalf("failed save changed memory: %+v", current.Endpoints)
	}
}
