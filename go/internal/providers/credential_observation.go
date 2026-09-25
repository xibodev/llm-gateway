package providers

import "llmgw/internal/iam"

// CredentialObservation names the stored credential revision a provider used
// for an operation, so that a health or check result is applied only while
// that revision is still current. It carries the credential fields of
// core.AccountEvidence: ConnectionID is CredentialKey, the provider connection
// ID that also keys the credential store, and CredentialRevision is
// CredentialRevision, which the credential store renders in decimal.
//
// A nil observation means there is nothing to guard: system, configured,
// bound and legacy credentials report none, and neither does a connection at
// revision 0, which has no account state yet. Codex is the exception: it
// reports the credential its operation held (see credentialCollector), at
// revision 0 too, and nil only when it never held one. The guards refuse
// revision 0, but callers let any non-nil observation replace the one they
// held, so dropping revision 0 from Codex would change what a verification
// records.
type CredentialObservation struct {
	ConnectionID       string
	CredentialRevision int64
}

// credentialObservation converts iam's observation where a provider reads a
// stored credential. Nil stays nil and revision 0 is kept, because whether
// revision 0 counts is each caller's rule: the factory drops it, and the
// catalog keeps it.
func credentialObservation(observation *iam.ProviderAccountObservation) *CredentialObservation {
	if observation == nil {
		return nil
	}
	return &CredentialObservation{
		ConnectionID:       observation.ConnectionID,
		CredentialRevision: observation.CredentialRevision,
	}
}
