package iam

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"llmgw/internal/diagnostics"
)

type AlertRule struct {
	ID          string `json:"id"`
	ProjectID   string `json:"project_id,omitempty"`
	PrincipalID string `json:"principal_id,omitempty"`
	Kind        string `json:"kind"`
	Metric      string `json:"metric"`
	Threshold   int    `json:"threshold"`
	Period      string `json:"period"`
	Enabled     bool   `json:"enabled"`
	CreatedAt   int64  `json:"created_at"`
	UpdatedAt   int64  `json:"updated_at"`
}

type OutboxEvent struct {
	ID          int64          `json:"id"`
	Timestamp   int64          `json:"ts"`
	Kind        string         `json:"kind"`
	ProjectID   string         `json:"project_id,omitempty"`
	PrincipalID string         `json:"principal_id,omitempty"`
	Payload     map[string]any `json:"payload"`
	Status      string         `json:"status"`
	Attempts    int            `json:"attempts"`
	AvailableAt int64          `json:"available_at"`
	DeliveredAt int64          `json:"delivered_at,omitempty"`
	LastError   string         `json:"last_error,omitempty"`
	ClaimedBy   string         `json:"claimed_by,omitempty"`
	LeaseUntil  int64          `json:"lease_until,omitempty"`
}

var quotaMetrics = map[string]bool{
	"requests": true, "input_tokens": true, "output_tokens": true,
	"total_tokens": true, "cost_microusd": true, "credits_milli": true,
}

func CreateAlertRule(rule AlertRule) (AlertRule, error) {
	switch rule.Kind {
	case "quota_usage", "key_expiry":
	default:
		return AlertRule{}, fmt.Errorf("unsupported alert kind %q", rule.Kind)
	}
	if rule.Kind == "quota_usage" && !quotaMetrics[rule.Metric] {
		return AlertRule{}, fmt.Errorf("unsupported quota metric %q", rule.Metric)
	}
	if rule.Kind == "key_expiry" {
		rule.Metric = "days"
	}
	if rule.Period != "day" && rule.Period != "month" && rule.Kind == "quota_usage" {
		return AlertRule{}, fmt.Errorf("quota alert period must be day or month")
	}
	if rule.Threshold <= 0 || (rule.Kind == "quota_usage" && rule.Threshold > 100) {
		return AlertRule{}, fmt.Errorf("invalid alert threshold")
	}
	id, err := newID("alert")
	if err != nil {
		return AlertRule{}, err
	}
	now := time.Now().Unix()
	rule.ID = id
	rule.Enabled = true
	rule.CreatedAt = now
	rule.UpdatedAt = now
	db, err := DB()
	if err != nil {
		return AlertRule{}, err
	}
	_, err = db.Exec(`
INSERT INTO alert_rules(
 id,project_id,principal_id,kind,threshold,period,enabled,created_at,updated_at,metric
) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		rule.ID, nullable(rule.ProjectID), nullable(rule.PrincipalID), rule.Kind,
		rule.Threshold, rule.Period, 1, now, now, rule.Metric,
	)
	return rule, err
}

func ListAlertRules() ([]AlertRule, error) {
	db, err := DB()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`
SELECT id,COALESCE(project_id,''),COALESCE(principal_id,''),kind,metric,
       threshold,period,enabled,created_at,updated_at
FROM alert_rules ORDER BY created_at,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AlertRule{}
	for rows.Next() {
		var rule AlertRule
		var enabled int
		if err := rows.Scan(
			&rule.ID, &rule.ProjectID, &rule.PrincipalID, &rule.Kind, &rule.Metric,
			&rule.Threshold, &rule.Period, &enabled, &rule.CreatedAt, &rule.UpdatedAt,
		); err != nil {
			return nil, err
		}
		rule.Enabled = enabled != 0
		out = append(out, rule)
	}
	return out, rows.Err()
}

func SetAlertRuleEnabled(id string, enabled bool) error {
	db, err := DB()
	if err != nil {
		return err
	}
	value := 0
	if enabled {
		value = 1
	}
	res, err := db.Exec(
		"UPDATE alert_rules SET enabled=?,updated_at=? WHERE id=?",
		value, time.Now().Unix(), id,
	)
	if err != nil {
		return err
	}
	return requireAffected(res, "alert rule")
}

func DeleteAlertRule(id string) error {
	db, err := DB()
	if err != nil {
		return err
	}
	res, err := db.Exec("DELETE FROM alert_rules WHERE id=?", id)
	if err != nil {
		return err
	}
	return requireAffected(res, "alert rule")
}

func PendingOutbox(limit int) ([]OutboxEvent, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	db, err := DB()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`
SELECT id,ts,kind,COALESCE(project_id,''),COALESCE(principal_id,''),
       payload_json,status,attempts,available_at,COALESCE(delivered_at,0),
       COALESCE(last_error,''),COALESCE(claimed_by,''),COALESCE(lease_until,0)
FROM outbox_events
WHERE status IN ('pending','failed') AND available_at<=?
ORDER BY id LIMIT ?`, time.Now().Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OutboxEvent{}
	for rows.Next() {
		var event OutboxEvent
		var payload string
		if err := rows.Scan(
			&event.ID, &event.Timestamp, &event.Kind, &event.ProjectID,
			&event.PrincipalID, &payload, &event.Status, &event.Attempts,
			&event.AvailableAt, &event.DeliveredAt, &event.LastError,
			&event.ClaimedBy, &event.LeaseUntil,
		); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(payload), &event.Payload)
		event.LastError = sanitizeOutboxError(event.LastError)
		out = append(out, event)
	}
	return out, rows.Err()
}

func ClaimOutbox(workerID string, limit int, lease time.Duration) ([]OutboxEvent, error) {
	workerID = strings.TrimSpace(workerID)
	if workerID == "" {
		return nil, fmt.Errorf("worker id is required")
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if lease <= 0 || lease > time.Hour {
		lease = 5 * time.Minute
	}
	db, err := DB()
	if err != nil {
		return nil, err
	}
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().Unix()
	rows, err := tx.Query(`
SELECT id FROM outbox_events
WHERE status IN ('pending','failed') AND available_at<=?
  AND (lease_until IS NULL OR lease_until<?)
ORDER BY id LIMIT ?`, now, now, limit)
	if err != nil {
		return nil, err
	}
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	leaseUntil := time.Now().Add(lease).Unix()
	claimed := []OutboxEvent{}
	for _, id := range ids {
		res, err := tx.Exec(`
UPDATE outbox_events SET claimed_by=?,lease_until=?
WHERE id=? AND status IN ('pending','failed')
  AND (lease_until IS NULL OR lease_until<?)`,
			workerID, leaseUntil, id, now,
		)
		if err != nil {
			return nil, err
		}
		affected, _ := res.RowsAffected()
		if affected == 0 {
			continue
		}
		event, err := outboxEventTx(tx, id)
		if err != nil {
			return nil, err
		}
		claimed = append(claimed, event)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return claimed, nil
}

func MarkOutboxDelivered(id int64, workerID string) error {
	db, err := DB()
	if err != nil {
		return err
	}
	res, err := db.Exec(`
UPDATE outbox_events SET status='delivered',delivered_at=?,attempts=attempts+1,
 last_error=NULL,lease_until=NULL
WHERE id=? AND claimed_by=? AND status IN ('pending','failed')`,
		time.Now().Unix(), id, workerID)
	if err != nil {
		return err
	}
	return requireAffected(res, "outbox event")
}

func MarkOutboxFailed(id int64, workerID, message string, retryAt int64) error {
	db, err := DB()
	if err != nil {
		return err
	}
	if retryAt == 0 {
		retryAt = time.Now().Add(5 * time.Minute).Unix()
	}
	res, err := db.Exec(`
UPDATE outbox_events SET status='failed',attempts=attempts+1,last_error=?,
 available_at=?,claimed_by=NULL,lease_until=NULL
WHERE id=? AND claimed_by=? AND status IN ('pending','failed')`,
		truncateError(message), retryAt, id, workerID)
	if err != nil {
		return err
	}

	return requireAffected(res, "outbox event")
}

func outboxEventTx(tx *sql.Tx, id int64) (OutboxEvent, error) {
	var event OutboxEvent
	var payload string
	err := tx.QueryRow(`
SELECT id,ts,kind,COALESCE(project_id,''),COALESCE(principal_id,''),
       payload_json,status,attempts,available_at,COALESCE(delivered_at,0),
       COALESCE(last_error,''),COALESCE(claimed_by,''),COALESCE(lease_until,0)
FROM outbox_events WHERE id=?`, id).Scan(
		&event.ID, &event.Timestamp, &event.Kind, &event.ProjectID,
		&event.PrincipalID, &payload, &event.Status, &event.Attempts,
		&event.AvailableAt, &event.DeliveredAt, &event.LastError,
		&event.ClaimedBy, &event.LeaseUntil,
	)
	_ = json.Unmarshal([]byte(payload), &event.Payload)
	event.LastError = sanitizeOutboxError(event.LastError)
	return event, err
}

func evaluateUsageAlertsTx(tx *sql.Tx, event UsageEvent) error {
	if event.KeyID == "" || event.IsStub {
		return nil
	}
	rows, err := tx.Query(`
SELECT id,COALESCE(project_id,''),COALESCE(principal_id,''),kind,metric,
       threshold,period
FROM alert_rules
WHERE enabled=1
  AND (project_id IS NULL OR project_id=?)
  AND (principal_id IS NULL OR principal_id=?)`,
		event.ProjectID, event.PrincipalID,
	)
	if err != nil {
		return err
	}
	defer rows.Close()
	type pendingRule struct {
		id, projectID, principalID, kind, metric, period string
		threshold                                        int
	}
	rules := []pendingRule{}
	for rows.Next() {
		var rule pendingRule
		if err := rows.Scan(
			&rule.id, &rule.projectID, &rule.principalID, &rule.kind, &rule.metric,
			&rule.threshold, &rule.period,
		); err != nil {
			return err
		}
		if rule.kind == "quota_usage" {
			rules = append(rules, rule)
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	at := time.Unix(event.Timestamp, 0).UTC()
	_, dayStart, monthStart := quotaPeriods(at)
	for _, rule := range rules {
		start := dayStart
		if rule.period == "month" {
			start = monthStart
		}
		counter := quotaCounter{}
		limit := int64(0)
		scopeID := event.KeyID
		if rule.projectID != "" {
			scopeID = event.ProjectID
			counter, err = readProjectCounterTx(tx, event.ProjectID, rule.period, start)
			if err == nil {
				limit, err = projectMetricLimitTx(
					tx, event.ProjectID, rule.period, rule.metric,
				)
			}
		} else {
			counter, err = readCounterTx(tx, event.KeyID, rule.period, start)
			if err == nil {
				limit, err = keyMetricLimitTx(tx, event.KeyID, rule.period, rule.metric)
			}
		}
		value := metricValue(counter, rule.metric)
		if err != nil || limit <= 0 || value*100 < limit*int64(rule.threshold) {
			continue
		}
		kind := "quota_warning"
		if value >= limit {
			kind = "quota_exhausted"
		}
		payload := map[string]any{
			"rule_id": rule.id, "metric": rule.metric, "period": rule.period,
			"period_start": start, "value": value, "limit": limit,
			"threshold_percent": rule.threshold, "key_id": event.KeyID,
			"scope_id": scopeID,
		}
		if err := enqueueOutboxTx(
			tx, kind, event.ProjectID, event.PrincipalID, payload,
			fmt.Sprintf("%s:%s:%d:%s", rule.id, scopeID, start, kind),
		); err != nil {
			return err
		}
	}
	return nil
}

func EvaluateScheduledAlerts(now time.Time) (int, error) {
	db, err := DB()
	if err != nil {
		return 0, err
	}
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.Query(`
SELECT r.id,COALESCE(r.project_id,''),COALESCE(r.principal_id,''),r.threshold,
       k.id,k.project_id,k.principal_id,k.name,k.prefix,k.expires_at
FROM alert_rules r
JOIN api_keys k
  ON (r.project_id IS NULL OR r.project_id=k.project_id)
 AND (r.principal_id IS NULL OR r.principal_id=k.principal_id)
WHERE r.enabled=1 AND r.kind='key_expiry' AND k.status='active'
  AND k.expires_at IS NOT NULL AND k.expires_at>?
  AND k.expires_at<=?`,
		now.Unix(), now.Add(365*24*time.Hour).Unix(),
	)
	if err != nil {
		return 0, err
	}
	type expiring struct {
		ruleID, ruleProject, rulePrincipal string
		days                               int
		keyID, projectID, principalID      string
		name, prefix                       string
		expiresAt                          int64
	}
	items := []expiring{}
	for rows.Next() {
		var item expiring
		if err := rows.Scan(
			&item.ruleID, &item.ruleProject, &item.rulePrincipal, &item.days,
			&item.keyID, &item.projectID, &item.principalID, &item.name,
			&item.prefix, &item.expiresAt,
		); err != nil {
			return 0, err
		}
		if item.expiresAt <= now.Add(time.Duration(item.days)*24*time.Hour).Unix() {
			items = append(items, item)
		}
	}
	rows.Close()
	count := 0
	for _, item := range items {
		payload := map[string]any{
			"rule_id": item.ruleID, "key_id": item.keyID, "key_name": item.name,
			"key_prefix": item.prefix, "expires_at": item.expiresAt,
			"days": item.days,
		}
		if err := enqueueOutboxTx(
			tx, "key_expiring", item.projectID, item.principalID, payload,
			fmt.Sprintf("%s:%s:%d", item.ruleID, item.keyID, item.expiresAt),
		); err != nil {
			return 0, err
		}
		count++
	}
	return count, tx.Commit()
}

func enqueueOutboxTx(
	tx *sql.Tx, kind, projectID, principalID string, payload map[string]any,
	dedupeKey string,
) error {
	raw, _ := json.Marshal(payload)
	_, err := tx.Exec(`
INSERT OR IGNORE INTO outbox_events(
 ts,kind,project_id,principal_id,payload_json,status,attempts,available_at,dedupe_key
) VALUES(?,?,?,?,?,'pending',0,?,?)`,
		time.Now().Unix(), kind, nullable(projectID), nullable(principalID),
		string(raw), time.Now().Unix(), dedupeKey,
	)
	return err
}

func metricValue(counter quotaCounter, metric string) int64 {
	switch metric {
	case "requests":
		return counter.Requests
	case "input_tokens":
		return counter.InputTokens
	case "output_tokens":
		return counter.OutputTokens
	case "total_tokens":
		return counter.InputTokens + counter.OutputTokens
	case "cost_microusd":
		return counter.CostMicroUSD
	case "credits_milli":
		return counter.CreditsMilli
	}
	return 0
}

func keyMetricLimitTx(
	tx *sql.Tx, keyID, period, metric string,
) (int64, error) {
	column := ""
	switch period + ":" + metric {
	case "day:requests":
		column = "daily_requests"
	case "month:requests":
		column = "monthly_requests"
	case "day:input_tokens":
		column = "daily_input_tokens"
	case "day:output_tokens":
		column = "daily_output_tokens"
	case "month:total_tokens":
		column = "monthly_total_tokens"
	case "day:cost_microusd":
		column = "daily_cost_microusd"
	case "month:cost_microusd":
		column = "monthly_cost_microusd"
	case "day:credits_milli":
		column = "daily_credits_milli"
	case "month:credits_milli":
		column = "monthly_credits_milli"
	default:
		return 0, nil
	}

	var limit int64
	err := tx.QueryRow("SELECT "+column+" FROM api_keys WHERE id=?", keyID).Scan(&limit)
	return limit, err
}

func projectMetricLimitTx(
	tx *sql.Tx, projectID, period, metric string,
) (int64, error) {
	column := ""
	switch period + ":" + metric {
	case "day:requests":
		column = "daily_requests"
	case "month:requests":
		column = "monthly_requests"
	case "day:input_tokens":
		column = "daily_input_tokens"
	case "day:output_tokens":
		column = "daily_output_tokens"
	case "month:total_tokens":
		column = "monthly_total_tokens"
	case "day:cost_microusd":
		column = "daily_cost_microusd"
	case "month:cost_microusd":
		column = "monthly_cost_microusd"
	case "day:credits_milli":
		column = "daily_credits_milli"
	case "month:credits_milli":
		column = "monthly_credits_milli"
	default:
		return 0, nil
	}
	var limit int64
	err := tx.QueryRow(
		"SELECT "+column+" FROM project_policies WHERE project_id=?", projectID,
	).Scan(&limit)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return limit, err
}

func truncateError(message string) string {
	return sanitizeOutboxError(strings.TrimSpace(message))
}

func sanitizeOutboxError(message string) string {
	return diagnostics.SanitizeTextLimit(message, 500)
}
