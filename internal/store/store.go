// Package store provides database access methods for agent-queue.
package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/irchelper/agent-queue/internal/model"
)

// Store wraps a *sql.DB and exposes task operations.
type Store struct {
	db *sql.DB
}

// New returns a Store backed by db.
func New(db *sql.DB) *Store { return &Store{db: db} }

// -------------------------------------------------------------------
// Create
// -------------------------------------------------------------------

// CreateTask inserts a new task and its dependency rows within a transaction.
func (s *Store) CreateTask(req model.CreateTaskRequest) (model.Task, error) {
	id := newID()
	now := time.Now().UTC()

	tx, err := s.db.Begin()
	if err != nil {
		return model.Task{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	_, err = tx.Exec(`
		INSERT INTO tasks (id, title, description, status, assigned_to, retry_assigned_to,
		                   parent_id, mode, requires_review, priority, version, created_at, updated_at)
		VALUES (?, ?, ?, 'pending', ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
		id, req.Title, req.Description, req.AssignedTo, req.RetryAssignedTo,
		req.ParentID, req.Mode, boolToInt(req.RequiresReview), req.Priority,
		now, now)
	if err != nil {
		return model.Task{}, fmt.Errorf("insert task: %w", err)
	}

	for _, dep := range req.DependsOn {
		_, err = tx.Exec(
			`INSERT INTO task_deps (task_id, depends_on) VALUES (?, ?)`, id, dep)
		if err != nil {
			return model.Task{}, fmt.Errorf("insert dep %s: %w", dep, err)
		}
	}

	// Record initial history entry.
	_, err = tx.Exec(`
		INSERT INTO task_history (task_id, from_status, to_status, changed_by, changed_at)
		VALUES (?, '', 'pending', ?, ?)`, id, req.AssignedTo, now)
	if err != nil {
		return model.Task{}, fmt.Errorf("insert history: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return model.Task{}, fmt.Errorf("commit: %w", err)
	}

	return s.GetByID(id)
}

// -------------------------------------------------------------------
// Read
// -------------------------------------------------------------------

// ListTasks returns tasks filtered by optional query parameters.
func (s *Store) ListTasks(status, assignedTo, parentID string, depsMetFilter *bool) ([]model.Task, error) {
	where := []string{"1=1"}
	args := []any{}

	if status != "" {
		where = append(where, "t.status = ?")
		args = append(args, status)
	}
	if assignedTo != "" {
		where = append(where, "t.assigned_to = ?")
		args = append(args, assignedTo)
	}
	if parentID != "" {
		where = append(where, "t.parent_id = ?")
		args = append(args, parentID)
	}

	query := `SELECT t.id, t.title, t.description, t.status, t.assigned_to, t.retry_assigned_to, t.parent_id,
	                 t.mode, t.requires_review, t.result, t.failure_reason, t.version, t.priority, t.started_at, t.created_at, t.updated_at
	          FROM tasks t
	          WHERE ` + strings.Join(where, " AND ") + `
	          ORDER BY t.created_at ASC`

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list tasks: %w", err)
	}
	defer rows.Close()

	var tasks []model.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, t)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}

	// If deps_met filter requested, post-filter in Go (avoids a complex correlated sub-query).
	if depsMetFilter != nil {
		filtered := tasks[:0]
		for _, t := range tasks {
			met, err := s.depsMetForID(t.ID)
			if err != nil {
				return nil, err
			}
			if met == *depsMetFilter {
				filtered = append(filtered, t)
			}
		}
		tasks = filtered
	}

	if tasks == nil {
		tasks = []model.Task{}
	}
	return tasks, nil
}

// GetByID returns a single task with its depends_on list and history.
func (s *Store) GetByID(id string) (model.Task, error) {
	row := s.db.QueryRow(`
		SELECT id, title, description, status, assigned_to, retry_assigned_to, parent_id,
		       mode, requires_review, result, failure_reason, version, priority, started_at, created_at, updated_at
		FROM tasks WHERE id = ?`, id)

	t, err := scanTaskRow(row)
	if err == sql.ErrNoRows {
		return model.Task{}, ErrNotFound
	}
	if err != nil {
		return model.Task{}, fmt.Errorf("get task: %w", err)
	}

	t.DependsOn, err = s.depsFor(id)
	if err != nil {
		return model.Task{}, err
	}

	t.History, err = s.historyFor(id)
	if err != nil {
		return model.Task{}, err
	}

	return t, nil
}

// DepsMet returns whether all declared dependencies of id are done.
func (s *Store) DepsMet(id string) (bool, error) {
	return s.depsMetForID(id)
}

// -------------------------------------------------------------------
// Claim (F2 – optimistic lock)
// -------------------------------------------------------------------

// Claim atomically transitions a task from pending→claimed with version check.
// Returns ErrConflict if the version or status does not match.
func (s *Store) Claim(id string, version int, agent string) (model.Task, error) {
	now := time.Now().UTC()

	tx, err := s.db.Begin()
	if err != nil {
		return model.Task{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	res, err := tx.Exec(`
		UPDATE tasks
		SET status = 'claimed', assigned_to = ?, version = version+1, updated_at = ?
		WHERE id = ? AND version = ? AND status = 'pending'`,
		agent, now, id, version)
	if err != nil {
		return model.Task{}, fmt.Errorf("claim update: %w", err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return model.Task{}, fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		// Check whether the task exists at all.
		var exists int
		_ = tx.QueryRow(`SELECT COUNT(*) FROM tasks WHERE id = ?`, id).Scan(&exists)
		if exists == 0 {
			return model.Task{}, ErrNotFound
		}
		return model.Task{}, ErrConflict
	}

	_, err = tx.Exec(`
		INSERT INTO task_history (task_id, from_status, to_status, changed_by, changed_at)
		VALUES (?, 'pending', 'claimed', ?, ?)`, id, agent, now)
	if err != nil {
		return model.Task{}, fmt.Errorf("insert history: %w", err)
	}

	if err = tx.Commit(); err != nil {
		return model.Task{}, fmt.Errorf("commit: %w", err)
	}

	return s.GetByID(id)
}

// -------------------------------------------------------------------
// Patch (F4 – state machine + F3 auto-advance)
// -------------------------------------------------------------------

// PatchTask applies a status/result update and returns the updated task plus
// any task IDs that became unblocked as a result of this change.
func (s *Store) PatchTask(id string, req model.PatchTaskRequest) (model.Task, []string, error) {
	now := time.Now().UTC()

	tx, err := s.db.Begin()
	if err != nil {
		return model.Task{}, nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// Fetch current task inside the transaction.
	row := tx.QueryRow(`
		SELECT id, title, description, status, assigned_to, retry_assigned_to, parent_id,
		       mode, requires_review, result, failure_reason, version, priority, started_at, created_at, updated_at
		FROM tasks WHERE id = ?`, id)

	current, err := scanTaskRow(row)
	if err == sql.ErrNoRows {
		return model.Task{}, nil, ErrNotFound
	}
	if err != nil {
		return model.Task{}, nil, fmt.Errorf("fetch current: %w", err)
	}

	// Version check.
	if req.Version != current.Version {
		return model.Task{}, nil, ErrConflict
	}

	setClauses := []string{"version = version+1", "updated_at = ?"}
	args := []any{now}

	var newStatus model.Status
	if req.Status != nil {
		newStatus = *req.Status
		// Validate transition.
		if err = validateTransition(current.Status, newStatus, current.RequiresReview); err != nil {
			return model.Task{}, nil, err
		}
		setClauses = append(setClauses, "status = ?")
		args = append(args, string(newStatus))

		// Record started_at when task first moves to in_progress.
		if newStatus == model.StatusInProgress && current.StartedAt == nil {
			setClauses = append(setClauses, "started_at = ?")
			args = append(args, now)
		}

		// On timeout/release back to pending, clear assigned_to.
		// On failed→pending retry: apply retry_assigned_to if set.
		if newStatus == model.StatusPending {
			retryTo := current.RetryAssignedTo
			// Allow PATCH body to override retry_assigned_to at retry time.
			if req.RetryAssignedTo != nil {
				retryTo = *req.RetryAssignedTo
			}
			if retryTo != "" {
				setClauses = append(setClauses, "assigned_to = ?")
				args = append(args, retryTo)
				setClauses = append(setClauses, "retry_assigned_to = ''")
			} else {
				setClauses = append(setClauses, "assigned_to = ''")
			}
		}
	} else {
		newStatus = current.Status
	}

	if req.Result != nil {
		setClauses = append(setClauses, "result = ?")
		args = append(args, *req.Result)
	}

	if req.FailureReason != nil {
		setClauses = append(setClauses, "failure_reason = ?")
		args = append(args, *req.FailureReason)
	}

	if req.RetryAssignedTo != nil && (req.Status == nil || *req.Status != model.StatusPending) {
		// Store retry_assigned_to for future use (not a retry transition yet).
		setClauses = append(setClauses, "retry_assigned_to = ?")
		args = append(args, *req.RetryAssignedTo)
	}

	args = append(args, id)
	updateSQL := "UPDATE tasks SET " + strings.Join(setClauses, ", ") + " WHERE id = ?"
	if _, err = tx.Exec(updateSQL, args...); err != nil {
		return model.Task{}, nil, fmt.Errorf("update task: %w", err)
	}

	if req.Status != nil {
		_, err = tx.Exec(`
			INSERT INTO task_history (task_id, from_status, to_status, changed_by, note, changed_at)
			VALUES (?, ?, ?, ?, ?, ?)`,
			id, string(current.Status), string(newStatus),
			req.ChangedBy, req.Note, now)
		if err != nil {
			return model.Task{}, nil, fmt.Errorf("insert history: %w", err)
		}
	}

	// F3: if task just became done, find tasks whose deps are now fully met.
	var triggered []string
	if newStatus == model.StatusDone {
		triggered, err = unlockDependents(tx, id, now)
		if err != nil {
			return model.Task{}, nil, err
		}
	}

	if err = tx.Commit(); err != nil {
		return model.Task{}, nil, fmt.Errorf("commit: %w", err)
	}

	task, err := s.GetByID(id)
	return task, triggered, err
}

// -------------------------------------------------------------------
// Delete
// -------------------------------------------------------------------

// Summary returns a compact view of active tasks and done-today count.
func (s *Store) Summary() (model.SummaryResponse, error) {
	// Count per-status (all statuses).
	rows, err := s.db.Query(`
		SELECT status, COUNT(*) FROM tasks GROUP BY status`)
	if err != nil {
		return model.SummaryResponse{}, fmt.Errorf("summary counts: %w", err)
	}
	defer rows.Close()

	var resp model.SummaryResponse
	for rows.Next() {
		var status string
		var count int
		if err = rows.Scan(&status, &count); err != nil {
			return model.SummaryResponse{}, err
		}
		switch model.Status(status) {
		case model.StatusPending:
			resp.Pending = count
		case model.StatusClaimed:
			resp.Claimed = count
		case model.StatusInProgress:
			resp.InProgress = count
		case model.StatusReview:
			resp.Review = count
		case model.StatusBlocked:
			resp.Blocked = count
		case model.StatusFailed:
			resp.Failed = count
		}
	}
	rows.Close() //nolint:errcheck

	// Done today: tasks done since the start of today (UTC).
	// We compare against midnight UTC using substr to handle Go's RFC3339 format.
	todayStart := time.Now().UTC().Truncate(24 * time.Hour)
	err = s.db.QueryRow(`
		SELECT COUNT(*) FROM tasks
		WHERE status = 'done' AND updated_at >= ?`, todayStart).Scan(&resp.DoneToday)
	if err != nil {
		return model.SummaryResponse{}, fmt.Errorf("done_today count: %w", err)
	}

	// Active tasks list (non-terminal), sorted by updated_at DESC.
	taskRows, err := s.db.Query(`
		SELECT id, title, status, assigned_to, updated_at FROM tasks
		WHERE status NOT IN ('done', 'cancelled')
		ORDER BY updated_at DESC`)
	if err != nil {
		return model.SummaryResponse{}, fmt.Errorf("active tasks: %w", err)
	}
	defer taskRows.Close()

	for taskRows.Next() {
		var t model.SummaryTask
		if err = taskRows.Scan(&t.ID, &t.Title, (*string)(&t.Status), &t.AssignedTo, &t.UpdatedAt); err != nil {
			return model.SummaryResponse{}, err
		}
		resp.Tasks = append(resp.Tasks, t)
	}
	if resp.Tasks == nil {
		resp.Tasks = []model.SummaryTask{}
	}
	return resp, taskRows.Err()
}

// Poll returns at most one pending, deps-met task for the given agent,
// sorted by priority DESC, created_at ASC (highest priority, oldest first).
// Returns (nil, nil) when no eligible task is found.
//
// NOTE: We close the rows cursor before calling depsMetForID to avoid a
// deadlock on SQLite's single-connection pool (MaxOpenConns=1).
func (s *Store) Poll(assignedTo string) (*model.Task, error) {
	if assignedTo == "" {
		return nil, fmt.Errorf("assigned_to is required")
	}

	// Phase 1: collect all candidate tasks (close cursor before deps check).
	rows, err := s.db.Query(`
		SELECT id, title, description, status, assigned_to, retry_assigned_to, parent_id, mode,
		       requires_review, result, failure_reason, version, priority, started_at, created_at, updated_at
		FROM tasks
		WHERE status = 'pending' AND assigned_to = ?
		ORDER BY priority DESC, created_at ASC
		LIMIT 20`, assignedTo)
	if err != nil {
		return nil, fmt.Errorf("poll query: %w", err)
	}

	var candidates []model.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			rows.Close() //nolint:errcheck
			return nil, err
		}
		candidates = append(candidates, t)
	}
	if err = rows.Err(); err != nil {
		rows.Close() //nolint:errcheck
		return nil, err
	}
	rows.Close() //nolint:errcheck

	// Phase 2: find the first task with all deps met (connection now free).
	for i := range candidates {
		met, err := s.depsMetForID(candidates[i].ID)
		if err != nil {
			return nil, err
		}
		if met {
			return &candidates[i], nil
		}
	}
	return nil, nil
}

// DeleteTask removes a task (cascade deletes deps + history).
func (s *Store) DeleteTask(id string) error {
	res, err := s.db.Exec(`DELETE FROM tasks WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete task: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// -------------------------------------------------------------------
// Internal helpers
// -------------------------------------------------------------------

// unlockDependents finds tasks that depend on completedID and, if all their
// deps are now done, logs them as triggered (they become claimable).
// Returns the IDs of tasks that were newly unblocked (deps fully met).
func unlockDependents(tx *sql.Tx, completedID string, now time.Time) ([]string, error) {
	// Find all tasks that declare completedID as a dependency.
	rows, err := tx.Query(
		`SELECT task_id FROM task_deps WHERE depends_on = ?`, completedID)
	if err != nil {
		return nil, fmt.Errorf("find dependents: %w", err)
	}
	defer rows.Close()

	var candidates []string
	for rows.Next() {
		var tid string
		if err = rows.Scan(&tid); err != nil {
			return nil, err
		}
		candidates = append(candidates, tid)
	}
	rows.Close() //nolint:errcheck

	var triggered []string
	for _, cid := range candidates {
		// Check if ALL deps of cid are done.
		var unmetCount int
		err = tx.QueryRow(`
			SELECT COUNT(*) FROM task_deps td
			JOIN tasks t ON t.id = td.depends_on
			WHERE td.task_id = ? AND t.status != 'done'`, cid).Scan(&unmetCount)
		if err != nil {
			return nil, fmt.Errorf("check unmet deps for %s: %w", cid, err)
		}
		if unmetCount == 0 {
			triggered = append(triggered, cid)
		}
	}
	return triggered, nil
}

func (s *Store) depsFor(id string) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT depends_on FROM task_deps WHERE task_id = ? ORDER BY depends_on`, id)
	if err != nil {
		return nil, fmt.Errorf("deps query: %w", err)
	}
	defer rows.Close()

	var deps []string
	for rows.Next() {
		var d string
		if err = rows.Scan(&d); err != nil {
			return nil, err
		}
		deps = append(deps, d)
	}
	return deps, rows.Err()
}

func (s *Store) historyFor(id string) ([]model.HistoryItem, error) {
	rows, err := s.db.Query(`
		SELECT id, task_id, from_status, to_status, changed_by, note, changed_at
		FROM task_history WHERE task_id = ? ORDER BY id ASC`, id)
	if err != nil {
		return nil, fmt.Errorf("history query: %w", err)
	}
	defer rows.Close()

	var items []model.HistoryItem
	for rows.Next() {
		var h model.HistoryItem
		err = rows.Scan(&h.ID, &h.TaskID, &h.FromStatus, &h.ToStatus,
			&h.ChangedBy, &h.Note, &h.ChangedAt)
		if err != nil {
			return nil, err
		}
		items = append(items, h)
	}
	return items, rows.Err()
}

func (s *Store) depsMetForID(id string) (bool, error) {
	var unmet int
	err := s.db.QueryRow(`
		SELECT COUNT(*) FROM task_deps td
		JOIN tasks t ON t.id = td.depends_on
		WHERE td.task_id = ? AND t.status != 'done'`, id).Scan(&unmet)
	if err != nil {
		return false, fmt.Errorf("deps_met query: %w", err)
	}
	return unmet == 0, nil
}

// -------------------------------------------------------------------
// Sentinel errors
// -------------------------------------------------------------------

type storeError string

func (e storeError) Error() string { return string(e) }

const (
	ErrNotFound = storeError("not found")
	ErrConflict = storeError("conflict")
)

// IsNotFound reports whether err is ErrNotFound.
func IsNotFound(err error) bool { return err == ErrNotFound }

// IsConflict reports whether err is ErrConflict.
func IsConflict(err error) bool { return err == ErrConflict }

// -------------------------------------------------------------------
// Scan helpers
// -------------------------------------------------------------------

type taskScanner interface {
	Scan(dest ...any) error
}

func scanTaskRow(r *sql.Row) (model.Task, error) {
	return scanTaskImpl(r)
}

func scanTask(r *sql.Rows) (model.Task, error) {
	return scanTaskImpl(r)
}

func scanTaskImpl(r taskScanner) (model.Task, error) {
	var t model.Task
	var rr int
	err := r.Scan(&t.ID, &t.Title, &t.Description, (*string)(&t.Status),
		&t.AssignedTo, &t.RetryAssignedTo, &t.ParentID, &t.Mode, &rr,
		&t.Result, &t.FailureReason, &t.Version, &t.Priority,
		&t.StartedAt, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return model.Task{}, err
	}
	t.RequiresReview = rr != 0
	return t, nil
}

// -------------------------------------------------------------------
// Misc
// -------------------------------------------------------------------

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// newID generates a unique task ID using timestamp + random suffix.
func newID() string {
	return fmt.Sprintf("%x", time.Now().UnixNano())
}

// validateTransition delegates to fsm.Validate (imported via a thin shim here
// to avoid a dependency cycle – store imports fsm, not the other way round).
func validateTransition(from, to model.Status, requiresReview bool) error {
	// Import inline to keep package dependencies clean.
	// We reproduce the check rather than importing fsm to avoid circular deps.
	type transition struct{ from, to model.Status }
	allowed := map[transition]bool{
		{model.StatusPending, model.StatusClaimed}:      true,
		{model.StatusPending, model.StatusCancelled}:    true,
		{model.StatusClaimed, model.StatusInProgress}:   true,
		{model.StatusClaimed, model.StatusPending}:      true,
		{model.StatusInProgress, model.StatusReview}:    true,
		{model.StatusInProgress, model.StatusDone}:      true,
		{model.StatusInProgress, model.StatusBlocked}:   true,
		{model.StatusInProgress, model.StatusFailed}:    true,
		{model.StatusInProgress, model.StatusPending}:   true,
		{model.StatusReview, model.StatusDone}:          true,
		{model.StatusReview, model.StatusInProgress}:    true,
		{model.StatusBlocked, model.StatusPending}:      true,
		{model.StatusBlocked, model.StatusInProgress}:   true,
		// failed → pending: CEO retry (optional retry_assigned_to)
		{model.StatusFailed, model.StatusPending}:       true,
	}
	if !allowed[transition{from, to}] {
		return &ValidationError{Msg: fmt.Sprintf("transition %s → %s is not allowed", from, to)}
	}
	if from == model.StatusInProgress {
		switch to {
		case model.StatusDone:
			if requiresReview {
				return &ValidationError{Msg: "task requires review: must transition to review before done"}
			}
		case model.StatusReview:
			if !requiresReview {
				return &ValidationError{Msg: "task does not require review: cannot transition to review"}
			}
		}
	}
	return nil
}

// ValidationError is returned for 422 scenarios.
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }
