package providers

import (
	"testing"

	"llmgw/internal/config"
)

// putProvider caches provider under key in the installed Runtime.
func putProvider(key string, provider Provider) {
	instances := &Current().instances
	instances.mu.Lock()
	defer instances.mu.Unlock()
	instances.instances[key] = provider
}

// putCatalogEntry caches entry under key in the installed Runtime's catalog.
func putCatalogEntry(key string, entry catalogEntry) {
	catalogs := &Current().catalogs
	catalogs.mu.Lock()
	defer catalogs.mu.Unlock()
	catalogs.loadLocked()
	catalogs.entries[key] = entry
}

// streak returns how many consecutive circuit failures rt holds for name.
func streak(rt *Runtime, name string) int {
	rt.circuits.mu.Lock()
	defer rt.circuits.mu.Unlock()
	if rt.circuits.tracker == nil {
		return 0
	}
	return rt.circuits.tracker.State(name).Streak
}

func TestInstallForTestsIsolatesAndRestoresTheRuntime(t *testing.T) {
	outer := Current()
	outer.ResetCircuit("")
	var inner *Runtime
	t.Run("inner", func(t *testing.T) {
		inner = InstallForTests(t)
		if Current() != inner || inner == outer {
			t.Fatal("InstallForTests did not install a new Runtime")
		}
		for range 3 {
			Current().circuits.record("fixture", config.ProviderPolicy{CircuitFailureThreshold: 5}, unavailable())
		}
	})
	if Current() != outer {
		t.Fatal("the previous Runtime was not restored")
	}
	if streak(inner, "fixture") != 3 {
		t.Fatal("the test's state did not stay in its own Runtime")
	}
	if streak(outer, "fixture") != 0 {
		t.Fatal("the test's state leaked into the previous Runtime")
	}
	outer.ResetCircuit("")
}

func TestCurrentInstallsARuntimeWhenNoneIsInstalled(t *testing.T) {
	previous := Install(nil)
	t.Cleanup(func() { Install(previous) })
	first := Current()
	if first == nil || Current() != first {
		t.Fatal("Current must install one Runtime and keep returning it")
	}
}
