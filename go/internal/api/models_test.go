package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
	"llmgw/internal/router"
)

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
// supported_surfaces key and the deprecated supported_endpoints key, with
// identical values, for the one-release migration window.
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
		{ID: "gpt-5.5", Label: "GPT-5.5", SupportedSurfaces: []string{"/responses"}},
		{ID: "bare-model"},
	})
	if len(rows) != 2 {
		t.Fatalf("rows=%+v", rows)
	}
	if rows[0]["id"] != "gpt-5.5" || rows[0]["label"] != "GPT-5.5" {
		t.Fatalf("row lost its canonical fields: %+v", rows[0])
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
