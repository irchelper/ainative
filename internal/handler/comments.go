package handler

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// TaskComment represents a single comment on a task.
type TaskComment struct {
	ID        int64  `json:"id"`
	TaskID    string `json:"task_id"`
	Author    string `json:"author"`
	Content   string `json:"content"`
	CreatedAt string `json:"created_at"`
}

// registerCommentRoutes registers comment endpoints under /api/tasks/:id/comments.
// Called from handler.Register.
func (h *Handler) registerCommentRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/tasks/", func(w http.ResponseWriter, r *http.Request) {
		// Match /api/tasks/{id}/comments only.
		path := strings.TrimPrefix(r.URL.Path, "/api/tasks/")
		parts := strings.SplitN(path, "/", 2)
		if len(parts) != 2 || parts[1] != "comments" {
			writeError(w, http.StatusNotFound, "not found")
			return
		}
		taskID := parts[0]
		if taskID == "" {
			writeError(w, http.StatusBadRequest, "task_id required")
			return
		}
		switch r.Method {
		case http.MethodGet:
			h.listComments(w, r, taskID)
		case http.MethodPost:
			h.createComment(w, r, taskID)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	})
}

func (h *Handler) listComments(w http.ResponseWriter, _ *http.Request, taskID string) {
	rows, err := h.db.Query(`
		SELECT id, task_id, author, content, created_at
		FROM task_comments
		WHERE task_id = ?
		ORDER BY created_at ASC`, taskID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	comments := []TaskComment{}
	for rows.Next() {
		var c TaskComment
		var createdAt time.Time
		if err := rows.Scan(&c.ID, &c.TaskID, &c.Author, &c.Content, &createdAt); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		c.CreatedAt = createdAt.UTC().Format(time.RFC3339)
		comments = append(comments, c)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"comments": comments, "count": len(comments)})
}

func (h *Handler) createComment(w http.ResponseWriter, r *http.Request, taskID string) {
	var req struct {
		Author  string `json:"author"`
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Content) == "" {
		writeError(w, http.StatusBadRequest, "content is required")
		return
	}

	// Verify task exists.
	var exists int
	if err := h.db.QueryRow(`SELECT COUNT(1) FROM tasks WHERE id = ?`, taskID).Scan(&exists); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if exists == 0 {
		writeError(w, http.StatusNotFound, "task not found")
		return
	}

	result, err := h.db.Exec(`
		INSERT INTO task_comments (task_id, author, content)
		VALUES (?, ?, ?)`,
		taskID, strings.TrimSpace(req.Author), strings.TrimSpace(req.Content))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	id, _ := result.LastInsertId()

	var c TaskComment
	var createdAt time.Time
	if err := h.db.QueryRow(`
		SELECT id, task_id, author, content, created_at
		FROM task_comments WHERE id = ?`, id).
		Scan(&c.ID, &c.TaskID, &c.Author, &c.Content, &createdAt); err != nil && err != sql.ErrNoRows {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	c.CreatedAt = createdAt.UTC().Format(time.RFC3339)
	writeJSON(w, http.StatusCreated, map[string]any{"comment": c})

	// Broadcast SSE event so connected clients update in real-time.
	h.hub.Broadcast(SSEEvent{Type: "comment_created", TaskID: taskID})
}
