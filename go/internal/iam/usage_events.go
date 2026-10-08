package iam

import (
	"strconv"
	"strings"
)

// usageWhere is the WHERE clause, with its arguments, of the usage rows
// filter selects: real requests at or after From, before To when it is
// set, and of the provider, routed model, key, project and principal it
// names.
func usageWhere(filter UsageTimeSeriesFilter) (string, []any) {
	where := "WHERE ts >= ? AND is_stub=0"
	args := []any{filter.From}
	if filter.To > 0 {
		where += " AND ts < ?"
		args = append(args, filter.To)
	}
	for _, item := range []struct{ column, value string }{
		{"provider", filter.Provider}, {"routed_model", filter.Model}, {"key_id", filter.KeyID},
		{"project_id", filter.ProjectID}, {"principal_id", filter.PrincipalID},
	} {
		if value := strings.TrimSpace(item.value); value != "" {
			where += " AND " + item.column + "=?"
			args = append(args, value)
		}
	}
	return where, args
}

// UsageEventFilter selects recorded requests, newest first: those at or
// after From and before To when they are set, of the provider, routed model,
// key, project, principal or request it names, of a Status of "ok" (below
// 400), "error" (400 and above) or one exact code, and older than BeforeID,
// the cursor of the next page. Limit is 1 to 200, 50 when unset.
type UsageEventFilter struct {
	From, To                                                  int64
	Provider, Model, KeyID, ProjectID, PrincipalID, RequestID string
	Status                                                    string
	BeforeID                                                  int64
	Limit                                                     int
}

// RecordedRequest is one recorded request.
type RecordedRequest struct {
	ID             int64  `json:"id"`
	Timestamp      int64  `json:"ts"`
	RequestID      string `json:"request_id"`
	Endpoint       string `json:"endpoint"`
	StatusCode     int    `json:"status_code"`
	LatencyMS      int64  `json:"latency_ms"`
	RequestedModel string `json:"requested_model,omitempty"`
	RoutedModel    string `json:"routed_model,omitempty"`
	Provider       string `json:"provider,omitempty"`
	ProjectID      string `json:"project_id,omitempty"`
	PrincipalID    string `json:"principal_id,omitempty"`
	KeyID          string `json:"key_id,omitempty"`
	// KeyName names the request's key, a deleted one included.
	KeyName      string `json:"key_name,omitempty"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	CostMicroUSD int64  `json:"cost_microusd"`
	CreditsMilli int64  `json:"credits_milli"`
	ErrorCode    string `json:"error_code,omitempty"`
	Stub         bool   `json:"stub,omitempty"`
}

// ListUsageEvents returns the recorded requests filter selects and the
// cursor of the next page, or 0 when no older one matches.
func ListUsageEvents(filter UsageEventFilter) ([]RecordedRequest, int64, error) {
	if filter.Limit <= 0 || filter.Limit > 200 {
		filter.Limit = 50
	}
	db, err := DB()
	if err != nil {
		return nil, 0, err
	}
	where := []string{"1=1"}
	var args []any
	add := func(clause string, value any) {
		where = append(where, clause)
		args = append(args, value)
	}
	if filter.From > 0 {
		add("ts >= ?", filter.From)
	}
	if filter.To > 0 {
		add("ts < ?", filter.To)
	}
	if filter.BeforeID > 0 {
		add("id < ?", filter.BeforeID)
	}
	for _, item := range []struct{ column, value string }{
		{"provider", filter.Provider}, {"routed_model", filter.Model}, {"key_id", filter.KeyID},
		{"project_id", filter.ProjectID}, {"principal_id", filter.PrincipalID}, {"request_id", filter.RequestID},
	} {
		if value := strings.TrimSpace(item.value); value != "" {
			add(item.column+"=?", value)
		}
	}
	switch status := strings.TrimSpace(filter.Status); status {
	case "":
	case "ok":
		where = append(where, "status_code < 400")
	case "error":
		where = append(where, "status_code >= 400")
	default:
		code, err := strconv.Atoi(status)
		if err != nil {
			return nil, 0, &InvalidFilterError{Message: "status must be ok, error or a status code"}
		}
		add("status_code=?", code)
	}
	rows, err := db.Query(`
SELECT id,ts,COALESCE(request_id,''),COALESCE(endpoint,''),COALESCE(status_code,0),COALESCE(latency_ms,0),
       COALESCE(requested_model,''),COALESCE(routed_model,''),COALESCE(provider,''),
       COALESCE(project_id,''),COALESCE(principal_id,''),COALESCE(key_id,''),
       COALESCE(input_tokens,0),COALESCE(output_tokens,0),COALESCE(cost_microusd,0),
       COALESCE(credits_milli,0),COALESCE(error_code,''),COALESCE(is_stub,0)
FROM usage_events WHERE `+strings.Join(where, " AND ")+` ORDER BY id DESC LIMIT ?`,
		append(args, filter.Limit+1)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	events := []RecordedRequest{}
	for rows.Next() {
		var event RecordedRequest
		var stub int
		if err := rows.Scan(
			&event.ID, &event.Timestamp, &event.RequestID, &event.Endpoint, &event.StatusCode, &event.LatencyMS,
			&event.RequestedModel, &event.RoutedModel, &event.Provider,
			&event.ProjectID, &event.PrincipalID, &event.KeyID,
			&event.InputTokens, &event.OutputTokens, &event.CostMicroUSD,
			&event.CreditsMilli, &event.ErrorCode, &stub,
		); err != nil {
			return nil, 0, err
		}
		event.Stub = stub != 0
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	keyIDs := make([]string, 0, len(events))
	for _, event := range events {
		keyIDs = append(keyIDs, event.KeyID)
	}
	keyNames, err := apiKeyNames(db, keyIDs)
	if err != nil {
		return nil, 0, err
	}
	for index := range events {
		events[index].KeyName = keyNames[events[index].KeyID]
	}
	return page(events, filter.Limit, func(event RecordedRequest) int64 { return event.ID })
}

// page trims rows, read one past limit, to limit, and returns the cursor of
// the next page: the ID of the last row kept, or 0 when no row followed.
func page[T any](rows []T, limit int, id func(T) int64) ([]T, int64, error) {
	if len(rows) <= limit {
		return rows, 0, nil
	}
	rows = rows[:limit]
	return rows, id(rows[len(rows)-1]), nil
}

// InvalidFilterError is a filter a listing cannot apply.
type InvalidFilterError struct{ Message string }

func (e *InvalidFilterError) Error() string { return e.Message }
