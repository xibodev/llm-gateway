package roster

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"llmgw/internal/config"
)

const refreshCooldown = 5 * time.Minute

// Options is explicit dependency injection for isolated stores. Client is a
// trusted Go-only transport override for tests; there is no insecure env switch.
// New does not start network work. AutoRefresh controls Start, never Refresh.
type Options struct {
	URL         string
	Keys        map[string]ed25519.PublicKey
	StateDir    string
	AutoRefresh bool
	Client      *http.Client
	Resolver    Resolver
}

type Store struct {
	mu          sync.Mutex
	options     Options
	client      *http.Client
	snapshot    Snapshot
	digest      [32]byte
	published   time.Time
	lastAttempt time.Time
	busy        chan struct{}
	startOnce   sync.Once
	stop        func()
	clock       func() time.Time
}

type cache struct {
	Envelope    json.RawMessage `json:"envelope"`
	LastSuccess string          `json:"last_success"`
}

// New loads only a bounded, validated cache. Invalid configuration or cache is
// status data, never a startup error that could interrupt inference. When no
// signing keys are provided, the feed is validated as plain HTTPS JSON.
func New(o Options) *Store {
	keys := make(map[string]ed25519.PublicKey, len(o.Keys))
	for id, key := range o.Keys {
		keys[id] = append(ed25519.PublicKey(nil), key...)
	}
	o.Keys = keys
	s := &Store{options: o, clock: time.Now, snapshot: Snapshot{Entries: []Entry{}, Sources: []Source{}, AutoRefresh: o.AutoRefresh, Stale: true}}
	s.client = publicClient(o.Resolver)
	if o.Client != nil {
		copy := *o.Client
		copy.Timeout = 30 * time.Second
		copy.CheckRedirect = noRedirect
		s.client = &copy
	}
	_, urlErr := publicURL(o.URL)
	validKeys := len(keys) > 0
	for id, key := range keys {
		if id == "" || len(key) != ed25519.PublicKeySize {
			validKeys = false
		}
	}
	// Plain mode (no keys) is valid: HTTPS plus structural validation is the
	// trust model for discovery-only data. Signed mode requires valid keys.
	plainMode := len(keys) == 0
	if plainMode {
		s.snapshot.Configured = urlErr == nil
	} else {
		s.snapshot.Configured = urlErr == nil && validKeys
	}
	if o.URL != "" && urlErr != nil {
		s.snapshot.Error = urlErr.Error()
	}
	if len(keys) > 0 && !validKeys {
		s.snapshot.Error = "Roster trust configuration is invalid."
	}
	s.loadCache()
	return s
}

// FromEnvironment uses LLMGW_PROVIDER_ROSTER_{URL,AUTO_REFRESH} and optionally
// LLMGW_PROVIDER_ROSTER_{PUBLIC_KEY,KEY_ID}. URL has no default. When no public
// key is configured, the feed is consumed as plain HTTPS JSON. When a public key
// is provided, the feed must be a signed Ed25519 envelope.
func FromEnvironment() *Store {
	o := Options{URL: os.Getenv("LLMGW_PROVIDER_ROSTER_URL"), StateDir: config.StateDir(), AutoRefresh: true}
	keyID := os.Getenv("LLMGW_PROVIDER_ROSTER_KEY_ID")
	if keyID == "" {
		keyID = "staging"
	}
	var configErr string
	if value := os.Getenv("LLMGW_PROVIDER_ROSTER_PUBLIC_KEY"); value != "" {
		key, err := base64.StdEncoding.Strict().DecodeString(value)
		if err != nil || len(key) != ed25519.PublicKeySize {
			configErr = "Roster trust configuration is invalid."
		}
		o.Keys = map[string]ed25519.PublicKey{keyID: key}
	}
	if value := os.Getenv("LLMGW_PROVIDER_ROSTER_AUTO_REFRESH"); value != "" {
		var err error
		o.AutoRefresh, err = strconv.ParseBool(value)
		if err != nil {
			configErr = "Roster auto-refresh configuration is invalid."
		}
	}
	s := New(o)
	if configErr != "" {
		s.snapshot.Error = configErr
		s.snapshot.Configured = false
	}
	return s
}

var defaultStore = sync.OnceValue(FromEnvironment)

// Default is the process singleton shared by HTTP handlers and main. Tests and
// embedders should use New for independent stores instead of replacing globals.
func Default() *Store { return defaultStore() }

func (s *Store) cachePath() string {
	return filepath.Join(s.options.StateDir, "provider-roster-cache.json")
}

func (s *Store) loadCache() {
	if s.options.StateDir == "" {
		return
	}
	f, err := os.Open(s.cachePath())
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		s.snapshot.Error = "Roster cache could not be read."
		return
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxFeedBytes+4097))
	var c cache
	if err != nil || len(b) > maxFeedBytes+4096 || json.Unmarshal(b, &c) != nil {
		s.snapshot.Error = "Roster cache is invalid."
		return
	}
	// Try signed verification first when keys are configured, then plain.
	// When no keys are configured, only plain verification applies.
	var p Payload
	var digest [32]byte
	var verifyErr error
	if len(s.options.Keys) > 0 {
		p, digest, verifyErr = verify(c.Envelope, s.options.Keys, s.clock())
	}
	if verifyErr != nil || len(s.options.Keys) == 0 {
		p, digest, verifyErr = verifyPlain(c.Envelope, s.clock())
	}
	success, timeErr := time.Parse(time.RFC3339, c.LastSuccess)
	if verifyErr != nil || timeErr != nil || success.After(s.clock().Add(10*time.Minute)) {
		s.snapshot.Error = "Roster cache is invalid."
		return
	}
	s.accept(p, digest, c.LastSuccess)
	// Last-check metadata is diagnostic, not part of the trust decision.
	s.snapshot.LastChecked = c.LastSuccess
	s.lastAttempt = success
}

func (s *Store) accept(p Payload, digest [32]byte, success string) {
	s.snapshot.Entries, s.snapshot.Sources = p.Entries, p.Sources
	s.snapshot.Revision, s.snapshot.PublishedAt = p.Revision, p.PublishedAt
	s.snapshot.LastSuccess = success
	s.digest = digest
	s.published, _ = time.Parse(time.RFC3339, p.PublishedAt)
}

func (s *Store) snapshotLocked() Snapshot {
	result := s.snapshot
	result.Stale = result.Revision == 0 || result.Error != "" || s.clock().Sub(s.published) > 72*time.Hour || staleSources(result.Sources)
	// Republishing retained entries cannot make failed collection fresh again.
	for _, entry := range result.Entries {
		result.Stale = result.Stale || staleSources(entry.Sources)
	}
	// Never let callers mutate nested slices/pointers owned by the store.
	b, _ := json.Marshal(result)
	var detached Snapshot
	_ = json.Unmarshal(b, &detached)
	return detached
}

func (s *Store) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotLocked()
}

// Refresh coalesces concurrent callers and globally cools down all attempts,
// including failures. Operational failures are sanitized in the response; the
// last verified roster stays available. Manual refresh works with auto off.
func (s *Store) Refresh(ctx context.Context) Snapshot {
	s.mu.Lock()
	if s.busy != nil {
		done := s.busy
		s.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
		}
		return s.Snapshot()
	}
	if !s.snapshot.Configured || (!s.lastAttempt.IsZero() && s.clock().Sub(s.lastAttempt) < refreshCooldown) {
		result := s.snapshotLocked()
		s.mu.Unlock()
		return result
	}
	s.lastAttempt = s.clock()
	s.snapshot.LastChecked = s.lastAttempt.UTC().Format(time.RFC3339)
	s.busy = make(chan struct{})
	s.mu.Unlock()

	p, digest, raw, err := s.fetch(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	defer func() { close(s.busy); s.busy = nil }()
	if err == nil && (p.Revision < s.snapshot.Revision || (p.Revision == s.snapshot.Revision && digest != s.digest)) {
		err = errors.New("Roster rollback or conflicting revision rejected.")
	}
	if err == nil {
		published, _ := time.Parse(time.RFC3339, p.PublishedAt)
		if published.Before(s.published) {
			err = errors.New("Roster publication rollback rejected.")
		}
	}
	if err == nil {
		success := s.clock().UTC().Format(time.RFC3339)
		if err = s.saveCache(raw, success); err == nil {
			s.accept(p, digest, success)
		}
	}
	s.snapshot.Error = ""
	if err != nil {
		s.snapshot.Error = err.Error()
	}
	return s.snapshotLocked()
}

func (s *Store) fetch(ctx context.Context) (Payload, [32]byte, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var empty Payload
	var digest [32]byte
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.options.URL, nil)
	if err != nil {
		return empty, digest, nil, errors.New("Roster request failed.")
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return empty, digest, nil, errors.New("Roster fetch failed.")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return empty, digest, nil, errors.New("Roster server did not return a successful response.")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxFeedBytes+1))
	if err != nil || len(raw) > maxFeedBytes {
		return empty, digest, nil, errors.New("Roster response is unreadable or too large.")
	}
	// Try signed verification first when keys are configured, then plain.
	// When no keys are configured, only plain verification applies.
	if len(s.options.Keys) > 0 {
		p, d, signErr := verify(raw, s.options.Keys, s.clock())
		if signErr == nil {
			return p, d, raw, nil
		}
	}
	p, d, plainErr := verifyPlain(raw, s.clock())
	if plainErr != nil {
		if len(s.options.Keys) > 0 {
			return empty, digest, nil, errors.New("Roster signature or payload is invalid.")
		}
		return empty, digest, nil, plainErr
	}
	return p, d, raw, nil
}

func (s *Store) saveCache(raw []byte, success string) error {
	if s.options.StateDir == "" {
		return nil
	}
	failed := errors.New("Verified roster could not be saved to cache.")
	if os.MkdirAll(s.options.StateDir, 0700) != nil {
		return failed
	}
	f, err := os.CreateTemp(s.options.StateDir, ".provider-roster-*")
	if err != nil {
		return failed
	}
	defer os.Remove(f.Name())
	if err := json.NewEncoder(f).Encode(cache{Envelope: raw, LastSuccess: success}); err != nil {
		f.Close()
		return failed
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return failed
	}
	if err := f.Close(); err != nil {
		return failed
	}
	if os.Rename(f.Name(), s.cachePath()) != nil {
		return failed
	}
	return nil
}

// Start begins a cancellable daily schedule (24h +/- 1h), with an asynchronous
// initial refresh. Its idempotent stop function cancels and joins the worker.
func (s *Store) Start(parent context.Context) func() {
	s.startOnce.Do(func() {
		ctx, cancel := context.WithCancel(parent)
		done := make(chan struct{})
		s.stop = func() { cancel(); <-done; s.client.CloseIdleConnections() }
		go func() {
			defer close(done)
			if !s.options.AutoRefresh || !s.Snapshot().Configured {
				return
			}
			for {
				if ctx.Err() != nil {
					return
				}
				s.Refresh(ctx)
				delay := 24 * time.Hour
				if n, err := rand.Int(rand.Reader, big.NewInt(int64(2*time.Hour))); err == nil {
					delay += time.Duration(n.Int64()) - time.Hour
				}
				timer := time.NewTimer(delay)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
		}()
	})
	return s.stop
}
