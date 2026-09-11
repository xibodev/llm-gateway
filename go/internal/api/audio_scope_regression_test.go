package api

import (
	"net/http/httptest"
	"testing"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"
)

type scopeSpeechSynthesizer struct{ voice string }

func (s *scopeSpeechSynthesizer) DefaultVoice() string { return "voice-default" }
func (s *scopeSpeechSynthesizer) Synthesize(voice, _, _ string) ([]byte, string, error) {
	s.voice = voice
	return []byte("synthetic-audio"), "audio/mpeg", nil
}

func TestNativeSpeechPinsScopedVoice(t *testing.T) {
	for _, tc := range []struct {
		name    string
		key     config.Principal
		project iam.KeyPolicy
		want    string
	}{
		{name: "route only", key: config.Principal{RoutesOnly: true, AllowedRoutes: []string{"speech"}}, want: "voice-a"},
		{name: "route allowlist", key: config.Principal{AllowedRoutes: []string{"speech"}}, want: "voice-a"},
		{name: "model allowlist", key: config.Principal{AllowedModels: []string{"speech"}}, want: "voice-a"},
		{name: "provider allowlist", key: config.Principal{AllowedProviders: []string{"edge"}}, want: "voice-a"},
		{name: "project model scope", project: iam.KeyPolicy{AllowedModels: []string{"speech"}}, want: "voice-a"},
		{name: "project provider scope", project: iam.KeyPolicy{AllowedProviders: []string{"edge"}}, want: "voice-a"},
		{name: "unscoped compatibility", want: "voice-b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetState(t)
			old := *config.Get()
			t.Cleanup(func() { config.Update(func(s *config.Settings) { *s = old }) })
			config.Update(func(s *config.Settings) {
				s.Providers = map[string]*config.ProviderConfig{"edge": {Type: "edge_tts"}}
				s.Endpoints = map[string]*config.EndpointConfig{
					"speech": {Failover: []config.EndpointMember{{Provider: "edge", Model: "voice-a"}}},
				}
			})
			project, err := iam.CreateProject("speech-test", "Speech test")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := iam.SetProjectPolicy(project.ID, tc.project); err != nil {
				t.Fatal(err)
			}
			p := tc.key
			p.Token, p.ProjectID = "synthetic-key", project.ID
			provider, model, status, message := resolveAudioTarget(&p, "speech")
			if status != 0 || model != "voice-a" {
				t.Fatalf("resolve: %s %d %s", model, status, message)
			}
			for _, voice := range []string{"voice-b", "default", "", "voice-a"} {
				synth := &scopeSpeechSynthesizer{}
				rec := httptest.NewRecorder()
				serveNativeSpeech(rec, map[string]any{"input": "hello", "voice": voice}, synth, provider, model, &p, time.Now(), "speech")
				want := tc.want
				if tc.name == "unscoped compatibility" {
					switch voice {
					case "", "voice-a":
						want = "voice-a"
					case "default":
						want = "voice-default"
					}
				}
				if rec.Code != 200 || synth.voice != want {
					t.Fatalf("voice=%q: status=%d synthesized=%q want=%q body=%s", voice, rec.Code, synth.voice, want, rec.Body.String())
				}
			}
		})
	}
}

func TestNativeSpeechAdminVoiceCompatibility(t *testing.T) {
	resetState(t)
	synth := &scopeSpeechSynthesizer{}
	rec := httptest.NewRecorder()
	serveNativeSpeech(rec, map[string]any{"input": "hello", "voice": "voice-b"}, synth,
		"edge", "voice-a", &config.Principal{}, time.Now(), "edge/voice-a")
	if rec.Code != 200 || synth.voice != "voice-b" {
		t.Fatalf("status=%d voice=%q", rec.Code, synth.voice)
	}
}
