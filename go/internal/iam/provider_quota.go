package iam

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
)

var quotaIdentifierPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._:-]*$`)
var quotaCredentialValuePattern = regexp.MustCompile(
	`(?i)(bearer\s+\S+|sk-[a-z0-9_-]{8,}|gh[pousr]_[a-z0-9]{8,}|github_pat_[a-z0-9_]{8,}|eyj[a-z0-9_-]{10,}\.[a-z0-9_-]{10,}\.)`,
)

var providerQuotaMetadataAllowlist = map[string]bool{
	"category": true, "currency": true, "display_label": true, "plan": true,
	"quota_id": true, "region": true, "reset_label": true, "resource": true, "tier": true,
}

const ProviderQuotaDefaultFreshness = 15 * time.Minute
const ProviderQuotaMaxClockSkew = 5 * time.Minute

var ErrProviderQuotaCredentialChanged = errors.New("provider credential changed during quota refresh")

// ProviderQuotaSnapshot is one verified/manual upstream quota dimension for a
// provider account. Nil numeric pointers mean the upstream did not supply that
// value; unknown values are never converted to zero.
type ProviderQuotaSnapshot struct {
	ID                 string         `json:"id"`
	ConnectionID       string         `json:"connection_id"`
	CredentialRevision int64          `json:"credential_revision"`
	PrincipalID        string         `json:"principal_id,omitempty"`
	ProviderID         string         `json:"provider_id,omitempty"`
	Metric             string         `json:"metric"`
	Unit               string         `json:"unit"`
	Window             string         `json:"window"`
	Label              string         `json:"label,omitempty"`
	LimitValue         *float64       `json:"limit,omitempty"`
	UsedValue          *float64       `json:"used,omitempty"`
	RemainingValue     *float64       `json:"remaining,omitempty"`
	ResetAt            int64          `json:"reset_at,omitempty"`
	Source             string         `json:"source"`
	Confidence         string         `json:"confidence"`
	RefreshedAt        int64          `json:"refreshed_at"`
	ExpiresAt          int64          `json:"expires_at,omitempty"`
	Metadata           map[string]any `json:"metadata,omitempty"`
}

type ProviderQuotaFilter struct {
	ConnectionID string
	PrincipalID  string
	ProviderID   string
	FreshOnly    bool
	Now          int64
}

func ProviderQuotaSnapshotIsFresh(snapshot ProviderQuotaSnapshot, now int64) bool {
	if now == 0 {
		now = time.Now().Unix()
	}
	if snapshot.ExpiresAt > 0 {
		return snapshot.RefreshedAt <= now+int64(ProviderQuotaMaxClockSkew/time.Second) &&
			snapshot.ExpiresAt > now
	}
	return snapshot.RefreshedAt <= now+int64(ProviderQuotaMaxClockSkew/time.Second) &&
		snapshot.RefreshedAt > now-int64(ProviderQuotaDefaultFreshness/time.Second)
}

func ProviderQuotaSnapshotHasUsableValue(snapshot ProviderQuotaSnapshot) bool {
	return snapshot.Confidence != "unknown" &&
		(snapshot.LimitValue != nil || snapshot.UsedValue != nil || snapshot.RemainingValue != nil)
}

func ReplaceProviderQuotaSnapshots(
	connectionID string, credentialRevision int64, snapshots []ProviderQuotaSnapshot,
) error {
	connectionID = strings.TrimSpace(connectionID)
	if connectionID == "" {
		return fmt.Errorf("connection id is required")
	}
	db, err := DB()
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if credentialRevision <= 0 {
		return fmt.Errorf("credential revision must be positive")
	}
	var currentRevision int64
	err = tx.QueryRow(`
SELECT a.credential_revision
FROM provider_connections c
JOIN provider_account_state a ON a.connection_id=c.id
WHERE c.id=? AND c.status!='revoked'`,
		connectionID,
	).Scan(&currentRevision)
	if err == sql.ErrNoRows {
		return ErrProviderConnectionNotFound
	}
	if err != nil {
		return err
	}
	if currentRevision != credentialRevision {
		return ErrProviderQuotaCredentialChanged
	}

	prepared := make([]ProviderQuotaSnapshot, len(snapshots))
	maxRefreshedAt := int64(0)
	seen := map[string]bool{}
	for index, snapshot := range snapshots {
		snapshot.ConnectionID = connectionID
		snapshot.CredentialRevision = credentialRevision
		if snapshot.RefreshedAt == 0 {
			snapshot.RefreshedAt = time.Now().Unix()
		}
		if err := normalizeAndValidateProviderQuotaSnapshot(&snapshot); err != nil {
			return fmt.Errorf("quota snapshot %d: %w", index+1, err)
		}
		key := snapshot.Metric + "\x00" + snapshot.Window
		if seen[key] {
			return fmt.Errorf("duplicate quota metric/window %q/%q", snapshot.Metric, snapshot.Window)
		}
		seen[key] = true
		if snapshot.ID == "" {
			snapshot.ID, err = newID("pquota")
			if err != nil {
				return err
			}
		}
		if snapshot.RefreshedAt > maxRefreshedAt {
			maxRefreshedAt = snapshot.RefreshedAt
		}
		prepared[index] = snapshot
	}

	if _, err := tx.Exec(
		"DELETE FROM provider_quota_snapshots WHERE connection_id=?", connectionID,
	); err != nil {
		return err
	}
	for _, snapshot := range prepared {
		metadata, err := json.Marshal(snapshot.Metadata)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`
INSERT INTO provider_quota_snapshots(
    id,connection_id,credential_revision,metric,unit,window,label,limit_value,used_value,
    remaining_value,reset_at,source,confidence,refreshed_at,expires_at,metadata_json
) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			snapshot.ID, connectionID, credentialRevision,
			snapshot.Metric, snapshot.Unit, snapshot.Window,
			nullableString(snapshot.Label), nullableFloat64(snapshot.LimitValue),
			nullableFloat64(snapshot.UsedValue), nullableFloat64(snapshot.RemainingValue),
			nullablePositiveInt64(snapshot.ResetAt), snapshot.Source, snapshot.Confidence,
			snapshot.RefreshedAt, nullablePositiveInt64(snapshot.ExpiresAt), string(metadata),
		); err != nil {
			return err
		}
	}
	now := time.Now().Unix()
	if _, err := tx.Exec(`
UPDATE provider_account_state
SET last_quota_refresh=?,updated_at=?
WHERE connection_id=?`,
		nullablePositiveInt64(maxRefreshedAt), now, connectionID,
	); err != nil {
		return err
	}
	return tx.Commit()
}

func ListProviderQuotaSnapshots(filter ProviderQuotaFilter) ([]ProviderQuotaSnapshot, error) {
	db, err := DB()
	if err != nil {
		return nil, err
	}
	query := `
SELECT q.id,q.connection_id,q.credential_revision,c.principal_id,c.provider_id,
       q.metric,q.unit,q.window,
       COALESCE(q.label,''),q.limit_value,q.used_value,q.remaining_value,
       COALESCE(q.reset_at,0),q.source,q.confidence,q.refreshed_at,
       COALESCE(q.expires_at,0),q.metadata_json
FROM provider_quota_snapshots q
JOIN provider_connections c ON c.id=q.connection_id
JOIN provider_account_state a ON a.connection_id=c.id`
	conditions := []string{"c.status!='revoked'", "q.credential_revision=a.credential_revision"}
	args := []any{}
	if value := strings.TrimSpace(filter.ConnectionID); value != "" {
		conditions = append(conditions, "q.connection_id=?")
		args = append(args, value)
	}
	if value := strings.TrimSpace(filter.PrincipalID); value != "" {
		conditions = append(conditions, "c.principal_id=?")
		args = append(args, value)
	}
	if value := strings.TrimSpace(filter.ProviderID); value != "" {
		conditions = append(conditions, "c.provider_id=?")
		args = append(args, value)
	}
	if filter.FreshOnly {
		now := filter.Now
		if now == 0 {
			now = time.Now().Unix()
		}
		conditions = append(conditions, `(q.refreshed_at<=? AND (
    (q.expires_at IS NOT NULL AND q.expires_at>?)
    OR (q.expires_at IS NULL AND q.refreshed_at>?)
))`)
		args = append(
			args,
			now+int64(ProviderQuotaMaxClockSkew/time.Second),
			now,
			now-int64(ProviderQuotaDefaultFreshness/time.Second),
		)
	}
	query += " WHERE " + strings.Join(conditions, " AND ")
	query += " ORDER BY c.provider_id,c.principal_id,q.connection_id,q.metric,q.window"
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ProviderQuotaSnapshot{}
	for rows.Next() {
		snapshot, err := scanProviderQuotaSnapshot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, snapshot)
	}
	return out, rows.Err()
}

func normalizeAndValidateProviderQuotaSnapshot(snapshot *ProviderQuotaSnapshot) error {
	snapshot.ConnectionID = strings.TrimSpace(snapshot.ConnectionID)
	snapshot.Metric = strings.ToLower(strings.TrimSpace(snapshot.Metric))
	snapshot.Unit = strings.ToLower(strings.TrimSpace(snapshot.Unit))
	snapshot.Window = strings.ToLower(strings.TrimSpace(snapshot.Window))
	snapshot.Label = strings.TrimSpace(snapshot.Label)
	snapshot.Source = strings.TrimSpace(snapshot.Source)
	snapshot.Confidence = strings.ToLower(strings.TrimSpace(snapshot.Confidence))
	if snapshot.ConnectionID == "" {
		return fmt.Errorf("connection id is required")
	}
	for name, value := range map[string]string{
		"metric": snapshot.Metric, "unit": snapshot.Unit, "window": snapshot.Window,
	} {
		if !quotaIdentifierPattern.MatchString(value) {
			return fmt.Errorf("invalid %s %q", name, value)
		}
	}
	if snapshot.Source == "" {
		return fmt.Errorf("source is required")
	}
	switch snapshot.Confidence {
	case "verified", "manual", "unknown":
	default:
		return fmt.Errorf("invalid confidence %q", snapshot.Confidence)
	}
	for name, value := range map[string]*float64{
		"limit": snapshot.LimitValue, "used": snapshot.UsedValue, "remaining": snapshot.RemainingValue,
	} {
		if value != nil && (*value < 0 || math.IsNaN(*value) || math.IsInf(*value, 0)) {
			return fmt.Errorf("%s must be a finite non-negative number", name)
		}
	}
	now := time.Now().Unix()
	if snapshot.RefreshedAt <= 0 {
		return fmt.Errorf("refreshed_at must be positive")
	}
	if snapshot.RefreshedAt > now+int64(ProviderQuotaMaxClockSkew/time.Second) {
		return fmt.Errorf("refreshed_at exceeds allowed clock skew")
	}
	if snapshot.ResetAt < 0 || snapshot.ExpiresAt < 0 {
		return fmt.Errorf("quota timestamps must be non-negative")
	}
	if snapshot.ExpiresAt > 0 && snapshot.ExpiresAt < snapshot.RefreshedAt {
		return fmt.Errorf("expires_at must not precede refreshed_at")
	}
	if snapshot.Metadata == nil {
		snapshot.Metadata = map[string]any{}
	}
	for key, value := range snapshot.Metadata {
		normalized := strings.ToLower(strings.TrimSpace(key))
		compact := strings.NewReplacer("-", "", "_", "", ".", "", " ", "").Replace(normalized)
		for _, sensitive := range []string{
			"token", "secret", "authorization", "auth", "password", "cookie", "apikey", "credential",
		} {
			if strings.Contains(compact, sensitive) {
				return fmt.Errorf("metadata key %q is credential-shaped", key)
			}
		}
		if !providerQuotaMetadataAllowlist[normalized] {
			return fmt.Errorf("metadata key %q is not allowed", key)
		}
		switch value.(type) {
		case nil, bool, string, float64, float32, int, int32, int64, uint, uint32, uint64, json.Number:
		default:
			return fmt.Errorf("metadata value %q must be scalar", key)
		}
		if text, ok := value.(string); ok {
			if len(text) > 256 || quotaCredentialValuePattern.MatchString(text) {
				return fmt.Errorf("metadata value %q is credential-shaped or too long", key)
			}
		}
	}
	return nil
}

func clearProviderQuotaSnapshotsTx(tx *sql.Tx, connectionID string, now int64) error {
	if _, err := tx.Exec(
		"DELETE FROM provider_quota_snapshots WHERE connection_id=?", connectionID,
	); err != nil {
		return err
	}
	_, err := tx.Exec(`
UPDATE provider_account_state
SET last_quota_refresh=NULL,updated_at=?
WHERE connection_id=?`, now, connectionID)
	return err
}

func scanProviderQuotaSnapshot(
	scanner interface{ Scan(...any) error },
) (ProviderQuotaSnapshot, error) {
	var snapshot ProviderQuotaSnapshot
	var limit, used, remaining sql.NullFloat64
	var metadata string
	err := scanner.Scan(
		&snapshot.ID, &snapshot.ConnectionID, &snapshot.CredentialRevision,
		&snapshot.PrincipalID, &snapshot.ProviderID,
		&snapshot.Metric, &snapshot.Unit, &snapshot.Window, &snapshot.Label,
		&limit, &used, &remaining, &snapshot.ResetAt, &snapshot.Source,
		&snapshot.Confidence, &snapshot.RefreshedAt, &snapshot.ExpiresAt, &metadata,
	)
	if err != nil {
		return ProviderQuotaSnapshot{}, err
	}
	if limit.Valid {
		snapshot.LimitValue = float64Pointer(limit.Float64)
	}
	if used.Valid {
		snapshot.UsedValue = float64Pointer(used.Float64)
	}
	if remaining.Valid {
		snapshot.RemainingValue = float64Pointer(remaining.Float64)
	}
	if err := json.Unmarshal([]byte(metadata), &snapshot.Metadata); err != nil {
		return ProviderQuotaSnapshot{}, err
	}
	return snapshot, nil
}

func nullableFloat64(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}

func float64Pointer(value float64) *float64 {
	return &value
}

func SortProviderQuotaSnapshots(snapshots []ProviderQuotaSnapshot) {
	sort.Slice(snapshots, func(i, j int) bool {
		left := snapshots[i]
		right := snapshots[j]
		if left.ProviderID != right.ProviderID {
			return left.ProviderID < right.ProviderID
		}
		if left.ConnectionID != right.ConnectionID {
			return left.ConnectionID < right.ConnectionID
		}
		if left.Metric != right.Metric {
			return left.Metric < right.Metric
		}
		return left.Window < right.Window
	})
}
