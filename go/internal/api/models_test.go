package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
)

func TestIsChatModel(t *testing.T) {
	cases := []struct {
		name string
		row  providers.ModelInfo
		want bool
	}{
		{"claude via caps", providers.ModelInfo{ID: "claude-opus-4.8", Capabilities: map[string]any{"context_window": 1000000, "tool_calls": true}}, true},
		{"gpt via caps", providers.ModelInfo{ID: "gpt-4o-mini", Capabilities: map[string]any{"context_window": 128000}}, true},
		{"chat via endpoint only", providers.ModelInfo{ID: "some-chat", SupportedEndpoints: []string{"/v1/messages"}}, true},
		{"responses endpoint", providers.ModelInfo{ID: "gpt-5.5", SupportedEndpoints: []string{"/responses", "ws:/responses"}}, true},
		{"embedding caps family only", providers.ModelInfo{ID: "text-embedding-3-small", Capabilities: map[string]any{"family": "embed"}}, false},
		{"utility trajectory-compaction has chat caps but excluded", providers.ModelInfo{ID: "trajectory-compaction", Capabilities: map[string]any{"context_window": 262144, "tool_calls": true, "family": "trajectory-compaction"}, SupportedEndpoints: []string{"/chat/completions"}}, false},
		{"mai-code coding model kept", providers.ModelInfo{ID: "mai-code-1-flash-picker", Capabilities: map[string]any{"context_window": 256000, "tool_calls": true, "family": "oswe-vscode-modelD"}, SupportedEndpoints: []string{"/responses"}}, true},
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

func TestModelListRespectsKeyAndProjectPolicy(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	_, _ = iam.Initialize()
	config.Update(func(s *config.Settings) {
		s.AllowUnauthenticatedAPI = false
		s.APIKey = "admin-secret"
		s.Providers = map[string]*config.ProviderConfig{"echo": {Type: "echo"}}
		s.Categories = map[string]*config.CategoryConfig{
			"smart": {Failover: []config.CategoryMember{{Provider: "echo", Model: "echo-default"}}},
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
