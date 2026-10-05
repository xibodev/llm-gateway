package api

import (
	"bytes"
	"log"
	"net/http"
	"os"
	"strings"
	"testing"

	"llmgw/internal/config"
)

// A static administrator key on /v1 still works by default, and the first
// use of each key is logged under its fingerprint so an operator can find
// the client; LLMGW_ADMIN_KEYS_ON_DATA_PLANE=false refuses it there and
// leaves the administrator API open to it.
func TestStaticAdministratorKeysOnTheDataPlane(t *testing.T) {
	handler := setupAPISmokeFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"choices": []any{map[string]any{
			"message": map[string]any{"role": "assistant", "content": "ok"}, "finish_reason": "stop",
		}}})
	}))
	if !config.Defaults().AdminKeysOnDataPlane {
		t.Fatal("static keys are refused on the data plane by default")
	}
	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	staticKeysWarned.Clear()

	chat := map[string]any{"model": "fixture/model", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	for range 2 {
		if w := apiSmokeRequest(handler, "/v1/chat/completions", chat); w.Code != http.StatusOK {
			t.Fatalf("allowed: status=%d body=%s", w.Code, w.Body.String())
		}
	}
	fingerprint := staticKeyFingerprint("fixture-gateway-token")
	if strings.Count(logged.String(), fingerprint) != 1 || strings.Contains(logged.String(), "fixture-gateway-token") ||
		!strings.Contains(logged.String(), "LLMGW_ADMIN_KEYS_ON_DATA_PLANE=false") {
		t.Fatalf("log = %q, want one warning naming the key's fingerprint and never the key", logged.String())
	}

	config.Update(func(s *config.Settings) { s.AdminKeysOnDataPlane = false })
	w := apiSmokeRequest(handler, "/v1/chat/completions", chat)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "gateway-issued project key") {
		t.Fatalf("denied: status=%d body=%s", w.Code, w.Body.String())
	}
	w = apiSmokeRequest(handler, "/v1/messages", map[string]any{"model": "fixture/model", "max_tokens": 8, "messages": chat["messages"]})
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"type":"permission_error"`) {
		t.Fatalf("denied messages: status=%d body=%s", w.Code, w.Body.String())
	}
	if w := adminGet(handler, "/admin/api/state"); w.Code != http.StatusOK {
		t.Fatalf("the administrator API refused its key: status=%d", w.Code)
	}
}
