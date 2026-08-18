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

var quotaAdapterRegistry = struct {
	sync.RWMutex
	values map[string]QuotaAdapter
}{values: map[string]QuotaAdapter{}}

func RegisterQuotaAdapter(adapter QuotaAdapter) error {
	if adapter == nil {
		return fmt.Errorf("quota adapter is required")
	}
	id := strings.ToLower(strings.TrimSpace(adapter.ID()))
	if !registryIdentifierPattern.MatchString(id) {
		return fmt.Errorf("invalid quota adapter id %q", id)
	}
	quotaAdapterRegistry.Lock()
	defer quotaAdapterRegistry.Unlock()
	if _, exists := quotaAdapterRegistry.values[id]; exists {
		return fmt.Errorf("quota adapter %q is already registered", id)
	}
	quotaAdapterRegistry.values[id] = adapter
	return nil
}

func QuotaAdapterByID(id string) (QuotaAdapter, bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	quotaAdapterRegistry.RLock()
	defer quotaAdapterRegistry.RUnlock()
	adapter, ok := quotaAdapterRegistry.values[id]
	return adapter, ok
}

func resetQuotaAdaptersForTests() {
	quotaAdapterRegistry.Lock()
	quotaAdapterRegistry.values = map[string]QuotaAdapter{}
	quotaAdapterRegistry.Unlock()
}
