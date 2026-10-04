package iam

import (
	"strings"
	"testing"
	"time"
)

// A request ID names one usage record, but a client chooses its own and can
// send one twice. The repeat is recorded under an ID of its own, and what it
// consumed still counts against its key and project.
func TestARepeatedRequestIDIsRecordedUnderAnIDOfItsOwn(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	ResetForTests()
	t.Cleanup(ResetForTests)
	p, _ := quotaPrincipal(t, KeyPolicy{})
	now := time.Date(2026, 7, 10, 1, 2, 3, 0, time.UTC).Unix()
	for range 2 {
		if err := RecordUsageEvent(UsageEvent{
			RequestID: "client-retry.1", Timestamp: now, Endpoint: "openai.chat", StatusCode: 200,
			ProjectID: p.ProjectID, PrincipalID: p.PrincipalID, KeyID: p.KeyID, InputTokens: 10,
		}); err != nil {
			t.Fatal(err)
		}
	}
	db, err := DB()
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`SELECT request_id FROM usage_events ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != "client-retry.1" || !strings.HasPrefix(ids[1], "req_") {
		t.Fatalf("request IDs %q, want the first as sent and the repeat's own", ids)
	}
	var keyInput, projectInput int64
	if err := db.QueryRow(`SELECT input_tokens FROM quota_counters WHERE key_id=? AND period='day'`, p.KeyID).Scan(&keyInput); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT input_tokens FROM project_quota_counters WHERE project_id=? AND period='day'`, p.ProjectID).Scan(&projectInput); err != nil {
		t.Fatal(err)
	}
	if keyInput != 20 || projectInput != 20 {
		t.Fatalf("daily input tokens: key=%d project=%d, want both requests' 20", keyInput, projectInput)
	}
}
