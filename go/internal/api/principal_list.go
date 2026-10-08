package api

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"llmgw/internal/iam"
)

// principalListFilter reads a principal listing's query: id and kind, each
// repeatable, status, project_id, q, sort, order (asc or desc), limit and
// offset. Without a sort, principals are listed by name.
func principalListFilter(query url.Values) (iam.PrincipalListFilter, error) {
	filter := iam.PrincipalListFilter{
		IDs: query["id"], Kinds: query["kind"], Status: query.Get("status"), ProjectID: query.Get("project_id"),
		Search: query.Get("q"), Sort: strings.TrimSpace(query.Get("sort")),
	}
	switch order := strings.TrimSpace(query.Get("order")); order {
	case "", "asc":
	case "desc":
		filter.Descending = true
	default:
		return filter, errors.New("order must be asc or desc")
	}
	return filter, readPageBounds(query, &filter.Limit, &filter.Offset)
}

// readPageBounds reads a listing's limit and offset, each a whole number
// when present.
func readPageBounds(query url.Values, limit, offset *int) error {
	for name, target := range map[string]*int{"limit": limit, "offset": offset} {
		value := strings.TrimSpace(query.Get(name))
		if value == "" {
			continue
		}
		number, err := strconv.Atoi(value)
		if err != nil || number < 0 {
			return errors.New(name + " must be a whole number")
		}
		*target = number
	}
	return nil
}

// handleListPrincipals answers a page of the gateway's principals, which the
// console lists, picks and names principals from.
func handleListPrincipals(w http.ResponseWriter, r *http.Request) {
	if !adminAuthed(w, r) {
		return
	}
	filter, err := principalListFilter(r.URL.Query())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	principals, total, err := iam.ListPrincipalsPage(filter)
	var invalid *iam.InvalidFilterError
	switch {
	case errors.As(err, &invalid):
		writeError(w, http.StatusBadRequest, invalid.Message)
		return
	case err != nil:
		writeError(w, 500, "Identity store unavailable.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"principals": principals, "total": total})
}
