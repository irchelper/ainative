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
	StatusFailed     Status = "failed" // task execution error; may retry via failed→pending
)

// Task is the primary entity managed by agent-queue.
type Task struct {
	ID                    string     `json:"id"`
	Title                 string     `json:"title"`
	Description           string     `json:"description,omitempty"`
	Status                Status     `json:"status"`
	AssignedTo            string     `json:"assigned_to,omitempty"`
	RetryAssignedTo       string     `json:"retry_assigned_to,omitempty"`
	SupersededBy          string     `json:"superseded_by,omitempty"`
	ChainID               string     `json:"chain_id,omitempty"`
	NotifyCEOOnComplete   bool       `json:"notify_ceo_on_complete,omitempty"`
	ParentID              string     `json:"parent_id,omitempty"`
	Mode                  string     `json:"mode,omitempty"`
	RequiresReview        bool       `json:"requires_review"`
	Result                string     `json:"result,omitempty"`
	FailureReason         string     `json:"failure_reason,omitempty"`
	Version               int        `json:"version"`
	Priority              int        `json:"priority"`
	StaleDispatchCount    int        `json:"stale_dispatch_count,omitempty"`
	TimeoutMinutes           *int       `json:"timeout_minutes,omitempty"`
	TimeoutAction            *string    `json:"timeout_action,omitempty"`
	CommitURL                *string    `json:"commit_url,omitempty"`
	AutoAdvanceTo            string     `json:"auto_advance_to,omitempty"`
	AdvanceTaskTitle         string     `json:"advance_task_title,omitempty"`
	AdvanceTaskDescription   string     `json:"advance_task_description,omitempty"`
	SpecFile                 string     `json:"spec_file,omitempty"`
	Acceptance               []string   `json:"acceptance,omitempty"`
	StartedAt                *time.Time `json:"started_at,omitempty"`
	CEONotifiedAt            *time.Time `json:"ceo_notified_at,omitempty"`
	CreatedAt             time.Time  `json:"created_at"`
	UpdatedAt             time.Time  `json:"updated_at"`

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
	Title                 string   `json:"title"`
	Description           string   `json:"description"`
	AssignedTo            string   `json:"assigned_to"`
	RetryAssignedTo       string   `json:"retry_assigned_to"`
	ChainID               string   `json:"chain_id"`
	NotifyCEOOnComplete   bool     `json:"notify_ceo_on_complete"`
	ParentID              string   `json:"parent_id"`
	Mode                  string   `json:"mode"`
	RequiresReview        bool     `json:"requires_review"`
	Priority              int      `json:"priority"`
	TimeoutMinutes           *int     `json:"timeout_minutes"`
	TimeoutAction            *string  `json:"timeout_action"`
	CommitURL                *string  `json:"commit_url"`
	AutoAdvanceTo            string   `json:"auto_advance_to"`
	AdvanceTaskTitle         string   `json:"advance_task_title"`
	AdvanceTaskDescription   string   `json:"advance_task_description"`
	SpecFile                 string   `json:"spec_file,omitempty"`
	Acceptance               []string `json:"acceptance,omitempty"`
	DependsOn                []string `json:"depends_on"`
}

// PatchTaskRequest is the body for PATCH /tasks/:id.
type PatchTaskRequest struct {
	Status          *Status `json:"status"`
	Result          *string `json:"result"`
	FailureReason   *string `json:"failure_reason"`    // written on in_progress→failed
	RetryAssignedTo *string `json:"retry_assigned_to"` // set by CEO before failed→pending retry
	CommitURL       *string `json:"commit_url"`        // V12: optional commit URL
	Priority        *int    `json:"priority"`          // V19: dynamic priority (0=normal,1=high,2=urgent)
	Note            string  `json:"note"`
	ChangedBy       string  `json:"changed_by"`
	Version         int     `json:"version"`
}

// BlockedDownstream is a compact view of a downstream task blocked by a failed dep.
type BlockedDownstream struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	AssignedTo string `json:"assigned_to"`
}

// ClaimRequest is the body for POST /tasks/:id/claim.
type ClaimRequest struct {
	Version int    `json:"version"`
	Agent   string `json:"agent"`
}

// PatchTaskResponse wraps the updated task with optional triggered IDs.
type PatchTaskResponse struct {
	Task               Task                `json:"task"`
	Triggered          []string            `json:"triggered,omitempty"`
	BlockedDownstream  []BlockedDownstream `json:"blocked_downstream,omitempty"`
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
	Title               string   `json:"title"`
	AssignedTo          string   `json:"assigned_to"`
	Description         string   `json:"description"`
	RequiresReview      bool     `json:"requires_review"`
	DependsOn           []string `json:"depends_on"`
	Result              string   `json:"result"`
	NotifyCEOOnComplete      bool     `json:"notify_ceo_on_complete"`
	Priority                 int      `json:"priority"`
	CommitURL                *string  `json:"commit_url"`
	TimeoutMinutes           *int     `json:"timeout_minutes"`
	TimeoutAction            *string  `json:"timeout_action"`
	AutoAdvanceTo            string   `json:"auto_advance_to"`
	AdvanceTaskTitle         string   `json:"advance_task_title"`
	AdvanceTaskDescription   string   `json:"advance_task_description"`
	// SpecFile is an optional path to a local file containing the full task spec.
	// When set, the file contents are read server-side and prepended to Description.
	SpecFile                 string   `json:"spec_file,omitempty"`
	Acceptance               []string `json:"acceptance,omitempty"`
}

// DispatchResponse wraps the created task and a flag indicating whether the
// sessions_send notification was sent successfully.
type DispatchResponse struct {
	Task         Task   `json:"task"`
	Notified     bool   `json:"notified"`
	NotifyError  string `json:"notify_error,omitempty"`
}

// -------------------------------------------------------------------
// F9: POST /dispatch/chain
// -------------------------------------------------------------------

// ChainTaskSpec is one task entry in a chain submission.
type ChainTaskSpec struct {
	Title          string `json:"title"`
	AssignedTo     string `json:"assigned_to"`
	Description    string `json:"description"`
	RequiresReview bool   `json:"requires_review"`
	Priority       int    `json:"priority"`
	// SpecFile is an optional path to a local file containing the full task spec.
	// When set, the file contents are read server-side and prepended to Description.
	SpecFile       string   `json:"spec_file,omitempty"`
	Acceptance     []string `json:"acceptance,omitempty"`
}

// ChainRequest is the body for POST /dispatch/chain.
type ChainRequest struct {
	Tasks                 []ChainTaskSpec `json:"tasks"`
	NotifyCEOOnComplete   bool            `json:"notify_ceo_on_complete"`
	ChainTitle            string          `json:"chain_title"`
}

// RetryRoute is a row from the retry_routing table.
type RetryRoute struct {
	ID              int    `json:"id"`
	AssignedTo      string `json:"assigned_to"`
	ErrorKeyword    string `json:"error_keyword"`
	RetryAssignedTo string `json:"retry_assigned_to"`
	Priority        int    `json:"priority"`
}

// ChainStatusResponse is returned by GET /chains/:chain_id.
type ChainStatusResponse struct {
	ChainID    string `json:"chain_id"`
	ChainTitle string `json:"chain_title,omitempty"`
	Total      int    `json:"total"`
	Done       int    `json:"done"`
	IsComplete bool   `json:"is_complete"`
	Tasks      []Task `json:"tasks"`
}

// ChainResponse is returned by POST /dispatch/chain.
type ChainResponse struct {
	ChainID         string `json:"chain_id"`
	Tasks           []Task `json:"tasks"`
	FirstDispatched string `json:"first_dispatched"`
	Notified        bool   `json:"notified"`
	NotifyError     string `json:"notify_error,omitempty"`
}

// -------------------------------------------------------------------
// F10: GET /tasks/poll
// -------------------------------------------------------------------

// PollResponse is returned by GET /tasks/poll.
type PollResponse struct {
	Task *Task `json:"task"` // nil when no eligible task found
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
	Failed     int           `json:"failed"`
	DoneToday  int           `json:"done_today"`
	Tasks      []SummaryTask `json:"tasks"`
}

// -------------------------------------------------------------------
// V15: Task Templates
// -------------------------------------------------------------------

// TemplateTaskSpec is one task entry stored in a template.
type TemplateTaskSpec struct {
	AssignedTo     string   `json:"assigned_to"`
	Title          string   `json:"title"`
	Description    string   `json:"description,omitempty"`
	RequiresReview bool     `json:"requires_review,omitempty"`
	Priority       int      `json:"priority,omitempty"`
	Acceptance     []string `json:"acceptance,omitempty"`
}

// Template defines a reusable chain/task pattern.
type Template struct {
	ID          int64              `json:"id"`
	Name        string             `json:"name"`
	Description string             `json:"description,omitempty"`
	Tasks       []TemplateTaskSpec `json:"tasks"`
	CreatedAt   time.Time          `json:"created_at"`
	UpdatedAt   time.Time          `json:"updated_at"`
}

// CreateTemplateRequest is the body for POST /templates.
type CreateTemplateRequest struct {
	Name        string             `json:"name"`
	Description string             `json:"description"`
	Tasks       []TemplateTaskSpec `json:"tasks"`
}

// DispatchFromTemplateRequest is the body for POST /dispatch/from-template/:name.
type DispatchFromTemplateRequest struct {
	Vars                map[string]string `json:"vars"`
	NotifyCEOOnComplete bool              `json:"notify_ceo_on_complete"`
	ChainTitle          string            `json:"chain_title"`
}

// -------------------------------------------------------------------
// V18: /dispatch/graph
// -------------------------------------------------------------------

// GraphNodeSpec describes a single node (task) in a DAG submission.
type GraphNodeSpec struct {
	// Key is a caller-chosen identifier used in edges; not stored.
	Key            string   `json:"key"`
	Title          string   `json:"title"`
	AssignedTo     string   `json:"assigned_to"`
	Description    string   `json:"description"`
	Priority       int      `json:"priority"`
	RequiresReview bool     `json:"requires_review"`
	Acceptance     []string `json:"acceptance,omitempty"`
}

// GraphEdge expresses a dependency: task at To depends on task at From.
type GraphEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// GraphRequest is the body for POST /dispatch/graph.
type GraphRequest struct {
	Nodes               []GraphNodeSpec `json:"nodes"`
	Edges               []GraphEdge     `json:"edges"`
	NotifyCEOOnComplete bool            `json:"notify_ceo_on_complete"`
}

// GraphResponse is returned by POST /dispatch/graph.
type GraphResponse struct {
	// NodeIDMap maps each node Key to its created task ID.
	NodeIDMap map[string]string `json:"node_id_map"`
	Tasks     []Task            `json:"tasks"`
	// FirstDispatched holds the IDs of all tasks with no pending dependencies (roots).
	FirstDispatched []string `json:"first_dispatched"`
}
