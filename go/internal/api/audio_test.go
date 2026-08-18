package api

import (
	"testing"

	"llmgw/internal/config"
)

func TestResolveAudioTargetSanitizesAmbiguousCategoryErrors(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	oldProviders, oldCategories := config.Get().Providers, config.Get().Categories
	t.Cleanup(func() {
		config.Update(func(s *config.Settings) {
			s.Providers, s.Categories = oldProviders, oldCategories
		})
	})
	config.Update(func(s *config.Settings) {
		s.Providers = map[string]*config.ProviderConfig{
			"one": {Type: "echo"},
			"two": {Type: "echo"},
		}
		s.Categories = map[string]*config.CategoryConfig{
			"smart": {
				Failover: []config.CategoryMember{{Provider: "one", Model: "echo-default"}},
			},
			"SMART": {
				Failover: []config.CategoryMember{{Provider: "two", Model: "echo-default"}},
			},
		}
	})

	_, _, status, message := resolveAudioTarget(nil, "SmArT")
	if status != 500 || message != "Gateway is not configured for the requested model." {
		t.Fatalf("ambiguous route status=%d message=%q", status, message)
	}
}
