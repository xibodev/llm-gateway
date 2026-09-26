package iam

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"llmgw/internal/config"
)

type QuotaExceeded struct {
	Metric string
	Limit  int64
}

func (e *QuotaExceeded) Error() string {
	return fmt.Sprintf("%s quota exceeded (limit %d)", e.Metric, e.Limit)
}

type quotaCounter struct {
	Requests     int64
	InputTokens  int64
	OutputTokens int64
	CostMicroUSD int64
	CreditsMilli int64
}

// CheckAndConsumeRequest atomically enforces request-rate and accumulated usage
// limits, then consumes one request slot in the current minute/day/month. Token,
// cost and credit counters are reconciled after the provider response.
func CheckAndConsumeRequest(p *config.Principal, now time.Time) error {
	if p == nil || p.KeyID == "" {
		return nil // static admin key / unauthenticated local mode
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

	minuteStart, dayStart, monthStart := quotaPeriods(now)
	minute, err := readCounterTx(tx, p.KeyID, "minute", minuteStart)
	if err != nil {
		return err
	}
	day, err := readCounterTx(tx, p.KeyID, "day", dayStart)
	if err != nil {
		return err
	}
	month, err := readCounterTx(tx, p.KeyID, "month", monthStart)
	if err != nil {
		return err
	}
	keyPolicy := KeyPolicy{
		RPM: p.RPM, DailyRequests: p.DailyRequests, MonthlyRequests: p.MonthlyRequests,
		DailyInputTokens: p.DailyInputTokens, DailyOutputTokens: p.DailyOutputTokens,
		MonthlyTotalTokens: p.MonthlyTotalTokens,
		DailyCostMicroUSD:  p.DailyCostMicroUSD, MonthlyCostMicroUSD: p.MonthlyCostMicroUSD,
		DailyCreditsMilli: p.DailyCreditsMilli, MonthlyCreditsMilli: p.MonthlyCreditsMilli,
	}
	if err := checkPolicyCounters("", keyPolicy, minute, day, month); err != nil {
		return err
	}
	projectPolicy, err := projectPolicyTx(tx, p.ProjectID)
	if err != nil {
		return err
	}
	projectMinute, err := readProjectCounterTx(tx, p.ProjectID, "minute", minuteStart)
	if err != nil {
		return err
	}
	projectDay, err := readProjectCounterTx(tx, p.ProjectID, "day", dayStart)
	if err != nil {
		return err
	}
	projectMonth, err := readProjectCounterTx(tx, p.ProjectID, "month", monthStart)
	if err != nil {
		return err
	}
	if err := checkPolicyCounters(
		"project ", projectPolicy.KeyPolicy, projectMinute, projectDay, projectMonth,
	); err != nil {
		return err
	}
	for _, period := range []struct {
		name  string
		start int64
	}{
		{"minute", minuteStart}, {"day", dayStart}, {"month", monthStart},
	} {
		if _, err := tx.Exec(`
INSERT INTO quota_counters(key_id,period,period_start,requests)
VALUES(?,?,?,1)
ON CONFLICT(key_id,period,period_start)
DO UPDATE SET requests=requests+1`, p.KeyID, period.name, period.start); err != nil {
			return err
		}
	}
	for _, period := range []struct {
		name  string
		start int64
	}{
		{"minute", minuteStart}, {"day", dayStart}, {"month", monthStart},
	} {
		if _, err := tx.Exec(`
INSERT INTO project_quota_counters(project_id,period,period_start,requests)
VALUES(?,?,?,1)
ON CONFLICT(project_id,period,period_start)
DO UPDATE SET requests=requests+1`, p.ProjectID, period.name, period.start); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func checkPolicyCounters(
	prefix string, policy KeyPolicy, minute, day, month quotaCounter,
) error {
	checks := []struct {
		metric string
		value  int64
		limit  int64
	}{
		{prefix + "requests/minute", minute.Requests, int64(policy.RPM)},
		{prefix + "requests/day", day.Requests, int64(policy.DailyRequests)},
		{prefix + "requests/month", month.Requests, int64(policy.MonthlyRequests)},
		{prefix + "input tokens/day", day.InputTokens, policy.DailyInputTokens},
		{prefix + "output tokens/day", day.OutputTokens, policy.DailyOutputTokens},
		{prefix + "total tokens/month", month.InputTokens + month.OutputTokens, policy.MonthlyTotalTokens},
		{prefix + "estimated cost/day (micro-USD)", day.CostMicroUSD, policy.DailyCostMicroUSD},
		{prefix + "estimated cost/month (micro-USD)", month.CostMicroUSD, policy.MonthlyCostMicroUSD},
		{prefix + "credits/day (milli)", day.CreditsMilli, policy.DailyCreditsMilli},
		{prefix + "credits/month (milli)", month.CreditsMilli, policy.MonthlyCreditsMilli},
	}
	for _, check := range checks {
		if check.limit > 0 && check.value >= check.limit {
			return &QuotaExceeded{Metric: check.metric, Limit: check.limit}
		}
	}
	return nil
}

func readCounterTx(
	tx *sql.Tx, keyID, period string, start int64,
) (quotaCounter, error) {
	var c quotaCounter
	err := tx.QueryRow(`
SELECT requests,input_tokens,output_tokens,cost_microusd,credits_milli
FROM quota_counters WHERE key_id=? AND period=? AND period_start=?`,
		keyID, period, start,
	).Scan(&c.Requests, &c.InputTokens, &c.OutputTokens, &c.CostMicroUSD, &c.CreditsMilli)
	if err == sql.ErrNoRows {
		return quotaCounter{}, nil
	}
	return c, err
}

func readProjectCounterTx(
	tx *sql.Tx, projectID, period string, start int64,
) (quotaCounter, error) {
	var c quotaCounter
	err := tx.QueryRow(`
SELECT requests,input_tokens,output_tokens,cost_microusd,credits_milli
FROM project_quota_counters WHERE project_id=? AND period=? AND period_start=?`,
		projectID, period, start,
	).Scan(&c.Requests, &c.InputTokens, &c.OutputTokens, &c.CostMicroUSD, &c.CreditsMilli)
	if err == sql.ErrNoRows {
		return quotaCounter{}, nil
	}
	return c, err
}

func projectPolicyTx(tx *sql.Tx, projectID string) (ProjectPolicy, error) {
	var policy ProjectPolicy
	var models, providers string
	policy.ProjectID = projectID
	err := tx.QueryRow(`
SELECT allowed_models_json,allowed_providers_json,rpm,daily_requests,
       monthly_requests,daily_input_tokens,daily_output_tokens,
       monthly_total_tokens,daily_cost_microusd,monthly_cost_microusd,
       daily_credits_milli,monthly_credits_milli,updated_at
FROM project_policies WHERE project_id=?`, projectID).Scan(
		&models, &providers, &policy.RPM, &policy.DailyRequests,
		&policy.MonthlyRequests, &policy.DailyInputTokens,
		&policy.DailyOutputTokens, &policy.MonthlyTotalTokens,
		&policy.DailyCostMicroUSD, &policy.MonthlyCostMicroUSD,
		&policy.DailyCreditsMilli, &policy.MonthlyCreditsMilli,
		&policy.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return ProjectPolicy{ProjectID: projectID}, nil
	}
	if err != nil {
		return ProjectPolicy{}, err
	}
	_ = json.Unmarshal([]byte(models), &policy.AllowedModels)
	_ = json.Unmarshal([]byte(providers), &policy.AllowedProviders)
	return policy, nil
}

func quotaPeriods(now time.Time) (minute, day, month int64) {
	now = now.UTC()
	minute = now.Unix() / 60 * 60
	day = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).Unix()
	month = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).Unix()
	return
}

// CheckAndConsumeProjectRequest enforces a project policy for keyless internal
// traffic such as the owner playground. It consumes only project counters and
// never mints, stores, or exposes a browser API key.
func CheckAndConsumeProjectRequest(projectID string, now time.Time) error {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return fmt.Errorf("project_id is required")
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
	policy, err := projectPolicyTx(tx, projectID)
	if err != nil {
		return err
	}
	minuteStart, dayStart, monthStart := quotaPeriods(now)
	minute, err := readProjectCounterTx(tx, projectID, "minute", minuteStart)
	if err != nil {
		return err
	}
	day, err := readProjectCounterTx(tx, projectID, "day", dayStart)
	if err != nil {
		return err
	}
	month, err := readProjectCounterTx(tx, projectID, "month", monthStart)
	if err != nil {
		return err
	}
	if err := checkPolicyCounters("project ", policy.KeyPolicy, minute, day, month); err != nil {
		return err
	}
	for _, period := range []struct {
		name  string
		start int64
	}{
		{"minute", minuteStart}, {"day", dayStart}, {"month", monthStart},
	} {
		if _, err := tx.Exec(`
INSERT INTO project_quota_counters(project_id,period,period_start,requests)
VALUES(?,?,?,1)
ON CONFLICT(project_id,period,period_start)
DO UPDATE SET requests=requests+1`, projectID, period.name, period.start); err != nil {
			return err
		}
	}
	return tx.Commit()
}
