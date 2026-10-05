package iam

import (
	"encoding/json"
	"strings"
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
	events, _, err := ListAuditFiltered(AuditFilter{Limit: limit})
	return events, err
}

// AuditFilter selects audit events, newest first: those whose action starts
// with Action, with an exact Result, whose actor principal or key is Actor,
// with an exact TargetType and TargetID, at or after From and before To when
// they are set, and older than BeforeID, the cursor of the next page. Limit
// is 1 to 1000, 100 when unset.
type AuditFilter struct {
	Action, Result, Actor, TargetType, TargetID string
	From, To, BeforeID                          int64
	Limit                                       int
}

// ListAuditFiltered returns the events filter selects and the cursor of the
// next page, or 0 when no older event matches.
func ListAuditFiltered(filter AuditFilter) ([]AuditEvent, int64, error) {
	if filter.Limit <= 0 || filter.Limit > 1000 {
		filter.Limit = 100
	}
	db, err := DB()
	if err != nil {
		return nil, 0, err
	}
	where := []string{"1=1"}
	var args []any
	add := func(clause string, values ...any) {
		where = append(where, clause)
		args = append(args, values...)
	}
	if action := strings.TrimSpace(filter.Action); action != "" {
		escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(action)
		add(`action LIKE ? ESCAPE '\'`, escaped+"%")
	}
	if actor := strings.TrimSpace(filter.Actor); actor != "" {
		add("(actor_principal_id=? OR actor_key_id=?)", actor, actor)
	}
	for _, item := range []struct{ column, value string }{
		{"result", filter.Result}, {"target_type", filter.TargetType}, {"target_id", filter.TargetID},
	} {
		if value := strings.TrimSpace(item.value); value != "" {
			add(item.column+"=?", value)
		}
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
	rows, err := db.Query(`
SELECT id,ts,COALESCE(actor_principal_id,''),COALESCE(actor_key_id,''),
       action,COALESCE(target_type,''),COALESCE(target_id,''),result,detail_json
FROM audit_events WHERE `+strings.Join(where, " AND ")+` ORDER BY id DESC LIMIT ?`, append(args, filter.Limit+1)...)
	if err != nil {
		return nil, 0, err
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
			return nil, 0, err
		}
		event.Detail = sanitizedAuditDetail(detail)
		out = append(out, event)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return page(out, filter.Limit, func(event AuditEvent) int64 { return event.ID })
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
