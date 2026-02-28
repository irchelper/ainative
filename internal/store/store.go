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
		                   chain_id, notify_ceo_on_complete,
		                   parent_id, mode, requires_review, priority, version,
		                   timeout_minutes, timeout_action, commit_url,
		                   auto_advance_to, advance_task_title, advance_task_description,
		                   spec_file, created_at, updated_at)
		VALUES (?, ?, ?, 'pending', ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, req.Title, req.Description, req.AssignedTo, req.RetryAssignedTo,
		req.ChainID, boolToInt(req.NotifyCEOOnComplete),
		req.ParentID, req.Mode, boolToInt(req.RequiresReview), req.Priority,
		req.TimeoutMinutes, req.TimeoutAction, req.CommitURL,
		req.AutoAdvanceTo, req.AdvanceTaskTitle, req.AdvanceTaskDescription,
		req.SpecFile, now, now)
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
// ListTasks returns tasks matching the given filters.
// search is a substring match on title/description (empty = no filter).
func (s *Store) ListTasks(status, assignedTo, parentID string, depsMetFilter *bool) ([]model.Task, error) {
	return s.listTasksInternal(status, assignedTo, parentID, "", depsMetFilter)
}

// ListTasksSearch is like ListTasks but adds a fuzzy text search on title and description.
func (s *Store) ListTasksSearch(status, assignedTo, parentID, search string, depsMetFilter *bool) ([]model.Task, error) {
	return s.listTasksInternal(status, assignedTo, parentID, search, depsMetFilter)
}

func (s *Store) listTasksInternal(status, assignedTo, parentID, search string, depsMetFilter *bool) ([]model.Task, error) {
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
	if search != "" {
		where = append(where, "(t.title LIKE ? OR t.description LIKE ?)")
		like := "%" + search + "%"
		args = append(args, like, like)
	}

	query := `SELECT t.id, t.title, t.description, t.status, t.assigned_to, t.retry_assigned_to, t.superseded_by,
	                 t.chain_id, t.notify_ceo_on_complete, t.stale_dispatch_count, t.parent_id,
	                 t.mode, t.requires_review, t.result, t.failure_reason, t.version, t.priority, t.started_at, t.created_at, t.updated_at,
				 t.timeout_minutes, t.timeout_action, t.commit_url,
				 t.auto_advance_to, t.advance_task_title, t.advance_task_description, t.spec_file
	          FROM tasks t
	          WHERE ` + strings.Join(where, " AND ") + `
	          ORDER BY t.priority DESC, t.created_at ASC`

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
		SELECT id, title, description, status, assigned_to, retry_assigned_to, superseded_by,
		       chain_id, notify_ceo_on_complete, stale_dispatch_count, parent_id,
		       mode, requires_review, result, failure_reason, version, priority, started_at, created_at, updated_at,
		       timeout_minutes, timeout_action, commit_url,
		       auto_advance_to, advance_task_title, advance_task_description, spec_file
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
		SELECT id, title, description, status, assigned_to, retry_assigned_to, superseded_by,
		       chain_id, notify_ceo_on_complete, stale_dispatch_count, parent_id,
		       mode, requires_review, result, failure_reason, version, priority, started_at, created_at, updated_at,
		       timeout_minutes, timeout_action, commit_url,
		       auto_advance_to, advance_task_title, advance_task_description, spec_file
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
		// On failed→pending retry: apply retry_assigned_to if set; also clear superseded_by.
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
			// Clear superseded_by when retrying to prevent double-unlock conflicts.
			if current.Status == model.StatusFailed {
				setClauses = append(setClauses, "superseded_by = ''")
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

	if req.CommitURL != nil {
		setClauses = append(setClauses, "commit_url = ?")
		args = append(args, *req.CommitURL)
	}

	// V19: priority can be updated at any time regardless of FSM state.
	if req.Priority != nil {
		setClauses = append(setClauses, "priority = ?")
		args = append(args, *req.Priority)
	}

	args = append(args, id)
	updateSQL := "UPDATE tasks SET " + strings.Join(setClauses, ", ") + " WHERE id = ?"
	if _, err = tx.Exec(updateSQL, args...); err != nil {
		return model.Task{}, nil, fmt.Errorf("update task: %w", err)
	}

	if req.Status != nil {
		note := req.Note
		// V27-A P0-3: annotate recovery when agent delivers result after timeout.
		if current.Status == model.StatusFailed && newStatus == model.StatusDone && note == "" {
			note = "recovered from failed"
		}
		_, err = tx.Exec(`
			INSERT INTO task_history (task_id, from_status, to_status, changed_by, note, changed_at)
			VALUES (?, ?, ?, ?, ?, ?)`,
			id, string(current.Status), string(newStatus),
			req.ChangedBy, note, now)
		if err != nil {
			return model.Task{}, nil, fmt.Errorf("insert history: %w", err)
		}
	}

	// V31-P1-C: failed→done permission check.
	// Only the original assignee or "system" may recover a failed task to done.
	// Empty changed_by is treated as anonymous and is also rejected.
	if req.Status != nil && current.Status == model.StatusFailed && newStatus == model.StatusDone {
		caller := req.ChangedBy
		if caller != "system" && caller != current.AssignedTo {
			return model.Task{}, nil, &ValidationError{
				Msg: fmt.Sprintf("failed→done only allowed for original assignee (%s) or system, got %q",
					current.AssignedTo, caller),
			}
		}
	}

	// V31-P0-1: failed→done recovery — clean up orphan retry tasks.
	// When a task recovers from failed to done (e.g. agent delivers result after timeout),
	// any retry tasks spawned from it (superseded_by = this task's ID) are now redundant.
	// Clear our own superseded_by and cancel pending/claimed orphan retry tasks.
	if req.Status != nil && current.Status == model.StatusFailed && newStatus == model.StatusDone {
		// 1. Clear this task's own superseded_by field (it is no longer superseded).
		if _, err = tx.Exec(`UPDATE tasks SET superseded_by = '', updated_at = ? WHERE id = ?`, now, id); err != nil {
			return model.Task{}, nil, fmt.Errorf("clear superseded_by: %w", err)
		}
		// 2. Find pending/claimed retry tasks that point to this task as their originator.
		orphanRows, oErr := tx.Query(
			`SELECT id FROM tasks WHERE superseded_by = ? AND status IN ('pending', 'claimed')`, id)
		if oErr != nil {
			return model.Task{}, nil, fmt.Errorf("find orphan retries: %w", oErr)
		}
		var orphanIDs []string
		for orphanRows.Next() {
			var oid string
			if sErr := orphanRows.Scan(&oid); sErr != nil {
				orphanRows.Close() //nolint:errcheck
				return model.Task{}, nil, sErr
			}
			orphanIDs = append(orphanIDs, oid)
		}
		orphanRows.Close() //nolint:errcheck
		// 3. Cancel each orphan + record history.
		for _, oid := range orphanIDs {
			if _, err = tx.Exec(
				`UPDATE tasks SET status = 'cancelled', superseded_by = '', updated_at = ? WHERE id = ?`, now, oid); err != nil {
				return model.Task{}, nil, fmt.Errorf("cancel orphan %s: %w", oid, err)
			}
			if _, err = tx.Exec(`
				INSERT INTO task_history (task_id, from_status, to_status, changed_by, note, changed_at)
				VALUES (?, 'pending', 'cancelled', 'system', 'orphan retry cancelled: original task recovered', ?)`,
				oid, now); err != nil {
				return model.Task{}, nil, fmt.Errorf("history orphan %s: %w", oid, err)
			}
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
// SupersededBy helpers
// -------------------------------------------------------------------

// SetSupersededBy marks originalID.superseded_by = retryID.
// Called by autoRetry after creating the retry task.
func (s *Store) SetSupersededBy(originalID, retryID string) error {
	_, err := s.db.Exec(
		`UPDATE tasks SET superseded_by = ?, updated_at = ? WHERE id = ?`,
		retryID, time.Now().UTC(), originalID)
	if err != nil {
		return fmt.Errorf("set superseded_by: %w", err)
	}
	return nil
}

// UpdateSupersededByChain updates all tasks whose superseded_by = oldTargetID
// to point to newTargetID. Used by multi-level review-reject chains to ensure
// depsMetForID follows the latest re-review task, not the stale one.
func (s *Store) UpdateSupersededByChain(oldTargetID, newTargetID string) error {
	_, err := s.db.Exec(
		`UPDATE tasks SET superseded_by = ?, updated_at = ? WHERE superseded_by = ?`,
		newTargetID, time.Now().UTC(), oldTargetID)
	if err != nil {
		return fmt.Errorf("UpdateSupersededByChain: %w", err)
	}
	return nil
}

// ScanBlockedDownstream returns all pending/claimed tasks that (directly or
// indirectly) depend on failedID. This is a read-only scan; no state is
// modified.
func (s *Store) ScanBlockedDownstream(failedID string) ([]model.BlockedDownstream, error) {
	visited := make(map[string]struct{})
	var result []model.BlockedDownstream

	var walk func(id string) error
	walk = func(id string) error {
		rows, err := s.db.Query(`
			SELECT td.task_id, t.title, t.assigned_to
			FROM task_deps td
			JOIN tasks t ON t.id = td.task_id
			WHERE td.depends_on = ?
			  AND t.status IN ('pending','claimed')`, id)
		if err != nil {
			return fmt.Errorf("scan downstream of %s: %w", id, err)
		}
		var children []model.BlockedDownstream
		for rows.Next() {
			var b model.BlockedDownstream
			if err = rows.Scan(&b.ID, &b.Title, &b.AssignedTo); err != nil {
				rows.Close() //nolint:errcheck
				return err
			}
			children = append(children, b)
		}
		rows.Close() //nolint:errcheck
		if err = rows.Err(); err != nil {
			return err
		}
		for _, c := range children {
			if _, seen := visited[c.ID]; seen {
				continue
			}
			visited[c.ID] = struct{}{}
			result = append(result, c)
			if err = walk(c.ID); err != nil {
				return err
			}
		}
		return nil
	}

	if err := walk(failedID); err != nil {
		return nil, err
	}
	return result, nil
}

// -------------------------------------------------------------------
// Chain helpers (V8)
// -------------------------------------------------------------------

// IsChainComplete returns true if all tasks in the chain are done/cancelled,
// or their superseder is done (V7 superseded_by semantics).
func (s *Store) IsChainComplete(chainID string) (bool, error) {
	if chainID == "" {
		return false, nil
	}
	var unfinished int
	err := s.db.QueryRow(`
		SELECT COUNT(*) FROM tasks
		WHERE chain_id = ?
		  AND status NOT IN ('done', 'cancelled')
		  AND (superseded_by = '' OR NOT EXISTS (
		        SELECT 1 FROM tasks s WHERE s.id = superseded_by AND s.status = 'done'
		      ))`, chainID).Scan(&unfinished)
	if err != nil {
		return false, fmt.Errorf("IsChainComplete: %w", err)
	}
	return unfinished == 0, nil
}

// GetChainTasks returns all tasks belonging to chainID, ordered by created_at.
func (s *Store) GetChainTasks(chainID string) ([]model.Task, error) {
	rows, err := s.db.Query(`
		SELECT id, title, description, status, assigned_to, retry_assigned_to, superseded_by,
		       chain_id, notify_ceo_on_complete, stale_dispatch_count, parent_id, mode,
		       requires_review, result, failure_reason, version, priority, started_at, created_at, updated_at,
		       timeout_minutes, timeout_action, commit_url,
		       auto_advance_to, advance_task_title, advance_task_description, spec_file
		FROM tasks WHERE chain_id = ?
		ORDER BY created_at ASC`, chainID)
	if err != nil {
		return nil, fmt.Errorf("GetChainTasks: %w", err)
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
	return tasks, rows.Err()
}

// -------------------------------------------------------------------
// Stale task helpers (V9)
// -------------------------------------------------------------------

// StaleCandidateRow is a compact view used by the stale ticker.
type StaleCandidateRow struct {
	ID                 string
	Title              string
	AssignedTo         string
	StaleDispatchCount int
}

// ListStaleCandidates returns pending tasks that have not been updated for at
// least threshold duration and have a non-empty assigned_to.
// Go-layer post-filter (deps_met) is done by the caller.
//
// NOTE: Go stores updated_at in RFC3339Nano format ("2006-01-02T15:04:05.999999999Z").
// We pass the cutoff as the same format so lexicographic comparison works correctly.
func (s *Store) ListStaleCandidates(threshold time.Duration) ([]StaleCandidateRow, error) {
	cutoff := time.Now().UTC().Add(-threshold).Format(time.RFC3339Nano)
	rows, err := s.db.Query(`
		SELECT id, title, assigned_to, stale_dispatch_count FROM tasks
		WHERE status = 'pending'
		  AND assigned_to != ''
		  AND updated_at < ?
		ORDER BY updated_at ASC
		LIMIT 20`, cutoff)
	if err != nil {
		return nil, fmt.Errorf("ListStaleCandidates: %w", err)
	}
	defer rows.Close()

	var result []StaleCandidateRow
	for rows.Next() {
		var r StaleCandidateRow
		if err = rows.Scan(&r.ID, &r.Title, &r.AssignedTo, &r.StaleDispatchCount); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// TouchUpdatedAt updates updated_at to now and increments stale_dispatch_count.
// Does NOT change version (avoids optimistic lock conflicts).
// Used by stale ticker to reset the 30-minute countdown after re-dispatch.
func (s *Store) TouchUpdatedAt(id string) error {
	_, err := s.db.Exec(
		`UPDATE tasks SET updated_at = ?, stale_dispatch_count = stale_dispatch_count + 1 WHERE id = ?`,
		time.Now().UTC(), id)
	if err != nil {
		return fmt.Errorf("TouchUpdatedAt: %w", err)
	}
	return nil
}

// -------------------------------------------------------------------
// retry_routing helpers (V8)
// -------------------------------------------------------------------

// GetRetryRoute queries the retry_routing table for a matching rule.
// Priority: error_keyword match (priority DESC) > empty keyword (catch-all).
// Returns ("", nil) when no rule matches.
func (s *Store) GetRetryRoute(assignedTo, result string) (string, error) {
	var retryAgent string
	err := s.db.QueryRow(`
		SELECT retry_assigned_to FROM retry_routing
		WHERE assigned_to = ?
		  AND (error_keyword = '' OR ? LIKE '%' || error_keyword || '%')
		ORDER BY priority DESC
		LIMIT 1`, assignedTo, result).Scan(&retryAgent)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("GetRetryRoute: %w", err)
	}
	return retryAgent, nil
}

// ListRetryRoutes returns all retry routing rules.
func (s *Store) ListRetryRoutes() ([]model.RetryRoute, error) {
	rows, err := s.db.Query(
		`SELECT id, assigned_to, error_keyword, retry_assigned_to, priority FROM retry_routing ORDER BY assigned_to, priority DESC`)
	if err != nil {
		return nil, fmt.Errorf("ListRetryRoutes: %w", err)
	}
	defer rows.Close()
	var routes []model.RetryRoute
	for rows.Next() {
		var r model.RetryRoute
		if err = rows.Scan(&r.ID, &r.AssignedTo, &r.ErrorKeyword, &r.RetryAssignedTo, &r.Priority); err != nil {
			return nil, err
		}
		routes = append(routes, r)
	}
	return routes, rows.Err()
}

// CreateRetryRoute inserts a new retry routing rule.
func (s *Store) CreateRetryRoute(r model.RetryRoute) (model.RetryRoute, error) {
	res, err := s.db.Exec(
		`INSERT INTO retry_routing (assigned_to, error_keyword, retry_assigned_to, priority) VALUES (?, ?, ?, ?)`,
		r.AssignedTo, r.ErrorKeyword, r.RetryAssignedTo, r.Priority)
	if err != nil {
		return model.RetryRoute{}, fmt.Errorf("CreateRetryRoute: %w", err)
	}
	id, _ := res.LastInsertId()
	r.ID = int(id)
	return r, nil
}

// DeleteRetryRoute deletes a rule by ID. Returns ErrNotFound if not found.
func (s *Store) DeleteRetryRoute(id int) error {
	res, err := s.db.Exec(`DELETE FROM retry_routing WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("DeleteRetryRoute: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// -------------------------------------------------------------------
// Delete
// -------------------------------------------------------------------

// Summary returns a compact view of active tasks and done-today count.
// Summary returns aggregate counts for all tasks (no filter).
func (s *Store) Summary() (model.SummaryResponse, error) {
	return s.SummaryFiltered("")
}

// SummaryFiltered returns aggregate counts optionally filtered by assigned_to.
func (s *Store) SummaryFiltered(assignedTo string) (model.SummaryResponse, error) {
	whereClause := ""
	args := []any{}
	if assignedTo != "" {
		whereClause = " WHERE assigned_to = ?"
		args = append(args, assignedTo)
	}
	// Count per-status.
	rows, err := s.db.Query(
		`SELECT status, COUNT(*) FROM tasks`+whereClause+` GROUP BY status`,
		args...)
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
	doneTodayWhere := "status = 'done' AND updated_at >= ?"
	doneTodayArgs := []any{todayStart}
	if assignedTo != "" {
		doneTodayWhere += " AND assigned_to = ?"
		doneTodayArgs = append(doneTodayArgs, assignedTo)
	}
	err = s.db.QueryRow(
		`SELECT COUNT(*) FROM tasks WHERE `+doneTodayWhere,
		doneTodayArgs...,
	).Scan(&resp.DoneToday)
	if err != nil {
		return model.SummaryResponse{}, fmt.Errorf("done_today count: %w", err)
	}

	// Active tasks list (non-terminal), sorted by updated_at DESC.
	activeWhere := "status NOT IN ('done', 'cancelled')"
	activeArgs := []any{}
	if assignedTo != "" {
		activeWhere += " AND assigned_to = ?"
		activeArgs = append(activeArgs, assignedTo)
	}
	taskRows, err := s.db.Query(
		`SELECT id, title, status, assigned_to, updated_at FROM tasks WHERE `+activeWhere+` ORDER BY updated_at DESC`,
		activeArgs...,
	)
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
		SELECT id, title, description, status, assigned_to, retry_assigned_to, superseded_by,
		       chain_id, notify_ceo_on_complete, stale_dispatch_count, parent_id, mode,
		       requires_review, result, failure_reason, version, priority, started_at, created_at, updated_at,
		       timeout_minutes, timeout_action, commit_url,
		       auto_advance_to, advance_task_title, advance_task_description, spec_file
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
// deps are now done (or superseded by a done task), logs them as triggered.
// Also checks tasks that depend on tasks superseded by completedID.
// Returns the IDs of tasks that were newly unblocked (deps fully met).
func unlockDependents(tx *sql.Tx, completedID string, now time.Time) ([]string, error) {
	// Collect all candidate task IDs to check.
	// 1. Tasks that directly depend on completedID.
	// 2. Tasks that depend on a task superseded by completedID (i.e., the failed original).
	candidateSet := make(map[string]struct{})

	// Direct dependents.
	rows, err := tx.Query(`SELECT task_id FROM task_deps WHERE depends_on = ?`, completedID)
	if err != nil {
		return nil, fmt.Errorf("find dependents: %w", err)
	}
	for rows.Next() {
		var tid string
		if err = rows.Scan(&tid); err != nil {
			rows.Close() //nolint:errcheck
			return nil, err
		}
		candidateSet[tid] = struct{}{}
	}
	rows.Close() //nolint:errcheck

	// Dependents of tasks that were superseded by completedID (the failed originals).
	// e.g. A failed, A' (=completedID) supersedes A. B depends on A.
	// When A' is done, we look: who depends on A? → B.
	supersededRows, err := tx.Query(
		`SELECT id FROM tasks WHERE superseded_by = ?`, completedID)
	if err != nil {
		return nil, fmt.Errorf("find superseded tasks: %w", err)
	}
	var supersededIDs []string
	for supersededRows.Next() {
		var sid string
		if err = supersededRows.Scan(&sid); err != nil {
			supersededRows.Close() //nolint:errcheck
			return nil, err
		}
		supersededIDs = append(supersededIDs, sid)
	}
	supersededRows.Close() //nolint:errcheck

	for _, sid := range supersededIDs {
		depRows, err := tx.Query(`SELECT task_id FROM task_deps WHERE depends_on = ?`, sid)
		if err != nil {
			return nil, fmt.Errorf("find dependents of superseded %s: %w", sid, err)
		}
		for depRows.Next() {
			var tid string
			if err = depRows.Scan(&tid); err != nil {
				depRows.Close() //nolint:errcheck
				return nil, err
			}
			candidateSet[tid] = struct{}{}
		}
		depRows.Close() //nolint:errcheck
	}

	var triggered []string
	for cid := range candidateSet {
		met, err := depsMetForIDTx(tx, cid)
		if err != nil {
			return nil, fmt.Errorf("check deps for %s: %w", cid, err)
		}
		if met {
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

// GetHistory returns the full task_history for a given task ID.
func (s *Store) GetHistory(id string) ([]model.HistoryItem, error) {
	return s.historyFor(id)
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
	// A dep is "satisfied" if it is done itself, OR its superseder (superseded_by) is done.
	// We count deps that are NOT satisfied (i.e., unmet).
	err := s.db.QueryRow(`
		SELECT COUNT(*) FROM task_deps td
		JOIN tasks t ON t.id = td.depends_on
		WHERE td.task_id = ?
		  AND t.status != 'done'
		  AND (t.superseded_by = '' OR NOT EXISTS (
		        SELECT 1 FROM tasks s WHERE s.id = t.superseded_by AND s.status = 'done'
		      ))`, id).Scan(&unmet)
	if err != nil {
		return false, fmt.Errorf("deps_met query: %w", err)
	}
	return unmet == 0, nil
}

// depsMetForIDTx is the same as depsMetForID but operates within a transaction.
func depsMetForIDTx(tx *sql.Tx, id string) (bool, error) {
	var unmet int
	err := tx.QueryRow(`
		SELECT COUNT(*) FROM task_deps td
		JOIN tasks t ON t.id = td.depends_on
		WHERE td.task_id = ?
		  AND t.status != 'done'
		  AND (t.superseded_by = '' OR NOT EXISTS (
		        SELECT 1 FROM tasks s WHERE s.id = t.superseded_by AND s.status = 'done'
		      ))`, id).Scan(&unmet)
	if err != nil {
		return false, fmt.Errorf("deps_met query (tx): %w", err)
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
	var rr, notifyCEO int
	err := r.Scan(&t.ID, &t.Title, &t.Description, (*string)(&t.Status),
		&t.AssignedTo, &t.RetryAssignedTo, &t.SupersededBy, &t.ChainID, &notifyCEO,
		&t.StaleDispatchCount,
		&t.ParentID, &t.Mode, &rr,
		&t.Result, &t.FailureReason, &t.Version, &t.Priority,
		&t.StartedAt, &t.CreatedAt, &t.UpdatedAt,
		&t.TimeoutMinutes, &t.TimeoutAction, &t.CommitURL,
		&t.AutoAdvanceTo, &t.AdvanceTaskTitle, &t.AdvanceTaskDescription, &t.SpecFile)
	if err != nil {
		return model.Task{}, err
	}
	t.RequiresReview = rr != 0
	t.NotifyCEOOnComplete = notifyCEO != 0
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
		{model.StatusClaimed, model.StatusCancelled}:    true, // V25-A: admin cancel
		{model.StatusInProgress, model.StatusReview}:    true,
		{model.StatusInProgress, model.StatusDone}:      true,
		{model.StatusInProgress, model.StatusBlocked}:   true,
		{model.StatusInProgress, model.StatusFailed}:    true,
		{model.StatusInProgress, model.StatusPending}:   true,
		{model.StatusInProgress, model.StatusCancelled}: true, // V25-A: admin cancel
		{model.StatusReview, model.StatusDone}:          true,
		{model.StatusReview, model.StatusInProgress}:    true,
		{model.StatusBlocked, model.StatusPending}:      true,
		{model.StatusBlocked, model.StatusInProgress}:   true,
		// failed → pending: CEO retry (optional retry_assigned_to)
		{model.StatusFailed, model.StatusPending}:  true,
		// failed → cancelled: CEO cancels a failed task (no retry, no downstream unlock)
		{model.StatusFailed, model.StatusCancelled}: true,
		// V27-A P0-3: failed → done: agent delivers result after timeout; idempotent recovery.
		{model.StatusFailed, model.StatusDone}: true,
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
