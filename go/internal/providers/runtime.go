package providers

import (
	"sync"
	"sync/atomic"

	"llmgw/internal/config"

	antigravityauth "github.com/xibodev/llm-provider-auth/antigravity"
	copilotauth "github.com/xibodev/llm-provider-auth/copilot"
	gcpauth "github.com/xibodev/llm-provider-auth/gcp"
	core "github.com/xibodev/llmgw-core"
	coreruntime "github.com/xibodev/llmgw-core/runtime"
)

// Runtime owns the provider stack's mutable state. A process builds one after
// it loads settings and initializes IAM; each test can build its own.
//
// The first group is the state llmgw-core's runtime.Runtime also keeps, so
// that value can take it over from here; the rest has no core counterpart.
// For Codex it has taken over: core holds the Codex provider and the
// coordinators that refresh Codex credentials, so no Codex refresh lock is
// kept here.
type Runtime struct {
	instances providerCache
	catalogs  catalogCache
	core      *coreruntime.Runtime[*config.Settings]
	// codexCredentials is the credential store behind core and behind the
	// Codex catalog and console refresh, which refresh outside core.
	codexCredentials codexStore
	// antigravityRefresh serializes the OAuth refreshes of one Antigravity
	// connection. Codex refreshes take the credential store's lease instead.
	antigravityRefresh refreshLocks

	circuits circuitBreakers
	// gcpTokens caches service-account access tokens for every Vertex
	// provider. Providers are rebuilt on each settings change and per
	// principal, so the cache lives here, where tokens outlive those rebuilds.
	gcpTokens gcpauth.TokenCache
	// copilot reads the live settings at the start of every operation, so hot
	// reload keeps working, and it is shared so concurrent polls of one device
	// code serialize.
	copilot       *copilotauth.Client
	authAdapters  authAdapterRegistry
	quotaAdapters quotaAdapterRegistry
	edgeTTSClock  edgeTTSClock

	// Only tests set the seams; production keeps the canonical endpoints and
	// the OAuth client configured in settings.
	codexEndpoints   seam[CodexEndpoints]
	copilotEndpoints seam[copilotauth.Endpoints]
	antigravityOAuth seam[func(redirectURI string) antigravityauth.Config]
}

// NewRuntime returns a Runtime with empty caches and the built-in auth
// adapters. The catalog loads catalog.json from the state directory on first
// use, and Codex credentials are the IAM provider connections.
func NewRuntime() *Runtime { return newRuntime(iamCredentialStore) }

// newRuntime returns a Runtime that opens the store of its Codex credentials
// with credentials, once per operation. Tests pass an in-memory store.
func newRuntime(credentials func() (core.CredentialStore, error)) *Runtime {
	runtime := &Runtime{}
	runtime.instances.instances = map[string]Provider{}
	runtime.antigravityRefresh.entries = map[string]*refreshLock{}
	runtime.circuits.circuits = map[string]*circuitState{}
	runtime.authAdapters.factories = builtInAuthAdapters()
	runtime.quotaAdapters.values = map[string]QuotaAdapter{}
	runtime.copilot = copilotauth.NewDynamic(runtime.copilotSettings)
	runtime.codexCredentials = codexStore{runtime: runtime, open: credentials}
	coreRuntime, err := newCoreRuntime(runtime)
	if err != nil {
		// New fails only without settings or a provider factory, and
		// newCoreRuntime always passes both.
		panic("providers: build the core runtime: " + err.Error())
	}
	runtime.core = coreRuntime
	return runtime
}

// installed is the Runtime that code without one of its own reaches through
// Current. It is the package's only mutable package-level state: callers that
// own a Runtime pass it explicitly, and the deep paths that do not yet take
// one read it here.
var installed atomic.Pointer[Runtime]

// Install makes runtime the one Current returns and returns the Runtime it
// replaces, nil when none was installed.
func Install(runtime *Runtime) (previous *Runtime) {
	return installed.Swap(runtime)
}

// Current returns the installed Runtime. When none is installed it installs a
// new one, so code that runs without an owner, such as another package's
// tests, shares one Runtime per process as it shared package state before.
func Current() *Runtime {
	for {
		if runtime := installed.Load(); runtime != nil {
			return runtime
		}
		installed.CompareAndSwap(nil, NewRuntime())
	}
}

// InstallForTests installs a new Runtime until the test ends, then restores
// the previous one. It takes only the Cleanup method of a testing.TB, so the
// gateway binary does not link the testing package.
func InstallForTests(t interface{ Cleanup(func()) }) *Runtime {
	runtime := NewRuntime()
	previous := Install(runtime)
	t.Cleanup(func() { Install(previous) })
	return runtime
}

// refreshLocks serializes the refreshes of one credential. An entry exists
// only while a refresh holds or waits for it.
type refreshLocks struct {
	mu      sync.Mutex
	entries map[string]*refreshLock
}

type refreshLock struct {
	mu   sync.Mutex
	refs int
}

// lock blocks until no other refresh holds key and returns the release.
func (l *refreshLocks) lock(key string) (unlock func()) {
	l.mu.Lock()
	entry := l.entries[key]
	if entry == nil {
		entry = &refreshLock{}
		l.entries[key] = entry
	}
	entry.refs++
	l.mu.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		l.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(l.entries, key)
		}
		l.mu.Unlock()
	}
}

// seam holds a replacement a test installs for a production default. The
// zero value holds none. The lock keeps a test's swap from racing requests a
// background goroutine still serves.
type seam[T any] struct {
	mu    sync.RWMutex
	value T
}

func (s *seam[T]) get() T {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.value
}

// swap installs value and returns the previous one for the test to restore.
func (s *seam[T]) swap(value T) (previous T) {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, s.value = s.value, value
	return previous
}
