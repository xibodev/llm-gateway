package router

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/providers"
)

// holdingCatalog is an OpenAI-compatible catalog upstream that answers at
// once until hold is set. From then on it holds each listing until release
// is called, or until its client leaves, which closes left.
type holdingCatalog struct {
	url      string
	hold     atomic.Bool
	held     atomic.Int32
	listed   chan struct{}
	left     chan struct{}
	released chan struct{}
	release  func()
}

func newHoldingCatalog(t *testing.T, answer, afterRelease string) *holdingCatalog {
	t.Helper()
	upstream := &holdingCatalog{listed: make(chan struct{}, 8), left: make(chan struct{}), released: make(chan struct{})}
	var leaving, releasing sync.Once
	upstream.release = func() { releasing.Do(func() { close(upstream.released) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/models") {
			http.NotFound(w, r)
			return
		}
		if !upstream.hold.Load() {
			_, _ = w.Write([]byte(answer))
			return
		}
		upstream.held.Add(1)
		select {
		case upstream.listed <- struct{}{}:
		default:
		}
		select {
		case <-upstream.released:
			_, _ = w.Write([]byte(afterRelease))
		case <-r.Context().Done():
			leaving.Do(func() { close(upstream.left) })
		}
	}))
	t.Cleanup(func() {
		upstream.release()
		server.Close()
	})
	upstream.url = server.URL
	return upstream
}

// useCatalogProvider configures one OpenAI-compatible provider, "slow", at
// url, over a state directory and a provider runtime of the test's own, and
// returns the directory. The provider sets no timeout, so nothing but a
// context ends a request it holds.
func useCatalogProvider(t *testing.T, url string) string {
	t.Helper()
	useStateDir(t)
	old := *config.Get()
	t.Cleanup(func() { config.Update(func(s *config.Settings) { *s = old }) })
	unbounded := 0.0
	config.Update(func(s *config.Settings) {
		s.Savings.Enabled = false
		s.Providers = map[string]*config.ProviderConfig{"slow": {
			Type: "openai_compatible", BaseURL: url, APIKey: "fixture-key", Timeout: &unbounded,
		}}
		s.Endpoints = map[string]*config.EndpointConfig{}
	})
	providers.InstallForTests(t)
	return config.StateDir()
}

// ageCatalog makes the stored catalog of provider older than any catalog
// TTL, as a process that kept it since then would find it.
func ageCatalog(t *testing.T, dir, provider string) {
	t.Helper()
	path := filepath.Join(dir, "catalog.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var entries map[string]map[string]any
	if err := json.Unmarshal(data, &entries); err != nil || entries[provider] == nil {
		t.Fatalf("catalog.json holds no catalog of %s: %v", provider, err)
	}
	entries[provider]["refreshed_at"] = time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339Nano)
	if data, err = json.Marshal(entries); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// within returns what done delivers, and fails the test when that takes
// longer than a request may wait.
func within[T any](t *testing.T, done <-chan T, failure string) T {
	t.Helper()
	select {
	case value := <-done:
		return value
	case <-time.After(10 * time.Second):
		t.Fatal(failure)
	}
	var zero T
	return zero
}

type resolved struct {
	resolution Resolution
	err        error
}

// resolveWithin resolves model as a request would, and fails the test when
// the resolution takes longer than a request may wait.
func resolveWithin(t *testing.T, model string) resolved {
	t.Helper()
	done := make(chan resolved, 1)
	go func() {
		resolution, err := ResolveForPrincipal(context.Background(), model, anonymous)
		done <- resolved{resolution, err}
	}()
	return within(t, done, "the resolution waited on the catalog upstream")
}

// Once a catalog is stored, a bare model name resolves against it as stored:
// a stale catalog is served at once and refreshed in the background, one
// refresh at a time, so an upstream that holds its catalog request delays no
// resolution.
func TestNativeAliasesServeAStaleCatalogWhileItRefreshes(t *testing.T) {
	upstream := newHoldingCatalog(t,
		`{"data":[{"id":"fixture-cached-model"}]}`, `{"data":[{"id":"fixture-refreshed-model"}]}`)
	dir := useCatalogProvider(t, upstream.url)
	if rows := providers.RefreshCatalog("slow"); len(rows) != 1 {
		t.Fatalf("seeded catalog=%+v", rows)
	}
	ageCatalog(t, dir, "slow")
	// A restarted process loads the aged catalog.
	providers.InstallForTests(t)
	upstream.hold.Store(true)

	want := Target{Provider: "slow", Model: "fixture-cached-model"}
	for round := range 2 {
		got := resolveWithin(t, want.Model)
		if got.err != nil || len(got.resolution.Targets) != 1 || got.resolution.Targets[0] != want {
			t.Fatalf("resolution %d=%+v err=%v, want %v from the stale catalog", round, got.resolution, got.err, want)
		}
		if round == 0 {
			within(t, upstream.listed, "the stale catalog was not refreshed in the background")
		}
	}
	if held := upstream.held.Load(); held != 1 {
		t.Fatalf("catalog requests=%d while one refresh was under way, want 1", held)
	}

	upstream.release()
	// The refresh holds the catalog's discovery until it has stored what it
	// listed, so this read waits for it, and then finds a fresh catalog.
	result := providers.ReadCatalogForPrincipal("slow", anonymous)
	if result.Err != nil || !result.Diagnostics.FromCache || len(result.Models) != 1 || result.Models[0].ID != "fixture-refreshed-model" {
		t.Fatalf("catalog after the refresh was released=%+v", result)
	}
}

// A bare name resolved before its catalog was ever stored waits for the
// catalog's discovery, but only while its request lasts: the request leaving
// ends the resolution and the catalog request upstream, and stores nothing.
func TestNativeAliasDiscoveryEndsWithItsRequest(t *testing.T) {
	upstream := newHoldingCatalog(t, "", `{"data":[{"id":"fixture-model"}]}`)
	upstream.hold.Store(true)
	useCatalogProvider(t, upstream.url)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan resolved, 1)
	go func() {
		resolution, err := ResolveForPrincipal(ctx, "fixture-model", anonymous)
		done <- resolved{resolution, err}
	}()
	within(t, upstream.listed, "the resolution did not discover the catalog")
	cancel()
	if got := within(t, done, "the resolution outlived its request"); got.err == nil {
		t.Fatalf("a resolution whose catalog discovery was cancelled resolved to %+v", got.resolution)
	}
	within(t, upstream.left, "the catalog request outlived the request that started it")
	if models, refreshed := providers.CatalogCached("slow"); len(models) != 0 || !refreshed.IsZero() {
		t.Fatalf("a cancelled discovery stored a catalog: %+v at %v", models, refreshed)
	}
}
