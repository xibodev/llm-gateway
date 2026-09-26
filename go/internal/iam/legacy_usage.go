package iam

import (
	"database/sql"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"llmgw/internal/config"
)

const legacyUsageMigration = "legacy_usage_db_v1"

func MigrateLegacyUsage() (int, error) {
	path := filepath.Join(config.StateDir(), "usage.db")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	target, err := DB()
	if err != nil {
		return 0, err
	}
	var done int
	if err := target.QueryRow(
		"SELECT COUNT(*) FROM control_metadata WHERE key=?", legacyUsageMigration,
	).Scan(&done); err != nil {
		return 0, err
	}
	if done != 0 {
		return 0, nil
	}
	source, err := sql.Open("sqlite", path)
	if err != nil {
		return 0, err
	}
	defer source.Close()
	rows, err := source.Query(`
SELECT id,ts,endpoint,requested_model,routed_model,provider,project,key_name,
       input_tokens,output_tokens,cost_usd,is_stub
FROM usage_ledger ORDER BY id`)
	if err != nil {
		// Older/local state may have an empty SQLite file without the ledger.
		return 0, nil
	}
	defer rows.Close()
	tx, err := target.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	count := 0
	for rows.Next() {
		var (
			id, ts, inputTokens, outputTokens, isStub int64
			endpoint, requested, routed, provider     sql.NullString
			projectSlug, keyName                      sql.NullString
			cost                                      sql.NullFloat64
		)
		if err := rows.Scan(
			&id, &ts, &endpoint, &requested, &routed, &provider, &projectSlug,
			&keyName, &inputTokens, &outputTokens, &cost, &isStub,
		); err != nil {
			return 0, err
		}
		projectID, principalID, keyID, err := legacyUsageIdentityTx(
			tx, projectSlug.String, keyName.String,
		)
		if err != nil {
			return 0, err
		}
		credits := int64(0)
		if isStub == 0 {
			credits = 1000
		}
		costMicro := int64(math.Round(cost.Float64 * 1_000_000))
		res, err := tx.Exec(`
INSERT OR IGNORE INTO usage_events(
    request_id,ts,endpoint,status_code,latency_ms,requested_model,routed_model,
    provider,project_id,principal_id,key_id,input_tokens,output_tokens,
    cost_microusd,credits_milli,is_stub
) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			fmt.Sprintf("legacy-usage:%d", id), ts, endpoint.String, 200, 0,
			nullable(requested.String), nullable(routed.String), nullable(provider.String),
			nullable(projectID), nullable(principalID), nullable(keyID), inputTokens,
			outputTokens, costMicro, credits, isStub,
		)
		if err != nil {
			return 0, err
		}
		affected, _ := res.RowsAffected()
		if affected == 0 {
			continue
		}
		count++
		if keyID != "" && isStub == 0 {
			at := time.Unix(ts, 0).UTC()
			_, dayStart, monthStart := quotaPeriods(at)
			for _, period := range []struct {
				name  string
				start int64
			}{{"day", dayStart}, {"month", monthStart}} {
				if _, err := tx.Exec(`
INSERT INTO quota_counters(
    key_id,period,period_start,requests,input_tokens,output_tokens,cost_microusd,credits_milli
) VALUES(?,?,?,?,?,?,?,?)
ON CONFLICT(key_id,period,period_start) DO UPDATE SET
    requests=requests+1,
    input_tokens=input_tokens+excluded.input_tokens,
    output_tokens=output_tokens+excluded.output_tokens,
    cost_microusd=cost_microusd+excluded.cost_microusd,
    credits_milli=credits_milli+excluded.credits_milli`,
					keyID, period.name, period.start, 1, inputTokens, outputTokens,
					costMicro, credits,
				); err != nil {
					return 0, err
				}
			}
			if projectID != "" && isStub == 0 {
				at := time.Unix(ts, 0).UTC()
				_, dayStart, monthStart := quotaPeriods(at)
				for _, period := range []struct {
					name  string
					start int64
				}{{"day", dayStart}, {"month", monthStart}} {
					if _, err := tx.Exec(`
INSERT INTO project_quota_counters(
    project_id,period,period_start,requests,input_tokens,output_tokens,cost_microusd,credits_milli
) VALUES(?,?,?,?,?,?,?,?)
ON CONFLICT(project_id,period,period_start) DO UPDATE SET
    requests=requests+1,
    input_tokens=input_tokens+excluded.input_tokens,
    output_tokens=output_tokens+excluded.output_tokens,
    cost_microusd=cost_microusd+excluded.cost_microusd,
    credits_milli=credits_milli+excluded.credits_milli`,
						projectID, period.name, period.start, 1, inputTokens, outputTokens,
						costMicro, credits,
					); err != nil {
						return 0, err
					}
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`
INSERT INTO control_metadata(key,value,updated_at) VALUES(?,?,?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`,
		legacyUsageMigration, "done", time.Now().Unix(),
	); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}

func legacyUsageIdentityTx(
	tx *sql.Tx, rawProject, keyName string,
) (projectID, principalID, keyID string, err error) {
	if rawProject == "" {
		return "", "", "", nil
	}
	slug, err := normalizeSlug(rawProject)
	if err != nil {
		return "", "", "", nil
	}
	err = tx.QueryRow("SELECT id FROM projects WHERE slug=?", slug).Scan(&projectID)
	if err == sql.ErrNoRows {
		projectID, _, err = ensureProjectTx(tx, slug, slug)
	}
	if err != nil {
		return "", "", "", err
	}
	if keyName == "" {
		return projectID, "", "", nil
	}
	err = tx.QueryRow(`
SELECT k.id,k.principal_id
FROM api_keys k
WHERE k.project_id=? AND k.name=?
ORDER BY k.created_at LIMIT 1`, projectID, keyName).Scan(&keyID, &principalID)
	if err == sql.ErrNoRows {
		return projectID, "", "", nil
	}
	return projectID, principalID, keyID, err
}
