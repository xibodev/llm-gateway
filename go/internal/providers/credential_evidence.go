package providers

import (
	"context"
	"strconv"
	"sync"

	core "github.com/xibodev/llmgw-core"
)

// credentialEvidence is the providers Runtime's core.EvidenceSink. Probes and
// verifications attribute their result to the credential revision an
// operation used, so an operation that reports an observation collects the
// evidence of its own context; every other operation carries no collector,
// and the sink drops its evidence. Inference therefore still records nothing
// about accounts, as before the core Runtime served it.
type credentialEvidence struct{}

// Record implements core.EvidenceSink. Evidence without a credential key
// comes from an operation that failed before it had one, which leaves the
// collector with what the credential store handed out.
func (credentialEvidence) Record(ctx context.Context, evidence core.AccountEvidence) {
	credentialCollectorFrom(ctx).observe(evidence.CredentialKey, evidence.CredentialRevision)
}

// credentialCollector keeps the last credential an operation saw. The
// credential store reports each record it hands out under the collector's
// context, and the core Runtime then reports the evidence of the operation.
// The evidence comes last, so it names the credential the operation used,
// including one a refresh replaced mid-request; an operation that failed
// before it had a credential keeps the record its refresh started from, as
// the Codex path reported before.
type credentialCollector struct {
	mu          sync.Mutex
	observation *CredentialObservation
}

type credentialCollectorKey struct{}

// collectCredentials returns ctx carrying a new collector.
func collectCredentials(ctx context.Context) (context.Context, *credentialCollector) {
	collector := &credentialCollector{}
	return context.WithValue(ctx, credentialCollectorKey{}, collector), collector
}

// credentialCollectorFrom returns the collector ctx carries, or nil.
func credentialCollectorFrom(ctx context.Context) *credentialCollector {
	collector, _ := ctx.Value(credentialCollectorKey{}).(*credentialCollector)
	return collector
}

// observe records that the operation holds the credential key at revision.
// The credential store renders revisions in decimal; one that does not parse
// is revision 0, which every guard refuses.
func (c *credentialCollector) observe(key, revision string) {
	if c == nil || key == "" {
		return
	}
	parsed, _ := strconv.ParseInt(revision, 10, 64)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.observation = &CredentialObservation{ConnectionID: key, CredentialRevision: parsed}
}

// Observation returns a copy of the last credential observed, or nil when the
// operation never held one.
func (c *credentialCollector) Observation() *CredentialObservation {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.observation == nil {
		return nil
	}
	observation := *c.observation
	return &observation
}
