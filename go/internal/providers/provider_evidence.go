package providers

// ProviderEvidenceState keeps discovery, credential acceptance, and inference
// evidence separate. A catalog response is never completion evidence.
type ProviderEvidenceState struct {
	Authentication string
	Catalog        string
	Completion     string
}

func ClassifyProviderEvidence(catalogErr error, modelCount int, completionAttempted, completionSucceeded bool) ProviderEvidenceState {
	state := ProviderEvidenceState{
		Authentication: "unknown",
		Catalog:        "failed",
		Completion:     "not_probed",
	}
	if catalogErr == nil {
		state.Authentication = "accepted"
		state.Catalog = "empty"
		if modelCount > 0 {
			state.Catalog = "discovered"
		}
	} else {
		code, _, status := CatalogFailure(catalogErr)
		if code == "catalog_authentication_failed" || code == "catalog_refresh_failed" || status == 401 || status == 403 {
			state.Authentication = "rejected"
		}
	}
	if completionAttempted {
		state.Completion = "failed"
		if completionSucceeded {
			state.Completion = "verified"
		}
	}
	return state
}
