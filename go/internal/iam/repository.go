package iam

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

func CreatePrincipal(kind, externalSubject, email, displayName string) (Principal, error) {
	switch kind {
	case "human", "service", "system":
	default:
		return Principal{}, fmt.Errorf("invalid principal kind %q", kind)
	}
	displayName = strings.TrimSpace(displayName)
	if displayName == "" {
		return Principal{}, fmt.Errorf("display name is required")
	}
	id, err := newID("prn")
	if err != nil {
		return Principal{}, err
	}
	now := time.Now().Unix()
	p := Principal{
		ID: id, Kind: kind, ExternalSubject: strings.TrimSpace(externalSubject),
		Email: strings.TrimSpace(email), DisplayName: displayName,
		Status: "active", CreatedAt: now, UpdatedAt: now,
	}
	db, err := DB()
	if err != nil {
		return Principal{}, err
	}
	_, err = db.Exec(`
INSERT INTO principals(id,kind,external_subject,email,display_name,status,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?)`,
		p.ID, p.Kind, nullable(p.ExternalSubject), nullable(p.Email), p.DisplayName,
		p.Status, p.CreatedAt, p.UpdatedAt,
	)
	if err != nil {
		return Principal{}, fmt.Errorf("create principal: %w", err)
	}
	return p, nil
}

func EnsurePrincipalBySubject(kind, externalSubject, email, displayName string) (Principal, error) {
	externalSubject = strings.TrimSpace(externalSubject)
	if externalSubject == "" {
		return Principal{}, fmt.Errorf("external subject is required")
	}
	if p, ok, err := PrincipalBySubject(externalSubject); err != nil {
		return Principal{}, err
	} else if ok {
		return p, nil
	}
	created, err := CreatePrincipal(kind, externalSubject, email, displayName)
	if err == nil {
		return created, nil
	}
	// Concurrent first-login requests can race the unique external_subject
	// insert. If the other request won, load and return that principal.
	if existing, ok, readErr := PrincipalBySubject(externalSubject); readErr == nil && ok {
		return existing, nil
	}
	return Principal{}, err
}

func PrincipalBySubject(subject string) (Principal, bool, error) {
	db, err := DB()
	if err != nil {
		return Principal{}, false, err
	}
	row := db.QueryRow(`
SELECT id,kind,external_subject,email,display_name,status,created_at,updated_at
FROM principals WHERE external_subject=?`, strings.TrimSpace(subject))
	p, err := scanPrincipal(row)
	if err == sql.ErrNoRows {
		return Principal{}, false, nil
	}
	return p, err == nil, err
}

func PrincipalByID(id string) (Principal, bool, error) {
	db, err := DB()
	if err != nil {
		return Principal{}, false, err
	}
	row := db.QueryRow(`
SELECT id,kind,external_subject,email,display_name,status,created_at,updated_at
FROM principals WHERE id=?`, id)
	p, err := scanPrincipal(row)
	if err == sql.ErrNoRows {
		return Principal{}, false, nil
	}
	return p, err == nil, err
}

func ListPrincipals() ([]Principal, error) {
	db, err := DB()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`
SELECT id,kind,external_subject,email,display_name,status,created_at,updated_at
FROM principals ORDER BY display_name,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Principal{}
	for rows.Next() {
		p, err := scanPrincipal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func SetPrincipalStatus(id, status string) error {
	if status != "active" && status != "disabled" {
		return fmt.Errorf("invalid principal status %q", status)
	}
	db, err := DB()
	if err != nil {
		return err
	}
	res, err := db.Exec(
		"UPDATE principals SET status=?,updated_at=? WHERE id=?",
		status, time.Now().Unix(), id,
	)
	if err != nil {
		return err
	}
	return requireAffected(res, "principal")
}

func CreateProject(slug, name string) (Project, error) {
	slug, err := normalizeSlug(slug)
	if err != nil {
		return Project{}, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = slug
	}
	id, err := newID("prj")
	if err != nil {
		return Project{}, err
	}
	now := time.Now().Unix()
	p := Project{ID: id, Slug: slug, Name: name, Status: "active", CreatedAt: now, UpdatedAt: now}
	db, err := DB()
	if err != nil {
		return Project{}, err
	}
	_, err = db.Exec(`
INSERT INTO projects(id,slug,name,status,created_at,updated_at) VALUES(?,?,?,?,?,?)`,
		p.ID, p.Slug, p.Name, p.Status, p.CreatedAt, p.UpdatedAt,
	)
	if err != nil {
		return Project{}, fmt.Errorf("create project: %w", err)
	}
	return p, nil
}

func EnsureProject(slug, name string) (Project, error) {
	normalized, err := normalizeSlug(slug)
	if err != nil {
		return Project{}, err
	}
	if p, ok, err := ProjectBySlug(normalized); err != nil {
		return Project{}, err
	} else if ok {
		return p, nil
	}
	return CreateProject(normalized, name)
}

func ProjectBySlug(slug string) (Project, bool, error) {
	db, err := DB()
	if err != nil {
		return Project{}, false, err
	}
	row := db.QueryRow(`
SELECT id,slug,name,status,created_at,updated_at FROM projects WHERE slug=?`, slug)
	p, err := scanProject(row)
	if err == sql.ErrNoRows {
		return Project{}, false, nil
	}
	return p, err == nil, err
}

func ProjectByID(id string) (Project, bool, error) {
	db, err := DB()
	if err != nil {
		return Project{}, false, err
	}
	row := db.QueryRow(`
SELECT id,slug,name,status,created_at,updated_at FROM projects WHERE id=?`, id)
	p, err := scanProject(row)
	if err == sql.ErrNoRows {
		return Project{}, false, nil
	}
	return p, err == nil, err
}

func ListProjects() ([]Project, error) {
	db, err := DB()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`
SELECT id,slug,name,status,created_at,updated_at FROM projects ORDER BY name,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Project{}
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func SetProjectStatus(id, status string) error {
	if status != "active" && status != "disabled" {
		return fmt.Errorf("invalid project status %q", status)
	}
	db, err := DB()
	if err != nil {
		return err
	}
	res, err := db.Exec(
		"UPDATE projects SET status=?,updated_at=? WHERE id=?",
		status, time.Now().Unix(), id,
	)
	if err != nil {
		return err
	}
	return requireAffected(res, "project")
}

func SetMembership(projectID, principalID, role string) error {
	switch role {
	case "owner", "admin", "member", "viewer":
	default:
		return fmt.Errorf("invalid membership role %q", role)
	}
	db, err := DB()
	if err != nil {
		return err
	}
	_, err = db.Exec(`
INSERT INTO project_memberships(project_id,principal_id,role,created_at)
VALUES(?,?,?,?)
ON CONFLICT(project_id,principal_id) DO UPDATE SET role=excluded.role`,
		projectID, principalID, role, time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("set membership: %w", err)
	}
	return nil
}

func RemoveMembership(projectID, principalID string) error {
	db, err := DB()
	if err != nil {
		return err
	}
	_, err = db.Exec(
		"DELETE FROM project_memberships WHERE project_id=? AND principal_id=?",
		projectID, principalID,
	)
	return err
}

func ListMemberships(projectID string) ([]Membership, error) {
	db, err := DB()
	if err != nil {
		return nil, err
	}

	rows, err := db.Query(`
SELECT project_id,principal_id,role,created_at
FROM project_memberships WHERE project_id=? ORDER BY role,principal_id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Membership{}
	for rows.Next() {
		var m Membership
		if err := rows.Scan(&m.ProjectID, &m.PrincipalID, &m.Role, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func ListAllMemberships() ([]Membership, error) {
	db, err := DB()
	if err != nil {
		return nil, err
	}

	rows, err := db.Query(`
SELECT project_id,principal_id,role,created_at
FROM project_memberships ORDER BY project_id,role,principal_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Membership{}
	for rows.Next() {
		var m Membership
		if err := rows.Scan(&m.ProjectID, &m.PrincipalID, &m.Role, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func ListPrincipalMemberships(principalID string) ([]Membership, error) {
	db, err := DB()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`
SELECT project_id,principal_id,role,created_at
FROM project_memberships WHERE principal_id=? ORDER BY role,project_id`, principalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Membership{}
	for rows.Next() {
		var m Membership
		if err := rows.Scan(&m.ProjectID, &m.PrincipalID, &m.Role, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func MembershipRole(projectID, principalID string) (string, bool, error) {
	db, err := DB()
	if err != nil {
		return "", false, err
	}
	var role string
	err = db.QueryRow(`
SELECT role FROM project_memberships WHERE project_id=? AND principal_id=?`,
		projectID, principalID,
	).Scan(&role)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	return role, err == nil, err
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanPrincipal(row rowScanner) (Principal, error) {
	var p Principal
	var subject, email sql.NullString
	err := row.Scan(
		&p.ID, &p.Kind, &subject, &email, &p.DisplayName, &p.Status,
		&p.CreatedAt, &p.UpdatedAt,
	)
	p.ExternalSubject = subject.String
	p.Email = email.String
	return p, err
}

func scanProject(row rowScanner) (Project, error) {
	var p Project
	err := row.Scan(&p.ID, &p.Slug, &p.Name, &p.Status, &p.CreatedAt, &p.UpdatedAt)
	return p, err
}

func nullable(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func requireAffected(res sql.Result, entity string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%s not found", entity)
	}
	return nil
}
