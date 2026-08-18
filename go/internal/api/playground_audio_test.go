package api

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
	"llmgw/internal/router"
)

// uatScope builds the human principal + project + membership the playground
// requires, mirroring the Access page flow.
func uatScope(t *testing.T, serverURL string) (string, string) {
	t.Helper()
	status, principal := jsonRequest(t, serverURL+"/admin/api/principals", http.MethodPost, "admin-secret", map[string]any{
		"kind": "human", "display_name": "Audio Owner",
	})
	if status != http.StatusCreated {
		t.Fatalf("create principal: %d %+v", status, principal)
	}
	ownerID, _ := principal["id"].(string)
	status, project := jsonRequest(t, serverURL+"/admin/api/projects", http.MethodPost, "admin-secret", map[string]any{
		"slug": "audio", "name": "Audio",
	})
	if status != http.StatusCreated {
		t.Fatalf("create project: %d %+v", status, project)
	}
	projectID, _ := project["id"].(string)
	status, membership := jsonRequest(t, serverURL+"/admin/api/memberships", http.MethodPost, "admin-secret", map[string]any{
		"project_id": projectID, "principal_id": ownerID, "role": "owner",
	})
	if status != http.StatusOK {
		t.Fatalf("membership: %d %+v", status, membership)
	}
	return ownerID, projectID
}

func resetAudioTestState(t *testing.T) {
	t.Helper()
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
}

func TestPlaygroundSpeechRejectsNonSpeechProvider(t *testing.T) {
	resetAudioTestState(t)
	config.Update(func(s *config.Settings) {
		s.APIKey = "admin-secret"
		s.AllowUnauthenticatedAPI = false
		s.Providers = map[string]*config.ProviderConfig{"echo": {Type: "echo"}}
		s.Categories = map[string]*config.CategoryConfig{}
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)

	server := httptest.NewServer(NewServer())
	defer server.Close()
	ownerID, projectID := uatScope(t, server.URL)

	status, result := jsonRequest(t, server.URL+"/admin/api/playground/speech", http.MethodPost, "admin-secret", map[string]any{
		"principal_id": ownerID, "project_id": projectID,
		"model": "echo/echo-1", "input": "hello",
	})
	if status == http.StatusOK {
		t.Fatalf("speech against a chat provider should fail, got %+v", result)
	}
}

func TestPlaygroundSpeechProxiesOpenAICompatibleProvider(t *testing.T) {
	const providerKey = "test-provider-key"
	var upstreamPath, upstreamAuth string
	var upstreamBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(
		w http.ResponseWriter, r *http.Request,
	) {
		upstreamPath = r.URL.Path
		upstreamAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&upstreamBody)
		w.Header().Set("Content-Type", "audio/mpeg")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte{0xff, 0xf3, 0x01, 0x02})
	}))
	defer upstream.Close()

	resetAudioTestState(t)
	config.Update(func(s *config.Settings) {
		s.APIKey = "admin-secret"
		s.AllowUnauthenticatedAPI = false
		s.Providers = map[string]*config.ProviderConfig{
			"localai": {
				Type: "openai_compatible", BaseURL: upstream.URL + "/v1",
				APIKey: providerKey,
			},
		}
		s.Categories = map[string]*config.CategoryConfig{}
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)

	server := httptest.NewServer(NewServer())
	defer server.Close()
	ownerID, projectID := uatScope(t, server.URL)
	status, result := jsonRequest(
		t, server.URL+"/admin/api/playground/speech", http.MethodPost,
		"admin-secret", map[string]any{
			"principal_id": ownerID, "project_id": projectID,
			"model": "localai/en_US-amy-piper", "input": "hello",
			"speed": 1.25,
		},
	)
	if status != http.StatusOK {
		t.Fatalf("speech status=%d result=%+v", status, result)
	}
	encoded, ok := result["audio_base64"].(string)
	if !ok {
		t.Fatalf("missing audio payload: %+v", result)
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || string(decoded) != string([]byte{0xff, 0xf3, 0x01, 0x02}) {
		t.Fatalf("audio payload=%v err=%v", decoded, err)
	}
	if upstreamPath != "/v1/audio/speech" {
		t.Fatalf("upstream path=%q", upstreamPath)
	}
	if upstreamAuth != "Bearer "+providerKey {
		t.Fatalf("upstream authorization=%q", upstreamAuth)
	}
	if upstreamBody["model"] != "en_US-amy-piper" ||
		upstreamBody["voice"] != "en_US-amy-piper" ||
		upstreamBody["input"] != "hello" {
		body, _ := json.Marshal(upstreamBody)
		t.Fatalf("upstream body=%s", body)
	}
}

func TestPlaygroundSpeechRequiresInputAndScope(t *testing.T) {
	resetAudioTestState(t)
	config.Update(func(s *config.Settings) {
		s.APIKey = "admin-secret"
		s.AllowUnauthenticatedAPI = false
		s.Providers = map[string]*config.ProviderConfig{"edge_tts": {Type: "edge_tts"}}
		s.Categories = map[string]*config.CategoryConfig{}
	})
	providers.ResetProviders()
	t.Cleanup(providers.ResetProviders)

	server := httptest.NewServer(NewServer())
	defer server.Close()
	ownerID, projectID := uatScope(t, server.URL)

	status, _ := jsonRequest(t, server.URL+"/admin/api/playground/speech", http.MethodPost, "admin-secret", map[string]any{
		"principal_id": ownerID, "project_id": projectID, "model": "edge_tts/en-US-EmmaMultilingualNeural",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("empty input status = %d, want 400", status)
	}

	status, _ = jsonRequest(t, server.URL+"/admin/api/playground/speech", http.MethodPost, "admin-secret", map[string]any{
		"project_id": projectID, "model": "edge_tts/en-US-EmmaMultilingualNeural", "input": "hi",
	})
	if status != http.StatusBadRequest {
		t.Fatalf("missing principal status = %d, want 400", status)
	}
}

func TestTranscriptionTextPrefersJSONField(t *testing.T) {
	if got := transcriptionText([]byte(`{"text":"hello world"}`)); got != "hello world" {
		t.Fatalf("json transcript = %q", got)
	}
	if got := transcriptionText([]byte("  plain text  ")); got != "plain text" {
		t.Fatalf("plain transcript = %q", got)
	}
}

func TestSpeechResponseEncodesPlayableAudio(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte{0xff, 0xf3, 0x01, 0x02})
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(decoded) != 4 {
		t.Fatalf("round trip failed: %v", err)
	}
}
