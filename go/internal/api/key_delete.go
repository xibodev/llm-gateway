package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"llmgw/internal/iam"
)

// maxKeyDeletions bounds the keys one deletion request names.
const maxKeyDeletions = 500

// keyDeletion is the body of a key deletion: the IDs of the keys to delete.
type keyDeletion struct {
	IDs []string `json:"ids"`
}

// deleteKeys deletes each key the request names that owner owns, unless
// owner is empty, and that is revoked or expired, calls record for each one
// deleted, and answers which keys were deleted and why the others were not.
// Deleting is separate from revoking, which stops a key: a deleted key only
// leaves the listings, while usage and audit history still name it.
func deleteKeys(w http.ResponseWriter, r *http.Request, owner string, record func(iam.APIKey)) {
	var body keyDeletion
	if !decodeBody(r, &body) {
		writeError(w, 400, "invalid body")
		return
	}
	ids := []string{}
	seen := map[string]bool{}
	for _, id := range body.IDs {
		if id = strings.TrimSpace(id); id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		writeError(w, 400, "ids required")
		return
	}
	if len(ids) > maxKeyDeletions {
		writeError(w, 400, fmt.Sprintf("at most %d keys can be deleted at once", maxKeyDeletions))
		return
	}
	deleted := []string{}
	refused := []map[string]string{}
	for _, id := range ids {
		key, err := iam.DeleteAPIKey(id, owner)
		switch {
		case err == nil:
			record(key)
			deleted = append(deleted, key.ID)
		case errors.Is(err, iam.ErrAPIKeyNotFound), errors.Is(err, iam.ErrAPIKeyLive):
			refused = append(refused, map[string]string{"id": id, "error": err.Error()})
		default:
			refused = append(refused, map[string]string{"id": id, "error": "Identity store unavailable."})
		}
	}
	writeJSON(w, 200, map[string]any{"deleted": deleted, "refused": refused})
}

// deletedKeyDetail is what the audit log keeps of a deleted key, so the
// event names it without a listing to look it up in.
func deletedKeyDetail(key iam.APIKey) map[string]any {
	status := key.Status
	if status != "revoked" && key.IsExpired() {
		status = "expired"
	}
	return map[string]any{
		"name": key.Name, "prefix": key.Prefix, "project_id": key.ProjectID,
		"principal_id": key.PrincipalID, "status": status,
	}
}

// POST /admin/api/keys/delete deletes revoked or expired keys.
func handleDeleteKeys(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	deleteKeys(w, r, "", func(key iam.APIKey) {
		auditAdmin(r, "api_key.delete", "api_key", key.ID, deletedKeyDetail(key))
	})
}

// POST /user/api/keys/delete deletes the signed-in user's revoked or expired
// keys.
func handleUserDeleteKeys(w http.ResponseWriter, r *http.Request) {
	principal, ok := requireSSOUser(w, r)
	if !ok {
		return
	}
	deleteKeys(w, r, principal.ID, func(key iam.APIKey) {
		detail := deletedKeyDetail(key)
		detail["source"] = "self-service"
		_ = iam.RecordAudit(iam.AuditEvent{
			ActorPrincipalID: principal.ID, Action: "api_key.delete",
			TargetType: "api_key", TargetID: key.ID, Result: "success", Detail: detail,
		})
	})
}
