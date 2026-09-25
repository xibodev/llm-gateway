package providers

import (
	"sync"
	"sync/atomic"

	"llmgw/internal/config"

	antigravityauth "github.com/xibodev/llm-provider-auth/antigravity"
	copilotauth "github.com/xibodev/llm-provider-auth/copilot"
	gcpauth "github.com/xibodev/llm-provider-auth/gcp"
	core "github.com/xibodev/llmgw-core"
	"github.com/xibodev/llmgw-core/catalog"
	coreruntime "github.com/xibodev/llmgw-core/runtime"
)

// Runtime owns the provider stack's mutable state. A process builds one after
// it loads settings and initializes IAM; each test can build its own.
//
// The first group is the state llmgw-core's runtime.Runtime also keeps, so
// that value can take it over from here; the rest has no core counterpart.
// For the types in verticals it has taken over: core holds their providers
// and the coordinators that refresh their credentials, so no refresh lock is
// kept here.
type Runtime struct {
	instances providerCache
	// catalogs is catalog.json, the store core's catalog service keeps the
	// gateway's catalogs in, and catalogService reads, discovers and
	// invalidates them there; the core Runtime holds the same service.
	catalogs       catalogFile
	catalogService *catalog.Service
	core           *coreruntime.Runtime[*config.Settings]
	// verticals is the registration table of the provider types core
	// serves; see coreVerticals.
	verticals map[string]coreVertical
	// openCredentials opens the IAM credential store a vertical's store
	// wraps, once per operation; see iamCredentialStore.
	openCredentials func(oauth bool) (core.CredentialStore, error)
	// credentials is the OAuth store behind core's Codex and Antigravity
	// and behind the gateway's own Coordinators: the Codex and Antigravity
	// catalogs, Antigravity image generation and the console refreshes,
	// which refresh outside core.
	credentials oauthStore
	// circuits holds every provider's circuit breaker in an llmgw-core
	// HealthTracker, as core's Runtime holds the circuits of the instances it
	// guards.
	circuits circuits

	// gcpTokens caches service-account access tokens for the gateway's Vertex
	// transport, which serves video and the catalog; core's Google caches its
	// own. Providers are rebuilt on each settings change and per principal,
	// so the cache lives here, where tokens outlive those rebuilds.
	gcpTokens gcpauth.TokenCache
	// copilot reads the live settings at the start of every operation, so hot
	// reload keeps working, and it is shared so concurrent polls of one device
	// code serialize.
	copilot       *copilotauth.Client
	authAdapters  authAdapterRegistry
	quotaAdapters quotaAdapterRegistry

	// Only tests set the seams; production keeps the canonical endpoints and
	// the OAuth client configured in settings.
	codexEndpoints      seam[CodexEndpoints]
	copilotEndpoints    seam[copilotauth.Endpoints]
	antigravityOAuth    seam[func(redirectURI string) antigravityauth.Config]
	antigravityEndpoint seam[antigravityEndpoint]
}

// NewRuntime returns a Runtime with empty caches and the built-in auth
// adapters. The catalog loads catalog.json from the state directory on first
// use, and the credentials of the types core serves are the IAM provider
// connections and configured keys.
func NewRuntime() *Runtime { return newRuntime(iamCredentialStore) }

// newRuntime returns a Runtime that opens the stores of its credentials with
// open, once per operation: the OAuth store of Codex and Antigravity and
// Copilot's store with oauth set, and any other vertical's without (see
// iamCredentialStore). Tests pass in-memory stores.
func newRuntime(open func(oauth bool) (core.CredentialStore, error)) *Runtime {
	runtime := &Runtime{}
	runtime.instances.instances = map[string]Provider{}
	runtime.authAdapters.factories = builtInAuthAdapters()
	runtime.quotaAdapters.values = map[string]QuotaAdapter{}
	runtime.copilot = copilotauth.NewDynamic(runtime.copilotSettings)
	runtime.openCredentials = open
	runtime.credentials = oauthStore{runtime: runtime, open: func() (core.CredentialStore, error) { return open(true) }}
	runtime.catalogService = catalog.New(catalog.Options{
		Store: &runtime.catalogs, TTL: catalogTTL, SchemaVersion: catalogSchemaVersion, KeepStale: true,
	})
	runtime.verticals = runtime.coreVerticals()
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
