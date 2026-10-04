package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// Local providers found at startup are published only once the
// configuration file holds them, and finding nothing new writes nothing.
func TestLocalProvidersArePublishedOnlyOnceSaved(t *testing.T) {
	keepSettings(t)
	dir := t.TempDir()
	t.Setenv("LLMGW_STATE_DIR", dir)
	t.Setenv("LLMGW_CONFIG", filepath.Join(dir, "missing", "config.yaml"))
	Update(func(s *Settings) { s.Providers = map[string]*ProviderConfig{} })
	candidates := []LocalCandidate{{ID: "ollama", Type: "ollama", BaseURL: "http://127.0.0.1:11434"}}

	generation := Generation()
	if added, err := addLocalProviders(candidates, true); err == nil || added != nil {
		t.Fatalf("unwritable configuration: added=%v err=%v", added, err)
	}
	if Generation() != generation || Get().Providers["ollama"] != nil {
		t.Fatal("a provider the file did not take was published")
	}

	t.Setenv("LLMGW_CONFIG", filepath.Join(dir, "config.yaml"))
	if added, err := addLocalProviders(candidates, true); err != nil || !slices.Equal(added, []string{"ollama"}) {
		t.Fatalf("added=%v err=%v", added, err)
	}
	if mustLoad(t).Providers["ollama"] == nil {
		t.Fatal("the added provider was not saved")
	}
	if err := os.Remove(ConfigFilePath()); err != nil {
		t.Fatal(err)
	}
	if added, err := addLocalProviders(candidates, true); err != nil || added != nil {
		t.Fatalf("nothing new: added=%v err=%v", added, err)
	}
	if _, err := os.Stat(ConfigFilePath()); !os.IsNotExist(err) {
		t.Fatalf("finding nothing new wrote the configuration: %v", err)
	}
}

func TestLocalHostsReflectGatewayProcessBoundary(t *testing.T) {
	original := processRunsInContainer
	t.Cleanup(func() { processRunsInContainer = original })

	processRunsInContainer = func() bool { return false }
	if hosts := localHosts(); !slices.Equal(hosts, []string{"127.0.0.1", "localhost"}) {
		t.Fatalf("native process hosts=%v", hosts)
	}

	processRunsInContainer = func() bool { return true }
	if hosts := localHosts(); !slices.Equal(hosts, []string{"127.0.0.1", "localhost", "host.docker.internal"}) {
		t.Fatalf("container process hosts=%v", hosts)
	}
}
