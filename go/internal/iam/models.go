package iam

import "time"

type Principal struct {
	ID              string `json:"id"`
	Kind            string `json:"kind"`
	ExternalSubject string `json:"external_subject,omitempty"`
	Email           string `json:"email,omitempty"`
	DisplayName     string `json:"display_name"`
	Status          string `json:"status"`
	CreatedAt       int64  `json:"created_at"`
	UpdatedAt       int64  `json:"updated_at"`
}

type Project struct {
	ID        string `json:"id"`
	Slug      string `json:"slug"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
}

type Membership struct {
	ProjectID   string `json:"project_id"`
	PrincipalID string `json:"principal_id"`
	Role        string `json:"role"`
	CreatedAt   int64  `json:"created_at"`
}

type KeyPolicy struct {
	AllowedModels       []string `json:"allowed_models,omitempty"`
	AllowedProviders    []string `json:"allowed_providers,omitempty"`
	RPM                 int      `json:"rpm,omitempty"`
	DailyRequests       int      `json:"daily_requests,omitempty"`
	MonthlyRequests     int      `json:"monthly_requests,omitempty"`
	DailyInputTokens    int64    `json:"daily_input_tokens,omitempty"`
	DailyOutputTokens   int64    `json:"daily_output_tokens,omitempty"`
	MonthlyTotalTokens  int64    `json:"monthly_total_tokens,omitempty"`
	DailyCostMicroUSD   int64    `json:"daily_cost_microusd,omitempty"`
	MonthlyCostMicroUSD int64    `json:"monthly_cost_microusd,omitempty"`
	DailyCreditsMilli   int64    `json:"daily_credits_milli,omitempty"`
	MonthlyCreditsMilli int64    `json:"monthly_credits_milli,omitempty"`
}

type APIKey struct {
	ID          string    `json:"id"`
	Prefix      string    `json:"prefix"`
	ProjectID   string    `json:"project_id"`
	Project     string    `json:"project"`
	PrincipalID string    `json:"principal_id"`
	Principal   string    `json:"principal"`
	Kind        string    `json:"principal_kind"`
	Name        string    `json:"name"`
	Status      string    `json:"status"`
	CreatedAt   int64     `json:"created_at"`
	ExpiresAt   int64     `json:"expires_at,omitempty"`
	LastUsedAt  int64     `json:"last_used_at,omitempty"`
	Policy      KeyPolicy `json:"policy"`
	Expired     bool      `json:"expired"`
}

type IssuedKey struct {
	APIKey
	Token string `json:"token"`
}

func (k APIKey) IsExpired() bool {
	return k.ExpiresAt > 0 && time.Now().Unix() >= k.ExpiresAt
}
