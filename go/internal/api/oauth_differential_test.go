package api

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"
)

// completionTwins are two gateway databases that start byte for byte the
// same: the gateway's old completion writes to one and the Service's to the
// other, each from the same settings.
type completionTwins struct {
	principal iam.Principal
	dirs      [2]string
	settings  config.Settings
	// lifetime, when set, is the lifetime a fixture token names. The
	// provider library counts it from its own clock, so each run stores its
	// own expiry, which the comparison reads as that lifetime from now.
	lifetime int64
}

var tenDigits = regexp.MustCompile(`\b\d{10}\b`)

// markExpiry replaces the expiries a token of lifetime gets around now.
func (c *completionTwins) markExpiry(text string) string {
	if c.lifetime == 0 {
		return text
	}
	around := time.Now().Unix() + c.lifetime
	return tenDigits.ReplaceAllStringFunc(text, func(number string) string {
		if value, _ := strconv.ParseInt(number, 10, 64); value >= around-60 && value <= around+60 {
			return "<expiry>"
		}
		return number
	})
}

// newCompletionTwins seeds a database whose owner already has a default
// connection to providerID and a recorded check of it, both of which a new
// connection displaces, and copies it.
func newCompletionTwins(t *testing.T, providerID, kind string) *completionTwins {
	t.Helper()
	dirs := [2]string{t.TempDir(), t.TempDir()}
	t.Setenv("LLMGW_STATE_DIR", dirs[0])
	iam.ResetForTests()
	t.Cleanup(iam.ResetForTests)
	if _, err := iam.Initialize(); err != nil {
		t.Fatal(err)
	}
	principal, err := iam.CreatePrincipal("human", "fixture:differential-owner", "", "Differential Owner")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := iam.PutOAuthProviderConnection(iam.OAuthConnectionCreate{
		PrincipalID: principal.ID, ProviderID: providerID, Name: "existing", Kind: kind,
		Source: iam.ConnectionSourceUser, MakeDefault: true, AccessToken: "fixture-existing-access",
		ExpiresAt: 1_900_000_000, Status: "active",
	}); err != nil {
		t.Fatal(err)
	}
	if err := iam.RecordProviderCheck(iam.ProviderCheck{
		ProviderID: providerID, Operation: iam.CheckReachability, ScopeKey: principal.ID,
		Success: true, CheckedAt: 1_800_000_000,
	}); err != nil {
		t.Fatal(err)
	}
	iam.ResetForTests()
	files, err := filepath.Glob(filepath.Join(dirs[0], "gateway.db*"))
	if err != nil || len(files) == 0 {
		t.Fatalf("seeded database files=%v err=%v", files, err)
	}
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dirs[1], filepath.Base(file)), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return &completionTwins{principal: principal, dirs: dirs, settings: *config.Get()}
}

// completionState is what one completion left behind and answered.
type completionState struct {
	rows      []string
	response  string
	providers string
	file      string
}

// run completes on twin index from the shared settings, and returns what the
// completion wrote.
func (c *completionTwins) run(t *testing.T, index int, complete func() map[string]any) completionState {
	t.Helper()
	t.Setenv("LLMGW_STATE_DIR", c.dirs[index])
	t.Setenv("LLMGW_CONFIG", filepath.Join(c.dirs[index], "config.yaml"))
	iam.ResetForTests()
	config.Update(func(settings *config.Settings) { *settings = c.settings })
	state := completionState{response: c.markExpiry(normalizedOAuthResponse(t, complete()))}
	for _, row := range dumpCompletionRows(t, c.principal) {
		state.rows = append(state.rows, c.markExpiry(row))
	}
	configured, _ := json.Marshal(config.Get().Providers)
	file, _ := os.ReadFile(config.ConfigFilePath())
	state.providers, state.file = string(configured), string(file)
	return state
}

// sameCompletion fails unless the old and the new completion wrote and
// answered the same, and answered status.
func sameCompletion(t *testing.T, old, next completionState, status string) {
	t.Helper()
	if strings.Join(old.rows, "\n") != strings.Join(next.rows, "\n") {
		t.Errorf("stored state differs\nold:\n%s\nnew:\n%s", strings.Join(old.rows, "\n"), strings.Join(next.rows, "\n"))
	}
	if old.response != next.response {
		t.Errorf("response differs\nold: %s\nnew: %s", old.response, next.response)
	}
	if old.providers != next.providers || old.file != next.file {
		t.Errorf("provider settings differ\nold: %s\n%s\nnew: %s\n%s", old.providers, old.file, next.providers, next.file)
	}
	if !strings.Contains(old.response, `"status":"`+status+`"`) {
		t.Errorf("the completions answered %s, want status %s", old.response, status)
	}
}

// oauthConnectionNames names each connection by what makes it unique, since
// a new connection's ID differs between any two runs.
func oauthConnectionNames(t *testing.T) map[string]string {
	t.Helper()
	db, err := iam.DB()
	if err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query("SELECT id, principal_id, provider_id, connection_name FROM provider_connections")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	names := map[string]string{}
	for rows.Next() {
		var id, principal, provider, name string
		if err := rows.Scan(&id, &principal, &provider, &name); err != nil {
			t.Fatal(err)
		}
		names[id] = "connection(" + principal + "/" + provider + "/" + name + ")"
	}
	return names
}

func normalizedOAuthResponse(t *testing.T, response map[string]any) string {
	t.Helper()
	raw, _ := json.Marshal(response)
	decoded := map[string]any{}
	_ = json.Unmarshal(raw, &decoded)
	if connection, ok := decoded["connection"].(map[string]any); ok {
		connection["id"] = oauthConnectionNames(t)[fmt.Sprint(connection["id"])]
		delete(connection, "created_at")
		delete(connection, "updated_at")
	}
	normalized, _ := json.Marshal(decoded)
	return string(normalized)
}

// sortedLines keeps a table's rows in a stable order.
func sortedLines(lines []string) []string {
	sort.Strings(lines)
	return lines
}
