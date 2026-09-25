package api

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
	"llmgw/internal/router"

	core "github.com/xibodev/llmgw-core"
)

func TestAnonymousModelListPublishesOnlyExactVerifiedTargets(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	profile := providers.AnonymousProviderProfiles()[0]
	oldProviders := config.Get().Providers
	t.Cleanup(func() { config.Update(func(s *config.Settings) { s.Providers = oldProviders }) })
	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{profile.ProviderID: {
			Type: profile.RuntimeType, RegistryID: profile.RegistryID, BaseURL: profile.BaseURL,
		}}
	})
	if err := iam.MarkAnonymousProviderManaged(profile.ProviderID); err != nil {
		t.Fatal(err)
	}
	generation, err := iam.ProviderCheckGeneration(profile.ProviderID, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, evidence := range []iam.ProviderModelEvidence{
		{ProviderID: profile.ProviderID, Model: "working", Operation: iam.ModelEvidenceCompletion, State: "verified", Generation: generation},
		{ProviderID: profile.ProviderID, Model: "broken", Operation: iam.ModelEvidenceCompletion, State: "failed", Generation: generation},
	} {
		if err := iam.RecordProviderModelEvidence(evidence); err != nil {
			t.Fatal(err)
		}
	}
	originalCatalog := catalogModelsForPrincipal
	catalogModelsForPrincipal = func(providerID string, _ *config.Principal) []providers.ModelInfo {
		if providerID == profile.ProviderID {
			return []providers.ModelInfo{{ID: "working"}, {ID: "broken"}, {ID: "sibling"}}
		}
		return nil
	}
	t.Cleanup(func() { catalogModelsForPrincipal = originalCatalog })

	public, err := buildModelList(nil)
	if err != nil {
		t.Fatal(err)
	}
	rows := public["data"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["id"] != profile.ProviderID+"/working" {
		t.Fatalf("public rows=%+v", rows)
	}
	scoped, err := buildModelList(&config.Principal{PrincipalID: "human-owner", PrincipalKind: "human", ProjectID: "project"})
	if err != nil {
		t.Fatal(err)
	}
	scopedRows := scoped["data"].([]any)
	if len(scopedRows) != 1 || scopedRows[0].(map[string]any)["id"] != profile.ProviderID+"/working" {
		t.Fatalf("scoped rows=%+v", scopedRows)
	}
	for _, field := range []string{"publication_state", "published", "disabled", "admin_unverified_opt_in", "observed_at", "latency_ms", "failure_code"} {
		if _, exists := rows[0].(map[string]any)[field]; exists {
			t.Fatalf("public row exposed admin diagnostic %q: %+v", field, rows[0])
		}
	}
	diagnostics, err := buildModelListWithDiagnostics(nil, true)
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for _, raw := range diagnostics["data"].([]any) {
		row := raw.(map[string]any)
		states[row["id"].(string)] = row["publication_state"].(string)
	}
	if !reflect.DeepEqual(states, map[string]string{
		profile.ProviderID + "/working": "verified",
		profile.ProviderID + "/broken":  "failed",
		profile.ProviderID + "/sibling": "unverified",
	}) {
		t.Fatalf("diagnostic states=%+v", states)
	}
}

func TestIsChatModel(t *testing.T) {
	cases := []struct {
		name string
		row  providers.ModelInfo
		want bool
	}{
		{"claude via caps", providers.ModelInfo{ID: "claude-opus-4.8", Capabilities: map[string]any{"context_window": 1000000, "tool_calls": true}}, true},
		{"gpt via caps", providers.ModelInfo{ID: "gpt-4o-mini", Capabilities: map[string]any{"context_window": 128000}}, true},
		{"chat via endpoint only", providers.ModelInfo{ID: "some-chat", SupportedSurfaces: []string{"/v1/messages"}}, true},
		{"responses endpoint", providers.ModelInfo{ID: "gpt-5.5", SupportedSurfaces: []string{"/responses", "ws:/responses"}}, true},
		{"embedding caps family only", providers.ModelInfo{ID: "text-embedding-3-small", Capabilities: map[string]any{"family": "embed"}}, false},
		{"utility trajectory-compaction has chat caps but excluded", providers.ModelInfo{ID: "trajectory-compaction", Capabilities: map[string]any{"context_window": 262144, "tool_calls": true, "family": "trajectory-compaction"}, SupportedSurfaces: []string{"/chat/completions"}}, false},
		{"mai-code coding model kept", providers.ModelInfo{ID: "mai-code-1-flash-picker", Capabilities: map[string]any{"context_window": 256000, "tool_calls": true, "family": "oswe-vscode-modelD"}, SupportedSurfaces: []string{"/responses"}}, true},
		{"local audio no metadata", providers.ModelInfo{ID: "vits-piper-en_US-amy-medium.tar.bz2"}, false},
		{"whisper no metadata", providers.ModelInfo{ID: "whisper-base"}, false},
	}
	for _, c := range cases {
		if got := isChatModel(c.row); got != c.want {
			t.Errorf("%s: isChatModel = %v, want %v", c.name, got, c.want)
		}
	}

}

func TestModelListAliasesAreDeterministicUniqueAndRoundTrip(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		models := `{"data":[{"id":"echo-strong"},{"id":"echo-deep"}]}`
		if r.Header.Get("x-api-key") == "alpha-fixture" {
			models = `{"data":[{"id":"echo-strong"},{"id":"echo-deep"},{"id":"alpha-only"}]}`
		}
		_, _ = w.Write([]byte(models))
	}))
	defer upstream.Close()
	config.Update(func(s *config.Settings) {
		s.AllowUnauthenticatedAPI = true
		s.AnthropicDiscoveryAliases = true
		s.AnthropicDiscoveryAllModels = true
		s.Providers = map[string]*config.ProviderConfig{
			"zeta":  {Type: "anthropic", BaseURL: upstream.URL, APIKey: "zeta-fixture"},
			"alpha": {Type: "anthropic", BaseURL: upstream.URL, APIKey: "alpha-fixture"},
		}
		s.Endpoints = map[string]*config.EndpointConfig{
			"coding":           {Failover: []config.EndpointMember{{Provider: "zeta", Model: "echo-default"}}},
			"CLAUDE-ECHO-DEEP": {Failover: []config.EndpointMember{{Provider: "zeta", Model: "echo-deep"}}},
			"alpha/echo-deep":  {Failover: []config.EndpointMember{{Provider: "zeta", Model: "echo-deep"}}},
		}
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)
	for _, providerID := range []string{"alpha", "zeta"} {
		if rows := providers.RefreshCatalog(providerID); len(rows) == 0 {
			t.Fatalf("%s refresh returned no models", providerID)
		}
	}
	originalCatalog := catalogModelsForPrincipal
	catalogCalls := map[string]int{}
	catalogModelsForPrincipal = func(provider string, principal *config.Principal) []providers.ModelInfo {
		catalogCalls[provider]++
		return originalCatalog(provider, principal)
	}
	t.Cleanup(func() { catalogModelsForPrincipal = originalCatalog })

	first, err := buildModelList(nil)
	if err != nil {
		t.Fatal(err)
	}
	for provider, calls := range catalogCalls {
		if calls != 1 {
			t.Fatalf("catalog calls for %s=%d, want 1 per model-list request", provider, calls)
		}
	}
	second, err := buildModelList(nil)
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("model list changed between calls: %v", err)
	}
	for provider, calls := range catalogCalls {
		if calls != 2 {
			t.Fatalf("catalog calls for %s=%d after two requests, want 2", provider, calls)
		}
	}
	rows := first["data"].([]any)
	last := ""
	for _, raw := range rows {
		id := raw.(map[string]any)["id"].(string)
		if id < last {
			t.Fatalf("rows not sorted: %q before %q", last, id)
		}
		last = id
		if id == "claude-echo-strong" || id == "claude-echo-deep" {
			t.Fatalf("ambiguous/colliding alias advertised: %q", id)
		}
	}
	for _, exact := range []string{"alpha/echo-strong", "zeta/echo-strong", "coding", "CLAUDE-ECHO-DEEP"} {
		found := false
		for _, raw := range rows {
			found = found || raw.(map[string]any)["id"] == exact
		}
		if !found {
			t.Fatalf("exact row %q missing: %+v", exact, rows)
		}
	}
	owners := map[string]string{}
	for _, raw := range rows {
		row := raw.(map[string]any)
		owners[row["id"].(string)] = row["owned_by"].(string)
	}
	if owners["alpha/echo-deep"] != "endpoint" {
		t.Fatalf("endpoint did not win exact ID collision: %+v", owners)
	}
	catalogModelsForPrincipal = originalCatalog
	for _, raw := range rows {
		row := raw.(map[string]any)
		id, owner := row["id"].(string), row["owned_by"].(string)
		resolution, err := resolveAs(id, nil)
		if err != nil {
			t.Fatalf("advertised row %q does not resolve: %v", id, err)
		}
		switch owner {
		case "endpoint":
			failover := row["failover"].([]any)
			if len(resolution.Targets) != len(failover) || resolution.Category != id {
				t.Fatalf("endpoint %q resolution=%+v row=%+v", id, resolution, row)
			}
			for i, rawTarget := range failover {
				target := rawTarget.(map[string]any)
				if resolution.Targets[i] != (router.Target{Provider: target["provider"].(string), Model: target["model"].(string)}) {
					t.Fatalf("endpoint %q target %d mismatch: %+v vs %+v", id, i, resolution.Targets[i], target)
				}
			}
		case strings.SplitN(id, "/", 2)[0]:
			parts := strings.SplitN(id, "/", 2)
			if len(parts) != 2 || len(resolution.Targets) != 1 || resolution.Targets[0] != (router.Target{Provider: parts[0], Model: parts[1]}) {
				t.Fatalf("exact row %q resolution=%+v", id, resolution)
			}
		default:
			wantModel := strings.TrimPrefix(id, "claude-")
			if len(resolution.Targets) != 1 || resolution.Targets[0] != (router.Target{Provider: owner, Model: wantModel}) {
				t.Fatalf("alias row %q owner=%q resolution=%+v", id, owner, resolution)
			}
		}
	}

	principal := &config.Principal{AllowedProviders: []string{"alpha"}}
	filtered, err := buildModelList(principal)
	if err != nil {
		t.Fatal(err)
	}
	alias := "claude-echo-strong"
	found := false
	for _, raw := range filtered["data"].([]any) {
		row := raw.(map[string]any)
		if row["id"] == alias {
			found = row["owned_by"] == "alpha"
		}
	}
	resolution, err := resolveAs(alias, principal)
	if err != nil || !found || resolution.Targets[0] != (router.Target{Provider: "alpha", Model: "echo-strong"}) {
		t.Fatalf("policy-unique alias found=%v resolution=%+v err=%v", found, resolution, err)
	}
}

func TestModelPresentationMetadataInfersLocalAudioCapabilities(t *testing.T) {
	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{
			"localai": {Type: "openai_compatible"},
			"gemini": {
				Type: "openai_compatible", RegistryID: "gemini",
			},
			"custom": {Type: "openai_compatible"},
			"ollama": {Type: "ollama"},
		}
	})
	for _, testCase := range []struct {
		name       string
		provider   string
		model      providers.ModelInfo
		capability string
		endpoint   string
		want       bool
	}{
		{
			name:       "whisper transcription",
			provider:   "localai",
			model:      providers.ModelInfo{ID: "whisper-base"},
			capability: "transcription", endpoint: "/v1/audio/transcriptions",
			want: true,
		},
		{
			name:       "piper speech",
			provider:   "localai",
			model:      providers.ModelInfo{ID: "en_US-amy-piper"},
			capability: "tts", endpoint: "/v1/audio/speech",
			want: true,
		},
		{
			name:       "ollama name is not inferred",
			provider:   "ollama",
			model:      providers.ModelInfo{ID: "whisper-base"},
			capability: "transcription", endpoint: "/v1/audio/transcriptions",
			want: false,
		},
		{
			name:       "gemini tts name is not inferred",
			provider:   "gemini",
			model:      providers.ModelInfo{ID: "gemini-2.5-flash-preview-tts"},
			capability: "tts", endpoint: "/v1/audio/speech",
			want: false,
		},
		{
			name:       "custom compatible provider is not inferred",
			provider:   "custom",
			model:      providers.ModelInfo{ID: "whisper-base"},
			capability: "transcription", endpoint: "/v1/audio/transcriptions",
			want: false,
		},
		{
			name:     "explicit negative is authoritative",
			provider: "localai",
			model: providers.ModelInfo{
				ID:           "whisper-base",
				Capabilities: map[string]any{"transcription": false},
			},
			capability: "transcription", endpoint: "/v1/audio/transcriptions",
			want: false,
		},
		{
			name:     "explicit tts negative is authoritative",
			provider: "localai",
			model: providers.ModelInfo{
				ID:           "en_US-amy-piper",
				Capabilities: map[string]any{"tts": false},
			},
			capability: "tts", endpoint: "/v1/audio/speech",
			want: false,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			capabilities, endpoints := modelPresentationMetadata(
				testCase.provider, testCase.model,
			)
			if got := capabilities[testCase.capability] == true; got != testCase.want {
				t.Fatalf("capabilities=%+v", capabilities)
			}
			found := false
			for _, endpoint := range endpoints {
				if endpoint == testCase.endpoint {
					found = true
				}
			}
			if found != testCase.want {
				t.Fatalf("endpoints=%+v", endpoints)
			}
		})
	}
}

func TestCommonCategoryPresentationMetadata(t *testing.T) {
	capabilities, endpoints := commonCategoryPresentationMetadata(
		[]map[string]bool{{"chat": true}, {"chat": true}},
	)
	if capabilities["chat"] != true || !reflect.DeepEqual(endpoints, []string{"/v1/chat/completions"}) {
		t.Fatalf("chat category metadata=%+v endpoints=%+v", capabilities, endpoints)
	}

	capabilities, endpoints = commonCategoryPresentationMetadata(
		[]map[string]bool{
			{"transcription": true},
			{"transcription": true},
		},
	)
	if capabilities["transcription"] != true ||
		len(endpoints) != 1 ||
		endpoints[0] != "/v1/audio/transcriptions" {
		t.Fatalf("audio category metadata=%+v endpoints=%+v", capabilities, endpoints)
	}

	capabilities, endpoints = commonCategoryPresentationMetadata(
		[]map[string]bool{
			{"transcription": true},
			{"tts": true},
		},
	)
	if len(capabilities) != 0 || len(endpoints) != 0 {
		t.Fatalf("mixed category should have no modality: %+v %+v", capabilities, endpoints)
	}
}

// setSurfaces is the sole place /v1/models writes the HTTP-surfaces field, so
// this is what guarantees the wire format actually carries both the canonical
// supported_surfaces key and the deprecated supported_endpoints compatibility
// key, with identical values.
func TestSetSurfacesEmitsBothKeysWithEqualValues(t *testing.T) {
	entry := map[string]any{}
	setSurfaces(entry, []string{"/v1/chat/completions", "/v1/messages"})

	surfaces, ok := entry["supported_surfaces"].([]string)
	if !ok {
		t.Fatalf("supported_surfaces missing or wrong type: %+v", entry)
	}
	endpoints, ok := entry["supported_endpoints"].([]string)
	if !ok {
		t.Fatalf("supported_endpoints missing or wrong type: %+v", entry)
	}
	if len(surfaces) != 2 || len(endpoints) != 2 {
		t.Fatalf("expected 2 values in both keys, got surfaces=%v endpoints=%v", surfaces, endpoints)
	}
	for i := range surfaces {
		if surfaces[i] != endpoints[i] {
			t.Fatalf("supported_surfaces=%v does not equal supported_endpoints=%v", surfaces, endpoints)
		}
	}

	// An empty surface list sets neither key, matching the pre-rename behavior
	// (omitted rather than an empty array) so unrelated wire snapshots don't
	// gain new empty fields.
	empty := map[string]any{}
	setSurfaces(empty, nil)
	if _, ok := empty["supported_surfaces"]; ok {
		t.Fatalf("supported_surfaces set for an empty surface list: %+v", empty)
	}
	if _, ok := empty["supported_endpoints"]; ok {
		t.Fatalf("supported_endpoints set for an empty surface list: %+v", empty)
	}
}

func TestModelListAddsTypedCapabilitiesWithoutChangingLegacyFields(t *testing.T) {
	config.Update(func(s *config.Settings) {
		s.AllowUnauthenticatedAPI = true
		s.Providers = map[string]*config.ProviderConfig{"typed": {Type: "echo"}}
		s.Endpoints = map[string]*config.EndpointConfig{}
	})
	originalCatalog := catalogModelsForPrincipal
	catalogModelsForPrincipal = func(string, *config.Principal) []providers.ModelInfo {
		return []providers.ModelInfo{{
			ID: "model", Capabilities: map[string]any{
				"chat": true, "vision": true, "context_window": 32000,
			}, SupportedSurfaces: []string{"/v1/chat/completions"},
		}}
	}
	t.Cleanup(func() { catalogModelsForPrincipal = originalCatalog })

	response, err := buildModelList(nil)
	if err != nil {
		t.Fatal(err)
	}
	var row map[string]any
	for _, raw := range response["data"].([]any) {
		candidate := raw.(map[string]any)
		if candidate["id"] == "typed/model" {
			row = candidate
			break
		}
	}
	if row == nil {
		t.Fatalf("typed model missing: %+v", response["data"])
	}
	legacy := row["capabilities"].(map[string]any)
	if legacy["chat"] != true || legacy["vision"] != true || legacy["context_window"] != 32000 {
		t.Fatalf("legacy capabilities changed: %+v", legacy)
	}
	if row["supported_surfaces"].([]string)[0] != "/v1/chat/completions" ||
		row["supported_endpoints"].([]string)[0] != "/v1/chat/completions" {
		t.Fatalf("legacy surfaces changed: %+v", row)
	}
	typed, ok := row["typed_capabilities"].(*core.ModelCapabilities)
	if !ok || typed.Operations.Chat != core.SupportSupported || typed.Inputs.Image != core.SupportSupported ||
		typed.Limits.ContextTokens == nil || *typed.Limits.ContextTokens != 32000 {
		t.Fatalf("typed capabilities = %#v", row["typed_capabilities"])
	}
	if typed.Freshness.VerifiedAt != nil {
		t.Fatalf("model list invented verification: %+v", typed.Freshness)
	}
}

func TestModelListTransportMetadataUsesExecutionDeclarationsAndPlanner(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	config.Update(func(s *config.Settings) {
		s.OpenAICodexClientID = "fixture-client"
		s.CredentialEncryptionKey = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
		s.Providers = map[string]*config.ProviderConfig{
			"ai-studio":   {Type: "ai_studio", APIKey: "fixture"},
			"vertex":      {Type: "vertex_ai", APIKey: "fixture", Project: "fixture"},
			"antigravity": {Type: "google_antigravity"},
			"codex":       {Type: "openai_compatible", RegistryID: "openai_codex"},
			"anthropic":   {Type: "anthropic", APIKey: "fixture"},
		}
		s.Endpoints = map[string]*config.EndpointConfig{}
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	owner, err := iam.CreatePrincipal("human", "fixture:transport-metadata", "", "Transport metadata")
	if err != nil {
		t.Fatal(err)
	}
	for _, providerID := range []string{"codex", "antigravity"} {
		if _, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
			PrincipalID: owner.ID, ProviderID: providerID, Kind: providerID + "_oauth", AccessToken: "fixture-access",
		}); err != nil {
			t.Fatal(err)
		}
	}
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)

	trusted := func(surface string) *core.ModelCapabilities {
		capabilities := providers.AdaptModelCapabilities(
			map[string]any{"chat": true}, []string{surface}, time.Time{}, time.Time{},
		)
		capabilities.Provenance = core.ModelCapabilityProvenance{
			Source: core.ModelCapabilitySourceUpstreamReported, Confidence: core.ModelCapabilityConfidenceHigh,
		}
		return capabilities
	}
	originalCatalog := catalogModelsForPrincipal
	originalRefreshedAt := catalogRefreshedAtForPrincipal
	catalogModelsForPrincipal = func(providerID string, _ *config.Principal) []providers.ModelInfo {
		surface := "/v1/chat/completions"
		switch providerID {
		case "codex":
			surface = "/v1/responses"
		case "anthropic":
			surface = "/v1/messages"
		}
		return []providers.ModelInfo{{
			ID: "fixture-model", Capabilities: map[string]any{"chat": true},
			SupportedSurfaces: []string{surface}, TypedCapabilities: trusted(surface),
		}}
	}
	catalogRefreshedAtForPrincipal = func(string, *config.Principal) time.Time { return time.Now() }
	t.Cleanup(func() {
		catalogModelsForPrincipal = originalCatalog
		catalogRefreshedAtForPrincipal = originalRefreshedAt
	})

	response, err := buildModelList(&config.Principal{PrincipalID: owner.ID, PrincipalKind: owner.Kind})
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]map[string]any{}
	for _, raw := range response["data"].([]any) {
		row := raw.(map[string]any)
		rows[row["id"].(string)] = row
	}
	assertSurfaces := func(id string, native, emulated []string) {
		t.Helper()
		row := rows[id]
		if row == nil {
			t.Fatalf("model %q missing from %+v", id, rows)
		}
		if got, _ := row["native_surfaces"].([]string); !reflect.DeepEqual(got, native) {
			t.Fatalf("%s native_surfaces=%v, want %v", id, got, native)
		}
		if got, _ := row["emulated_surfaces"].([]string); !reflect.DeepEqual(got, emulated) {
			t.Fatalf("%s emulated_surfaces=%v, want %v", id, got, emulated)
		}
	}
	assertSurfaces("ai-studio/fixture-model", nil, []string{
		"/v1/chat/completions", "/v1/responses", "/v1/messages",
	})
	assertSurfaces("vertex/fixture-model", nil, []string{
		"/v1/chat/completions", "/v1/responses", "/v1/messages",
	})
	assertSurfaces("antigravity/fixture-model", nil, []string{
		"/v1/chat/completions", "/v1/responses", "/v1/messages",
	})
	assertSurfaces("codex/fixture-model", []string{"/v1/responses"}, []string{
		"/v1/chat/completions", "/v1/messages",
	})
	assertSurfaces("anthropic/fixture-model", []string{"/v1/messages"}, []string{
		"/v1/chat/completions", "/v1/responses",
	})
}

func TestModelListRespectsKeyAndProjectPolicy(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	_, _ = iam.Initialize()
	config.Update(func(s *config.Settings) {
		s.AllowUnauthenticatedAPI = false
		s.APIKey = "admin-secret"
		s.Providers = map[string]*config.ProviderConfig{"echo": {Type: "echo"}}
		s.Endpoints = map[string]*config.EndpointConfig{
			"smart": {Failover: []config.EndpointMember{{Provider: "echo", Model: "echo-default"}}},
		}
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)
	principal, _ := iam.CreatePrincipal("service", "service:models", "", "Models")
	project, _ := iam.CreateProject("models-project", "Models Project")
	_ = iam.SetMembership(project.ID, principal.ID, "member")
	_, _ = iam.SetProjectPolicy(project.ID, iam.KeyPolicy{
		AllowedModels:    []string{"echo/echo-strong"},
		AllowedProviders: []string{"echo"},
	})
	issued, err := iam.IssueKey(iam.KeyCreate{
		ProjectID: project.ID, PrincipalID: principal.ID, Name: "restricted",
		Policy: iam.KeyPolicy{
			AllowedModels:    []string{"echo/echo-strong"},
			AllowedProviders: []string{"echo"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rows := providers.RefreshCatalogForPrincipal("echo", callerOf(&config.Principal{
		PrincipalID: principal.ID, PrincipalKind: principal.Kind, ProjectID: project.ID, Project: project.Slug,
	})); len(rows) == 0 {
		t.Fatal("service project refresh returned no models")
	}
	server := httptest.NewServer(NewServer())
	defer server.Close()
	status, payload := jsonRequest(
		t, server.URL+"/v1/models", http.MethodGet, issued.Token, nil,
	)
	if status != http.StatusOK {
		t.Fatalf("models status=%d payload=%+v", status, payload)
	}
	rows := payload["data"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["id"] != "echo/echo-strong" {
		t.Fatalf("policy-filtered models=%+v", rows)
	}
}

// A routing chain is surfaced into /v1/models as a pseudo-model row. Its
// owned_by is client-visible, so the rename shows up in the wire format.
func TestEndpointRowsAreOwnedByEndpoint(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	router.ResetSavingsState()
	router.ResetTelemetryState()
	t.Cleanup(func() {
		iam.ResetForTests()
		router.ResetSavingsState()
		router.ResetTelemetryState()
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	config.Update(func(s *config.Settings) {
		s.AllowUnauthenticatedAPI = true
		s.Providers = map[string]*config.ProviderConfig{"echo": {Type: "echo"}}
		s.Endpoints = map[string]*config.EndpointConfig{
			"smart": {Failover: []config.EndpointMember{{Provider: "echo", Model: "echo-default"}}},
		}
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)

	server := httptest.NewServer(NewServer())
	defer server.Close()
	status, payload := jsonRequest(t, server.URL+"/v1/models", http.MethodGet, "", nil)
	if status != http.StatusOK {
		t.Fatalf("models status=%d payload=%+v", status, payload)
	}
	rows := payload["data"].([]any)

	var endpointRow map[string]any
	for _, r := range rows {
		row := r.(map[string]any)
		if row["id"] == "smart" {
			endpointRow = row
		}
		if row["owned_by"] == "category" {
			t.Fatalf("row still carries the pre-rename owned_by=%q: %+v", "category", row)
		}
	}
	if endpointRow == nil {
		t.Fatalf("no row for endpoint chain %q: rows=%+v", "smart", rows)
	}
	if endpointRow["owned_by"] != "endpoint" {
		t.Fatalf("endpoint row owned_by=%v, want %q", endpointRow["owned_by"], "endpoint")
	}
}

// The admin model-card route hands out the same catalog rows, and the README
// documents the dual-key window for the field itself, not for one route. This
// is the guard against it regressing to marshalling providers.ModelInfo
// directly, which emits only the canonical key.
func TestAdminCatalogRowsCarryBothSurfaceKeys(t *testing.T) {
	rows := catalogRowsWithLegacySurfaces([]providers.ModelInfo{
		{ID: "gpt-5.5", Label: "GPT-5.5", Free: true, SupportedSurfaces: []string{"/responses"}},
		{ID: "bare-model"},
	})
	if len(rows) != 2 {
		t.Fatalf("rows=%+v", rows)
	}
	if rows[0]["id"] != "gpt-5.5" || rows[0]["label"] != "GPT-5.5" {
		t.Fatalf("row lost its canonical fields: %+v", rows[0])
	}
	if rows[0]["free"] != true {
		t.Fatalf("row lost its free marker: %+v", rows[0])
	}
	surfaces, _ := rows[0]["supported_surfaces"].([]any)
	if len(surfaces) != 1 || surfaces[0] != "/responses" {
		t.Fatalf("supported_surfaces missing: %+v", rows[0])
	}
	legacy, _ := rows[0]["supported_endpoints"].([]string)
	if len(legacy) != 1 || legacy[0] != "/responses" {
		t.Fatalf("deprecated supported_endpoints missing: %+v", rows[0])
	}
	// A row with no surfaces gains neither key, matching setSurfaces.
	if _, ok := rows[1]["supported_surfaces"]; ok {
		t.Fatalf("empty surface list emitted a key: %+v", rows[1])
	}
	if _, ok := rows[1]["supported_endpoints"]; ok {
		t.Fatalf("empty surface list emitted the deprecated key: %+v", rows[1])
	}
}
