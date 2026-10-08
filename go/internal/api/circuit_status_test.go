package api

import (
	"net/http"
	"testing"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"
	"llmgw/internal/providers"
)

// A provider instance's status lists its open circuits with the callers each
// refuses: every caller's for an administrator, a user's own for the user.
func TestProviderStatusListsOpenCircuits(t *testing.T) {
	handler := setupAPISmokeFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": map[string]any{"message": "overloaded"}})
	}))
	config.Update(func(s *config.Settings) {
		s.Policies.Defaults.CircuitFailureThreshold = 1
		s.Policies.Defaults.CircuitCooldownSeconds = 60
	})
	providers.ResetProviders()
	t.Cleanup(func() { providers.ResetCircuit("fixture") })
	chat := map[string]any{"model": "fixture/model", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	if w := apiSmokeRequest(handler, "/v1/chat/completions", chat); w.Code < 500 {
		t.Fatalf("administrator request: status=%d body=%s", w.Code, w.Body.String())
	}
	token := admissionFixtureKey(t, "circuit-owner", iam.KeyPolicy{})
	owner, _, err := iam.ResolveAPIKey(token)
	if err != nil {
		t.Fatal(err)
	}
	if w := admissionRequest(handler, "/v1/chat/completions", token, nil, chat); w.Code < 500 {
		t.Fatalf("key request: status=%d body=%s", w.Code, w.Body.String())
	}
	openCircuits := func(scope string) []providers.OpenCircuit {
		t.Helper()
		rows, err := providerStatusSnapshots(config.Get(), nil, nil, scope)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			for _, instance := range row["instances"].([]map[string]any) {
				if instance["id"] == "fixture" {
					open, _ := instance["open_circuits"].([]providers.OpenCircuit)
					return open
				}
			}
		}
		t.Fatal("no fixture instance in the provider status")
		return nil
	}
	all := openCircuits("")
	if len(all) != 2 || all[0].PrincipalID != "" || all[1].PrincipalID != owner.PrincipalID ||
		all[0].Failures != 1 || !all[0].OpenUntil.After(time.Now()) {
		t.Fatalf("administrator's view: %+v", all)
	}
	if own := openCircuits(owner.PrincipalID); len(own) != 1 || own[0].PrincipalID != owner.PrincipalID {
		t.Fatalf("the owner's view: %+v", own)
	}
	if other := openCircuits("prn_someone_else"); len(other) != 0 {
		t.Fatalf("another user's view: %+v", other)
	}
}
