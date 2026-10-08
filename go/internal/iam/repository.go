package iam

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// maxPrincipalNameRunes bounds the display name an administrator gives a
// principal.
const maxPrincipalNameRunes = 200

// ErrPrincipalNotFound reports a principal that does not exist.
var ErrPrincipalNotFound = errors.New("principal not found")

// CreatePrincipal creates a principal whose display name sign-ins may
// refresh, as an SSO sign-in provisions one.
func CreatePrincipal(kind, externalSubject, email, displayName string) (Principal, error) {
	return createPrincipal(kind, externalSubject, email, displayName, false)
}

// CreatePrincipalNamedByAdmin creates a principal with the display name an
// administrator chose, which sign-ins do not refresh.
func CreatePrincipalNamedByAdmin(kind, externalSubject, email, displayName string) (Principal, error) {
	return createPrincipal(kind, externalSubject, email, displayName, true)
}

func createPrincipal(kind, externalSubject, email, displayName string, nameSetByAdmin bool) (Principal, error) {
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
		Email: strings.TrimSpace(email), DisplayName: displayName, NameSetByAdmin: nameSetByAdmin,
		Status: "active", CreatedAt: now, UpdatedAt: now,
	}
	db, err := DB()
	if err != nil {
		return Principal{}, err
	}
	_, err = db.Exec(`
INSERT INTO principals(id,kind,external_subject,email,display_name,display_name_locked,status,created_at,updated_at)
VALUES(?,?,?,?,?,?,?,?,?)`,
		p.ID, p.Kind, nullable(p.ExternalSubject), nullable(p.Email), p.DisplayName,
		boolInt(p.NameSetByAdmin), p.Status, p.CreatedAt, p.UpdatedAt,
	)
	if err != nil {
		return Principal{}, fmt.Errorf("create principal: %w", err)
	}
	return p, nil
}

// RenamePrincipal gives principal id the display name an administrator
// chose and returns the principal before and after. Sign-ins no longer
// refresh the name of a principal an administrator named. The built-in
// system principal keeps its name.
func RenamePrincipal(id, displayName string) (Principal, Principal, error) {
	displayName = strings.TrimSpace(displayName)
	if displayName == "" {
		return Principal{}, Principal{}, fmt.Errorf("display name is required")
	}
	if utf8.RuneCountInString(displayName) > maxPrincipalNameRunes {
		return Principal{}, Principal{}, fmt.Errorf("display name is longer than %d characters", maxPrincipalNameRunes)
	}
	before, found, err := PrincipalByID(strings.TrimSpace(id))
	if err != nil {
		return Principal{}, Principal{}, err
	}
	if !found {
		return Principal{}, Principal{}, ErrPrincipalNotFound
	}
	if before.Kind == "system" {
		return Principal{}, Principal{}, fmt.Errorf("the built-in system principal keeps its name")
	}
	db, err := DB()
	if err != nil {
		return Principal{}, Principal{}, err
	}
	after := before
	after.DisplayName, after.NameSetByAdmin, after.UpdatedAt = displayName, true, time.Now().Unix()
	res, err := db.Exec(
		"UPDATE principals SET display_name=?,display_name_locked=1,updated_at=? WHERE id=?",
		after.DisplayName, after.UpdatedAt, after.ID,
	)
	if err != nil {
		return Principal{}, Principal{}, err
	}
	if err := requireAffected(res, "principal"); err != nil {
		return Principal{}, Principal{}, err
	}
	return before, after, nil
}

// RefreshPrincipalIdentity keeps principal p as its identity provider
// describes it at sign-in: a non-empty email replaces the stored one, and a
// non-empty display name the stored name, unless an administrator named the
// principal. Empty values change nothing, so a sign-in that reports less
// keeps what an earlier one reported. It writes only when something changed
// and returns the principal as stored.
func RefreshPrincipalIdentity(p Principal, email, displayName string) (Principal, error) {
	email, displayName = strings.TrimSpace(email), strings.TrimSpace(displayName)
	emailChanged := email != "" && email != p.Email
	nameChanged := displayName != "" && displayName != p.DisplayName && !p.NameSetByAdmin
	if !emailChanged && !nameChanged {
		return p, nil
	}
	db, err := DB()
	if err != nil {
		return p, err
	}
	next := p
	if emailChanged {
		next.Email = email
	}
	if nameChanged {
		next.DisplayName = displayName
	}
	next.UpdatedAt = time.Now().Unix()
	// The name changes only while no administrator has named the principal,
	// however a rename races this sign-in.
	if _, err := db.Exec(`
UPDATE principals SET email=?,
    display_name=CASE WHEN display_name_locked=0 THEN ? ELSE display_name END,
    updated_at=?
WHERE id=?`, nullable(next.Email), next.DisplayName, next.UpdatedAt, p.ID); err != nil {
		return p, err
	}
	stored, found, err := PrincipalByID(p.ID)
	if err != nil {
		return p, err
	}
	if !found {
		return p, ErrPrincipalNotFound
	}
	return stored, nil
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
SELECT id,kind,external_subject,email,display_name,display_name_locked,status,created_at,updated_at
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
SELECT id,kind,external_subject,email,display_name,display_name_locked,status,created_at,updated_at
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
SELECT id,kind,external_subject,email,display_name,display_name_locked,status,created_at,updated_at
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

// RemoveMembership removes a principal from a project and revokes the
// principal's keys in it. Key resolution already requires the membership, so
// the keys stop working either way; revoking them, disabled ones included,
// keeps a later SetMembership from making them usable again.
func RemoveMembership(projectID, principalID string) error {
	db, err := DB()
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(
		"DELETE FROM project_memberships WHERE project_id=? AND principal_id=?",
		projectID, principalID,
	); err != nil {
		return err
	}
	if _, err := tx.Exec(`
UPDATE api_keys SET status='revoked'
WHERE project_id=? AND principal_id=? AND status!='revoked'`,
		projectID, principalID,
	); err != nil {
		return err
	}
	return tx.Commit()
}

func ListMemberships(projectID string) ([]Membership, error) {
	return listMemberships("WHERE m.project_id=? ORDER BY m.role,m.principal_id", projectID)
}

func ListAllMemberships() ([]Membership, error) {
	return listMemberships("ORDER BY m.project_id,m.role,m.principal_id")
}

func ListPrincipalMemberships(principalID string) ([]Membership, error) {
	return listMemberships("WHERE m.principal_id=? ORDER BY m.role,m.project_id", principalID)
}

// listMemberships lists the memberships clause selects, in its order, each
// with its principal's name, kind and status.
func listMemberships(clause string, args ...any) ([]Membership, error) {
	db, err := DB()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`
SELECT m.project_id,m.principal_id,m.role,m.created_at,n.display_name,n.kind,n.status
FROM project_memberships m JOIN principals n ON n.id=m.principal_id `+clause, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Membership{}
	for rows.Next() {
		var m Membership
		if err := rows.Scan(&m.ProjectID, &m.PrincipalID, &m.Role, &m.CreatedAt, &m.PrincipalName, &m.PrincipalKind, &m.PrincipalStatus); err != nil {
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
	var nameLocked int
	err := row.Scan(
		&p.ID, &p.Kind, &subject, &email, &p.DisplayName, &nameLocked, &p.Status,
		&p.CreatedAt, &p.UpdatedAt,
	)
	p.ExternalSubject = subject.String
	p.Email = email.String
	p.NameSetByAdmin = nameLocked != 0
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
