package iam

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"llmgw/internal/config"
)

type KeyCreate struct {
	ProjectID   string
	PrincipalID string
	Name        string
	ExpiresAt   int64
	Policy      KeyPolicy
}

type KeyUpdate struct {
	Status    *string
	ExpiresAt *int64
	Policy    *KeyPolicy
}

var ErrAPIKeyNotRevealable = errors.New("API key was issued before encrypted key recovery was enabled")

func HasAPIKeys() (bool, error) {
	db, err := DB()
	if err != nil {
		return false, err
	}
	var count int
	if err := db.QueryRow(
		"SELECT COUNT(*) FROM api_keys WHERE status != 'revoked'",
	).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

// IssueKey creates a high-entropy API key. Authentication uses its SHA-256 hash;
// when credential encryption is configured, an encrypted copy supports explicit
// owner/admin reveal without weakening request authentication.
func IssueKey(in KeyCreate) (IssuedKey, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = "key"
	}
	if err := validatePolicy(in.Policy); err != nil {
		return IssuedKey{}, err
	}
	db, err := DB()
	if err != nil {
		return IssuedKey{}, err
	}
	var projectStatus, projectSlug, principalStatus, principalName, principalKind, role string
	err = db.QueryRow(`
SELECT p.status, p.slug, n.status, n.display_name, n.kind, m.role
FROM projects p
JOIN project_memberships m ON m.project_id=p.id
JOIN principals n ON n.id=m.principal_id
WHERE p.id=? AND n.id=?`, in.ProjectID, in.PrincipalID).Scan(
		&projectStatus, &projectSlug, &principalStatus, &principalName, &principalKind, &role,
	)
	if err == sql.ErrNoRows {
		return IssuedKey{}, fmt.Errorf("principal is not a member of the project")
	}
	if err != nil {
		return IssuedKey{}, err
	}
	if projectStatus != "active" || principalStatus != "active" {
		return IssuedKey{}, fmt.Errorf("project and principal must be active")
	}

	token, err := randomToken()
	if err != nil {
		return IssuedKey{}, err
	}
	id, err := newID("key")
	if err != nil {
		return IssuedKey{}, err
	}
	var ciphertext, nonce []byte
	if strings.TrimSpace(config.Get().CredentialEncryptionKey) != "" {
		key, err := credentialKey()
		if err != nil {
			return IssuedKey{}, fmt.Errorf("encrypt API key: %w", err)
		}
		ciphertext, nonce, err = encryptCredential(
			key, []byte(token), apiKeyAdditionalData(id, in.ProjectID, in.PrincipalID),
		)
		if err != nil {
			return IssuedKey{}, fmt.Errorf("encrypt API key: %w", err)
		}
	}
	sum := sha256.Sum256([]byte(token))
	models, _ := json.Marshal(in.Policy.AllowedModels)
	providers, _ := json.Marshal(in.Policy.AllowedProviders)
	now := time.Now().Unix()
	_, err = db.Exec(`
INSERT INTO api_keys(
    id,prefix,secret_hash,project_id,principal_id,name,status,created_at,expires_at,
    allowed_models_json,allowed_providers_json,rpm,daily_requests,monthly_requests,
    daily_input_tokens,daily_output_tokens,monthly_total_tokens,daily_cost_microusd,
    monthly_cost_microusd,daily_credits_milli,monthly_credits_milli,
    secret_ciphertext,secret_nonce
) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, displayPrefix(token), sum[:], in.ProjectID, in.PrincipalID, name, "active", now,
		nullInt(in.ExpiresAt), string(models), string(providers), in.Policy.RPM,
		in.Policy.DailyRequests, in.Policy.MonthlyRequests, in.Policy.DailyInputTokens,
		in.Policy.DailyOutputTokens, in.Policy.MonthlyTotalTokens, in.Policy.DailyCostMicroUSD,
		in.Policy.MonthlyCostMicroUSD, in.Policy.DailyCreditsMilli, in.Policy.MonthlyCreditsMilli,
		ciphertext, nonce,
	)
	if err != nil {
		return IssuedKey{}, fmt.Errorf("issue API key: %w", err)
	}
	key := APIKey{
		ID: id, Prefix: displayPrefix(token), ProjectID: in.ProjectID,
		Project: projectSlug, PrincipalID: in.PrincipalID, Principal: principalName,
		Kind: principalKind, Name: name, Status: "active",
		CreatedAt: now, ExpiresAt: in.ExpiresAt, Policy: in.Policy,
		Revealable: len(ciphertext) > 0,
	}
	return IssuedKey{APIKey: key, Token: token}, nil
}

func ResolveAPIKey(token string) (*config.Principal, bool, error) {
	if token == "" {
		return nil, false, nil
	}
	sum := sha256.Sum256([]byte(token))
	db, err := DB()
	if err != nil {
		return nil, false, err
	}
	var (
		storedHash, modelsJSON, providersJSON []byte
		keyID, keyName, keyStatus             string
		principalID, principalKind, pStatus   string
		projectID, projectSlug, projectStatus string
		role                                  string
		expiresAt                             sql.NullInt64
		rpm, daily, monthly                   int
		dailyIn, dailyOut, monthlyTokens      int64
		dailyCost, monthlyCost                int64
		dailyCredits, monthlyCredits          int64
	)
	err = db.QueryRow(`
SELECT k.secret_hash,k.id,k.name,k.status,k.expires_at,k.allowed_models_json,
       k.allowed_providers_json,k.rpm,k.daily_requests,k.monthly_requests,
       k.daily_input_tokens,k.daily_output_tokens,k.monthly_total_tokens,
       k.daily_cost_microusd,k.monthly_cost_microusd,
       k.daily_credits_milli,k.monthly_credits_milli,
       n.id,n.kind,n.status,p.id,p.slug,p.status,m.role
FROM api_keys k
JOIN principals n ON n.id=k.principal_id
JOIN projects p ON p.id=k.project_id
JOIN project_memberships m ON m.project_id=p.id AND m.principal_id=n.id
WHERE k.secret_hash=?`, sum[:]).Scan(
		&storedHash, &keyID, &keyName, &keyStatus, &expiresAt, &modelsJSON,
		&providersJSON, &rpm, &daily, &monthly, &dailyIn, &dailyOut, &monthlyTokens,
		&dailyCost, &monthlyCost, &dailyCredits, &monthlyCredits,
		&principalID, &principalKind, &pStatus,
		&projectID, &projectSlug, &projectStatus, &role,
	)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if subtle.ConstantTimeCompare(storedHash, sum[:]) != 1 {
		return nil, false, nil
	}
	if keyStatus != "active" || pStatus != "active" || projectStatus != "active" {
		return nil, false, nil
	}
	if expiresAt.Valid && expiresAt.Int64 > 0 && time.Now().Unix() >= expiresAt.Int64 {
		return nil, false, nil
	}
	var allowedModels, allowedProviders []string
	_ = json.Unmarshal(modelsJSON, &allowedModels)
	_ = json.Unmarshal(providersJSON, &allowedProviders)
	_, _ = db.Exec("UPDATE api_keys SET last_used_at=? WHERE id=?", time.Now().Unix(), keyID)
	return &config.Principal{
		PrincipalID: principalID, PrincipalKind: principalKind,
		ProjectID: projectID, Project: projectSlug, KeyID: keyID,
		Key: keyName, Role: role, Token: token,
		AllowedModels: allowedModels, AllowedProviders: allowedProviders,
		RPM: rpm, DailyRequests: daily, MonthlyRequests: monthly,
		DailyInputTokens: dailyIn, DailyOutputTokens: dailyOut,
		MonthlyTotalTokens: monthlyTokens,
		DailyCostMicroUSD:  dailyCost, MonthlyCostMicroUSD: monthlyCost,
		DailyCreditsMilli: dailyCredits, MonthlyCreditsMilli: monthlyCredits,
	}, true, nil
}

func ListAPIKeys(projectID string) ([]APIKey, error) {
	db, err := DB()
	if err != nil {
		return nil, err
	}

	query := `
SELECT k.id,k.prefix,k.project_id,p.slug,k.principal_id,n.display_name,n.kind,
       k.name,k.status,k.created_at,k.expires_at,k.last_used_at,
       k.allowed_models_json,k.allowed_providers_json,k.rpm,k.daily_requests,k.monthly_requests,
       daily_input_tokens,daily_output_tokens,monthly_total_tokens,daily_cost_microusd,
       monthly_cost_microusd,daily_credits_milli,monthly_credits_milli,
       CASE WHEN k.secret_ciphertext IS NOT NULL AND k.secret_nonce IS NOT NULL THEN 1 ELSE 0 END
FROM api_keys k
JOIN projects p ON p.id=k.project_id
JOIN principals n ON n.id=k.principal_id`
	args := []any{}
	if projectID != "" {
		query += " WHERE k.project_id=?"
		args = append(args, projectID)
	}
	query += " ORDER BY k.created_at DESC,k.id"
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []APIKey{}
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func ListPrincipalAPIKeys(principalID string) ([]APIKey, error) {
	db, err := DB()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`
SELECT k.id,k.prefix,k.project_id,p.slug,k.principal_id,n.display_name,n.kind,
       k.name,k.status,k.created_at,k.expires_at,k.last_used_at,
       k.allowed_models_json,k.allowed_providers_json,k.rpm,k.daily_requests,
       k.monthly_requests,k.daily_input_tokens,k.daily_output_tokens,
       k.monthly_total_tokens,k.daily_cost_microusd,k.monthly_cost_microusd,
       k.daily_credits_milli,k.monthly_credits_milli,
       CASE WHEN k.secret_ciphertext IS NOT NULL AND k.secret_nonce IS NOT NULL THEN 1 ELSE 0 END
FROM api_keys k
JOIN projects p ON p.id=k.project_id
JOIN principals n ON n.id=k.principal_id
WHERE k.principal_id=? ORDER BY k.created_at DESC,k.id`, principalID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []APIKey{}
	for rows.Next() {
		key, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, key)
	}
	return out, rows.Err()
}

func APIKeyByID(id string) (APIKey, bool, error) {
	db, err := DB()
	if err != nil {
		return APIKey{}, false, err
	}
	row := db.QueryRow(`
SELECT k.id,k.prefix,k.project_id,p.slug,k.principal_id,n.display_name,n.kind,
       k.name,k.status,k.created_at,k.expires_at,k.last_used_at,
       k.allowed_models_json,k.allowed_providers_json,k.rpm,k.daily_requests,k.monthly_requests,
       daily_input_tokens,daily_output_tokens,monthly_total_tokens,daily_cost_microusd,
       monthly_cost_microusd,daily_credits_milli,monthly_credits_milli,
       CASE WHEN k.secret_ciphertext IS NOT NULL AND k.secret_nonce IS NOT NULL THEN 1 ELSE 0 END
FROM api_keys k
JOIN projects p ON p.id=k.project_id
JOIN principals n ON n.id=k.principal_id
WHERE k.id=?`, id)
	k, err := scanAPIKey(row)
	if err == sql.ErrNoRows {
		return APIKey{}, false, nil
	}
	return k, err == nil, err
}

func UpdateAPIKey(id string, update KeyUpdate) error {
	key, ok, err := APIKeyByID(id)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("API key not found")
	}
	if update.Status != nil {
		if key.Status == "revoked" && *update.Status != "revoked" {
			return fmt.Errorf("revoked API keys cannot change status")
		}
		switch *update.Status {
		case "active", "disabled", "revoked":
			key.Status = *update.Status
		default:
			return fmt.Errorf("invalid key status %q", *update.Status)
		}
	}
	if update.ExpiresAt != nil {
		key.ExpiresAt = *update.ExpiresAt
	}
	if update.Policy != nil {
		if err := validatePolicy(*update.Policy); err != nil {
			return err
		}
		key.Policy = *update.Policy
	}
	return saveAPIKey(id, key)
}

func saveAPIKey(id string, key APIKey) error {
	models, _ := json.Marshal(key.Policy.AllowedModels)
	providers, _ := json.Marshal(key.Policy.AllowedProviders)
	db, err := DB()
	if err != nil {
		return err
	}
	result, err := db.Exec(`
UPDATE api_keys SET status=?,expires_at=?,allowed_models_json=?,allowed_providers_json=?,
 rpm=?,daily_requests=?,monthly_requests=?,daily_input_tokens=?,daily_output_tokens=?,
 monthly_total_tokens=?,daily_cost_microusd=?,monthly_cost_microusd=?,
 daily_credits_milli=?,monthly_credits_milli=?
 WHERE id=? AND (status != 'revoked' OR ? = 'revoked')`,
		key.Status, nullInt(key.ExpiresAt), string(models), string(providers),
		key.Policy.RPM, key.Policy.DailyRequests, key.Policy.MonthlyRequests,
		key.Policy.DailyInputTokens, key.Policy.DailyOutputTokens,
		key.Policy.MonthlyTotalTokens, key.Policy.DailyCostMicroUSD,
		key.Policy.MonthlyCostMicroUSD, key.Policy.DailyCreditsMilli,
		key.Policy.MonthlyCreditsMilli, id, key.Status,
	)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("revoked API keys cannot change status")
	}
	return nil
}

func RevokeAPIKey(id string) error {
	status := "revoked"
	return UpdateAPIKey(id, KeyUpdate{Status: &status})
}

// RevealAPIKey decrypts a stored token. The boolean reports whether the key ID
// exists; older hash-only keys return ErrAPIKeyNotRevealable.
func RevealAPIKey(id string) (string, bool, error) {
	db, err := DB()
	if err != nil {
		return "", false, err
	}
	var projectID, principalID string
	var storedHash, ciphertext, nonce []byte
	err = db.QueryRow(`
SELECT project_id,principal_id,secret_hash,secret_ciphertext,secret_nonce
FROM api_keys WHERE id=?`, strings.TrimSpace(id)).Scan(
		&projectID, &principalID, &storedHash, &ciphertext, &nonce,
	)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if len(ciphertext) == 0 || len(nonce) == 0 {
		return "", true, ErrAPIKeyNotRevealable
	}
	key, err := credentialKey()
	if err != nil {
		return "", true, err
	}
	plaintext, err := decryptCredential(
		key, ciphertext, nonce, apiKeyAdditionalData(id, projectID, principalID),
	)
	if err != nil {
		return "", true, fmt.Errorf("decrypt API key: %w", err)
	}
	sum := sha256.Sum256(plaintext)
	if subtle.ConstantTimeCompare(storedHash, sum[:]) != 1 {
		return "", true, fmt.Errorf("decrypted API key does not match its authentication hash")
	}
	return string(plaintext), true, nil
}

func apiKeyAdditionalData(id, projectID, principalID string) []byte {
	return []byte("api-key|" + id + "|" + projectID + "|" + principalID)
}

func scanAPIKey(row rowScanner) (APIKey, error) {
	var (
		k                         APIKey
		expiresAt, lastUsedAt     sql.NullInt64
		modelsJSON, providersJSON string
		revealable                int
	)
	err := row.Scan(
		&k.ID, &k.Prefix, &k.ProjectID, &k.Project, &k.PrincipalID, &k.Principal,
		&k.Kind, &k.Name, &k.Status, &k.CreatedAt, &expiresAt, &lastUsedAt,
		&modelsJSON, &providersJSON,
		&k.Policy.RPM, &k.Policy.DailyRequests, &k.Policy.MonthlyRequests,
		&k.Policy.DailyInputTokens, &k.Policy.DailyOutputTokens,
		&k.Policy.MonthlyTotalTokens, &k.Policy.DailyCostMicroUSD,
		&k.Policy.MonthlyCostMicroUSD, &k.Policy.DailyCreditsMilli,
		&k.Policy.MonthlyCreditsMilli, &revealable,
	)
	if err != nil {
		return APIKey{}, err
	}
	k.ExpiresAt = expiresAt.Int64
	k.LastUsedAt = lastUsedAt.Int64
	_ = json.Unmarshal([]byte(modelsJSON), &k.Policy.AllowedModels)
	_ = json.Unmarshal([]byte(providersJSON), &k.Policy.AllowedProviders)
	k.Expired = k.IsExpired()
	k.Revealable = revealable != 0
	return k, nil
}

func validatePolicy(p KeyPolicy) error {
	if p.RPM < 0 || p.DailyRequests < 0 || p.MonthlyRequests < 0 ||
		p.DailyInputTokens < 0 || p.DailyOutputTokens < 0 ||
		p.MonthlyTotalTokens < 0 || p.DailyCostMicroUSD < 0 ||
		p.MonthlyCostMicroUSD < 0 || p.DailyCreditsMilli < 0 ||
		p.MonthlyCreditsMilli < 0 {
		return fmt.Errorf("quota values cannot be negative")
	}
	return nil
}

func randomToken() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "llmgw_" + base64.RawURLEncoding.EncodeToString(buf), nil
}

func nullInt(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}
