package iam

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"llmgw/internal/config"
)

// QuotaExceeded refuses a request that a request, token, cost or credit limit
// does not admit. Reset is when the window the limit counts ends, after which
// the same request may be admitted.
type QuotaExceeded struct {
	// Scope is whose limit refused the request: "key" or "project".
	Scope string
	// Field names the limit as policies do, such as "daily_requests".
	Field  string
	Metric string
	Limit  int64
	Reset  time.Time
}

func (e *QuotaExceeded) Error() string {
	return fmt.Sprintf("%s quota exceeded (limit %d)", e.Metric, e.Limit)
}

// Code names the refusing limit as the request's usage records it:
// "quota:<scope>:<field>", such as "quota:project:daily_requests".
func (e *QuotaExceeded) Code() string {
	return "quota:" + e.Scope + ":" + e.Field
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
	if p == nil || (p.KeyID == "" && p.ProjectID == "") {
		return nil // static admin key / unauthenticated local mode
	}
	if p.KeyID == "" {
		// An externally managed key has no stored policy or counters, but its
		// usage settles against its project, so the project's limits apply.
		return CheckAndConsumeProjectRequest(p.ProjectID, now)
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
	counters, err := readKeyCountersTx(tx, p.KeyID, now)
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
	if err := checkPolicyCounters("key", keyPolicy, counters, quotaWindowEnds(now)); err != nil {
		return err
	}
	projectPolicy, err := projectPolicyTx(tx, p.ProjectID)
	if err != nil {
		return err
	}
	projectCounters, err := readProjectCountersTx(tx, p.ProjectID, now)
	if err != nil {
		return err
	}
	if err := checkPolicyCounters("project", projectPolicy.KeyPolicy, projectCounters, quotaWindowEnds(now)); err != nil {
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

// checkPolicyCounters refuses a request when counters have reached a limit
// policy sets, scope's "key" or "project". A refusal by a project's limit
// says so in its message.
func checkPolicyCounters(scope string, policy KeyPolicy, counters quotaCounters, ends quotaWindows) error {
	prefix := ""
	if scope == "project" {
		prefix = "project "
	}
	for _, limit := range quotaLimits {
		value := limit.of(policy)
		if value > 0 && metricValue(counters.in(limit.period), limit.metric) >= value {
			return &QuotaExceeded{
				Scope: scope, Field: limit.field, Metric: prefix + limit.label,
				Limit: value, Reset: ends.at(limit.period),
			}
		}
	}
	return nil
}

// quotaCounters are a key's or a project's counters of the minute, day and
// month one time falls in.
type quotaCounters struct{ minute, day, month quotaCounter }

func (c quotaCounters) in(period string) quotaCounter {
	switch period {
	case "minute":
		return c.minute
	case "day":
		return c.day
	}
	return c.month
}

func readKeyCountersTx(tx *sql.Tx, keyID string, now time.Time) (quotaCounters, error) {
	return readCountersTx(tx, keyID, now, readCounterTx)
}

func readProjectCountersTx(tx *sql.Tx, projectID string, now time.Time) (quotaCounters, error) {
	return readCountersTx(tx, projectID, now, readProjectCounterTx)
}

func readCountersTx(
	tx *sql.Tx, id string, now time.Time,
	read func(tx *sql.Tx, id, period string, start int64) (quotaCounter, error),
) (quotaCounters, error) {
	minuteStart, dayStart, monthStart := quotaPeriods(now)
	var counters quotaCounters
	var err error
	if counters.minute, err = read(tx, id, "minute", minuteStart); err != nil {
		return quotaCounters{}, err
	}
	if counters.day, err = read(tx, id, "day", dayStart); err != nil {
		return quotaCounters{}, err
	}
	if counters.month, err = read(tx, id, "month", monthStart); err != nil {
		return quotaCounters{}, err
	}
	return counters, nil
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

// quotaWindows are the ends of the UTC minute, day and month a request falls
// in: when a limit that refused it counts afresh.
type quotaWindows struct{ minute, day, month time.Time }

func (w quotaWindows) at(period string) time.Time {
	switch period {
	case "minute":
		return w.minute
	case "day":
		return w.day
	}
	return w.month
}

func quotaWindowEnds(now time.Time) quotaWindows {
	minute, day, month := quotaPeriods(now)
	return quotaWindows{
		minute: time.Unix(minute+60, 0).UTC(),
		day:    time.Unix(day, 0).UTC().AddDate(0, 0, 1),
		month:  time.Unix(month, 0).UTC().AddDate(0, 1, 0),
	}
}

// CheckAndConsumeProjectRequest enforces a project policy for traffic without a
// gateway-issued key: externally managed keys and keyless internal traffic
// such as the owner playground. It consumes only project counters and never
// mints, stores, or exposes a browser API key.
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
	counters, err := readProjectCountersTx(tx, projectID, now)
	if err != nil {
		return err
	}
	if err := checkPolicyCounters("project", policy.KeyPolicy, counters, quotaWindowEnds(now)); err != nil {
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
