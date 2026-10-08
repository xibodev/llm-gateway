package api

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
)

// The console builds its provider lists from the administrator's state, so
// the state lists providers by ID, the same from one request to the next.
func TestAdminStateListsProvidersByID(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	old := *config.Get()
	t.Cleanup(func() { config.Update(func(s *config.Settings) { *s = old }) })
	ids := []string{"delta", "alpha", "echo", "charlie", "bravo", "foxtrot", "golf", "hotel"}
	config.Update(func(s *config.Settings) {
		s.APIKey, s.AllowUnauthenticatedAPI = "admin-secret", false
		s.Providers = map[string]*config.ProviderConfig{}
		for _, id := range ids {
			s.Providers[id] = &config.ProviderConfig{Type: "echo"}
		}
	})
	server := httptest.NewServer(NewServer(Runtime{}))
	defer server.Close()
	want := slices.Sorted(slices.Values(ids))
	for range 5 {
		status, state := jsonRequest(t, server.URL+"/admin/api/state", http.MethodGet, "admin-secret", nil)
		if status != http.StatusOK {
			t.Fatalf("state status=%d payload=%+v", status, state)
		}
		got := []string{}
		for _, provider := range state["providers"].([]any) {
			got = append(got, provider.(map[string]any)["id"].(string))
		}
		if !slices.Equal(got, want) {
			t.Fatalf("providers=%v, want %v", got, want)
		}
	}
}
