package providers

import (
	"context"
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
