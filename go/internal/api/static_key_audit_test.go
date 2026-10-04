package api

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"llmgw/internal/config"
	"llmgw/internal/iam"
)

func TestAuditTellsStaticAdministratorKeysApartWithoutStoringThem(t *testing.T) {
	t.Setenv("LLMGW_STATE_DIR", t.TempDir())
	iam.ResetForTests()
	old := *config.Get()
	t.Cleanup(func() {
		config.Update(func(s *config.Settings) { *s = old })
		iam.ResetForTests()
	})
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	keys := map[string]string{"first": "fixture-admin-first", "second": "fixture-admin-second"}
	config.Update(func(s *config.Settings) {
		s.APIKey, s.APIKeys, s.AllowUnauthenticatedAPI = keys["first"], []string{keys["second"]}, false
		s.SSOEnabled = false
	})
	server := httptest.NewServer(NewServer(Runtime{}))
	defer server.Close()
	for slug, key := range keys {
		status, body := jsonRequest(t, server.URL+"/admin/api/projects", http.MethodPost, key, map[string]any{"slug": slug, "name": slug})
		if status != http.StatusCreated {
			t.Fatalf("create %s: status=%d body=%v", slug, status, body)
		}
	}

	status, audit := jsonRequest(t, server.URL+"/admin/api/audit", http.MethodGet, keys["first"], nil)
	if status != http.StatusOK {
		t.Fatalf("audit: status=%d", status)
	}
	events, _ := audit["events"].([]any)
	seen := map[string]string{}
	for _, raw := range events {
		event, _ := raw.(map[string]any)
		detail, _ := event["detail"].(map[string]any)
		if event["action"] != "project.create" {
			continue
		}
		slug, _ := detail["slug"].(string)
		fingerprint, _ := detail["actor_key_fingerprint"].(string)
		seen[slug] = fingerprint
	}
	for slug, key := range keys {
		sum := sha256.Sum256([]byte(key))
		if want := hex.EncodeToString(sum[:])[:12]; seen[slug] != want {
			t.Errorf("%s: actor_key_fingerprint=%q, want %q", slug, seen[slug], want)
		}
	}
	if seen["first"] == seen["second"] {
		t.Fatalf("both keys recorded the same fingerprint %q", seen["first"])
	}

	db, err := iam.DB()
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query("SELECT detail_json FROM audit_events")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var detail string
		if err := rows.Scan(&detail); err != nil {
			t.Fatal(err)
		}
		for _, key := range keys {
			if strings.Contains(detail, key) {
				t.Fatal("an audit record stores a static administrator key")
			}
		}
	}
}

func TestStartupWarningCountsAdditionalStaticKeysWithoutNamingThem(t *testing.T) {
	old := *config.Get()
	t.Cleanup(func() { config.Update(func(s *config.Settings) { *s = old }) })
	keys := []string{"fixture-admin", "fixture-extra-one", "fixture-extra-two"}
	config.Update(func(s *config.Settings) {
		s.AllowUnauthenticatedAPI = false
		s.APIKey, s.APIKeys = keys[0], []string{keys[1], "", keys[2]}
	})
	warnings := StartupWarnings("127.0.0.1")
	if len(warnings) != 1 || !strings.Contains(warnings[0], "LLMGW_API_KEYS holds 2 static key") ||
		!strings.Contains(warnings[0], "project keys") {
		t.Fatalf("warnings=%q, want one that counts the two additional keys", warnings)
	}
	for _, key := range keys {
		if strings.Contains(warnings[0], key) {
			t.Fatal("the startup warning names a static administrator key")
		}
	}
	config.Update(func(s *config.Settings) { s.APIKeys = nil })
	if warnings := StartupWarnings("127.0.0.1"); len(warnings) != 0 {
		t.Fatalf("LLMGW_API_KEY alone: warnings=%q, want none", warnings)
	}
}
