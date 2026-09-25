package providers

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"llmgw/internal/config"

	core "github.com/xibodev/llmgw-core"
)

// catalogFile is catalog.json, the gateway's persisted catalog, as the
// core.ConditionalCatalogStore core's catalog service keeps the gateway's
// catalogs in. The file maps the key of each catalog, its instance and then
// the caller scope after an @ (see catalogCacheKey), to a catalogEntry; the
// scope is the core key's CredentialKey. The file is one process's, as it
// always was: it loads on first use, dropping the entries another schema
// stamped, and every change writes it whole. Revisions live in memory, so
// each entry the file loads gets a new one.
type catalogFile struct {
	mu     sync.Mutex
	loaded bool
	// records holds each key's catalog, and revisions each key's revision:
	// its record's, the one Delete left, or empty for a key only read. A key
	// in revisions is one a discovery may be storing, which an invalidation
	// must reach.
	records   map[string]core.CatalogEvidence
	revisions map[string]string
	revision  uint64
	// batches counts the invalidations in progress that write the file once
	// they end (see batch), and dirty marks a change they deferred.
	batches int
	dirty   bool
}

var _ core.ConditionalCatalogStore = (*catalogFile)(nil)

func catalogPath() string { return filepath.Join(config.StateDir(), "catalog.json") }

// catalogFileKey is the key catalog.json keeps the catalog of key under.
func catalogFileKey(key core.CatalogKey) string {
	instance, scope := strings.TrimSpace(key.Instance), strings.TrimSpace(key.CredentialKey)
	if scope == "" {
		return instance
	}
	return instance + "@" + scope
}

// catalogKeyOf is the core key of a catalog.json key: the instance, and the
// caller scope after the first @.
func catalogKeyOf(name string) core.CatalogKey {
	instance, scope, _ := strings.Cut(name, "@")
	return core.CatalogKey{Instance: instance, CredentialKey: scope}
}

// Load implements core.CatalogStore.
func (c *catalogFile) Load(ctx context.Context, key core.CatalogKey) (core.CatalogRecord, error) {
	if err := ctx.Err(); err != nil {
		return core.CatalogRecord{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loadLocked()
	name := catalogFileKey(key)
	revision, known := c.revisions[name]
	if !known {
		c.revisions[name] = ""
	}
	evidence, ok := c.records[name]
	if !ok {
		return core.CatalogRecord{Revision: revision}, core.ErrCatalogNotFound
	}
	evidence.Models = append(evidence.Models[:0:0], evidence.Models...)
	return core.CatalogRecord{Evidence: evidence, Revision: revision}, nil
}

// Save implements core.CatalogStore.
func (c *catalogFile) Save(ctx context.Context, key core.CatalogKey, evidence core.CatalogEvidence) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loadLocked()
	return c.saveLocked(catalogFileKey(key), evidence), nil
}

// SaveIf implements core.ConditionalCatalogStore.
func (c *catalogFile) SaveIf(ctx context.Context, key core.CatalogKey, evidence core.CatalogEvidence, expected string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loadLocked()
	name := catalogFileKey(key)
	if c.revisions[name] != expected {
		return "", core.ErrCatalogConflict
	}
	return c.saveLocked(name, evidence), nil
}

// Delete implements core.ConditionalCatalogStore.
func (c *catalogFile) Delete(ctx context.Context, key core.CatalogKey) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loadLocked()
	name := catalogFileKey(key)
	_, held := c.records[name]
	delete(c.records, name)
	c.revisions[name] = c.nextRevisionLocked()
	if held {
		c.changedLocked()
	}
	return nil
}

// saveLocked stores evidence with the typed capabilities each persisted row
// has carried since schema 3: its own, or those the gateway infers from its
// untyped map and surfaces, stamped with the discovery.
func (c *catalogFile) saveLocked(name string, evidence core.CatalogEvidence) string {
	rows := make([]core.ModelInfo, len(evidence.Models))
	copy(rows, evidence.Models)
	for index := range rows {
		if rows[index].Capabilities == nil {
			rows[index].Capabilities = AdaptModelCapabilities(
				rows[index].LegacyCapabilities, rows[index].SupportedAPIs, evidence.ObservedAt, time.Time{},
			)
		}
	}
	evidence.Models = rows
	c.records[name] = evidence
	revision := c.nextRevisionLocked()
	c.revisions[name] = revision
	c.changedLocked()
	return revision
}

func (c *catalogFile) nextRevisionLocked() string {
	c.revision++
	return strconv.FormatUint(c.revision, 10)
}

func (c *catalogFile) loadLocked() {
	if c.loaded {
		return
	}
	c.loaded = true
	c.records, c.revisions = map[string]core.CatalogEvidence{}, map[string]string{}
	entries := map[string]catalogEntry{}
	if b, err := os.ReadFile(catalogPath()); err == nil {
		_ = json.Unmarshal(b, &entries)
	}
	// Drop every entry this build cannot vouch for. The file is not rewritten
	// here: a read must not have a write side-effect, and the next change
	// persists the pruned map anyway. Until then the drop simply repeats on
	// each process start, which costs one map walk.
	for name, entry := range entries {
		if entry.SchemaVersion != catalogSchemaVersion {
			continue
		}
		c.records[name] = entry.evidence()
		c.revisions[name] = c.nextRevisionLocked()
	}
}

// evidence is the catalog core keeps for the entry: a discovery, empty or
// not, the one kind of catalog the file holds.
func (e catalogEntry) evidence() core.CatalogEvidence {
	status := core.CatalogDiscovered
	if len(e.Models) == 0 {
		status = core.CatalogEmpty
	}
	return core.CatalogEvidence{
		Status: status, Models: coreRows(e.Models), ObservedAt: e.RefreshedAt, SchemaVersion: e.SchemaVersion,
	}
}

// changedLocked writes the file, or leaves it to the batch in progress.
func (c *catalogFile) changedLocked() {
	if c.batches > 0 {
		c.dirty = true
		return
	}
	c.dirty = false
	entries := make(map[string]catalogEntry, len(c.records))
	for name, evidence := range c.records {
		if evidence.Status == core.CatalogDiscovered || evidence.Status == core.CatalogEmpty {
			entries[name] = catalogEntry{
				SchemaVersion: evidence.SchemaVersion, Models: gatewayRows(evidence.Models), RefreshedAt: evidence.ObservedAt,
			}
		}
	}
	_ = os.MkdirAll(config.StateDir(), 0o755)
	if b, err := json.MarshalIndent(entries, "", "  "); err == nil {
		_ = os.WriteFile(catalogPath(), b, 0o644)
	}
}

// batch runs invalidate and writes the file once afterwards, rather than
// once for each catalog invalidate forgets.
func (c *catalogFile) batch(invalidate func()) {
	c.mu.Lock()
	c.batches++
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.batches--; c.batches == 0 && c.dirty {
			c.changedLocked()
		}
	}()
	invalidate()
}

// keys returns every key of instance the store knows: those of its records,
// and those read, written or invalidated since it loaded, whose discovery
// may still be running.
func (c *catalogFile) keys(instance string) []core.CatalogKey {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loadLocked()
	var keys []core.CatalogKey
	for name := range c.revisions {
		if key := catalogKeyOf(name); key.Instance == instance {
			keys = append(keys, key)
		}
	}
	return keys
}

// CoreModelInfo is a gateway catalog row as core keeps and plans with it:
// its typed capabilities are core's Capabilities, its untyped map core's
// LegacyCapabilities, and its label core's DisplayName. gatewayModelInfo
// reads it back unchanged, so a row round-trips through core's catalog.
func CoreModelInfo(row ModelInfo) core.ModelInfo {
	return core.ModelInfo{
		ID: row.ID, SupportedAPIs: row.SupportedSurfaces, Capabilities: row.TypedCapabilities,
		DisplayName: row.Label, Vendor: row.Vendor, Free: row.Free, LegacyCapabilities: row.Capabilities,
	}
}

func gatewayModelInfo(row core.ModelInfo) ModelInfo {
	return ModelInfo{
		ID: row.ID, Vendor: row.Vendor, Label: row.DisplayName, Free: row.Free,
		Capabilities: row.LegacyCapabilities, TypedCapabilities: row.Capabilities, SupportedSurfaces: row.SupportedAPIs,
	}
}

func coreRows(rows []ModelInfo) []core.ModelInfo {
	if rows == nil {
		return nil
	}
	out := make([]core.ModelInfo, len(rows))
	for index, row := range rows {
		out[index] = CoreModelInfo(row)
	}
	return out
}

func gatewayRows(rows []core.ModelInfo) []ModelInfo {
	if rows == nil {
		return nil
	}
	out := make([]ModelInfo, len(rows))
	for index, row := range rows {
		out[index] = gatewayModelInfo(row)
	}
	return out
}
