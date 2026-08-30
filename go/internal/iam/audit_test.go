package iam

import (
	"encoding/json"
	"strings"
	"testing"

	"llmgw/internal/diagnostics"
)

func TestAuditDetailSanitizesPersistenceAndHistoricalRows(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	ResetForTests()
	t.Cleanup(ResetForTests)
	if _, err := Initialize(); err != nil {
		t.Fatal(err)
	}
	db, err := DB()
	if err != nil {
		t.Fatal(err)
	}
	principal, err := CreatePrincipal("human", "authentik:audit-owner", "", "Owner")
	if err != nil {
		t.Fatal(err)
	}
	secret := "llmgw_" + strings.Repeat("b", 32)
	model := "anthropic/model-ThisIdentifierSegmentIsLongEnough"
	voice := "en-US-EmmaMultilingualNeural"
	provider := "provider-ThisProviderIdentifierSegmentIsVeryLong"
	detail := map[string]any{
		"authorization":   "short-token",
		"model":           model,
		"served_model":    model,
		"voice":           voice,
		"provider":        provider,
		"served_provider": provider,
		"endpoint":        secret,
		"operation":       "gsk_abcdefghijklmnop",
		"route_id":        "token=query-secret",
		"unsafe_identifiers": map[string]any{
			"model":           secret,
			"voice":           "owner@example.test",
			"served_model":    secret,
			"served_provider": "gsk_abcdefghijklmnop",
		},
		"context": map[string]any{
			"message": "owner@example.test used " + secret,
			"count":   7,
		},
		"items": []any{"safe", map[string]any{"api_key": "tiny"}},
	}
	if err := RecordAudit(AuditEvent{
		Timestamp: 111, ActorPrincipalID: principal.ID, Action: "provider.verify",
		TargetType: "provider", TargetID: "fixture", Result: "denied", Detail: detail,
	}); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := db.QueryRow(`SELECT detail_json FROM audit_events
		WHERE action='provider.verify'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, secret) || strings.Contains(raw, "owner@example.test") ||
		strings.Contains(raw, "short-token") || strings.Contains(raw, "tiny") {
		t.Fatalf("unsafe audit detail persisted: %s", raw)
	}
	var persisted map[string]any
	if err := json.Unmarshal([]byte(raw), &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted["authorization"] != diagnostics.Redacted ||
		persisted["model"] != model || persisted["served_model"] != model ||
		persisted["voice"] != voice || persisted["provider"] != provider ||
		persisted["served_provider"] != provider || persisted["endpoint"] != diagnostics.Redacted ||
		persisted["operation"] != diagnostics.Redacted ||
		persisted["route_id"] != "token="+diagnostics.Redacted ||
		persisted["unsafe_identifiers"].(map[string]any)["model"] != diagnostics.Redacted ||
		persisted["unsafe_identifiers"].(map[string]any)["voice"] != diagnostics.Redacted ||
		persisted["unsafe_identifiers"].(map[string]any)["served_model"] != diagnostics.Redacted ||
		persisted["unsafe_identifiers"].(map[string]any)["served_provider"] != diagnostics.Redacted ||
		persisted["context"].(map[string]any)["count"] != float64(7) {
		t.Fatalf("audit structure changed: %#v", persisted)
	}
	if detail["model"] != model || detail["voice"] != voice || detail["endpoint"] != secret {
		t.Fatalf("audit input was mutated: %#v", detail)
	}

	historical := `{"nested":{"password":"short","message":"historical@example.test"},"ok":true}`
	if _, err := db.Exec(`INSERT INTO audit_events(
		ts,actor_principal_id,action,target_type,target_id,result,detail_json
	) VALUES(?,?,?,?,?,?,?)`, 222, principal.ID, "historical.valid", "provider", "fixture", "failure", historical); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO audit_events(
		ts,actor_principal_id,action,result,detail_json
	) VALUES(?,?,?,?,?)`, 333, principal.ID, "historical.malformed", "failure", `{"password":"leaked"`); err != nil {
		t.Fatal(err)
	}
	for name, list := range map[string]func() ([]AuditEvent, error){
		"admin":     func() ([]AuditEvent, error) { return ListAudit(10) },
		"principal": func() ([]AuditEvent, error) { return ListPrincipalAudit(principal.ID, 10) },
	} {
		events, err := list()
		if err != nil {
			t.Fatalf("%s list: %v", name, err)
		}
		if len(events) != 3 {
			t.Fatalf("%s events=%+v", name, events)
		}
		byAction := map[string]AuditEvent{}
		for _, event := range events {
			byAction[event.Action] = event
		}
		valid := byAction["historical.valid"]
		nested, ok := valid.Detail["nested"].(map[string]any)
		if !ok || nested["password"] != diagnostics.Redacted ||
			nested["message"] != diagnostics.Redacted || valid.Detail["ok"] != true ||
			valid.Result != "failure" || valid.Timestamp != 222 {
			t.Fatalf("%s historical event unsafe or changed: %+v", name, valid)
		}
		if malformed := byAction["historical.malformed"]; len(malformed.Detail) != 0 ||
			malformed.Action != "historical.malformed" || malformed.Result != "failure" {
			t.Fatalf("%s malformed event=%+v", name, malformed)
		}
		written := byAction["provider.verify"]
		if written.Detail["model"] != model || written.Detail["served_model"] != model ||
			written.Detail["voice"] != voice || written.Detail["provider"] != provider ||
			written.Detail["served_provider"] != provider || written.Detail["endpoint"] != diagnostics.Redacted ||
			written.Detail["operation"] != diagnostics.Redacted ||
			written.Detail["route_id"] != "token="+diagnostics.Redacted ||
			written.Detail["unsafe_identifiers"].(map[string]any)["model"] != diagnostics.Redacted ||
			written.Detail["unsafe_identifiers"].(map[string]any)["voice"] != diagnostics.Redacted ||
			written.Detail["unsafe_identifiers"].(map[string]any)["served_model"] != diagnostics.Redacted ||
			written.Detail["unsafe_identifiers"].(map[string]any)["served_provider"] != diagnostics.Redacted {
			t.Fatalf("%s functional fields unsafe or changed: %+v", name, written)
		}
	}
}
