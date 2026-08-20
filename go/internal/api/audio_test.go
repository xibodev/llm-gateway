package api

import (
	"testing"

	"llmgw/internal/config"
)

func TestResolveAudioTargetSanitizesAmbiguousCategoryErrors(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	oldProviders, oldEndpoints := config.Get().Providers, config.Get().Endpoints
	t.Cleanup(func() {
		config.Update(func(s *config.Settings) {
			s.Providers, s.Endpoints = oldProviders, oldEndpoints
		})
	})
	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{
			"one": {Type: "echo"},
			"two": {Type: "echo"},
		}
		s.Endpoints = map[string]*config.EndpointConfig{
			"smart": {
				Failover: []config.EndpointMember{{Provider: "one", Model: "echo-default"}},
			},
			"SMART": {
				Failover: []config.EndpointMember{{Provider: "two", Model: "echo-default"}},
			},
		}
	})

	_, _, status, message := resolveAudioTarget(nil, "SmArT")
	if status != 500 || message != "Gateway is not configured for the requested model." {
		t.Fatalf("ambiguous route status=%d message=%q", status, message)
	}
}
