package iam

import (
	"strings"
	"time"
)

// KeyListFilter selects one page of API keys, in order.
type KeyListFilter struct {
	PrincipalID string
	ProjectID   string
	// Route keeps the keys whose allowed routes name it.
	Route string
	// Status is "usable", the default: active and disabled keys that have
	// not expired; "ended": revoked and expired ones; "active",
	// "disabled", "expired" or "revoked"; or "all".
	Status string
	// Search keeps the keys whose name, prefix or ID, project slug or name,
	// or owner's name holds it, ignoring case.
	Search string
	// Sort is "created", the default, "name", "project", "owner",
	// "status", "expires" or "last_used". Keys that tie stay newest first.
	Sort       string
	Descending bool
	// Limit is 1 to 200, 50 by default.
	Limit  int
	Offset int
	// Now is when keys are judged expired; the current time when zero.
	Now int64
}

// keyListOrder is the column each sort orders keys by. A key that never
// expires sorts after every key that does, and one never used before every
// key that was.
var keyListOrder = map[string]string{
	"created":   "k.created_at",
	"name":      "k.name COLLATE NOCASE",
	"project":   "p.slug COLLATE NOCASE",
	"owner":     "n.display_name COLLATE NOCASE",
	"status":    "k.status",
	"expires":   "COALESCE(k.expires_at, 9223372036854775807)",
	"last_used": "COALESCE(k.last_used_at, 0)",
}

// ListAPIKeysPage returns the page of keys filter selects and how many keys
// it selects in all. Deleted keys are never listed.
func ListAPIKeysPage(filter KeyListFilter) ([]APIKey, int, error) {
	if filter.Limit <= 0 || filter.Limit > 200 {
		filter.Limit = 50
	}
	if filter.Offset < 0 {
		filter.Offset = 0
	}
	if filter.Now == 0 {
		filter.Now = time.Now().Unix()
	}
	order, ok := keyListOrder[strings.TrimSpace(filter.Sort)]
	if filter.Sort == "" {
		order, ok = keyListOrder["created"], true
	}
	if !ok {
		return nil, 0, &InvalidFilterError{Message: "sort must be created, name, project, owner, status, expires or last_used"}
	}
	where := []string{"k.deleted_at IS NULL"}
	var args []any
	add := func(clause string, values ...any) {
		where = append(where, clause)
		args = append(args, values...)
	}
	expired := "(k.status<>'revoked' AND k.expires_at IS NOT NULL AND k.expires_at<=?)"
	switch strings.TrimSpace(filter.Status) {
	case "", "usable":
		add("k.status IN ('active','disabled') AND NOT "+expired, filter.Now)
	case "ended":
		add("(k.status='revoked' OR "+expired+")", filter.Now)
	case "active", "disabled":
		add("k.status=? AND NOT "+expired, filter.Status, filter.Now)
	case "expired":
		add(expired, filter.Now)
	case "revoked":
		add("k.status='revoked'")
	case "all":
	default:
		return nil, 0, &InvalidFilterError{Message: "status must be usable, ended, active, disabled, expired, revoked or all"}
	}
	if id := strings.TrimSpace(filter.PrincipalID); id != "" {
		add("k.principal_id=?", id)
	}
	if id := strings.TrimSpace(filter.ProjectID); id != "" {
		add("k.project_id=?", id)
	}
	if route := strings.TrimSpace(filter.Route); route != "" {
		add(`EXISTS (SELECT 1 FROM json_each(COALESCE(json_extract(k.scope_json,'$.allowed_routes'),'[]')) WHERE value=?)`, route)
	}
	if search := strings.ToLower(strings.TrimSpace(filter.Search)); search != "" {
		pattern := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(search) + "%"
		clauses := []string{}
		for _, column := range []string{"k.name", "k.prefix", "k.id", "p.slug", "p.name", "n.display_name"} {
			clauses = append(clauses, "LOWER("+column+`) LIKE ? ESCAPE '\'`)
			args = append(args, pattern)
		}
		where = append(where, "("+strings.Join(clauses, " OR ")+")")
	}
	db, err := DB()
	if err != nil {
		return nil, 0, err
	}
	from := `
FROM api_keys k
JOIN projects p ON p.id=k.project_id
JOIN principals n ON n.id=k.principal_id
WHERE ` + strings.Join(where, " AND ")
	var total int
	if err := db.QueryRow("SELECT COUNT(*)"+from, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	direction := "ASC"
	if filter.Descending {
		direction = "DESC"
	}
	// Keys created in the same second keep the order they were issued in.
	ordering := order + " " + direction + ",k.created_at DESC,k.rowid DESC"
	if order == keyListOrder["created"] {
		ordering = "k.created_at " + direction + ",k.rowid " + direction
	}
	rows, err := db.Query(`
SELECT k.id,k.prefix,k.project_id,p.slug,k.principal_id,n.display_name,n.kind,
       k.name,k.status,k.created_at,k.expires_at,k.last_used_at,
       k.allowed_models_json,k.allowed_providers_json,k.rpm,k.daily_requests,k.monthly_requests,
       daily_input_tokens,daily_output_tokens,monthly_total_tokens,daily_cost_microusd,
       monthly_cost_microusd,daily_credits_milli,monthly_credits_milli,
       CASE WHEN k.secret_ciphertext IS NOT NULL AND k.secret_nonce IS NOT NULL THEN 1 ELSE 0 END,k.scope_json`+
		from+" ORDER BY "+ordering+" LIMIT ? OFFSET ?",
		append(args, filter.Limit, filter.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	keys := []APIKey{}
	for rows.Next() {
		key, err := scanAPIKey(rows)
		if err != nil {
			return nil, 0, err
		}
		keys = append(keys, key)
	}
	return keys, total, rows.Err()
}
