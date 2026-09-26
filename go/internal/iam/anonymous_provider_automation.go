package iam

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

const anonymousProviderAutomationOverrideKey = "anonymous_provider_automation.override"

func AnonymousProviderManaged(providerID string) (bool, error) {
	db, err := DB()
	if err != nil {
		return false, err
	}
	key := "anonymous_provider_automation.managed." + strings.TrimSpace(providerID)
	var count int
	err = db.QueryRow(`
SELECT COUNT(*) FROM control_metadata WHERE key=?
`, key).Scan(&count)
	return count > 0, err
}

func MarkAnonymousProviderManaged(providerID string) error {
	db, err := DB()
	if err != nil {
		return err
	}
	key := "anonymous_provider_automation.managed." + strings.TrimSpace(providerID)
	_, err = db.Exec(`
INSERT INTO control_metadata(key,value,updated_at) VALUES(?,?,?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`,
		key, "true", time.Now().Unix())
	return err
}

func ClearAnonymousProviderManaged(providerID string) error {
	db, err := DB()
	if err != nil {
		return err
	}
	key := "anonymous_provider_automation.managed." + strings.TrimSpace(providerID)
	_, err = db.Exec("DELETE FROM control_metadata WHERE key=?", key)
	return err
}

func AnonymousProviderAutomationOverride() (string, bool, error) {
	db, err := DB()
	if err != nil {
		return "", false, err
	}
	var value string
	err = db.QueryRow(
		"SELECT value FROM control_metadata WHERE key=?",
		anonymousProviderAutomationOverrideKey,
	).Scan(&value)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	value = strings.ToLower(strings.TrimSpace(value))
	if value != "on" && value != "off" {
		return "", false, fmt.Errorf("invalid anonymous provider automation override")
	}
	return value, true, nil
}

func SetAnonymousProviderAutomationOverride(value string) error {
	value = strings.ToLower(strings.TrimSpace(value))
	db, err := DB()
	if err != nil {
		return err
	}
	if value == "inherit" {
		_, err = db.Exec("DELETE FROM control_metadata WHERE key=?", anonymousProviderAutomationOverrideKey)
		return err
	}
	if value != "on" && value != "off" {
		return fmt.Errorf("override must be one of inherit, on, or off")
	}
	_, err = db.Exec(`
INSERT INTO control_metadata(key,value,updated_at) VALUES(?,?,?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`,
		anonymousProviderAutomationOverrideKey, value, time.Now().Unix(),
	)
	return err
}

// ClaimAnonymousProviderCheck atomically limits automatic network checks across
// process restarts. A failed check still consumes the interval to avoid hammering
// a degraded public service.
func ClaimAnonymousProviderCheck(providerID string, now time.Time, interval time.Duration) (bool, error) {
	providerID = strings.TrimSpace(providerID)
	if providerID == "" || interval <= 0 {
		return false, fmt.Errorf("provider id and positive interval are required")
	}
	db, err := DB()
	if err != nil {
		return false, err
	}
	key := "anonymous_provider_automation.check." + providerID
	result, err := db.Exec(`
INSERT INTO control_metadata(key,value,updated_at) VALUES(?,?,?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at
WHERE control_metadata.updated_at<=?`,
		key, "claimed", now.Unix(), now.Add(-interval).Unix(),
	)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	return changed == 1, err
}
