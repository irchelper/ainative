package handler

import (
	"net/http"
	"time"
)

// registerAdminRoutes registers admin-only endpoints.
func (h *Handler) registerAdminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/admin/cleanup-test-tasks", h.handleCleanupTestTasks)
}

// DELETE /api/admin/cleanup-test-tasks[?max_age=<duration>]
//
// Deletes terminal-state test tasks (done/failed/cancelled) whose
// updated_at is older than max_age (default: 1h).  Accepts any Go
// duration string, e.g. "30m", "2h", "24h", "0" (delete all).
//
// Response: {"deleted_count": N, "max_age": "<duration>"}
func (h *Handler) handleCleanupTestTasks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// Parse max_age query param (default 1h).
	maxAge := time.Hour
	if s := r.URL.Query().Get("max_age"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid max_age: "+err.Error())
			return
		}
		if d < 0 {
			writeError(w, http.StatusBadRequest, "max_age must be >= 0")
			return
		}
		maxAge = d
	}

	cutoff := time.Now().UTC().Add(-maxAge)

	res, err := h.db.Exec(`
		DELETE FROM tasks
		WHERE status IN ('done','failed','cancelled')
		  AND updated_at < ?
		  AND (LOWER(title) LIKE '%[test]%'
		       OR assigned_to LIKE 'e2e-%'
		       OR assigned_to = 'e2e-qa'
		       OR title LIKE '[TEST]%'
		       OR title LIKE '[e2e]%')
	`, cutoff)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	count, _ := res.RowsAffected()
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted_count": count,
		"max_age":       maxAge.String(),
	})
}
