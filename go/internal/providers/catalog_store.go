package providers

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime"
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
	// changes counts the changes to records. kept marks a file that could
	// be neither read nor set aside at load: it is never replaced, so the
	// catalogs live in memory only rather than overwrite what it holds.
	changes uint64
	kept    bool

	// writing orders the writes of the file, which run outside mu so that a
	// slow disk holds up no read, and written is the change the file holds.
	writing sync.Mutex
	written uint64

	// logf reports what the store could not read or write, never a
	// catalog's rows; nil uses the standard logger.
	logf func(format string, args ...any)
}

// catalogWrite is the file one change leaves: the entries to write, taken
// under mu, and the change they hold.
type catalogWrite struct {
	change  uint64
	entries map[string]catalogEntry
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
	c.loadLocked()
	revision, write := c.saveLocked(catalogFileKey(key), evidence)
	c.mu.Unlock()
	c.persist(write)
	return revision, nil
}

// SaveIf implements core.ConditionalCatalogStore.
func (c *catalogFile) SaveIf(ctx context.Context, key core.CatalogKey, evidence core.CatalogEvidence, expected string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	c.mu.Lock()
	c.loadLocked()
	name := catalogFileKey(key)
	if c.revisions[name] != expected {
		c.mu.Unlock()
		return "", core.ErrCatalogConflict
	}
	revision, write := c.saveLocked(name, evidence)
	c.mu.Unlock()
	c.persist(write)
	return revision, nil
}

// Delete implements core.ConditionalCatalogStore.
func (c *catalogFile) Delete(ctx context.Context, key core.CatalogKey) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.loadLocked()
	name := catalogFileKey(key)
	_, held := c.records[name]
	delete(c.records, name)
	c.revisions[name] = c.nextRevisionLocked()
	var write *catalogWrite
	if held {
		write = c.changedLocked()
	}
	c.mu.Unlock()
	c.persist(write)
	return nil
}

// saveLocked stores evidence with the typed capabilities each persisted row
// has carried since schema 3: its own, or those the gateway infers from its
// untyped map and surfaces, stamped with the discovery. It returns the
// record's revision and the write that persists it.
func (c *catalogFile) saveLocked(name string, evidence core.CatalogEvidence) (string, *catalogWrite) {
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
	return revision, c.changedLocked()
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
	// Drop every entry this build cannot vouch for. The file is not rewritten
	// here: a read must not have a write side-effect, and the next change
	// persists the pruned map anyway. Until then the drop simply repeats on
	// each process start, which costs one map walk.
	for name, entry := range c.readLocked() {
		if entry.SchemaVersion != catalogSchemaVersion {
			continue
		}
		c.records[name] = entry.evidence()
		c.revisions[name] = c.nextRevisionLocked()
	}
}

// readLocked returns the entries catalog.json holds, none when there is no
// file. A file that cannot be read or parsed, such as one cut short by a
// crash or edited by hand, is moved aside to catalog.json.corrupt-<UTC time>
// rather than left for the next write to replace, so what it holds can still
// be inspected or restored; the catalogs are then discovered again. One that
// cannot be moved either is kept: the store never replaces it.
func (c *catalogFile) readLocked() map[string]catalogEntry {
	path := catalogPath()
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err == nil {
		var entries map[string]catalogEntry
		if err = json.Unmarshal(data, &entries); err == nil {
			return entries
		}
	}
	aside := path + ".corrupt-" + time.Now().UTC().Format("20060102T150405Z")
	if moveErr := os.Rename(path, aside); moveErr != nil {
		c.kept = true
		c.log("catalog: %s is unreadable (%v) and could not be moved aside (%v); "+
			"catalogs are kept in memory only and the file is left as it is", path, err, moveErr)
		return nil
	}
	c.log("catalog: %s is unreadable (%v); moved it to %s and starting without stored catalogs",
		path, err, filepath.Base(aside))
	return nil
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

// changedLocked records a change and returns the write that persists it:
// nil while a batch defers it, or when the file is kept. The caller persists
// the write once it releases mu.
func (c *catalogFile) changedLocked() *catalogWrite {
	if c.batches > 0 {
		c.dirty = true
		return nil
	}
	c.dirty = false
	c.changes++
	if c.kept {
		return nil
	}
	// Rows are never changed in place once stored, so the entries may be
	// encoded after mu is released.
	entries := make(map[string]catalogEntry, len(c.records))
	for name, evidence := range c.records {
		if evidence.Status == core.CatalogDiscovered || evidence.Status == core.CatalogEmpty {
			entries[name] = catalogEntry{
				SchemaVersion: evidence.SchemaVersion, Models: gatewayRows(evidence.Models), RefreshedAt: evidence.ObservedAt,
			}
		}
	}
	return &catalogWrite{change: c.changes, entries: entries}
}

// persist writes the file write describes, unless the file already holds a
// later change. A failed write leaves the previous file whole; it is logged,
// and the next change writes the file again.
func (c *catalogFile) persist(write *catalogWrite) {
	if write == nil {
		return
	}
	c.writing.Lock()
	defer c.writing.Unlock()
	if write.change <= c.written {
		return
	}
	path := catalogPath()
	data, err := json.MarshalIndent(write.entries, "", "  ")
	if err == nil {
		err = replaceCatalogFile(path, data)
	}
	if err != nil {
		c.log("catalog: could not save catalogs to %s: %v", path, err)
		return
	}
	c.written = write.change
}

// log reports what the store could not read or write.
func (c *catalogFile) log(format string, args ...any) {
	if c.logf != nil {
		c.logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// replaceCatalogFile replaces the file at path with data so that a reader,
// or a process restarted after a crash, finds the old file or the new one and
// never a part of either: data is synced in a file of its own in the same
// directory, which a rename then puts in place.
func replaceCatalogFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".catalog-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if err := temp.Chmod(0o644); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(temp.Name(), path); err != nil {
		return err
	}
	syncDirectory(dir)
	return nil
}

// syncDirectory makes a rename in dir durable where the platform can. It is
// best effort: the rename has already put the new file in place, and a crash
// before the directory reaches the disk leaves the previous file, whose
// catalogs are only older. Windows cannot sync a directory handle.
func syncDirectory(dir string) {
	if runtime.GOOS == "windows" {
		return
	}
	directory, err := os.Open(dir)
	if err != nil {
		return
	}
	defer directory.Close()
	_ = directory.Sync()
}

// batch runs invalidate and writes the file once afterwards, rather than
// once for each catalog invalidate forgets.
func (c *catalogFile) batch(invalidate func()) {
	c.mu.Lock()
	c.batches++
	c.mu.Unlock()
	defer func() {
		var write *catalogWrite
		c.mu.Lock()
		if c.batches--; c.batches == 0 && c.dirty {
			write = c.changedLocked()
		}
		c.mu.Unlock()
		c.persist(write)
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
