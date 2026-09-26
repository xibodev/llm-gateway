package api

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"llmgw/internal/iam"
)

func handleListAlerts(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	rules, err := iam.ListAlertRules()
	if err != nil {
		writeError(w, 500, "Alert store unavailable.")
		return
	}
	writeJSON(w, 200, map[string]any{"rules": rules})
}

func handleCreateAlert(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	var body iam.AlertRule
	if !decodeBody(r, &body) {
		writeError(w, 400, "invalid body")
		return
	}
	rule, err := iam.CreateAlertRule(body)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	auditAdmin(r, "alert.create", "alert_rule", rule.ID, map[string]any{
		"kind": rule.Kind, "metric": rule.Metric, "threshold": rule.Threshold,
	})
	writeJSON(w, 201, rule)
}

type alertStatusBody struct {
	ID      string `json:"id"`
	Enabled bool   `json:"enabled"`
}

func handleAlertStatus(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	var body alertStatusBody
	if !decodeBody(r, &body) {
		writeError(w, 400, "invalid body")
		return
	}
	if err := iam.SetAlertRuleEnabled(strings.TrimSpace(body.ID), body.Enabled); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	auditAdmin(r, "alert.status", "alert_rule", body.ID, map[string]any{"enabled": body.Enabled})
	writeJSON(w, 200, map[string]any{"ok": true})
}

func handleDeleteAlert(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	if err := iam.DeleteAlertRule(strings.TrimSpace(r.PathValue("id"))); err != nil {
		writeError(w, 404, err.Error())
		return
	}
	auditAdmin(r, "alert.delete", "alert_rule", r.PathValue("id"), nil)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func handleEvaluateAlerts(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	count, err := iam.EvaluateScheduledAlerts(time.Now())
	if err != nil {
		writeError(w, 500, "Alert evaluation failed.")
		return
	}
	auditAdmin(r, "alert.evaluate", "outbox", "", map[string]any{"enqueued": count})
	writeJSON(w, 200, map[string]any{"ok": true, "enqueued": count})
}

func handleListOutbox(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	events, err := iam.PendingOutbox(limit)
	if err != nil {
		writeError(w, 500, "Outbox unavailable.")
		return
	}
	writeJSON(w, 200, map[string]any{"events": events})
}

type claimOutboxBody struct {
	WorkerID     string `json:"worker_id"`
	Limit        int    `json:"limit"`
	LeaseSeconds int    `json:"lease_seconds"`
}

func handleClaimOutbox(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	var body claimOutboxBody
	if !decodeBody(r, &body) {
		writeError(w, 400, "invalid body")
		return
	}
	events, err := iam.ClaimOutbox(
		body.WorkerID, body.Limit, time.Duration(body.LeaseSeconds)*time.Second,
	)
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"events": events})
}

type outboxAckBody struct {
	WorkerID string `json:"worker_id"`
}

func handleOutboxDelivered(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid outbox id")
		return
	}
	var body outboxAckBody
	if !decodeBody(r, &body) || strings.TrimSpace(body.WorkerID) == "" {
		writeError(w, 400, "worker_id required")
		return
	}
	if err := iam.MarkOutboxDelivered(id, body.WorkerID); err != nil {
		writeError(w, 404, err.Error())
		return
	}
	auditAdmin(r, "outbox.delivered", "outbox_event", strconv.FormatInt(id, 10), nil)
	writeJSON(w, 200, map[string]any{"ok": true})
}

type outboxFailedBody struct {
	WorkerID string `json:"worker_id"`
	Error    string `json:"error"`
	RetryAt  int64  `json:"retry_at"`
}

func handleOutboxFailed(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, 400, "invalid outbox id")
		return
	}
	var body outboxFailedBody
	if !decodeBody(r, &body) {
		writeError(w, 400, "invalid body")
		return
	}
	if strings.TrimSpace(body.WorkerID) == "" {
		writeError(w, 400, "worker_id required")
		return
	}
	if err := iam.MarkOutboxFailed(id, body.WorkerID, body.Error, body.RetryAt); err != nil {
		writeError(w, 404, err.Error())
		return
	}
	auditAdmin(r, "outbox.failed", "outbox_event", strconv.FormatInt(id, 10), map[string]any{
		"retry_at": body.RetryAt,
	})
	writeJSON(w, 200, map[string]any{"ok": true})
}
