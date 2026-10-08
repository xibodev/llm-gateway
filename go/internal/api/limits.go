package api

import (
	"net/http"
	"strings"
	"time"

	"llmgw/internal/config"
	"llmgw/internal/iam"
)

// handleKeyLimits answers the limits that apply to one key, each with its
// usage in its current window.
func handleKeyLimits(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	key, found, err := iam.APIKeyByID(strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		writeError(w, 500, "Identity store unavailable.")
		return
	}
	if !found {
		writeError(w, 404, "unknown key")
		return
	}
	writeKeyLimits(w, key)
}

// handleUserKeyLimits answers handleKeyLimits for a key of the signed-in user.
func handleUserKeyLimits(w http.ResponseWriter, r *http.Request) {
	principal, ok := requireSSOUser(w, r)
	if !ok {
		return
	}
	key, found, err := iam.APIKeyByID(strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		writeError(w, 500, "Identity store unavailable.")
		return
	}
	if !found || key.PrincipalID != principal.ID {
		writeError(w, 404, "unknown key")
		return
	}
	writeKeyLimits(w, key)
}

// writeKeyLimits writes the limits key's requests are admitted under: its
// own, its project's and the per-caller rate limit, whose count is this
// process's, as it is enforced per process.
func writeKeyLimits(w http.ResponseWriter, key iam.APIKey) {
	limits, err := iam.KeyLimitUsage(key, time.Now())
	if err != nil {
		writeError(w, 500, "Quota store unavailable.")
		return
	}
	if limit := config.Get().RateLimitPerMinute; limit > 0 {
		used, resets := callerRates.used(rateLimitCaller(&config.Principal{KeyID: key.ID}))
		limits = append(limits, iam.LimitUsage{
			Scope: "caller", Field: "rate_limit_per_minute", Metric: "requests", Period: "minute",
			Limit: int64(limit), Used: int64(used), ResetsAt: resets.Unix(),
		})
	}
	iam.MarkClosestLimit(limits)
	writeJSON(w, http.StatusOK, map[string]any{"key_id": key.ID, "project_id": key.ProjectID, "limits": limits})
}

// handleProjectLimits answers the limits of one project's policy, each with
// its usage in its current window.
func handleProjectLimits(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	project, found, err := iam.ProjectByID(strings.TrimSpace(r.PathValue("id")))
	if err != nil {
		writeError(w, 500, "Identity store unavailable.")
		return
	}
	if !found {
		writeError(w, 404, "unknown project")
		return
	}
	limits, err := iam.ProjectLimitUsage(project.ID, time.Now())
	if err != nil {
		writeError(w, 500, "Quota store unavailable.")
		return
	}
	iam.MarkClosestLimit(limits)
	writeJSON(w, http.StatusOK, map[string]any{"project_id": project.ID, "limits": limits})
}
