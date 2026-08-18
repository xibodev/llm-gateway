package api

import (
	"log"
	"net/http"
	"strconv"

	"llmgw/internal/iam"
)

func auditAdmin(
	r *http.Request, action, targetType, targetID string, detail map[string]any,
) {
	actor := getAdminActor(r)
	if detail == nil {
		detail = map[string]any{}
	}
	detail["actor_source"] = actor.Source
	if err := iam.RecordAudit(iam.AuditEvent{
		ActorPrincipalID: actor.PrincipalID, ActorKeyID: actor.KeyID,
		Action: action, TargetType: targetType, TargetID: targetID,
		Result: "success", Detail: detail,
	}); err != nil {
		log.Printf("record admin audit: %v", err)
	}
}

func handleAudit(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	events, err := iam.ListAudit(limit)
	if err != nil {
		writeError(w, 500, "Audit store unavailable.")
		return
	}
	writeJSON(w, 200, map[string]any{"events": events})
}
