package iam

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"llmgw/internal/config"
)

const maxExternalKeyDocumentBytes int64 = 32 << 20

type externalKeyRecord struct {
	Name             string   `json:"name"`
	Project          string   `json:"project,omitempty"`
	Key              string   `json:"key,omitempty"`
	KeySHA256        string   `json:"key_sha256,omitempty"`
	AllowedModels    []string `json:"allowed_models,omitempty"`
	AllowedRoutes    []string `json:"allowed_routes,omitempty"`
	AllowedProviders []string `json:"allowed_providers,omitempty"`
	ExpiresAt        string   `json:"expires_at,omitempty"`
}

type externalKeyDocument struct {
	Version int                 `json:"version"`
	Count   *int                `json:"count,omitempty"`
	Keys    []externalKeyRecord `json:"keys"`
}

type externalKey struct {
	digest           [sha256.Size]byte
	name             string
	project          string
	allowedModels    []string
	allowedRoutes    []string
	allowedProviders []string
	expiresAt        time.Time
	staleAt          time.Time
}

type externalKeySnapshot struct {
	keys map[[sha256.Size]byte]externalKey
}

type externalKeySource struct {
	name     string
	path     string
	url      string
	token    string
	interval time.Duration
	timeout  time.Duration

	mu       sync.Mutex
	etag     string
	loadedAt time.Time
	keys     map[[sha256.Size]byte]externalKey
}

type externalKeyStore struct {
	sources      []*externalKeySource
	maxStaleness time.Duration
	client       *http.Client
	now          func() time.Time
	snapshot     atomic.Pointer[externalKeySnapshot]
}

var activeExternalKeys atomic.Pointer[externalKeyStore]

// StartExternalKeysFromEnv enables reloadable external keys only when a file or
// URL is explicitly configured. This is substantially adapted from the atomic
// source snapshots and last-known-good reload design in openziti/llm-gateway,
// Apache-2.0, commit 4997232f15aeb900d36fb396269acf89ac0ee3cd.
func StartExternalKeysFromEnv(ctx context.Context) (func(), error) {
	file := strings.TrimSpace(os.Getenv("LLMGW_EXTERNAL_KEYS_FILE"))
	url := strings.TrimSpace(os.Getenv("LLMGW_EXTERNAL_KEYS_URL"))
	if file == "" && url == "" {
		activeExternalKeys.Store(nil)
		return func() {}, nil
	}
	interval, err := externalDurationEnv("LLMGW_EXTERNAL_KEYS_REFRESH_INTERVAL", 30*time.Second)
	if err != nil {
		return nil, err
	}
	timeout, err := externalDurationEnv("LLMGW_EXTERNAL_KEYS_HTTP_TIMEOUT", 5*time.Second)
	if err != nil {
		return nil, err
	}
	maxStaleness, err := externalDurationEnv("LLMGW_EXTERNAL_KEYS_MAX_STALENESS", 0)
	if err != nil {
		return nil, err
	}
	if interval <= 0 || timeout <= 0 || maxStaleness < 0 {
		return nil, fmt.Errorf("external key durations must be positive (max staleness may be zero)")
	}
	sources := make([]*externalKeySource, 0, 2)
	if file != "" {
		sources = append(sources, &externalKeySource{name: "file", path: file, interval: interval})
	}
	if url != "" {
		sources = append(sources, &externalKeySource{
			name: "http", url: url, token: os.Getenv("LLMGW_EXTERNAL_KEYS_HTTP_TOKEN"),
			interval: interval, timeout: timeout,
		})
	}
	store := &externalKeyStore{sources: sources, maxStaleness: maxStaleness, client: http.DefaultClient, now: time.Now}
	for _, source := range sources {
		if err := store.refresh(ctx, source); err != nil {
			return nil, fmt.Errorf("load external key source %s: %w", source.name, err)
		}
	}
	store.publish()
	activeExternalKeys.Store(store)
	runCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	for _, source := range sources {
		wg.Add(1)
		go func(source *externalKeySource) {
			defer wg.Done()
			store.run(runCtx, source)
		}(source)
	}
	return func() {
		cancel()
		wg.Wait()
		activeExternalKeys.CompareAndSwap(store, nil)
	}, nil
}

func externalDurationEnv(name string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return duration, nil
}

func (s *externalKeyStore) run(ctx context.Context, source *externalKeySource) {
	ticker := time.NewTicker(source.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.refresh(ctx, source); err != nil {
				log.Printf("external key source %s refresh failed; retaining last-known-good snapshot: %v", source.name, err)
			}
			s.publish()
		}
	}
}

func (s *externalKeyStore) refresh(ctx context.Context, source *externalKeySource) error {
	source.mu.Lock()
	defer source.mu.Unlock()
	var (
		data []byte
		etag string
		err  error
	)
	if source.path != "" {
		data, err = os.ReadFile(source.path)
		if err != nil {
			return fmt.Errorf("read file: %w", err)
		}
	} else {
		data, etag, err = s.fetch(ctx, source)
		if err != nil {
			return err
		}
		if data == nil {
			source.loadedAt = s.now()
			return nil
		}
	}
	keys, err := decodeExternalKeys(data, source.url != "")
	if err != nil {
		return err
	}
	source.keys = keys
	source.loadedAt = s.now()
	if source.url != "" {
		source.etag = etag
	}
	return nil
}

func (s *externalKeyStore) fetch(ctx context.Context, source *externalKeySource) ([]byte, string, error) {
	requestCtx, cancel := context.WithTimeout(ctx, source.timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, source.url, nil)
	if err != nil {
		return nil, "", fmt.Errorf("build request: %w", err)
	}
	request.Header.Set("Cache-Control", "no-cache")
	if source.token != "" {
		request.Header.Set("Authorization", "Bearer "+source.token)
	}
	if source.etag != "" {
		request.Header.Set("If-None-Match", source.etag)
	}
	response, err := s.client.Do(request)
	if err != nil {
		return nil, "", fmt.Errorf("request failed")
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotModified {
		if source.etag == "" {
			return nil, "", fmt.Errorf("HTTP 304 without a prior ETag")
		}
		return nil, source.etag, nil
	}
	if response.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("HTTP status %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxExternalKeyDocumentBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("read response: %w", err)
	}
	if int64(len(data)) > maxExternalKeyDocumentBytes {
		return nil, "", fmt.Errorf("response exceeds %d bytes", maxExternalKeyDocumentBytes)
	}
	return data, response.Header.Get("ETag"), nil
}

func decodeExternalKeys(data []byte, requireCount bool) (map[[sha256.Size]byte]externalKey, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var document externalKeyDocument
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode key document: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("decode key document: trailing content")
	}
	if document.Version != 1 {
		return nil, fmt.Errorf("unsupported key document version %d", document.Version)
	}
	if requireCount && document.Count == nil {
		return nil, fmt.Errorf("count is required for HTTP key documents")
	}
	if document.Count != nil && *document.Count != len(document.Keys) {
		return nil, fmt.Errorf("count is %d but keys contains %d records", *document.Count, len(document.Keys))
	}
	result := make(map[[sha256.Size]byte]externalKey, len(document.Keys))
	for index, wire := range document.Keys {
		if strings.TrimSpace(wire.Name) == "" || (wire.Key == "") == (wire.KeySHA256 == "") {
			return nil, fmt.Errorf("keys[%d] requires a name and exactly one of key or key_sha256", index)
		}
		var digest [sha256.Size]byte
		if wire.Key != "" {
			digest = sha256.Sum256([]byte(wire.Key))
		} else {
			decoded, err := hex.DecodeString(wire.KeySHA256)
			if err != nil || len(decoded) != sha256.Size {
				return nil, fmt.Errorf("keys[%d].key_sha256 must be 64 hexadecimal characters", index)
			}
			copy(digest[:], decoded)
		}
		var expiry time.Time
		if wire.ExpiresAt != "" {
			var err error
			expiry, err = time.Parse(time.RFC3339, wire.ExpiresAt)
			if err != nil {
				return nil, fmt.Errorf("keys[%d].expires_at must be RFC3339", index)
			}
		}
		if _, duplicate := result[digest]; duplicate {
			return nil, fmt.Errorf("keys[%d] duplicates another key", index)
		}
		project := strings.TrimSpace(wire.Project)
		if project == "" {
			return nil, fmt.Errorf("keys[%d].project is required", index)
		}
		result[digest] = externalKey{
			digest: digest, name: wire.Name, project: project,
			allowedModels:    append([]string(nil), wire.AllowedModels...),
			allowedRoutes:    append([]string(nil), wire.AllowedRoutes...),
			allowedProviders: append([]string(nil), wire.AllowedProviders...), expiresAt: expiry,
		}
	}
	return result, nil
}

func (s *externalKeyStore) publish() {
	now := s.now()
	combined := map[[sha256.Size]byte]externalKey{}
	for _, source := range s.sources {
		source.mu.Lock()
		stale := s.maxStaleness > 0 && !source.loadedAt.IsZero() && now.Sub(source.loadedAt) >= s.maxStaleness
		if !stale {
			for digest, key := range source.keys {
				if _, exists := combined[digest]; !exists {
					if s.maxStaleness > 0 {
						key.staleAt = source.loadedAt.Add(s.maxStaleness)
					}
					combined[digest] = key
				}
			}
		}
		source.mu.Unlock()
	}
	s.snapshot.Store(&externalKeySnapshot{keys: combined})
}

func resolveExternalAPIKey(token string) (*config.Principal, bool) {
	store := activeExternalKeys.Load()
	if store == nil || token == "" {
		return nil, false
	}
	snapshot := store.snapshot.Load()
	if snapshot == nil {
		return nil, false
	}
	key, found := snapshot.keys[sha256.Sum256([]byte(token))]
	now := store.now()
	if !found || (!key.expiresAt.IsZero() && !now.Before(key.expiresAt)) ||
		(!key.staleAt.IsZero() && !now.Before(key.staleAt)) {
		return nil, false
	}
	project, found, err := ProjectBySlug(key.project)
	if err != nil || !found || project.Status != "active" {
		return nil, false
	}
	return &config.Principal{
		ProjectID: project.ID, Project: project.Slug, Key: key.name, Token: token,
		AllowedModels: key.allowedModels, AllowedRoutes: key.allowedRoutes,
		AllowedProviders: key.allowedProviders,
	}, true
}

func HasExternalAPIKeys() bool {
	store := activeExternalKeys.Load()
	if store == nil {
		return false
	}
	snapshot := store.snapshot.Load()
	return snapshot != nil && len(snapshot.keys) > 0
}
