package config_test

import (
	"testing"

	"llmgw/internal/config"

	coreruntime "github.com/xibodev/llmgw-core/runtime"
)

// config stays free of llmgw-core; this external test pins that Source
// still satisfies the core Runtime's settings source.
var _ coreruntime.SettingsSource[*config.Settings] = config.Source{}

func TestSourceSnapshotsTheCurrentSettings(t *testing.T) {
	var source coreruntime.SettingsSource[*config.Settings] = config.Source{}
	settings, generation := source.Snapshot()
	if settings != config.Get() || generation != config.Generation() {
		t.Fatalf("source returned %p at %d, want %p at %d", settings, generation, config.Get(), config.Generation())
	}
}
