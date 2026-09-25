package providers

import (
	"context"
	"testing"

	core "github.com/xibodev/llmgw-core"
)

func TestCredentialEvidenceReachesOnlyItsCollector(t *testing.T) {
	sink := credentialEvidence{}
	evidence := core.AccountEvidence{CredentialKey: "conn_fixture", CredentialRevision: "7"}
	// Without a collector the evidence goes nowhere, and recording it must
	// not fail.
	sink.Record(context.Background(), evidence)

	ctx, collector := collectCredentials(context.Background())
	if collector.Observation() != nil {
		t.Fatal("a new collector reported an observation")
	}
	sink.Record(ctx, evidence)
	sink.Record(ctx, core.AccountEvidence{CredentialRevision: "9"})
	observation := collector.Observation()
	if observation == nil || *observation != (CredentialObservation{ConnectionID: "conn_fixture", CredentialRevision: 7}) {
		t.Fatalf("observation=%+v, want the keyed evidence", observation)
	}
	// The evidence of a later refresh replaces the earlier revision.
	sink.Record(ctx, core.AccountEvidence{CredentialKey: "conn_fixture", CredentialRevision: "8"})
	if observation.CredentialRevision != 7 || collector.Observation().CredentialRevision != 8 {
		t.Fatalf("observation=%+v later=%+v", observation, collector.Observation())
	}
	inner, nested := collectCredentials(ctx)
	sink.Record(inner, core.AccountEvidence{CredentialKey: "conn_other", CredentialRevision: "not-a-number"})
	if got := nested.Observation(); got == nil || got.ConnectionID != "conn_other" || got.CredentialRevision != 0 {
		t.Fatalf("nested observation=%+v", got)
	}
	if collector.Observation().ConnectionID != "conn_fixture" {
		t.Fatal("a nested collector's evidence reached the outer one")
	}
}
