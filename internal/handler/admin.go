package handler

import (
	"net/http"
)

// registerAdminRoutes registers admin-only endpoints.
func (h *Handler) registerAdminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/admin/cleanup-test-tasks", h.handleCleanupTestTasks)
}

// DELETE /api/admin/cleanup-test-tasks
// Deletes test tasks in terminal states and returns deleted_count.
func (h *Handler) handleCleanupTestTasks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	res, err := h.db.Exec(`
		DELETE FROM tasks
		WHERE status IN ('done','failed','cancelled')
		  AND (assigned_to = 'e2e-qa'
		       OR title LIKE '[TEST]%'
		       OR title LIKE '[e2e]%')
	`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	count, _ := res.RowsAffected()
	writeJSON(w, http.StatusOK, map[string]any{
		"deleted_count": count,
	})
}
