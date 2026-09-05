package iam

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type RetentionPolicy struct {
	UsageDays           int
	AuditDays           int
	DeliveredOutboxDays int
}

type RetentionResult struct {
	UsageEvents          int64
	AuditEvents          int64
	KeyQuotaCounters     int64
	ProjectQuotaCounters int64
	DeliveredOutbox      int64
}

func DefaultRetentionPolicy() RetentionPolicy {
	return RetentionPolicy{
		UsageDays:           envIntAtLeast("LLMGW_RETENTION_USAGE_DAYS", 90, 1),
		AuditDays:           envIntAtLeast("LLMGW_RETENTION_AUDIT_DAYS", 365, 1),
		DeliveredOutboxDays: envIntAtLeast("LLMGW_RETENTION_DELIVERED_OUTBOX_DAYS", 400, 400),
	}
}

func envIntAtLeast(name string, fallback, minimum int) int {
	value, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if err != nil || value < minimum {
		return fallback
	}
	return value
}

// PruneOperationalHistory removes only completed history. Current quota periods
// and retryable outbox work are retained regardless of age.
func PruneOperationalHistory(now time.Time, policy RetentionPolicy) (RetentionResult, error) {
	if policy.UsageDays <= 0 || policy.AuditDays <= 0 || policy.DeliveredOutboxDays < 400 {
		return RetentionResult{}, fmt.Errorf("retention must keep usage/audit for positive windows and delivered outbox for at least 400 days")
	}
	db, err := DB()
	if err != nil {
		return RetentionResult{}, err
	}
	tx, err := db.Begin()
	if err != nil {
		return RetentionResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	result := RetentionResult{}
	usageCutoff := now.AddDate(0, 0, -policy.UsageDays).Unix()
	auditCutoff := now.AddDate(0, 0, -policy.AuditDays).Unix()
	outboxCutoff := now.AddDate(0, 0, -policy.DeliveredOutboxDays).Unix()
	minute, day, month := quotaPeriods(now)

	deletions := []struct {
		query string
		args  []any
		count *int64
	}{
		{"DELETE FROM usage_events WHERE ts < ?", []any{usageCutoff}, &result.UsageEvents},
		{"DELETE FROM audit_events WHERE ts < ?", []any{auditCutoff}, &result.AuditEvents},
		{`DELETE FROM quota_counters WHERE
            (period='minute' AND period_start < ?) OR
            (period='day' AND period_start < ?) OR
            (period='month' AND period_start < ?)`, []any{minute, day, month}, &result.KeyQuotaCounters},
		{`DELETE FROM project_quota_counters WHERE
            (period='minute' AND period_start < ?) OR
            (period='day' AND period_start < ?) OR
            (period='month' AND period_start < ?)`, []any{minute, day, month}, &result.ProjectQuotaCounters},
		{`DELETE FROM outbox_events
          WHERE status='delivered' AND delivered_at IS NOT NULL AND delivered_at < ?`, []any{outboxCutoff}, &result.DeliveredOutbox},
	}
	for _, deletion := range deletions {
		res, err := tx.Exec(deletion.query, deletion.args...)
		if err != nil {
			return RetentionResult{}, err
		}
		*deletion.count, err = res.RowsAffected()
		if err != nil {
			return RetentionResult{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return RetentionResult{}, err
	}
	return result, nil
}
