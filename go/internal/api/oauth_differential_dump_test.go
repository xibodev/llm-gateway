package api

import (
	"fmt"
	"strings"
	"testing"

	"llmgw/internal/iam"
)

// uncomparedColumns differ between any two runs: write times, and the
// ciphertext, whose nonce is fresh each time. Envelopes are compared
// decrypted instead.
var uncomparedColumns = map[string]bool{"created_at": true, "updated_at": true, "ciphertext": true, "nonce": true}

// dumpCompletionRows renders every row of every table, connection IDs
// replaced by the owner, provider and name they stand for, followed by each
// active connection's decrypted envelope and the stored consumer_manual
// client.
func dumpCompletionRows(t *testing.T, principal iam.Principal) []string {
	t.Helper()
	db, err := iam.DB()
	if err != nil {
		t.Fatal(err)
	}
	names := oauthConnectionNames(t)
	tables, err := db.Query("SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name!='schema_migrations' ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	var tableNames []string
	for tables.Next() {
		var name string
		if err := tables.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tableNames = append(tableNames, name)
	}
	_ = tables.Close()
	var out []string
	for _, table := range tableNames {
		rows, err := db.Query("SELECT * FROM " + table)
		if err != nil {
			t.Fatal(err)
		}
		columns, _ := rows.Columns()
		var lines []string
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for index := range values {
				pointers[index] = &values[index]
			}
			if err := rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			fields := []string{table + ":"}
			for index, column := range columns {
				if uncomparedColumns[column] {
					continue
				}
				value := fmt.Sprint(values[index])
				if raw, ok := values[index].([]byte); ok {
					value = string(raw)
				}
				if name, ok := names[value]; ok {
					value = name
				}
				fields = append(fields, column+"="+value)
			}
			lines = append(lines, strings.Join(fields, " "))
		}
		_ = rows.Close()
		out = append(out, sortedLines(lines)...)
	}
	connections, err := iam.ListProviderConnections(principal.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, connection := range connections {
		if connection.Status != "active" {
			continue
		}
		envelope, _, ok, err := iam.OAuthProviderConnectionSecret(principal.ID, connection.ProviderID, connection.Name)
		out = append(out, fmt.Sprintf("envelope %s/%s ok=%v err=%v: %+v", connection.ProviderID, connection.Name, ok, err, envelope))
	}
	profile, found, err := iam.OAuthClientProfileByName("google_antigravity", antigravityManualProfile)
	return append(out, fmt.Sprintf("consumer_manual client found=%v err=%v: %+v", found, err, profile))
}
