package providers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"llmgw/internal/config"
)

// waitForCatalogRefreshes returns once rt runs no background catalog
// refresh, the file it writes last included.
func waitForCatalogRefreshes(t *testing.T, rt *Runtime) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		rt.catalogRefreshes.mu.Lock()
		running := len(rt.catalogRefreshes.running)
		rt.catalogRefreshes.mu.Unlock()
		if running == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("a background catalog refresh did not end")
		}
	}
}

// The Responses native check reads the catalog as a request does: a stale
// row answers it at once while one refresh runs in the background, so a
// catalog upstream that does not answer holds up no request, and the
// refreshed row answers the requests after it.
func TestNativeResponsesCheckServesAStaleCatalogWhileItRefreshes(t *testing.T) {
	var held atomic.Int32
	listed, released := make(chan struct{}, 4), make(chan struct{})
	var releasing sync.Once
	release := func() { releasing.Do(func() { close(released) }) }
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/models") {
			answerOpenAI(w, openAICall{path: r.URL.Path})
			return
		}
		held.Add(1)
		select {
		case listed <- struct{}{}:
		default:
		}
		select {
		case <-released:
			_, _ = w.Write([]byte(`{"data":[{"id":"fixture-model","supported_endpoints":["/chat/completions"]}]}`))
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		release()
		upstream.Close()
	})
	// No timeout: nothing but a context ends the catalog request.
	unbounded := 0.0
	rt := installAnonymousFixture(t, map[string]*config.ProviderConfig{
		"fixture": {Type: "openai_compatible", BaseURL: upstream.URL, APIKey: "fixture-key", Timeout: &unbounded},
	})
	rt.catalogs.put("fixture", catalogEntry{
		SchemaVersion: catalogSchemaVersion, RefreshedAt: time.Now().Add(-2 * catalogTTL),
		Models: []ModelInfo{{ID: "fixture-model", SupportedSurfaces: []string{"/responses"}}},
	})
	provider, err := rt.GetProvider("fixture")
	if err != nil {
		t.Fatal(err)
	}
	respond := func() error {
		t.Helper()
		done := make(chan error, 1)
		go func() {
			_, _, err := CompleteResponsesContext(context.Background(), provider, "fixture-model", map[string]any{"input": "hi"})
			done <- err
		}()
		select {
		case err := <-done:
			return err
		case <-time.After(10 * time.Second):
			t.Fatal("the Responses request waited on the catalog upstream")
		}
		return nil
	}
	for round := range 2 {
		if err := respond(); err != nil {
			t.Fatalf("request %d over the stale native row: %v", round, err)
		}
		if round == 0 {
			select {
			case <-listed:
			case <-time.After(10 * time.Second):
				t.Fatal("the stale catalog was not refreshed in the background")
			}
		}
	}
	if count := held.Load(); count != 1 {
		t.Fatalf("catalog requests=%d while one refresh was under way, want 1", count)
	}
	release()
	waitForCatalogRefreshes(t, rt)
	if err := respond(); !errors.Is(err, ErrResponsesUnsupported) {
		t.Fatalf("request after the refresh: err=%v, want the refreshed Chat-only row to refuse native Responses", err)
	}
}

// blockingLister lists its catalog only once release closes, whatever the
// context of its caller.
type blockingLister struct {
	Provider
	release chan struct{}
}

func (p blockingLister) ListModelsWithError() ([]ModelInfo, *CredentialObservation, error) {
	<-p.release
	return []ModelInfo{{ID: "fixture-model"}}, nil, nil
}

// A provider that lists without a context still lets its caller go when the
// caller's context ends, and its late result is dropped; without an end to
// the context the caller waits for the listing, as before.
func TestCatalogListingLeavesWithItsContext(t *testing.T) {
	lister := blockingLister{release: make(chan struct{})}
	provider := &ResilientProvider{inner: lister, name: "fixture"}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := listModelsContext(ctx, provider)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err=%v, want the context's", err)
		}
	case <-time.After(10 * time.Second):
		close(lister.release)
		t.Fatal("the listing held its caller past the context")
	}
	close(lister.release)
	if models, _, err := listModelsContext(context.Background(), provider); err != nil || len(models) != 1 {
		t.Fatalf("models=%+v err=%v, want the listing", models, err)
	}
}
