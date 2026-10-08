package iam

import "strings"

// PrincipalListFilter selects one page of principals, in order.
type PrincipalListFilter struct {
	// IDs keeps the principals with these IDs.
	IDs []string
	// Kinds keeps the principals of these kinds: human, service or system.
	Kinds []string
	// Status is "all", the default, "active" or "disabled".
	Status string
	// ProjectID keeps the members of a project.
	ProjectID string
	// Search keeps the principals whose name, email, ID or external subject
	// holds it, ignoring case.
	Search string
	// Sort is "name", the default, "kind", "email", "status" or "created".
	// Principals that tie stay in name order.
	Sort       string
	Descending bool
	// Limit is 1 to 200, 50 by default.
	Limit  int
	Offset int
}

// principalListOrder is the column each sort orders principals by.
var principalListOrder = map[string]string{
	"name":    "n.display_name COLLATE NOCASE",
	"kind":    "n.kind",
	"email":   "COALESCE(n.email,'') COLLATE NOCASE",
	"status":  "n.status",
	"created": "n.created_at",
}

// ListPrincipalsPage returns the page of principals filter selects and how
// many principals it selects in all.
func ListPrincipalsPage(filter PrincipalListFilter) ([]Principal, int, error) {
	if filter.Limit <= 0 || filter.Limit > 200 {
		filter.Limit = 50
	}
	if filter.Offset < 0 {
		filter.Offset = 0
	}
	sort := strings.TrimSpace(filter.Sort)
	if sort == "" {
		sort = "name"
	}
	order, ok := principalListOrder[sort]
	if !ok {
		return nil, 0, &InvalidFilterError{Message: "sort must be name, kind, email, status or created"}
	}
	where := []string{"1=1"}
	var args []any
	add := func(clause string, values ...any) {
		where = append(where, clause)
		args = append(args, values...)
	}
	if ids := nonBlank(filter.IDs); len(ids) > 0 {
		add("n.id IN ("+placeholders(len(ids))+")", ids...)
	}
	if kinds := nonBlank(filter.Kinds); len(kinds) > 0 {
		for _, kind := range kinds {
			if kind != "human" && kind != "service" && kind != "system" {
				return nil, 0, &InvalidFilterError{Message: "kind must be human, service or system"}
			}
		}
		add("n.kind IN ("+placeholders(len(kinds))+")", kinds...)
	}
	switch status := strings.TrimSpace(filter.Status); status {
	case "", "all":
	case "active", "disabled":
		add("n.status=?", status)
	default:
		return nil, 0, &InvalidFilterError{Message: "status must be active, disabled or all"}
	}
	if project := strings.TrimSpace(filter.ProjectID); project != "" {
		add("EXISTS (SELECT 1 FROM project_memberships m WHERE m.principal_id=n.id AND m.project_id=?)", project)
	}
	if search := strings.ToLower(strings.TrimSpace(filter.Search)); search != "" {
		pattern := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(search) + "%"
		clauses := []string{}
		for _, column := range []string{"n.display_name", "COALESCE(n.email,'')", "n.id", "COALESCE(n.external_subject,'')"} {
			clauses = append(clauses, "LOWER("+column+`) LIKE ? ESCAPE '\'`)
			args = append(args, pattern)
		}
		where = append(where, "("+strings.Join(clauses, " OR ")+")")
	}
	db, err := DB()
	if err != nil {
		return nil, 0, err
	}
	from := " FROM principals n WHERE " + strings.Join(where, " AND ")
	var total int
	if err := db.QueryRow("SELECT COUNT(*)"+from, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	direction := "ASC"
	if filter.Descending {
		direction = "DESC"
	}
	rows, err := db.Query(`
SELECT n.id,n.kind,n.external_subject,n.email,n.display_name,n.display_name_locked,n.status,n.created_at,n.updated_at`+
		from+" ORDER BY "+order+" "+direction+",n.display_name COLLATE NOCASE,n.id LIMIT ? OFFSET ?",
		append(args, filter.Limit, filter.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	principals := []Principal{}
	for rows.Next() {
		principal, err := scanPrincipal(rows)
		if err != nil {
			return nil, 0, err
		}
		principals = append(principals, principal)
	}
	return principals, total, rows.Err()
}

// IdentityCounts are the counts of keys and principals the console shows in
// place of their lists.
type IdentityCounts struct {
	// Keys counts the keys that are not deleted, whatever their status.
	Keys int `json:"keys"`
	// Principals counts every principal, and ActivePrincipals the active
	// ones a project can take as members: people and service principals.
	Principals       int `json:"principals"`
	ActivePrincipals int `json:"active_principals"`
	// ActiveHumans counts the active people, and ProjectOwners those of them
	// who own or administer an active project.
	ActiveHumans  int `json:"active_humans"`
	ProjectOwners int `json:"project_owners"`
}

// CountIdentities counts the keys and principals the console shows.
func CountIdentities() (IdentityCounts, error) {
	db, err := DB()
	if err != nil {
		return IdentityCounts{}, err
	}
	var counts IdentityCounts
	err = db.QueryRow(`
SELECT (SELECT COUNT(*) FROM api_keys WHERE deleted_at IS NULL),
       (SELECT COUNT(*) FROM principals),
       (SELECT COUNT(*) FROM principals WHERE status='active' AND kind IN ('human','service')),
       (SELECT COUNT(*) FROM principals WHERE status='active' AND kind='human'),
       (SELECT COUNT(DISTINCT n.id) FROM principals n
          JOIN project_memberships m ON m.principal_id=n.id AND m.role IN ('owner','admin')
          JOIN projects p ON p.id=m.project_id AND p.status='active'
         WHERE n.status='active' AND n.kind='human')`).Scan(
		&counts.Keys, &counts.Principals, &counts.ActivePrincipals, &counts.ActiveHumans, &counts.ProjectOwners)
	return counts, err
}

// nonBlank returns values without the blank ones, trimmed, as query
// arguments.
func nonBlank(values []string) []any {
	out := []any{}
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			out = append(out, value)
		}
	}
	return out
}

// placeholders returns n comma-separated query placeholders.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
