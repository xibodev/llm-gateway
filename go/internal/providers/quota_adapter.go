package providers

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"llmgw/internal/iam"
)

type QuotaFetchRequest struct {
	ConnectionID       string
	CredentialRevision int64
	PrincipalID        string
	ProviderID         string
	ConnectionName     string
}

type QuotaFetchResult struct {
	Snapshots      []iam.ProviderQuotaSnapshot
	AccountTier    string
	TokenExpiresAt int64
}

// QuotaAdapter fetches verified upstream quota for one provider account.
// Implementations may read encrypted credentials through IAM but must return
// only normalized, secret-free snapshots.
type QuotaAdapter interface {
	ID() string
	Fetch(context.Context, QuotaFetchRequest) (QuotaFetchResult, error)
}

// quotaAdapterRegistry maps adapter ids to quota adapters.
type quotaAdapterRegistry struct {
	mu     sync.RWMutex
	values map[string]QuotaAdapter
}

func (rt *Runtime) RegisterQuotaAdapter(adapter QuotaAdapter) error {
	if adapter == nil {
		return fmt.Errorf("quota adapter is required")
	}
	id := strings.ToLower(strings.TrimSpace(adapter.ID()))
	if !registryIdentifierPattern.MatchString(id) {
		return fmt.Errorf("invalid quota adapter id %q", id)
	}
	rt.quotaAdapters.mu.Lock()
	defer rt.quotaAdapters.mu.Unlock()
	if _, exists := rt.quotaAdapters.values[id]; exists {
		return fmt.Errorf("quota adapter %q is already registered", id)
	}
	rt.quotaAdapters.values[id] = adapter
	return nil
}

func (rt *Runtime) QuotaAdapterByID(id string) (QuotaAdapter, bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	rt.quotaAdapters.mu.RLock()
	defer rt.quotaAdapters.mu.RUnlock()
	adapter, ok := rt.quotaAdapters.values[id]
	return adapter, ok
}
