package iam

import (
	"database/sql"
	"encoding/json"
	"time"
)

type ProjectPolicy struct {
	ProjectID string `json:"project_id"`
	KeyPolicy
	UpdatedAt int64 `json:"updated_at"`
}

func SetProjectPolicy(projectID string, policy KeyPolicy) (ProjectPolicy, error) {
	if err := validatePolicy(policy); err != nil {
		return ProjectPolicy{}, err
	}
	db, err := DB()
	if err != nil {
		return ProjectPolicy{}, err
	}
	models, _ := json.Marshal(policy.AllowedModels)
	providers, _ := json.Marshal(policy.AllowedProviders)
	now := time.Now().Unix()
	_, err = db.Exec(`
INSERT INTO project_policies(
 project_id,allowed_models_json,allowed_providers_json,rpm,daily_requests,
 monthly_requests,daily_input_tokens,daily_output_tokens,monthly_total_tokens,
 daily_cost_microusd,monthly_cost_microusd,daily_credits_milli,
 monthly_credits_milli,updated_at
) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(project_id) DO UPDATE SET
 allowed_models_json=excluded.allowed_models_json,
 allowed_providers_json=excluded.allowed_providers_json,rpm=excluded.rpm,
 daily_requests=excluded.daily_requests,monthly_requests=excluded.monthly_requests,
 daily_input_tokens=excluded.daily_input_tokens,
 daily_output_tokens=excluded.daily_output_tokens,
 monthly_total_tokens=excluded.monthly_total_tokens,
 daily_cost_microusd=excluded.daily_cost_microusd,
 monthly_cost_microusd=excluded.monthly_cost_microusd,
 daily_credits_milli=excluded.daily_credits_milli,
 monthly_credits_milli=excluded.monthly_credits_milli,
 updated_at=excluded.updated_at`,
		projectID, string(models), string(providers), policy.RPM,
		policy.DailyRequests, policy.MonthlyRequests, policy.DailyInputTokens,
		policy.DailyOutputTokens, policy.MonthlyTotalTokens,
		policy.DailyCostMicroUSD, policy.MonthlyCostMicroUSD,
		policy.DailyCreditsMilli, policy.MonthlyCreditsMilli, now,
	)
	if err != nil {
		return ProjectPolicy{}, err
	}
	return ProjectPolicy{ProjectID: projectID, KeyPolicy: policy, UpdatedAt: now}, nil
}

func GetProjectPolicy(projectID string) (ProjectPolicy, error) {
	db, err := DB()
	if err != nil {
		return ProjectPolicy{}, err
	}
	var policy ProjectPolicy
	var models, providers string
	policy.ProjectID = projectID
	err = db.QueryRow(`
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
