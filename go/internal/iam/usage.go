package iam

import (
	"encoding/json"
	"time"
)

type UsageEvent struct {
	RequestID      string
	Timestamp      int64
	Endpoint       string
	StatusCode     int
	LatencyMS      int64
	RequestedModel string
	RoutedModel    string
	Provider       string
	ProjectID      string
	PrincipalID    string
	KeyID          string
	InputTokens    int
	OutputTokens   int
	CostMicroUSD   int64
	CreditsMilli   int64
	ErrorCode      string
	IsStub         bool
}

// RecordUsageEvent persists attribution and reconciles day/month token, cost and
// credit counters. Request counts are consumed at intake by CheckAndConsumeRequest.
func RecordUsageEvent(event UsageEvent) error {
	db, err := DB()
	if err != nil {
		return err
	}
	if event.RequestID == "" {
		event.RequestID, err = newID("req")
		if err != nil {
			return err
		}
	}
	if event.Timestamp == 0 {
		event.Timestamp = time.Now().Unix()
	}
	stub := 0
	if event.IsStub {
		stub = 1
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`
INSERT INTO usage_events(
    request_id,ts,endpoint,status_code,latency_ms,requested_model,routed_model,
    provider,project_id,principal_id,key_id,input_tokens,output_tokens,
    cost_microusd,credits_milli,error_code,is_stub
) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		event.RequestID, event.Timestamp, event.Endpoint, event.StatusCode,
		event.LatencyMS, nullable(event.RequestedModel), nullable(event.RoutedModel),
		nullable(event.Provider), nullable(event.ProjectID), nullable(event.PrincipalID),
		nullable(event.KeyID), event.InputTokens, event.OutputTokens,
		event.CostMicroUSD, event.CreditsMilli, nullable(event.ErrorCode), stub,
	)
	if err != nil {
		return err
	}
	if event.KeyID != "" {
		at := time.Unix(event.Timestamp, 0).UTC()
		_, dayStart, monthStart := quotaPeriods(at)
		for _, period := range []struct {
			name  string
			start int64
		}{
			{"day", dayStart}, {"month", monthStart},
		} {
			if _, err := tx.Exec(`
INSERT INTO quota_counters(
    key_id,period,period_start,input_tokens,output_tokens,cost_microusd,credits_milli
) VALUES(?,?,?,?,?,?,?)
ON CONFLICT(key_id,period,period_start) DO UPDATE SET
    input_tokens=input_tokens+excluded.input_tokens,
    output_tokens=output_tokens+excluded.output_tokens,
    cost_microusd=cost_microusd+excluded.cost_microusd,
    credits_milli=credits_milli+excluded.credits_milli`,
				event.KeyID, period.name, period.start, event.InputTokens,
				event.OutputTokens, event.CostMicroUSD, event.CreditsMilli,
			); err != nil {
				return err
			}
		}
	}
	if event.ProjectID != "" {
		at := time.Unix(event.Timestamp, 0).UTC()
		_, dayStart, monthStart := quotaPeriods(at)
		for _, period := range []struct {
			name  string
			start int64
		}{
			{"day", dayStart}, {"month", monthStart},
		} {
			if _, err := tx.Exec(`
INSERT INTO project_quota_counters(
    project_id,period,period_start,input_tokens,output_tokens,cost_microusd,credits_milli
) VALUES(?,?,?,?,?,?,?)
ON CONFLICT(project_id,period,period_start) DO UPDATE SET
    input_tokens=input_tokens+excluded.input_tokens,
    output_tokens=output_tokens+excluded.output_tokens,
    cost_microusd=cost_microusd+excluded.cost_microusd,
    credits_milli=credits_milli+excluded.credits_milli`,
				event.ProjectID, period.name, period.start, event.InputTokens,
				event.OutputTokens, event.CostMicroUSD, event.CreditsMilli,
			); err != nil {
				return err
			}
		}
	}
	if err := evaluateUsageAlertsTx(tx, event); err != nil {
		return err
	}
	return tx.Commit()
}

type UsageTotals struct {
	Requests       int64 `json:"requests"`
	InputTokens    int64 `json:"input_tokens"`
	OutputTokens   int64 `json:"output_tokens"`
	CostMicroUSD   int64 `json:"cost_microusd"`
	CreditsMilli   int64 `json:"credits_milli"`
	Errors         int64 `json:"errors"`
	AverageLatency int64 `json:"average_latency_ms"`
}

type UsageGroup struct {
	ProjectID   string `json:"project_id,omitempty"`
	PrincipalID string `json:"principal_id,omitempty"`
	KeyID       string `json:"key_id,omitempty"`
	Provider    string `json:"provider,omitempty"`
	Model       string `json:"model,omitempty"`
	UsageTotals
}

// UsageStats returns totals plus project/principal/key/model/provider rollups.
func UsageStats(since int64) (map[string]any, error) {
	db, err := DB()
	if err != nil {
		return nil, err
	}

	where := "WHERE ts >= ? AND is_stub=0"
	total, err := queryUsageTotals(db.QueryRow(`
SELECT COUNT(*),COALESCE(SUM(input_tokens),0),COALESCE(SUM(output_tokens),0),
       COALESCE(SUM(cost_microusd),0),COALESCE(SUM(credits_milli),0),
       COALESCE(SUM(CASE WHEN status_code>=400 THEN 1 ELSE 0 END),0),
       CAST(COALESCE(AVG(latency_ms),0) AS INTEGER)
FROM usage_events `+where, since))
	if err != nil {
		return nil, err
	}
	groups := map[string][]UsageGroup{}
	for _, group := range []struct {
		name   string
		column string
	}{
		{"project", "project_id"}, {"principal", "principal_id"}, {"key", "key_id"},
		{"provider", "provider"}, {"model", "routed_model"},
	} {
		rows, err := db.Query(`
SELECT COALESCE(`+group.column+`,''),COUNT(*),COALESCE(SUM(input_tokens),0),
       COALESCE(SUM(output_tokens),0),COALESCE(SUM(cost_microusd),0),
       COALESCE(SUM(credits_milli),0),
       COALESCE(SUM(CASE WHEN status_code>=400 THEN 1 ELSE 0 END),0),
       CAST(COALESCE(AVG(latency_ms),0) AS INTEGER)
FROM usage_events `+where+` GROUP BY `+group.column+` ORDER BY COUNT(*) DESC LIMIT 100`,
			since,
		)
		if err != nil {
			return nil, err
		}
		items := []UsageGroup{}
		for rows.Next() {
			var value string
			var item UsageGroup
			if err := rows.Scan(
				&value, &item.Requests, &item.InputTokens, &item.OutputTokens,
				&item.CostMicroUSD, &item.CreditsMilli, &item.Errors,
				&item.AverageLatency,
			); err != nil {
				rows.Close()
				return nil, err
			}
			switch group.name {
			case "project":
				item.ProjectID = value
			case "principal":
				item.PrincipalID = value
			case "key":
				item.KeyID = value
			case "provider":
				item.Provider = value
			case "model":
				item.Model = value
			}
			items = append(items, item)
		}
		rows.Close()
		groups[group.name] = items
	}
	// Ensure the result can always be serialized before returning it to the API.
	if _, err := json.Marshal(groups); err != nil {
		return nil, err
	}
	return map[string]any{"totals": total, "groups": groups}, nil
}

func PrincipalUsageStats(since int64, principalID string) (map[string]any, error) {
	db, err := DB()
	if err != nil {
		return nil, err
	}
	total, err := queryUsageTotals(db.QueryRow(`
SELECT COUNT(*),COALESCE(SUM(input_tokens),0),COALESCE(SUM(output_tokens),0),
       COALESCE(SUM(cost_microusd),0),COALESCE(SUM(credits_milli),0),
       COALESCE(SUM(CASE WHEN status_code>=400 THEN 1 ELSE 0 END),0),
       CAST(COALESCE(AVG(latency_ms),0) AS INTEGER)
FROM usage_events
WHERE ts>=? AND principal_id=? AND is_stub=0`, since, principalID))
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`
SELECT COALESCE(routed_model,''),COUNT(*),COALESCE(SUM(input_tokens),0),
       COALESCE(SUM(output_tokens),0),COALESCE(SUM(cost_microusd),0),
       COALESCE(SUM(credits_milli),0),
       COALESCE(SUM(CASE WHEN status_code>=400 THEN 1 ELSE 0 END),0),
       CAST(COALESCE(AVG(latency_ms),0) AS INTEGER)
FROM usage_events
WHERE ts>=? AND principal_id=? AND is_stub=0
GROUP BY routed_model ORDER BY COUNT(*) DESC LIMIT 100`, since, principalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	models := []UsageGroup{}
	for rows.Next() {
		var item UsageGroup
		if err := rows.Scan(
			&item.Model, &item.Requests, &item.InputTokens, &item.OutputTokens,
			&item.CostMicroUSD, &item.CreditsMilli, &item.Errors,
			&item.AverageLatency,
		); err != nil {
			return nil, err
		}
		models = append(models, item)
	}
	return map[string]any{"totals": total, "models": models}, rows.Err()
}

type queryRower interface {
	Scan(dest ...any) error
}

func queryUsageTotals(row queryRower) (UsageTotals, error) {
	var out UsageTotals
	err := row.Scan(
		&out.Requests, &out.InputTokens, &out.OutputTokens, &out.CostMicroUSD,
		&out.CreditsMilli, &out.Errors, &out.AverageLatency,
	)
	return out, err
}
