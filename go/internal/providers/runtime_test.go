package providers

import (
	"sync"
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
	Current().catalogs.put(key, entry)
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

// ResetProviders runs on every admin save while requests build providers. It
// must hold the cache's lock and advance its epoch: GetProviderForPrincipal
// stores a build only while the epoch it read before building still holds,
// so a provider built from settings a reset replaced is never cached.
func TestResetProvidersGuardsBuildsInFlight(t *testing.T) {
	rt := installAnonymousFixture(t, map[string]*config.ProviderConfig{"fixture": {Type: "echo"}})
	epoch := func() uint64 {
		rt.instances.mu.Lock()
		defer rt.instances.mu.Unlock()
		return rt.instances.epoch
	}
	inFlight := epoch()
	ResetProviders()
	if epoch() == inFlight {
		t.Fatal("reset kept the epoch a build in flight read, so that build would be cached")
	}

	stop := make(chan struct{})
	var builders sync.WaitGroup
	for range 4 {
		builders.Add(1)
		go func() {
			defer builders.Done()
			for {
				select {
				case <-stop:
					return
				default:
					// Constant resets may exhaust the attempts a build
					// gets; only what stays cached matters.
					_, _ = GetProvider("fixture")
				}
			}
		}()
	}
	const final = 64
	for attempts := 2; attempts <= final; attempts++ {
		config.Update(func(s *config.Settings) {
			s.Policies.Defaults = config.ProviderPolicy{RetryMaxAttempts: attempts}
		})
		ResetProviders()
	}
	close(stop)
	builders.Wait()
	provider, err := GetProvider("fixture")
	if err != nil {
		t.Fatal(err)
	}
	if resilient, ok := provider.(*ResilientProvider); !ok {
		t.Fatalf("cached %T, want a provider under the last retry policy", provider)
	} else if got := resilient.policy.RetryMaxAttempts; got != final {
		t.Fatalf("cached provider allows %d attempts, from settings a later reset replaced; want %d", got, final)
	}
}
