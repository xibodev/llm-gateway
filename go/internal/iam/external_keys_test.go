package iam

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestExternalFileSnapshotLastKnownGoodAndPolicy(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	ResetForTests()
	t.Cleanup(ResetForTests)
	if _, err := Initialize(); err != nil {
		t.Fatal(err)
	}
	project, err := CreateProject("tools", "Tools")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "keys.json")
	writeExternalKeyTestFile(t, path, `{"version":1,"keys":[{"name":"build","project":"tools","key":"file-secret","allowed_models":["model-a"],"allowed_routes":["coding"],"allowed_providers":["provider-a"]}]}`)
	source := &externalKeySource{name: "file", path: path, interval: time.Hour}
	store := &externalKeyStore{sources: []*externalKeySource{source}, client: http.DefaultClient, now: time.Now}
	if err := store.refresh(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	store.publish()
	activeExternalKeys.Store(store)
	t.Cleanup(func() { activeExternalKeys.Store(nil) })

	principal, found := resolveExternalAPIKey("file-secret")
	if !found || principal.ProjectID != project.ID || principal.Project != "tools" || principal.Key != "build" ||
		len(principal.AllowedModels) != 1 || principal.AllowedModels[0] != "model-a" ||
		len(principal.AllowedRoutes) != 1 || principal.AllowedRoutes[0] != "coding" ||
		len(principal.AllowedProviders) != 1 || principal.AllowedProviders[0] != "provider-a" {
		t.Fatalf("resolved principal = %+v, found=%v", principal, found)
	}

	writeExternalKeyTestFile(t, path, `{"version":1,"keys":[`)
	if err := store.refresh(context.Background(), source); err == nil {
		t.Fatal("invalid refresh succeeded")
	}
	store.publish()
	if _, found := resolveExternalAPIKey("file-secret"); !found {
		t.Fatal("failed refresh discarded the last-known-good snapshot")
	}
}

func TestExternalHTTPETagLastKnownGoodAndReplacement(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	ResetForTests()
	t.Cleanup(ResetForTests)
	if _, err := Initialize(); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateProject("external", "External"); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Cache-Control") != "no-cache" {
			t.Error("missing Cache-Control: no-cache")
		}
		if request.Header.Get("Authorization") != "Bearer source-token" {
			t.Error("missing source authorization")
		}
		switch calls.Add(1) {
		case 1:
			response.Header().Set("ETag", `"one"`)
			_, _ = response.Write([]byte(`{"version":1,"count":1,"keys":[{"name":"one","project":"external","key":"http-one"}]}`))
		case 2:
			if request.Header.Get("If-None-Match") != `"one"` {
				t.Errorf("If-None-Match = %q", request.Header.Get("If-None-Match"))
			}
			response.WriteHeader(http.StatusNotModified)
		case 3:
			response.Header().Set("ETag", `"bad"`)
			_, _ = response.Write([]byte(`{"version":1,"count":2,"keys":[{"name":"bad","project":"external","key":"http-bad"}]}`))
		case 4:
			if request.Header.Get("If-None-Match") != `"one"` {
				t.Errorf("rejected response advanced ETag to %q", request.Header.Get("If-None-Match"))
			}
			_, _ = response.Write([]byte(`{"version":1,"count":1,"keys":[{"name":"two","project":"external","key":"http-two"}]}`))
		}
	}))
	defer server.Close()

	source := &externalKeySource{name: "http", url: server.URL, token: "source-token", interval: time.Hour, timeout: time.Second}
	store := &externalKeyStore{sources: []*externalKeySource{source}, client: server.Client(), now: time.Now}
	if err := store.refresh(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	store.publish()
	activeExternalKeys.Store(store)
	t.Cleanup(func() { activeExternalKeys.Store(nil) })
	if _, found := resolveExternalAPIKey("http-one"); !found {
		t.Fatal("initial HTTP key not installed")
	}
	if err := store.refresh(context.Background(), source); err != nil {
		t.Fatalf("304 refresh: %v", err)
	}
	if err := store.refresh(context.Background(), source); err == nil {
		t.Fatal("count mismatch succeeded")
	}
	store.publish()
	if _, found := resolveExternalAPIKey("http-one"); !found {
		t.Fatal("rejected HTTP response discarded last-known-good")
	}
	if err := store.refresh(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	store.publish()
	if _, found := resolveExternalAPIKey("http-one"); found {
		t.Fatal("atomic replacement retained removed key")
	}
	if _, found := resolveExternalAPIKey("http-two"); !found {
		t.Fatal("replacement key not installed")
	}
}

func TestExternalKeyExpiryAndMaximumStalenessFailClosed(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	ResetForTests()
	t.Cleanup(ResetForTests)
	if _, err := Initialize(); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateProject("external", "External"); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2030, time.January, 1, 0, 0, 0, 0, time.UTC)
	now := base
	document := `{"version":1,"keys":[{"name":"short","project":"external","key":"short-secret","expires_at":"2030-01-01T00:01:00Z"}]}`
	keys, err := decodeExternalKeys([]byte(document), false)
	if err != nil {
		t.Fatal(err)
	}
	source := &externalKeySource{name: "file", interval: time.Minute, loadedAt: base, keys: keys}
	store := &externalKeyStore{sources: []*externalKeySource{source}, maxStaleness: 2 * time.Minute, now: func() time.Time { return now }}
	store.publish()
	activeExternalKeys.Store(store)
	t.Cleanup(func() { activeExternalKeys.Store(nil) })
	if _, found := resolveExternalAPIKey("short-secret"); !found {
		t.Fatal("fresh unexpired key denied")
	}
	now = base.Add(time.Minute)
	if _, found := resolveExternalAPIKey("short-secret"); found {
		t.Fatal("key accepted at its expiry boundary")
	}

	keys, err = decodeExternalKeys([]byte(`{"version":1,"keys":[{"name":"long","project":"external","key":"long-secret"}]}`), false)
	if err != nil {
		t.Fatal(err)
	}
	source.keys = keys
	now = base
	store.publish()
	now = base.Add(2 * time.Minute)
	if _, found := resolveExternalAPIKey("long-secret"); found {
		t.Fatal("stale source remained usable at the deadline")
	}
	if len(source.keys) != 1 {
		t.Fatal("stale source did not retain its last-known-good contribution")
	}
}

func writeExternalKeyTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
