package api

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"llmgw/internal/iam"
)

// adminKeyRow is a key as administrators read it: its policy beside its
// other fields rather than nested under policy.
func adminKeyRow(k iam.APIKey) map[string]any {
	return map[string]any{
		"id": k.ID, "prefix": k.Prefix, "project_id": k.ProjectID,
		"project": k.Project, "principal_id": k.PrincipalID,
		"principal": k.Principal, "principal_kind": k.Kind,
		"name": k.Name, "status": k.Status, "disabled": k.Status == "disabled", "revoked": k.Status == "revoked",
		"expired": k.Expired, "created": k.CreatedAt, "expires_at": k.ExpiresAt,
		"last_used_at":      k.LastUsedAt,
		"allowed_models":    k.Policy.AllowedModels,
		"allowed_routes":    k.Policy.AllowedRoutes,
		"routes_only":       k.Policy.RoutesOnly,
		"admin_managed":     k.Policy.AdminManaged,
		"allowed_providers": k.Policy.AllowedProviders,
		"rpm":               k.Policy.RPM, "daily_requests": k.Policy.DailyRequests,
		"monthly_requests":      k.Policy.MonthlyRequests,
		"daily_input_tokens":    k.Policy.DailyInputTokens,
		"daily_output_tokens":   k.Policy.DailyOutputTokens,
		"monthly_total_tokens":  k.Policy.MonthlyTotalTokens,
		"daily_cost_microusd":   k.Policy.DailyCostMicroUSD,
		"monthly_cost_microusd": k.Policy.MonthlyCostMicroUSD,
		"daily_credits_milli":   k.Policy.DailyCreditsMilli,
		"monthly_credits_milli": k.Policy.MonthlyCreditsMilli,
		"revealable":            k.Revealable,
	}
}

// keyListFilter reads a key listing's query: status, principal_id,
// project_id, route, q, sort, order (asc or desc), limit and offset. Without
// a sort, keys are listed newest first.
func keyListFilter(query url.Values) (iam.KeyListFilter, error) {
	filter := iam.KeyListFilter{
		Status: query.Get("status"), PrincipalID: query.Get("principal_id"), ProjectID: query.Get("project_id"),
		Route: query.Get("route"), Search: query.Get("q"), Sort: strings.TrimSpace(query.Get("sort")),
	}
	switch order := strings.TrimSpace(query.Get("order")); order {
	case "":
		filter.Descending = filter.Sort == ""
	case "asc", "desc":
		filter.Descending = order == "desc"
	default:
		return filter, errors.New("order must be asc or desc")
	}
	for name, target := range map[string]*int{"limit": &filter.Limit, "offset": &filter.Offset} {
		value := strings.TrimSpace(query.Get(name))
		if value == "" {
			continue
		}
		number, err := strconv.Atoi(value)
		if err != nil || number < 0 {
			return filter, errors.New(name + " must be a whole number")
		}
		*target = number
	}
	return filter, nil
}

// listKeys answers the page of keys filter selects, each as row renders it.
func listKeys(w http.ResponseWriter, filter iam.KeyListFilter, row func(iam.APIKey) any) {
	keys, total, err := iam.ListAPIKeysPage(filter)
	var invalid *iam.InvalidFilterError
	switch {
	case errors.As(err, &invalid):
		writeError(w, http.StatusBadRequest, invalid.Message)
		return
	case err != nil:
		writeError(w, 500, "Identity store unavailable.")
		return
	}
	rows := make([]any, 0, len(keys))
	for _, key := range keys {
		rows = append(rows, row(key))
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": rows, "total": total})
}

// handleListKeys answers a page of the gateway's keys.
func handleListKeys(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	filter, err := keyListFilter(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	listKeys(w, filter, func(key iam.APIKey) any { return adminKeyRow(key) })
}

// handleUserListKeys answers a page of the signed-in user's keys.
func handleUserListKeys(w http.ResponseWriter, r *http.Request) {
	principal, ok := requireSSOUser(w, r)
	if !ok {
		return
	}
	filter, err := keyListFilter(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	filter.PrincipalID = principal.ID
	listKeys(w, filter, func(key iam.APIKey) any { return key })
}
