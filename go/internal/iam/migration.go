package iam

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"llmgw/internal/config"
)

const legacyKeysMigration = "legacy_keys_json_v1"

type legacyKeyEntry struct {
	Project          string   `json:"project"`
	Name             string   `json:"name"`
	Created          int64    `json:"created"`
	Disabled         bool     `json:"disabled"`
	ExpiresAt        int64    `json:"expires_at"`
	AllowedModels    []string `json:"allowed_models"`
	AllowedProviders []string `json:"allowed_providers"`
	RPM              int      `json:"rpm"`
	DailyRequests    int      `json:"daily_requests"`
}

type MigrationResult struct {
	Keys              int `json:"keys"`
	Projects          int `json:"projects"`
	Principals        int `json:"principals"`
	UsageEvents       int `json:"usage_events"`
	SystemConnections int `json:"system_connections"`
	ProviderAccounts  int `json:"provider_accounts"`
}

// Initialize opens the control-plane database and imports any legacy plaintext
// keys.json. The migration is transactional and removes the plaintext file only
// after the database commit succeeds.
func Initialize() (MigrationResult, error) {
	if _, err := DB(); err != nil {
		return MigrationResult{}, err
	}
	result, err := MigrateLegacyKeys()
	if err != nil {
		return result, err
	}
	usage, err := MigrateLegacyUsage()
	result.UsageEvents = usage
	if err != nil {
		return result, err
	}
	result.SystemConnections, err = SeedSystemProviderConnectionsFromConfig()
	if err != nil {
		return result, err
	}
	result.ProviderAccounts, err = BackfillProviderAccountOAuthMetadata()
	return result, err
}

func MigrateLegacyKeys() (MigrationResult, error) {
	path := filepath.Join(config.StateDir(), "keys.json")
	db, err := DB()
	if err != nil {
		return MigrationResult{}, err
	}
	var done int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM control_metadata WHERE key=?", legacyKeysMigration,
	).Scan(&done); err != nil {
		return MigrationResult{}, err
	}
	if done != 0 {
		// A prior successful transaction may have been unable to remove the file.
		// Do not parse it again: a partial/corrupt leftover cannot brick startup.
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return MigrationResult{}, fmt.Errorf("remove migrated plaintext keys: %w", err)
		}
		return MigrationResult{}, nil
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return MigrationResult{}, nil
	}
	if err != nil {
		return MigrationResult{}, fmt.Errorf("read legacy keys: %w", err)
	}
	legacy := map[string]legacyKeyEntry{}
	if err := json.Unmarshal(raw, &legacy); err != nil {
		return MigrationResult{}, fmt.Errorf("decode legacy keys: %w", err)
	}
	tx, err := db.Begin()
	if err != nil {
		return MigrationResult{}, err
	}
	result := MigrationResult{}
	tokens := make([]string, 0, len(legacy))
	for token := range legacy {
		tokens = append(tokens, token)
	}
	sort.Strings(tokens)
	for _, token := range tokens {
		entry := legacy[token]
		projectSlug := entry.Project
		if strings.TrimSpace(projectSlug) == "" {
			projectSlug = "default"
		}
		projectSlug, err = normalizeSlug(projectSlug)
		if err != nil {
			_ = tx.Rollback()
			return MigrationResult{}, fmt.Errorf("legacy project %q: %w", entry.Project, err)
		}
		projectID, created, err := ensureProjectTx(tx, projectSlug, projectSlug)
		if err != nil {
			_ = tx.Rollback()
			return MigrationResult{}, err
		}
		if created {
			result.Projects++
		}
		sum := sha256.Sum256([]byte(token))
		name := strings.TrimSpace(entry.Name)
		if name == "" {
			name = "key"
		}
		subject := "legacy-key:" + projectSlug + ":" + hex.EncodeToString(sum[:8])
		principalID, created, err := ensurePrincipalTx(tx, "service", subject, "", name)
		if err != nil {
			_ = tx.Rollback()
			return MigrationResult{}, err
		}
		if created {
			result.Principals++
		}
		if _, err := tx.Exec(`
INSERT INTO project_memberships(project_id,principal_id,role,created_at)
VALUES(?,?,?,?)
ON CONFLICT(project_id,principal_id) DO UPDATE SET role=excluded.role`,
			projectID, principalID, "member", time.Now().Unix(),
		); err != nil {
			_ = tx.Rollback()
			return MigrationResult{}, err
		}
		var exists int
		if err := tx.QueryRow(
			"SELECT COUNT(*) FROM api_keys WHERE secret_hash=?", sum[:],
		).Scan(&exists); err != nil {
			_ = tx.Rollback()
			return MigrationResult{}, err
		}
		if exists != 0 {
			continue
		}
		keyID, err := newID("key")
		if err != nil {
			_ = tx.Rollback()
			return MigrationResult{}, err
		}
		models, _ := json.Marshal(entry.AllowedModels)
		providers, _ := json.Marshal(entry.AllowedProviders)
		status := "active"
		if entry.Disabled {
			status = "disabled"
		}
		createdAt := entry.Created
		if createdAt == 0 {
			createdAt = time.Now().Unix()
		}
		_, err = tx.Exec(`
INSERT INTO api_keys(
    id,prefix,secret_hash,project_id,principal_id,name,status,created_at,expires_at,
    allowed_models_json,allowed_providers_json,rpm,daily_requests
) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			keyID, displayPrefix(token), sum[:], projectID, principalID, name, status,
			createdAt, nullInt(entry.ExpiresAt), string(models), string(providers),
			entry.RPM, entry.DailyRequests,
		)
		if err != nil {
			_ = tx.Rollback()
			return MigrationResult{}, err
		}
		result.Keys++
	}
	if _, err := tx.Exec(`
INSERT INTO control_metadata(key,value,updated_at) VALUES(?,?,?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`,
		legacyKeysMigration, "done", time.Now().Unix(),
	); err != nil {
		_ = tx.Rollback()
		return MigrationResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return MigrationResult{}, err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return result, fmt.Errorf("remove migrated plaintext keys: %w", err)
	}
	return result, nil
}

func ensureProjectTx(tx *sql.Tx, slug, name string) (string, bool, error) {
	var id string
	err := tx.QueryRow("SELECT id FROM projects WHERE slug=?", slug).Scan(&id)
	if err == nil {
		return id, false, nil
	}
	if err != sql.ErrNoRows {
		return "", false, err
	}
	id, err = newID("prj")
	if err != nil {
		return "", false, err
	}
	now := time.Now().Unix()
	_, err = tx.Exec(`
INSERT INTO projects(id,slug,name,status,created_at,updated_at)
VALUES(?,?,?,?,?,?)`, id, slug, name, "active", now, now)
	return id, err == nil, err
}

func ensurePrincipalTx(
	tx *sql.Tx, kind, subject, email, displayName string,
) (string, bool, error) {
	var id string
	err := tx.QueryRow("SELECT id FROM principals WHERE external_subject=?", subject).Scan(&id)
	if err == nil {
		return id, false, nil
	}
	if err != sql.ErrNoRows {
		return "", false, err
	}
	id, err = newID("prn")
	if err != nil {
		return "", false, err
	}
	now := time.Now().Unix()
	_, err = tx.Exec(`
INSERT INTO principals(id,kind,external_subject,email,display_name,status,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?)`,
		id, kind, subject, nullable(email), displayName, "active", now, now,
	)
	return id, err == nil, err
}
