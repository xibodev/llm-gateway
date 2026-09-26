package router

import (
	"database/sql"
	"sync/atomic"

	"llmgw/internal/providers"
)

// Runtime holds the router's mutable state, the failover telemetry store and
// the savings ledger, and names the provider runtime its entry points act on.
// A process builds one next to its providers.Runtime.
type Runtime struct {
	providerRuntime *providers.Runtime
	telemetry       telemetryStore
	savings         savingsStore
}

// NewRuntime returns a router Runtime whose entry points act on
// providerRuntime. Nil follows the installed providers Runtime, so a router
// Runtime built before a test installs its own acts on the test's.
func NewRuntime(providerRuntime *providers.Runtime) *Runtime {
	runtime := &Runtime{providerRuntime: providerRuntime}
	runtime.savings.dbs = map[string]*sql.DB{}
	runtime.savings.initialized = map[string]bool{}
	return runtime
}

// providers returns the provider runtime the entry points act on.
func (rt *Runtime) providers() *providers.Runtime {
	if rt.providerRuntime != nil {
		return rt.providerRuntime
	}
	return providers.Current()
}

// installed is the router Runtime that code without one of its own reaches
// through Current. It is the package's only mutable package-level state:
// callers that own a Runtime pass it explicitly, and the paths that do not yet
// take one read it here.
var installed atomic.Pointer[Runtime]

// Install makes runtime the one Current returns and returns the Runtime it
// replaces, nil when none was installed.
func Install(runtime *Runtime) (previous *Runtime) {
	return installed.Swap(runtime)
}

// Current returns the installed router Runtime. When none is installed it
// installs one that follows the installed providers Runtime, so callers
// without an owner share one per process as they shared package state before.
func Current() *Runtime {
	for {
		if runtime := installed.Load(); runtime != nil {
			return runtime
		}
		installed.CompareAndSwap(nil, NewRuntime(nil))
	}
}

// InstallForTests installs a new router Runtime, following the installed
// providers Runtime, until the test ends. It then closes the new Runtime's
// databases, so the test's state directory can be removed, and restores the
// previous Runtime. It takes only the Cleanup method of a testing.TB, so the
// gateway binary does not link the testing package.
func InstallForTests(t interface{ Cleanup(func()) }) *Runtime {
	runtime := NewRuntime(nil)
	previous := Install(runtime)
	t.Cleanup(func() {
		Install(previous)
		runtime.ResetTelemetryState()
		runtime.ResetSavingsState()
	})
	return runtime
}
