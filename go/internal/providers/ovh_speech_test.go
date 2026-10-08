package providers

import (
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"

	core "github.com/xibodev/llmgw-core"
)

// An OVHcloud AI Endpoints instance's NVR voices speak through core, which
// sends them to each voice's own host, while its other models keep the
// gateway's audio proxy.
func TestOVHVoicesSpeakThroughCore(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	runtime := InstallForTests(t)
	old := config.Get().Providers
	t.Cleanup(func() { config.Update(func(s *config.Settings) { s.Providers = old }) })
	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{
			"ovh":   {Type: "openai_compatible", RegistryID: "ovh_ai_endpoints", BaseURL: "https://oai.endpoints.kepler.ai.cloud.ovh.net/v1"},
			"local": {Type: "openai_compatible", BaseURL: "http://127.0.0.1:9/v1"},
		}
	})
	caller := gatewayCaller()
	for _, testCase := range []struct {
		provider, model string
		surface         core.ModelSurface
		core            bool
	}{
		{"ovh", "nvr-tts-en-us", core.ModelSurfaceAudioSpeech, true},
		{"ovh", "nvr-tts-de-de", core.ModelSurfaceAudioSpeech, true},
		{"ovh", "whisper-large-v3", core.ModelSurfaceAudioTranscriptions, false},
		{"ovh", "gpt-oss-120b", core.ModelSurfaceAudioSpeech, false},
		{"local", "nvr-tts-en-us", core.ModelSurfaceAudioSpeech, false},
	} {
		if served := runtime.CoreServesSurfaceForPrincipal(testCase.provider, testCase.model, testCase.surface, caller); served != testCase.core {
			t.Errorf("%s/%s %s through core: %v, want %v", testCase.provider, testCase.model, testCase.surface, served, testCase.core)
		}
	}
}
