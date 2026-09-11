package api

import (
	"net/http"

	"llmgw/internal/roster"
)

func handleAdminRoster(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, roster.Default().Snapshot())
}

func handleUserRoster(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireSSOUser(w, r); !ok {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, roster.Default().Snapshot())
}

func handleRefreshRoster(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, roster.Default().Refresh(r.Context()))
}
