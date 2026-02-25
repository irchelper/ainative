// Package handler implements the HTTP handler for agent-queue.
package handler

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/irchelper/agent-queue/internal/failparser"
	"github.com/irchelper/agent-queue/internal/model"
	"github.com/irchelper/agent-queue/internal/notify"
	"github.com/irchelper/agent-queue/internal/openclaw"
	"github.com/irchelper/agent-queue/internal/store"
)

// Handler holds shared dependencies for all HTTP handlers.
type Handler struct {
	store     *store.Store
	notifier  notify.Notifier
	sessionN  *notify.SessionNotifier // CEO alert for unhandled failures
	oc        *openclaw.Client
	db        *sql.DB
}

// New creates a Handler and registers all routes on mux.
func New(db *sql.DB, s *store.Store, n notify.Notifier, oc *openclaw.Client) *Handler {
	var sn *notify.SessionNotifier
	if oc != nil {
		sn = notify.NewSessionNotifier(oc, "")
	}
	return &Handler{
		store:    s,
		notifier: n,
		sessionN: sn,
		oc:       oc,
		db:       db,
	}
}

// Register wires up all routes on mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/health", h.handleHealth)
	mux.HandleFunc("/dispatch", h.handleDispatch)
	mux.HandleFunc("/dispatch/chain", h.handleDispatchChain)
	mux.HandleFunc("/tasks", h.handleTasks)
	mux.HandleFunc("/tasks/", h.handleTasksID)
}

// -------------------------------------------------------------------
// F5: Health
// -------------------------------------------------------------------

func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	dbStatus := "ok"
	if err := h.db.Ping(); err != nil {
		dbStatus = "error: " + err.Error()
	}
	writeJSON(w, http.StatusOK, model.HealthResponse{Status: "ok", Database: dbStatus})
}

// -------------------------------------------------------------------
// -------------------------------------------------------------------
// POST /dispatch
// -------------------------------------------------------------------

func (h *Handler) handleDispatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req model.DispatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Title) == "" {
		writeError(w, http.StatusBadRequest, "title is required")
		return
	}
	if strings.TrimSpace(req.AssignedTo) == "" {
		writeError(w, http.StatusBadRequest, "assigned_to is required")
		return
	}

	// Create task.
	task, err := h.store.CreateTask(model.CreateTaskRequest{
		Title:          req.Title,
		Description:    req.Description,
		AssignedTo:     req.AssignedTo,
		RequiresReview: req.RequiresReview,
		DependsOn:      req.DependsOn,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Notify expert via sessions_send (fire-and-forget; failure does not
	// affect the HTTP response for task creation).
	resp := model.DispatchResponse{Task: task}

	sessionKey, known := openclaw.SessionKey(req.AssignedTo)
	if !known {
		resp.NotifyError = fmt.Sprintf("unknown agent %q – task created but no session_send sent", req.AssignedTo)
		log.Printf("[dispatch] unknown agent %q – skipping sessions_send", req.AssignedTo)
	} else if h.oc != nil {
		msg := "[agent-queue] 你有新的待处理任务。请执行 poll 流程认领。"
		if err = h.oc.SendToSession(sessionKey, msg); err != nil {
			resp.NotifyError = err.Error()
			log.Printf("[dispatch] sessions_send to %s failed: %v", sessionKey, err)
		} else {
			resp.Notified = true
		}
	}

	writeJSON(w, http.StatusCreated, resp)
}

// -------------------------------------------------------------------
// POST /dispatch/chain
// -------------------------------------------------------------------

func (h *Handler) handleDispatchChain(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req model.ChainRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if len(req.Tasks) == 0 {
		writeError(w, http.StatusBadRequest, "tasks must be non-empty")
		return
	}
	for i, t := range req.Tasks {
		if strings.TrimSpace(t.Title) == "" {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("tasks[%d].title is required", i))
			return
		}
		if strings.TrimSpace(t.AssignedTo) == "" {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("tasks[%d].assigned_to is required", i))
			return
		}
	}

	// Create tasks in order, chaining depends_on N → N-1.
	chainID := newChainID()
	created := make([]model.Task, 0, len(req.Tasks))

	for i, spec := range req.Tasks {
		var dependsOn []string
		if i > 0 {
			dependsOn = []string{created[i-1].ID}
		}
		task, err := h.store.CreateTask(model.CreateTaskRequest{
			Title:          spec.Title,
			AssignedTo:     spec.AssignedTo,
			Description:    spec.Description,
			RequiresReview: spec.RequiresReview,
			DependsOn:      dependsOn,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("create task[%d]: %v", i, err))
			return
		}
		created = append(created, task)
	}

	// Dispatch only the first task.
	first := created[0]
	resp := model.ChainResponse{
		ChainID:         chainID,
		Tasks:           created,
		FirstDispatched: first.ID,
	}

	sessionKey, known := openclaw.SessionKey(first.AssignedTo)
	if !known {
		resp.NotifyError = fmt.Sprintf("unknown agent %q – chain created but no session_send sent", first.AssignedTo)
		log.Printf("[dispatch/chain] unknown agent %q – skipping sessions_send", first.AssignedTo)
	} else if h.oc != nil {
		msg := "[agent-queue] 你有新的待处理任务。请执行 poll 流程认领。"
		if err := h.oc.SendToSession(sessionKey, msg); err != nil {
			resp.NotifyError = err.Error()
			log.Printf("[dispatch/chain] sessions_send to %s failed: %v", sessionKey, err)
		} else {
			resp.Notified = true
		}
	}

	writeJSON(w, http.StatusCreated, resp)
}

// -------------------------------------------------------------------
// F1: /tasks
// -------------------------------------------------------------------

func (h *Handler) handleTasks(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.listTasks(w, r)
	case http.MethodPost:
		h.createTask(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (h *Handler) listTasks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	status := q.Get("status")
	assignedTo := q.Get("assigned_to")
	parentID := q.Get("parent_id")

	var depsMetFilter *bool
	if dm := q.Get("deps_met"); dm != "" {
		b := dm == "true"
		depsMetFilter = &b
	}

	tasks, err := h.store.ListTasks(status, assignedTo, parentID, depsMetFilter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks, "count": len(tasks)})
}

func (h *Handler) createTask(w http.ResponseWriter, r *http.Request) {
	var req model.CreateTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Title) == "" {
		writeError(w, http.StatusBadRequest, "title is required")
		return
	}
	task, err := h.store.CreateTask(req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, task)
}

// -------------------------------------------------------------------
// F1/F2/F3: /tasks/:id  and  /tasks/:id/claim  and  /tasks/:id/deps-met
// -------------------------------------------------------------------

func (h *Handler) handleTasksID(w http.ResponseWriter, r *http.Request) {
	// Parse the path segments after "/tasks/"
	path := strings.TrimPrefix(r.URL.Path, "/tasks/")
	if path == "" {
		writeError(w, http.StatusBadRequest, "missing task id")
		return
	}

	// Special static sub-paths: /tasks/summary and /tasks/poll
	switch path {
	case "summary":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		h.tasksSummary(w, r)
		return
	case "poll":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		h.tasksPoll(w, r)
		return
	}

	parts := strings.SplitN(path, "/", 2)
	id := parts[0]
	var sub string
	if len(parts) == 2 {
		sub = parts[1]
	}

	switch sub {
	case "":
		switch r.Method {
		case http.MethodGet:
			h.getTask(w, r, id)
		case http.MethodPatch:
			h.patchTask(w, r, id)
		case http.MethodDelete:
			h.deleteTask(w, r, id)
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	case "claim":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		h.claimTask(w, r, id)
	case "deps-met":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		h.depsMetTask(w, r, id)
	default:
		writeError(w, http.StatusNotFound, "unknown sub-resource: "+sub)
	}
}

func (h *Handler) getTask(w http.ResponseWriter, _ *http.Request, id string) {
	task, err := h.store.GetByID(id)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, task)
}

func (h *Handler) patchTask(w http.ResponseWriter, r *http.Request, id string) {
	var req model.PatchTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Version == 0 {
		writeError(w, http.StatusBadRequest, "version is required")
		return
	}

	task, triggered, err := h.store.PatchTask(id, req)
	if err != nil {
		var ve *store.ValidationError
		if errors.As(err, &ve) {
			writeError(w, http.StatusUnprocessableEntity, ve.Error())
			return
		}
		handleStoreError(w, err)
		return
	}

	// F6: async notification.
	// done → Discord webhook (user @mention).
	// failed → Discord webhook (user) + SessionNotifier (CEO session).
	if task.Status == model.StatusDone {
		notify.AsyncNotify(h.notifier, task)
	}

	var blockedDownstream []model.BlockedDownstream
	if task.Status == model.StatusFailed {
		notify.AsyncNotify(h.notifier, task) // Discord webhook to user
		h.handleFailedTask(task)             // SessionNotifier to CEO (if no auto-retry)

		// V7: scan blocked downstream tasks (read-only).
		if bd, err := h.store.ScanBlockedDownstream(id); err != nil {
			log.Printf("[handler] ScanBlockedDownstream for %s: %v", id, err)
		} else {
			blockedDownstream = bd
		}
	}

	writeJSON(w, http.StatusOK, model.PatchTaskResponse{
		Task:              task,
		Triggered:         triggered,
		BlockedDownstream: blockedDownstream,
	})
}

// handleFailedTask is called asynchronously when a task transitions to failed.
//
// Retry directive resolution (priority order):
//  1. task.RetryAssignedTo field (set explicitly via PATCH body)
//  2. failparser.ParseRetryAgent(task.Result) – legacy inline format
//
// If a retry agent is identified → auto-create retry task + dispatch (no CEO notify).
// Otherwise → SessionNotifier alerts CEO for human intervention.
func (h *Handler) handleFailedTask(task model.Task) {
	go func() {
		// Priority 1: explicit retry_assigned_to field.
		retryAgent := task.RetryAssignedTo
		// Priority 2: inline result format (legacy/expert shorthand).
		if retryAgent == "" {
			retryAgent, _ = failparser.ParseRetryAgent(task.Result)
		}

		if retryAgent != "" {
			h.autoRetry(task, retryAgent)
		} else {
			if h.sessionN != nil {
				if err := h.sessionN.OnFailed(task); err != nil {
					log.Printf("[handler] CEO notification failed for task %s: %v", task.ID, err)
				}
			}
		}
	}()
}

// autoRetry creates a new task for the retry agent and dispatches it.
// V7: retry task inherits original.DependsOn; original.superseded_by is set to retry task ID.
// Per spec: does NOT notify CEO.
func (h *Handler) autoRetry(original model.Task, retryAgent string) {
	failureDesc := original.FailureReason
	if failureDesc == "" {
		failureDesc = original.Result
	}

	// Fetch original task details to get DependsOn (may not be populated in task passed in).
	origDetail, err := h.store.GetByID(original.ID)
	if err != nil {
		log.Printf("[handler] autoRetry: GetByID original %s failed: %v", original.ID, err)
		origDetail = original // fallback
	}

	newTask, err := h.store.CreateTask(model.CreateTaskRequest{
		Title:       "retry: " + original.Title,
		AssignedTo:  retryAgent,
		Priority:    original.Priority,
		Description: "failed原因: " + failureDesc,
		DependsOn:   origDetail.DependsOn, // V7: inherit original deps
	})
	if err != nil {
		log.Printf("[handler] autoRetry CreateTask failed for original %s: %v", original.ID, err)
		return
	}
	log.Printf("[handler] autoRetry: created task %s for agent %s (original: %s)",
		newTask.ID, retryAgent, original.ID)

	// V7: mark original as superseded by the retry task.
	if err := h.store.SetSupersededBy(original.ID, newTask.ID); err != nil {
		log.Printf("[handler] autoRetry SetSupersededBy failed (original %s → retry %s): %v",
			original.ID, newTask.ID, err)
	}

	// Dispatch to expert session (no CEO notification per spec).
	sessionKey, known := openclaw.SessionKey(retryAgent)
	if !known {
		log.Printf("[handler] autoRetry: unknown agent %q – task %s created but not dispatched", retryAgent, newTask.ID)
		return
	}
	if h.oc == nil {
		return
	}
	msg := "[agent-queue] 你有新的待处理任务。请执行 poll 流程认领。"
	if err := h.oc.SendToSession(sessionKey, msg); err != nil {
		log.Printf("[handler] autoRetry dispatch to %s failed: %v", sessionKey, err)
	} else {
		log.Printf("[handler] autoRetry dispatched task %s to %s", newTask.ID, retryAgent)
	}
}

func (h *Handler) deleteTask(w http.ResponseWriter, _ *http.Request, id string) {
	if err := h.store.DeleteTask(id); err != nil {
		handleStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// F2
func (h *Handler) claimTask(w http.ResponseWriter, r *http.Request, id string) {
	var req model.ClaimRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if req.Version == 0 {
		writeError(w, http.StatusBadRequest, "version is required")
		return
	}
	if strings.TrimSpace(req.Agent) == "" {
		writeError(w, http.StatusBadRequest, "agent is required")
		return
	}

	task, err := h.store.Claim(id, req.Version, req.Agent)
	if err != nil {
		handleStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, task)
}

// GET /tasks/poll?assigned_to=<agent>
func (h *Handler) tasksPoll(w http.ResponseWriter, r *http.Request) {
	assignedTo := r.URL.Query().Get("assigned_to")
	if strings.TrimSpace(assignedTo) == "" {
		writeError(w, http.StatusBadRequest, "assigned_to query param is required")
		return
	}
	task, err := h.store.Poll(assignedTo)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, model.PollResponse{Task: task})
}

// GET /tasks/summary
func (h *Handler) tasksSummary(w http.ResponseWriter, _ *http.Request) {
	summary, err := h.store.Summary()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

// F3
func (h *Handler) depsMetTask(w http.ResponseWriter, _ *http.Request, id string) {
	// Verify task exists.
	if _, err := h.store.GetByID(id); err != nil {
		handleStoreError(w, err)
		return
	}
	met, err := h.store.DepsMet(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, model.DepsMet{TaskID: id, DepsMet: met})
}

// -------------------------------------------------------------------
// Helpers
// -------------------------------------------------------------------

func handleStoreError(w http.ResponseWriter, err error) {
	switch {
	case store.IsNotFound(err):
		writeError(w, http.StatusNotFound, err.Error())
	case store.IsConflict(err):
		writeError(w, http.StatusConflict, "version conflict or task already claimed")
	default:
		log.Printf("[handler] internal error: %v", err)
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("[handler] encode response: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, model.ErrorResponse{Error: msg})
}

// newChainID generates a unique chain identifier.
func newChainID() string {
	return fmt.Sprintf("chain_%x", time.Now().UnixNano())
}
