package iam

import "testing"

func TestProviderModelEvidenceIsModelScopedAndGenerationFenced(t *testing.T) {
	setupConnectionTest(t)
	generation, err := ProviderCheckGeneration("zen", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, evidence := range []ProviderModelEvidence{
		{ProviderID: "zen", Model: "working", Operation: ModelEvidenceCompletion, State: "verified", Generation: generation, LatencyMS: 12},
		{ProviderID: "zen", Model: "broken", Operation: ModelEvidenceCompletion, State: "failed", FailureCode: "verification_failed", Generation: generation, LatencyMS: 19},
	} {
		if err := RecordProviderModelEvidence(evidence); err != nil {
			t.Fatal(err)
		}
	}
	models, err := ProviderModelEvidenceFor("zen", "")
	if err != nil || models["working"].State != "verified" || models["broken"].State != "failed" {
		t.Fatalf("models=%+v err=%v", models, err)
	}
	if err := InvalidateProviderChecks("zen"); err != nil {
		t.Fatal(err)
	}
	if err := RecordProviderModelEvidence(ProviderModelEvidence{
		ProviderID: "zen", Model: "stale", Operation: ModelEvidenceCompletion,
		State: "verified", Generation: generation,
	}); err == nil {
		t.Fatal("stale generation wrote model evidence")
	}
	models, err = ProviderModelEvidenceFor("zen", "")
	if err != nil || len(models) != 2 || models["working"].State != "stale" || models["broken"].State != "stale" {
		t.Fatalf("invalidation did not retain stale evidence: models=%+v err=%v", models, err)
	}
}

func TestProviderModelCatalogReconciliationStalesRemovedModelsWithoutVerifyingSiblings(t *testing.T) {
	setupConnectionTest(t)
	generation, err := ProviderCheckGeneration("zen", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := ReconcileProviderModelCatalog(
		"zen", "", ModelEvidenceCompletion, []string{"working", "sibling"}, generation,
	); err != nil {
		t.Fatal(err)
	}
	if err := RecordProviderModelEvidence(ProviderModelEvidence{
		ProviderID: "zen", Model: "working", Operation: ModelEvidenceCompletion,
		State: "verified", Generation: generation,
	}); err != nil {
		t.Fatal(err)
	}
	models, err := ProviderModelEvidenceFor("zen", "")
	if err != nil || models["working"].State != "verified" || models["sibling"].State != "unverified" {
		t.Fatalf("initial evidence=%+v err=%v", models, err)
	}
	if err := ReconcileProviderModelCatalog(
		"zen", "", ModelEvidenceCompletion, []string{"sibling"}, generation,
	); err != nil {
		t.Fatal(err)
	}
	models, err = ProviderModelEvidenceFor("zen", "")
	if err != nil || models["working"].State != "stale" || models["sibling"].State != "unverified" {
		t.Fatalf("reconciled evidence=%+v err=%v", models, err)
	}
}

func TestProviderModelCatalogReconciliationClearsCurrentVerifiedBeforeProbes(t *testing.T) {
	setupConnectionTest(t)
	generation, err := ProviderCheckGeneration("zen", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := RecordProviderModelEvidence(ProviderModelEvidence{
		ProviderID: "zen", Model: "working", Operation: ModelEvidenceCompletion,
		State: "verified", Generation: generation,
	}); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileProviderModelCatalog(
		"zen", "", ModelEvidenceCompletion, []string{"working", "new"}, generation,
	); err != nil {
		t.Fatal(err)
	}
	models, err := ProviderModelEvidenceFor("zen", "")
	if err != nil || models["working"].State != "unverified" ||
		models["new"].State != "unverified" {
		t.Fatalf("prepared evidence=%+v err=%v", models, err)
	}
}

func TestBeginProviderModelProbesRejectsStaleGeneration(t *testing.T) {
	setupConnectionTest(t)
	generation, err := ProviderCheckGeneration("zen", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := InvalidateProviderChecks("zen"); err != nil {
		t.Fatal(err)
	}
	if err := BeginProviderModelProbes(
		"zen", "", ModelEvidenceCompletion, []string{"working"}, generation,
	); err == nil {
		t.Fatal("stale generation prepared model evidence")
	}
}
