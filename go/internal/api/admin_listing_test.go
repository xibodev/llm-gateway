package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"llmgw/internal/iam"
)

func adminGet(handler http.Handler, path string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Header.Set("Authorization", "Bearer fixture-gateway-token")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, request)
	return w
}

// The request listing finds a request by the ID its response carried, and
// refuses filters it cannot apply.
func TestAdminRequestsListRecordedRequests(t *testing.T) {
	handler, _ := observedFixture(t)
	chat := apiSmokeRequest(handler, "/v1/chat/completions", map[string]any{
		"model": "fixture/model", "messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if chat.Code != http.StatusOK {
		t.Fatalf("chat status=%d body=%s", chat.Code, chat.Body.String())
	}
	id := chat.Header().Get("X-Request-Id")
	w := adminGet(handler, "/admin/api/requests?request_id="+id)
	var listing struct {
		Requests []iam.RecordedRequest `json:"requests"`
		Next     *int64                `json:"next_before_id"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &listing) != nil || listing.Next == nil ||
		len(listing.Requests) != 1 || listing.Requests[0].Provider != "fixture" || listing.Requests[0].StatusCode != 200 ||
		listing.Requests[0].InputTokens != 11 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	for _, query := range []string{"status=sometimes", "from=20&to=10", "before_id=-1"} {
		if w := adminGet(handler, "/admin/api/requests?"+query); w.Code != http.StatusBadRequest {
			t.Fatalf("%s: status=%d body=%s", query, w.Code, w.Body.String())
		}
	}
	request := httptest.NewRequest(http.MethodGet, "/admin/api/requests", nil)
	unauthenticated := httptest.NewRecorder()
	handler.ServeHTTP(unauthenticated, request)
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("without a key: status=%d", unauthenticated.Code)
	}
}

func TestAdminAuditPagesItsEvents(t *testing.T) {
	handler, _ := observedFixture(t)
	for _, action := range []string{"provider.update", "provider.delete", "key.issue"} {
		if err := iam.RecordAudit(iam.AuditEvent{Action: action, TargetType: "fixture"}); err != nil {
			t.Fatal(err)
		}
	}
	var page struct {
		Events []iam.AuditEvent `json:"events"`
		Next   int64            `json:"next_before_id"`
	}
	w := adminGet(handler, "/admin/api/audit?action=provider.&limit=1")
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Events) != 1 ||
		page.Events[0].Action != "provider.delete" || page.Next == 0 {
		t.Fatalf("first page: status=%d body=%s", w.Code, w.Body.String())
	}
	w = adminGet(handler, "/admin/api/audit?action=provider.&limit=1&before_id="+jsonNumber(page.Next))
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Events) != 1 ||
		page.Events[0].Action != "provider.update" || page.Next != 0 {
		t.Fatalf("second page: status=%d body=%s", w.Code, w.Body.String())
	}
	if w := adminGet(handler, "/admin/api/audit?from=x"); w.Code != http.StatusBadRequest {
		t.Fatalf("a malformed time: status=%d", w.Code)
	}
}

func jsonNumber(value int64) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}
