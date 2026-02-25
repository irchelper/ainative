// Package model defines the core data types for agent-queue.
package model

import "time"

// Status represents the lifecycle state of a Task.
type Status string

const (
	StatusPending    Status = "pending"
	StatusClaimed    Status = "claimed"
	StatusInProgress Status = "in_progress"
	StatusReview     Status = "review"
	StatusDone       Status = "done"
	StatusBlocked    Status = "blocked"
	StatusCancelled  Status = "cancelled"
)

// Task is the primary entity managed by agent-queue.
type Task struct {
	ID             string     `json:"id"`
	Title          string     `json:"title"`
	Description    string     `json:"description,omitempty"`
	Status         Status     `json:"status"`
	AssignedTo     string     `json:"assigned_to,omitempty"`
	ParentID       string     `json:"parent_id,omitempty"`
	Mode           string     `json:"mode,omitempty"`
	RequiresReview bool       `json:"requires_review"`
	Result         string     `json:"result,omitempty"`
	Version        int        `json:"version"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`

	// Populated by detail queries.
	DependsOn []string      `json:"depends_on,omitempty"`
	History   []HistoryItem `json:"history,omitempty"`
}

// HistoryItem records a single status transition.
type HistoryItem struct {
	ID         int64     `json:"id"`
	TaskID     string    `json:"task_id"`
	FromStatus string    `json:"from_status,omitempty"`
	ToStatus   string    `json:"to_status"`
	ChangedBy  string    `json:"changed_by,omitempty"`
	Note       string    `json:"note,omitempty"`
	ChangedAt  time.Time `json:"changed_at"`
}

// CreateTaskRequest is the body for POST /tasks.
type CreateTaskRequest struct {
	Title          string   `json:"title"`
	Description    string   `json:"description"`
	AssignedTo     string   `json:"assigned_to"`
	ParentID       string   `json:"parent_id"`
	Mode           string   `json:"mode"`
	RequiresReview bool     `json:"requires_review"`
	DependsOn      []string `json:"depends_on"`
}

// PatchTaskRequest is the body for PATCH /tasks/:id.
type PatchTaskRequest struct {
	Status    *Status `json:"status"`
	Result    *string `json:"result"`
	Note      string  `json:"note"`
	ChangedBy string  `json:"changed_by"`
	Version   int     `json:"version"`
}

// ClaimRequest is the body for POST /tasks/:id/claim.
type ClaimRequest struct {
	Version int    `json:"version"`
	Agent   string `json:"agent"`
}

// PatchTaskResponse wraps the updated task with optional triggered IDs.
type PatchTaskResponse struct {
	Task      Task     `json:"task"`
	Triggered []string `json:"triggered,omitempty"`
}

// DepsMet is the response for GET /tasks/:id/deps-met.
type DepsMet struct {
	TaskID  string `json:"task_id"`
	DepsMet bool   `json:"deps_met"`
}

// HealthResponse is returned by GET /health.
type HealthResponse struct {
	Status   string `json:"status"`
	Database string `json:"database"`
}

// ErrorResponse is a generic JSON error body.
type ErrorResponse struct {
	Error string `json:"error"`
}

// DispatchRequest is the body for POST /dispatch.
type DispatchRequest struct {
	Title          string   `json:"title"`
	AssignedTo     string   `json:"assigned_to"`
	Description    string   `json:"description"`
	RequiresReview bool     `json:"requires_review"`
	DependsOn      []string `json:"depends_on"`
	Result         string   `json:"result"`
}

// DispatchResponse wraps the created task and a flag indicating whether the
// sessions_send notification was sent successfully.
type DispatchResponse struct {
	Task         Task   `json:"task"`
	Notified     bool   `json:"notified"`
	NotifyError  string `json:"notify_error,omitempty"`
}

// SummaryTask is a compact task view used in GET /tasks/summary.
type SummaryTask struct {
	ID         string    `json:"id"`
	Title      string    `json:"title"`
	Status     Status    `json:"status"`
	AssignedTo string    `json:"assigned_to,omitempty"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// SummaryResponse is returned by GET /tasks/summary.
type SummaryResponse struct {
	Pending    int           `json:"pending"`
	Claimed    int           `json:"claimed"`
	InProgress int           `json:"in_progress"`
	Review     int           `json:"review"`
	Blocked    int           `json:"blocked"`
	DoneToday  int           `json:"done_today"`
	Tasks      []SummaryTask `json:"tasks"`
}
