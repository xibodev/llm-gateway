package iam

import "time"

// quotaLimit is one limit a key or project policy can set: the metric it
// counts over one UTC window, and how a refusal's message names it.
type quotaLimit struct {
	field  string // the policy field that sets it
	metric string // what it counts, as quota alert rules name metrics
	period string // the window it counts over: minute, day or month
	label  string
	of     func(KeyPolicy) int64
}

// quotaLimits are the limits a policy can set, in the order a request is
// checked against them. Enforcement and the limit report both read them, so
// what the report shows is what refuses a request.
var quotaLimits = []quotaLimit{
	{"rpm", "requests", "minute", "requests/minute", func(p KeyPolicy) int64 { return int64(p.RPM) }},
	{"daily_requests", "requests", "day", "requests/day", func(p KeyPolicy) int64 { return int64(p.DailyRequests) }},
	{"monthly_requests", "requests", "month", "requests/month", func(p KeyPolicy) int64 { return int64(p.MonthlyRequests) }},
	{"daily_input_tokens", "input_tokens", "day", "input tokens/day", func(p KeyPolicy) int64 { return p.DailyInputTokens }},
	{"daily_output_tokens", "output_tokens", "day", "output tokens/day", func(p KeyPolicy) int64 { return p.DailyOutputTokens }},
	{"monthly_total_tokens", "total_tokens", "month", "total tokens/month", func(p KeyPolicy) int64 { return p.MonthlyTotalTokens }},
	{"daily_cost_microusd", "cost_microusd", "day", "estimated cost/day (micro-USD)", func(p KeyPolicy) int64 { return p.DailyCostMicroUSD }},
	{"monthly_cost_microusd", "cost_microusd", "month", "estimated cost/month (micro-USD)", func(p KeyPolicy) int64 { return p.MonthlyCostMicroUSD }},
	{"daily_credits_milli", "credits_milli", "day", "credits/day (milli)", func(p KeyPolicy) int64 { return p.DailyCreditsMilli }},
	{"monthly_credits_milli", "credits_milli", "month", "credits/month (milli)", func(p KeyPolicy) int64 { return p.MonthlyCreditsMilli }},
}

// LimitUsage is one limit that applies to a key or a project, with how much
// of it the window it counts has used.
type LimitUsage struct {
	// Scope is whose limit it is: "key" or "project", or "caller" for the
	// gateway's per-caller rate limit.
	Scope string `json:"scope"`
	// Field names the limit as policies do, such as "daily_requests".
	Field string `json:"field"`
	// Metric is what the limit counts, and Period the UTC window it counts
	// over: minute, day or month.
	Metric string `json:"metric"`
	Period string `json:"period"`
	Limit  int64  `json:"limit"`
	Used   int64  `json:"used"`
	// ResetsAt is when the window ends and the limit counts afresh, in Unix
	// seconds.
	ResetsAt int64 `json:"resets_at"`
	// Closest marks the limit nearest to refusing a request.
	Closest bool `json:"closest,omitempty"`
}

// KeyLimitUsage reports the limits that apply to key: those its own policy
// and its project's set, each with the usage of the window now falls in.
func KeyLimitUsage(key APIKey, now time.Time) ([]LimitUsage, error) {
	db, err := DB()
	if err != nil {
		return nil, err
	}
	// One transaction reads every counter as of the same moment.
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	counters, err := readKeyCountersTx(tx, key.ID, now)
	if err != nil {
		return nil, err
	}
	projectPolicy, err := projectPolicyTx(tx, key.ProjectID)
	if err != nil {
		return nil, err
	}
	projectCounters, err := readProjectCountersTx(tx, key.ProjectID, now)
	if err != nil {
		return nil, err
	}
	ends := quotaWindowEnds(now)
	return append(
		limitUsage("key", key.Policy, counters, ends),
		limitUsage("project", projectPolicy.KeyPolicy, projectCounters, ends)...,
	), nil
}

// ProjectLimitUsage reports the limits projectID's policy sets, each with the
// usage of the window now falls in.
func ProjectLimitUsage(projectID string, now time.Time) ([]LimitUsage, error) {
	db, err := DB()
	if err != nil {
		return nil, err
	}
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	policy, err := projectPolicyTx(tx, projectID)
	if err != nil {
		return nil, err
	}
	counters, err := readProjectCountersTx(tx, projectID, now)
	if err != nil {
		return nil, err
	}
	return limitUsage("project", policy.KeyPolicy, counters, quotaWindowEnds(now)), nil
}

// limitUsage lists the limits policy sets, in enforcement order, with what
// counters have used of each.
func limitUsage(scope string, policy KeyPolicy, counters quotaCounters, ends quotaWindows) []LimitUsage {
	limits := []LimitUsage{}
	for _, limit := range quotaLimits {
		value := limit.of(policy)
		if value <= 0 {
			continue
		}
		limits = append(limits, LimitUsage{
			Scope: scope, Field: limit.field, Metric: limit.metric, Period: limit.period,
			Limit: value, Used: metricValue(counters.in(limit.period), limit.metric),
			ResetsAt: ends.at(limit.period).Unix(),
		})
	}
	return limits
}

// MarkClosestLimit marks the limit nearest to refusing a request: the one
// that has used the largest share of itself and, of limits used up alike,
// the one whose window ends last, since it refuses longest. A limit counted
// past its end, as a token limit can be, is only used up. Nothing is marked
// while no limit has been used.
func MarkClosestLimit(limits []LimitUsage) {
	closest := -1
	for index := range limits {
		limits[index].Closest = false
		if limits[index].Used <= 0 {
			continue
		}
		if closest < 0 || nearerLimit(limits[index], limits[closest]) {
			closest = index
		}
	}
	if closest >= 0 {
		limits[closest].Closest = true
	}
}

func nearerLimit(a, b LimitUsage) bool {
	if shareA, shareB := usedShare(a), usedShare(b); shareA != shareB {
		return shareA > shareB
	}
	return a.ResetsAt > b.ResetsAt
}

func usedShare(limit LimitUsage) float64 {
	if limit.Limit <= 0 {
		return 0
	}
	return min(float64(limit.Used)/float64(limit.Limit), 1)
}
