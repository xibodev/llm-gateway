package providers

import (
	"testing"

	"llmgw/internal/iam"
)

func TestCredentialObservationConversion(t *testing.T) {
	// System, configured and bound credentials arrive as nil and must stay
	// nil: callers take any non-nil observation as the credential that served.
	if got := credentialObservation(nil); got != nil {
		t.Fatalf("nil converted to %+v", got)
	}
	// Codex relies on revision 0 surviving; the factory drops it itself.
	unstored := iam.ProviderAccountObservation{ConnectionID: "conn-fixture"}
	if got := credentialObservation(&unstored); got == nil ||
		*got != (CredentialObservation{ConnectionID: "conn-fixture"}) {
		t.Fatalf("revision 0 converted to %+v", got)
	}
	source := iam.ProviderAccountObservation{ConnectionID: "conn-fixture", CredentialRevision: 7}
	got := credentialObservation(&source)
	back := iam.ProviderAccountObservation{
		ConnectionID: got.ConnectionID, CredentialRevision: got.CredentialRevision,
	}
	if back != source {
		t.Fatalf("round trip = %+v, want %+v", back, source)
	}
}
