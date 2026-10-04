package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "github.com/xibodev/llmgw-core"
	"github.com/xibodev/llmgw-core/catalogtest"
)

// store saves models under the catalog.json key as a discovery stores them,
// stamped with the schema core's catalog service stamps.
func (c *catalogFile) store(key string, models []ModelInfo) {
	status := core.CatalogDiscovered
	if len(models) == 0 {
		status = core.CatalogEmpty
	}
	_, _ = c.Save(context.Background(), catalogKeyOf(key), core.CatalogEvidence{
		Status: status, Models: coreRows(models), ObservedAt: time.Now(), SchemaVersion: catalogSchemaVersion,
	})
}

// entry is the catalog.json entry of key, as the file holds it.
func (c *catalogFile) entry(key string) (catalogEntry, bool) {
	record, err := c.Load(context.Background(), catalogKeyOf(key))
	if err != nil {
		return catalogEntry{}, false
	}
	evidence := record.Evidence
	return catalogEntry{
		SchemaVersion: evidence.SchemaVersion, Models: gatewayRows(evidence.Models), RefreshedAt: evidence.ObservedAt,
	}, true
}

// put holds entry under key as a loaded file would, without writing it.
func (c *catalogFile) put(key string, entry catalogEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loadLocked()
	c.records[key] = entry.evidence()
	c.revisions[key] = c.nextRevisionLocked()
}

// catalog.json is one process's: the process loads it once and writes it
// after each change, so every read and write shares one handle, and so does
// the suite.
func TestCatalogFileKeepsTheCatalogStoreContract(t *testing.T) {
	catalogtest.Run(t, func(t *testing.T) catalogtest.Opener {
		t.Setenv("LLMGW_STATE_DIR", t.TempDir())
		store := &catalogFile{}
		return func(*testing.T) core.CatalogStore { return store }
	})
}

// recordingLog returns a logf for a catalogFile and what it logged. The
// store logs on the goroutine that changes it.
func recordingLog() (func(string, ...any), *[]string) {
	var logged []string
	return func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }, &logged
}

// readCatalogJSON parses catalog.json as a restarted process would.
func readCatalogJSON(path string) (map[string]catalogEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var entries map[string]catalogEntry
	return entries, json.Unmarshal(data, &entries)
}

// A catalog.json that does not parse, as one cut short by a crash, is moved
// aside with a warning rather than replaced by the next write, and the store
// starts without catalogs.
func TestCatalogFileMovesAnUnreadableFileAside(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LLMGW_STATE_DIR", dir)
	path := filepath.Join(dir, "catalog.json")
	torn := []byte(`{"fixture":{"schema_version":6,"models":[{"id":"fixture-model"`)
	if err := os.WriteFile(path, torn, 0o644); err != nil {
		t.Fatal(err)
	}
	logf, logged := recordingLog()
	store := &catalogFile{logf: logf}
	if _, err := store.Load(context.Background(), catalogKeyOf("fixture")); !errors.Is(err, core.ErrCatalogNotFound) {
		t.Fatalf("load over a torn file: err=%v, want no catalog", err)
	}
	aside, err := filepath.Glob(path + ".corrupt-*")
	if err != nil || len(aside) != 1 {
		t.Fatalf("moved aside=%v err=%v, want one copy of the torn file", aside, err)
	}
	if kept, err := os.ReadFile(aside[0]); err != nil || !bytes.Equal(kept, torn) {
		t.Fatalf("copy=%q err=%v, want the torn bytes unchanged", kept, err)
	}
	if len(*logged) != 1 || !strings.Contains((*logged)[0], filepath.Base(aside[0])) {
		t.Fatalf("logged=%q, want one warning that names the copy", *logged)
	}
	store.store("fixture", []ModelInfo{{ID: "rediscovered"}})
	if entries, err := readCatalogJSON(path); err != nil || len(entries["fixture"].Models) != 1 {
		t.Fatalf("catalog.json after a save: entries=%+v err=%v", entries, err)
	}
	if kept, _ := os.ReadFile(aside[0]); !bytes.Equal(kept, torn) {
		t.Fatal("the save replaced the copy of the torn file")
	}
}

// A write replaces catalog.json whole, so a reader of the file never sees a
// write in progress, and leaves no temporary file behind.
func TestCatalogFileReplacesTheFileWhole(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LLMGW_STATE_DIR", dir)
	path := filepath.Join(dir, "catalog.json")
	rows := func(count int) []ModelInfo {
		models := make([]ModelInfo, count)
		for index := range models {
			models[index] = ModelInfo{ID: fmt.Sprintf("fixture-model-%d", index), SupportedSurfaces: []string{"/chat/completions"}}
		}
		return models
	}
	// Windows refuses to replace a file another handle has open, so a write
	// that meets the reader there can fail; the store logs it, and the test
	// discards the log.
	store := &catalogFile{logf: func(string, ...any) {}}
	store.store("fixture", rows(1))
	done, failure := make(chan struct{}), make(chan error, 1)
	go func() {
		reads := 0
		for {
			select {
			case <-done:
				if reads == 0 {
					failure <- errors.New("the reader never read the file")
				}
				close(failure)
				return
			default:
			}
			data, err := os.ReadFile(path)
			if err != nil {
				// Windows refuses to open a file a rename is replacing.
				continue
			}
			var entries map[string]catalogEntry
			if err := json.Unmarshal(data, &entries); err != nil {
				failure <- fmt.Errorf("a reader saw %d bytes that do not parse: %v", len(data), err)
				close(failure)
				return
			}
			reads++
		}
	}()
	for round := range 20 {
		store.store("fixture", rows(200+round*100))
	}
	close(done)
	if err := <-failure; err != nil {
		t.Fatal(err)
	}
	store.store("fixture", rows(3))
	if entries, err := readCatalogJSON(path); err != nil || len(entries["fixture"].Models) != 3 {
		t.Fatalf("catalog.json after the last save: %d models, err=%v", len(entries["fixture"].Models), err)
	}
	if leftovers, err := filepath.Glob(filepath.Join(dir, ".catalog-*")); err != nil || len(leftovers) != 0 {
		t.Fatalf("temporary files left behind: %v (err=%v)", leftovers, err)
	}
}

// A write that fails is logged, leaves the store serving what it holds, and
// the next write saves every catalog the store holds.
func TestCatalogFileLogsAWriteThatFails(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LLMGW_STATE_DIR", dir)
	path := filepath.Join(dir, "catalog.json")
	logf, logged := recordingLog()
	store := &catalogFile{logf: logf}
	if _, err := store.Load(context.Background(), catalogKeyOf("first")); !errors.Is(err, core.ErrCatalogNotFound) {
		t.Fatalf("load: err=%v, want no catalog", err)
	}
	// A directory where the file belongs fails the rename that would put the
	// written file in place.
	if err := os.MkdirAll(filepath.Join(path, "blocking"), 0o755); err != nil {
		t.Fatal(err)
	}
	store.store("first", []ModelInfo{{ID: "first-model"}})
	if len(*logged) != 1 || !strings.Contains((*logged)[0], "could not save catalogs") {
		t.Fatalf("logged=%q, want the failed write", *logged)
	}
	if leftovers, err := filepath.Glob(filepath.Join(dir, ".catalog-*")); err != nil || len(leftovers) != 0 {
		t.Fatalf("temporary files left behind: %v (err=%v)", leftovers, err)
	}
	if entry, ok := store.entry("first"); !ok || len(entry.Models) != 1 {
		t.Fatal("the store dropped the catalog it could not write")
	}
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	store.store("second", []ModelInfo{{ID: "second-model"}})
	if entries, err := readCatalogJSON(path); err != nil || len(entries) != 2 {
		t.Fatalf("catalog.json after a write succeeds again: entries=%+v err=%v", entries, err)
	}
	if len(*logged) != 1 {
		t.Fatalf("logged=%q, want only the failed write", *logged)
	}
}

func TestCatalogFileKeysRoundTripTheCallerScope(t *testing.T) {
	for _, name := range []string{"zen", "copilot@prn_owner", "copilot@prn_service#prj_one"} {
		if got := catalogFileKey(catalogKeyOf(name)); got != name {
			t.Fatalf("%q round-tripped to %q", name, got)
		}
	}
	if key := catalogKeyOf("copilot@prn_service#prj_one"); key.Instance != "copilot" || key.CredentialKey != "prn_service#prj_one" {
		t.Fatalf("key=%+v", key)
	}
}

// Rows cross core's catalog unchanged: the gateway reads back every field
// it wrote, so /v1/models and routing read what they always read.
func TestCatalogRowsRoundTripThroughCore(t *testing.T) {
	capabilities := AdaptModelCapabilities(map[string]any{"chat": true}, []string{"/responses"}, time.Time{}, time.Time{})
	row := ModelInfo{
		ID: "model", Vendor: "vendor", Label: "Model", Free: true, Capabilities: map[string]any{"chat": true},
		TypedCapabilities: capabilities, SupportedSurfaces: []string{"/responses"},
	}
	got := gatewayRows(coreRows([]ModelInfo{row}))
	if len(got) != 1 || got[0].ID != row.ID || got[0].Vendor != row.Vendor || got[0].Label != row.Label ||
		!got[0].Free || got[0].Capabilities["chat"] != true || got[0].TypedCapabilities != capabilities ||
		len(got[0].SupportedSurfaces) != 1 || got[0].SupportedSurfaces[0] != "/responses" {
		t.Fatalf("round trip=%+v, want %+v", got, row)
	}
	if gatewayRows(coreRows(nil)) != nil || gatewayRows(coreRows([]ModelInfo{})) == nil {
		t.Fatal("a missing catalog and an empty one must stay distinct")
	}
}
