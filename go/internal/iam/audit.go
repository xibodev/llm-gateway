package iam

import (
	"encoding/json"
	"time"

	"llmgw/internal/diagnostics"
)

type AuditEvent struct {
	ID               int64          `json:"id"`
	Timestamp        int64          `json:"ts"`
	ActorPrincipalID string         `json:"actor_principal_id,omitempty"`
	ActorKeyID       string         `json:"actor_key_id,omitempty"`
	Action           string         `json:"action"`
	TargetType       string         `json:"target_type,omitempty"`
	TargetID         string         `json:"target_id,omitempty"`
	Result           string         `json:"result"`
	Detail           map[string]any `json:"detail"`
}

func RecordAudit(event AuditEvent) error {
	db, err := DB()
	if err != nil {
		return err
	}
	if event.Timestamp == 0 {
		event.Timestamp = time.Now().Unix()
	}
	if event.Result == "" {
		event.Result = "success"
	}
	raw, _ := json.Marshal(diagnostics.SanitizeStructuredValue(event.Detail))
	_, err = db.Exec(`
INSERT INTO audit_events(
 ts,actor_principal_id,actor_key_id,action,target_type,target_id,result,detail_json
) VALUES(?,?,?,?,?,?,?,?)`,
		event.Timestamp, nullable(event.ActorPrincipalID), nullable(event.ActorKeyID),
		event.Action, nullable(event.TargetType), nullable(event.TargetID),
		event.Result, string(raw),
	)
	return err
}

func ListAudit(limit int) ([]AuditEvent, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	db, err := DB()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`
SELECT id,ts,COALESCE(actor_principal_id,''),COALESCE(actor_key_id,''),
       action,COALESCE(target_type,''),COALESCE(target_id,''),result,detail_json
FROM audit_events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditEvent{}
	for rows.Next() {
		var event AuditEvent
		var detail string
		if err := rows.Scan(
			&event.ID, &event.Timestamp, &event.ActorPrincipalID, &event.ActorKeyID,
			&event.Action, &event.TargetType, &event.TargetID, &event.Result, &detail,
		); err != nil {
			return nil, err
		}
		event.Detail = sanitizedAuditDetail(detail)
		out = append(out, event)
	}
	return out, rows.Err()
}

// ListPrincipalAudit returns immutable, secret-free events authored by one human.
func ListPrincipalAudit(principalID string, limit int) ([]AuditEvent, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	db, err := DB()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`
SELECT id,ts,COALESCE(actor_principal_id,'') ,COALESCE(actor_key_id,'') ,
       action,COALESCE(target_type,'') ,COALESCE(target_id,'') ,result,detail_json
FROM audit_events WHERE actor_principal_id=? ORDER BY id DESC LIMIT ?`, principalID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditEvent{}
	for rows.Next() {
		var event AuditEvent
		var detail string
		if err := rows.Scan(
			&event.ID, &event.Timestamp, &event.ActorPrincipalID, &event.ActorKeyID,
			&event.Action, &event.TargetType, &event.TargetID, &event.Result, &detail,
		); err != nil {
			return nil, err
		}
		event.Detail = sanitizedAuditDetail(detail)
		out = append(out, event)
	}
	return out, rows.Err()
}

func sanitizedAuditDetail(raw string) map[string]any {
	detail := map[string]any{}
	if json.Unmarshal([]byte(raw), &detail) != nil {
		return map[string]any{}
	}
	sanitized, ok := diagnostics.SanitizeStructuredValue(detail).(map[string]any)
	if !ok || sanitized == nil {
		return map[string]any{}
	}
	return sanitized
}
