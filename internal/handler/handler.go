// Package handler implements the HTTP handler for agent-queue.
package handler

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/irchelper/agent-queue/internal/failparser"
	"github.com/irchelper/agent-queue/internal/model"
	"github.com/irchelper/agent-queue/internal/notify"
	"github.com/irchelper/agent-queue/internal/openclaw"
	"github.com/irchelper/agent-queue/internal/store"
	"github.com/irchelper/agent-queue/internal/webui"
)

// Handler holds shared dependencies for all HTTP handlers.
type Handler struct {
	store    *store.Store
	notifier notify.Notifier
	sessionN *notify.SessionNotifier // CEO alert for unhandled failures
	oc       *openclaw.Client
	db       *sql.DB

	// SSE hub for real-time push to browser clients.
	hub *SSEHub

	// stale ticker fields
	staleStop          chan struct{}
	staleWG            sync.WaitGroup
	staleInterval      time.Duration
	staleThreshold     time.Duration
	maxStaleDispatches int

	// agent timeout: in_progress tasks older than this → failed
	agentTimeoutMinutes int

	// process metadata for /api/config
	startTime time.Time
}

// New creates a Handler and registers all routes on mux.
func New(db *sql.DB, s *store.Store, n notify.Notifier, oc *openclaw.Client) *Handler {
	var sn *notify.SessionNotifier
	if oc != nil {
		sn = notify.NewSessionNotifier(oc, "")
	}
	return &Handler{
		store:               s,
		notifier:            n,
		sessionN:            sn,
		oc:                  oc,
		db:                  db,
		hub:                 NewSSEHub(),
		staleStop:           make(chan struct{}),
		staleInterval:       envDuration("AGENT_QUEUE_STALE_CHECK_INTERVAL", 10*time.Minute),
		staleThreshold:      envDuration("AGENT_QUEUE_STALE_THRESHOLD", 30*time.Minute),
		maxStaleDispatches:  envInt("AGENT_QUEUE_MAX_STALE_DISPATCHES", 3),
		agentTimeoutMinutes: envInt("AGENT_QUEUE_AGENT_TIMEOUT_MINUTES", 90),
		startTime:           time.Now().UTC(),
	}
}

// SetStaleThresholdForTesting overrides the stale threshold. Test use only.
func (h *Handler) SetStaleThresholdForTesting(d time.Duration) { h.staleThreshold = d }

// SetMaxStaleDispatchesForTesting overrides the max stale dispatches. Test use only.
func (h *Handler) SetMaxStaleDispatchesForTesting(n int) { h.maxStaleDispatches = n }

// SetAgentTimeoutMinutesForTesting overrides the agent timeout threshold. Test use only.
func (h *Handler) SetAgentTimeoutMinutesForTesting(m int) { h.agentTimeoutMinutes = m }

// DB returns the underlying *sql.DB. Test use only (for backdating timestamps etc.).
func (h *Handler) DB() *sql.DB { return h.db }

// StartRetryQueue starts the SessionNotifier's internal retry queue goroutine.
func (h *Handler) StartRetryQueue() {
	if h.sessionN != nil {
		h.sessionN.Start()
		log.Println("[retry_queue] started")
	}
}

// StopRetryQueue stops the SessionNotifier's retry queue goroutine.
func (h *Handler) StopRetryQueue() {
	if h.sessionN != nil {
		h.sessionN.Stop()
	}
}

// StartStaleTicker launches the background stale task re-dispatch goroutine.
func (h *Handler) StartStaleTicker() {
	if h.sessionN == nil {
		log.Println("[stale_ticker] no SessionNotifier configured, stale ticker disabled")
		return
	}
	h.staleWG.Add(1)
	go h.runStaleTicker()
	log.Printf("[stale_ticker] started: interval=%v threshold=%v", h.staleInterval, h.staleThreshold)
}

// StopStaleTicker signals the stale ticker goroutine to stop and waits.
func (h *Handler) StopStaleTicker() {
	close(h.staleStop)
	h.staleWG.Wait()
}

// runStaleTicker is the background goroutine body.
func (h *Handler) runStaleTicker() {
	defer h.staleWG.Done()
	ticker := time.NewTicker(h.staleInterval)
	defer ticker.Stop()

	for {
		select {
		case <-h.staleStop:
			return
		case <-ticker.C:
			h.checkStaleTasks()
			h.checkHumanTimeouts()
			h.checkAgentTimeouts()
		}
	}
}

// CheckStaleTasks is exported for testing. In production it is called by runStaleTicker.
func (h *Handler) CheckStaleTasks() { h.checkStaleTasks() }

// checkStaleTasks fetches stale pending tasks and re-dispatches them.
func (h *Handler) checkStaleTasks() {
	candidates, err := h.store.ListStaleCandidates(h.staleThreshold)
	if err != nil {
		log.Printf("[stale_ticker] ListStaleCandidates: %v", err)
		return
	}

	// Post-filter: only tasks whose deps are met (inline deps check).
	for _, c := range candidates {
		// V31-TEST-SILENCE: test tasks do not re-dispatch; they still respect stale max alert.
		if isTestTaskTitleAssignee(c.Title, c.AssignedTo) {
			if c.StaleDispatchCount >= h.maxStaleDispatches {
				log.Printf("[stale_ticker] test task %s (%s) reached max stale dispatches (%d), alerting CEO",
					c.ID, c.Title, h.maxStaleDispatches)
				if h.sessionN != nil {
					alertTask, err2 := h.store.GetByID(c.ID)
					if err2 != nil {
						log.Printf("[stale_ticker] GetByID %s for CEO alert: %v", c.ID, err2)
						continue
					}
					alertTask.FailureReason = fmt.Sprintf("stale max dispatches reached (%d/%d)", c.StaleDispatchCount, h.maxStaleDispatches)
					go h.sessionN.OnFailed(alertTask) // 方案C: async, don't block ticker
				}
				// Touch to reset countdown and avoid repeated alerts every tick.
				if err := h.store.TouchUpdatedAt(c.ID); err != nil {
					log.Printf("[stale_ticker] TouchUpdatedAt(%s): %v", c.ID, err)
				}
			} else {
				// No re-dispatch for test tasks; just advance stale counter.
				if err := h.store.TouchUpdatedAt(c.ID); err != nil {
					log.Printf("[stale_ticker] TouchUpdatedAt(%s): %v", c.ID, err)
				}
			}
			continue
		}

		met, err := h.store.DepsMet(c.ID)
		if err != nil {
			log.Printf("[stale_ticker] DepsMet(%s): %v", c.ID, err)
			continue
		}
		if !met {
			continue
		}
		// Check stale dispatch limit.
		if c.StaleDispatchCount >= h.maxStaleDispatches {
			log.Printf("[stale_ticker] task %s (%s) reached max stale dispatches (%d), alerting CEO",
				c.ID, c.Title, h.maxStaleDispatches)
			if h.sessionN != nil {
				// Construct a minimal Task for the CEO alert.
				alertTask, err2 := h.store.GetByID(c.ID)
				if err2 != nil {
					log.Printf("[stale_ticker] GetByID %s for CEO alert: %v", c.ID, err2)
					continue
				}
				alertTask.FailureReason = fmt.Sprintf("stale max dispatches reached (%d/%d)", c.StaleDispatchCount, h.maxStaleDispatches)
				go h.sessionN.OnFailed(alertTask) // 方案C: async, don't block ticker
			}
			// Touch to reset countdown and avoid repeated alerts every tick.
			if err := h.store.TouchUpdatedAt(c.ID); err != nil {
				log.Printf("[stale_ticker] TouchUpdatedAt(%s): %v", c.ID, err)
			}
			continue
		}

		// Re-dispatch: fire-and-forget sessions_send (Dispatch does not retry).
		if _, ok := h.sessionN.Dispatch(c.AssignedTo); ok {
			log.Printf("[stale_ticker] re-dispatched stale task %s (%s) to %s", c.ID, c.Title, c.AssignedTo)
		}
		// Reset the 30-min countdown and increment stale_dispatch_count.
		if err := h.store.TouchUpdatedAt(c.ID); err != nil {
			log.Printf("[stale_ticker] TouchUpdatedAt(%s): %v", c.ID, err)
		}
	}
}

// checkHumanTimeouts processes tasks assigned to "human" that have exceeded their
// timeout_minutes threshold.
//   - timeout_action = "escalate" → transition to blocked
//   - timeout_action = "skip"     → transition to done (auto-skipped)
//   - timeout_action = ""         → no action (let stale ticker handle)
func (h *Handler) checkHumanTimeouts() {
	tasks, err := h.store.ListTasks("pending", "human", "", nil)
	if err != nil {
		log.Printf("[human_timeout] ListTasks: %v", err)
		return
	}
	now := time.Now().UTC()
	for _, task := range tasks {
		if task.TimeoutMinutes == nil || *task.TimeoutMinutes <= 0 {
			continue
		}
		deadline := task.CreatedAt.Add(time.Duration(*task.TimeoutMinutes) * time.Minute)
		if now.Before(deadline) {
			continue
		}
		// Timed out — check action.
		action := ""
		if task.TimeoutAction != nil {
			action = *task.TimeoutAction
		}
		switch action {
		case "escalate":
			// pending → claimed → in_progress → blocked (FSM requires intermediate steps)
			if err := h.advanceHumanTask(task, model.StatusBlocked, "human timeout: escalated"); err != nil {
				log.Printf("[human_timeout] escalate task %s: %v", task.ID, err)
			} else {
				log.Printf("[human_timeout] escalated task %s (%s)", task.ID, task.Title)
			}
		case "skip":
			if err := h.advanceHumanTask(task, model.StatusDone, "human timeout: skipped"); err != nil {
				log.Printf("[human_timeout] skip task %s: %v", task.ID, err)
			} else {
				log.Printf("[human_timeout] skipped task %s (%s)", task.ID, task.Title)
			}
		default:
			// No automatic action.
		}
	}
}

// checkAgentTimeouts scans in_progress tasks (assigned_to != "human") and
// transitions to failed if updated_at exceeds the effective timeout threshold.
//
// V27-A P0-1: use updated_at instead of started_at so that active agents
//   (which call PATCH to report progress) are not killed mid-execution.
// V27-A P0-2: per-task timeout_minutes takes priority over global agentTimeoutMinutes.
func (h *Handler) checkAgentTimeouts() {
	if h.agentTimeoutMinutes <= 0 {
		return
	}
	tasks, err := h.store.ListTasks("in_progress", "", "", nil)
	if err != nil {
		log.Printf("[agent_timeout] ListTasks: %v", err)
		return
	}
	now := time.Now().UTC()
	for _, task := range tasks {
		if task.AssignedTo == "human" {
			continue
		}
		// P0-2: per-task timeout wins over global config.
		timeoutMinutes := h.agentTimeoutMinutes
		if task.TimeoutMinutes != nil && *task.TimeoutMinutes > 0 {
			timeoutMinutes = *task.TimeoutMinutes
		}
		threshold := time.Duration(timeoutMinutes) * time.Minute

		if task.StartedAt == nil {
			continue
		}
		if now.Sub(*task.StartedAt) <= threshold {
			continue
		}
		// Timed out → PATCH failed
		failedStatus := model.StatusFailed
		reason := fmt.Sprintf("agent_timeout: exceeded %dmin SLA", timeoutMinutes)
		failedTask, _, err := h.store.PatchTask(task.ID, model.PatchTaskRequest{
			Status:        &failedStatus,
			FailureReason: &reason,
			Version:       task.Version,
			ChangedBy:     "system",
		})
		if err != nil {
			log.Printf("[agent_timeout] PatchTask failed for %s: %v", task.ID, err)
			continue
		}
		log.Printf("[agent_timeout] task %s (%s) timed out after %dmin → failed (updated_at idle)",
			failedTask.ID, failedTask.Title, timeoutMinutes)

		// V31-TEST-SILENCE: auto-cancel test tasks and skip notifications.
		if isTestTask(failedTask) {
			cancelStatus := model.StatusCancelled
			if _, _, err := h.store.PatchTask(failedTask.ID, model.PatchTaskRequest{
				Status:    &cancelStatus,
				Version:   failedTask.Version,
				ChangedBy: "system",
				Note:      "test task auto-cancel after failure",
			}); err != nil {
				log.Printf("[agent_timeout] auto-cancel test task %s failed: %v", failedTask.ID, err)
			}
			continue
		}

		// Notify CEO via SessionNotifier (same path as handleFailedTask)
		if h.sessionN != nil {
			failedTask.FailureReason = reason
			if err := h.sessionN.OnFailed(failedTask); err != nil {
				log.Printf("[agent_timeout] OnFailed notification for %s: %v", failedTask.ID, err)
			}
		}
	}
}

// CheckAgentTimeouts is exported for testing.
func (h *Handler) CheckAgentTimeouts() { h.checkAgentTimeouts() }

// advanceHumanTask drives a pending human task through FSM states to reach targetStatus.
// For done/blocked, the FSM path is: pending → claimed → in_progress → target.
func (h *Handler) advanceHumanTask(task model.Task, targetStatus model.Status, result string) error {
	ver := task.Version

	claimedStatus := model.Status("claimed")
	_, _, err := h.store.PatchTask(task.ID, model.PatchTaskRequest{Status: &claimedStatus, Version: ver, ChangedBy: "system"})
	if err != nil {
		return fmt.Errorf("claim: %w", err)
	}
	ver++

	ipStatus := model.StatusInProgress
	_, _, err = h.store.PatchTask(task.ID, model.PatchTaskRequest{Status: &ipStatus, Version: ver, ChangedBy: "system"})
	if err != nil {
		return fmt.Errorf("in_progress: %w", err)
	}
	ver++

	r := result
	_, _, err = h.store.PatchTask(task.ID, model.PatchTaskRequest{Status: &targetStatus, Result: &r, Version: ver, ChangedBy: "system"})
	return err
}

// envDuration reads an environment variable as a duration; falls back to def.
func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	// Accept plain seconds (e.g. "600") or Go duration strings (e.g. "10m").
	if secs, err := strconv.Atoi(v); err == nil {
		return time.Duration(secs) * time.Second
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Printf("[handler] invalid %s=%q, using default %v", key, v, def)
		return def
	}
	return d
}

// envInt reads an environment variable as an int; falls back to def.
func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Printf("[handler] invalid %s=%q, using default %d", key, v, def)
		return def
	}
	return n
}

func envString(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func formatUptime(d time.Duration) string {
	if d < time.Minute {
		return "0h 0m"
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	return fmt.Sprintf("%dh %dm", h, m)
}

// Register wires up all routes on mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/health", h.handleHealth)
	mux.HandleFunc("/dispatch", h.handleDispatch)
	mux.HandleFunc("/dispatch/chain", h.handleDispatchChain)
	mux.HandleFunc("/tasks", h.handleTasks)
	mux.HandleFunc("/tasks/", h.handleTasksID)
	mux.HandleFunc("/retry-routing", h.handleRetryRouting)
	mux.HandleFunc("/retry-routing/", h.handleRetryRoutingID)
	mux.HandleFunc("/chains/", h.handleChains)
	// V12: AI Workbench API endpoints.
	mux.HandleFunc("/api/dashboard", h.handleAPIDashboard)
	mux.HandleFunc("/api/timeline/", h.handleAPITimeline)
	mux.HandleFunc("/api/chains", h.handleAPIChains)
	mux.HandleFunc("/api/config", h.handleAPIConfig)
	// V15: Task Templates.
	h.registerTemplateRoutes(mux)
	// V17: SSE real-time updates.
	mux.Handle("/api/events", h.hub)
	// V18: Graph dispatch.
	h.registerGraphRoutes(mux)
	// V20: API docs + graph endpoint.
	mux.HandleFunc("/docs", h.handleDocs)
	mux.HandleFunc("/openapi.json", h.handleOpenAPISpec)
	mux.HandleFunc("/api/graph/", h.handleAPIGraph)
	// V21: Agent stats.
	mux.HandleFunc("/api/agents/stats", h.handleAPIAgentStats)
	// V22: Bulk operations.
	h.registerBulkRoutes(mux)
	h.registerCommentRoutes(mux)
	h.registerAdminRoutes(mux)
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

	// V30-v4: spec_file support — read file contents and prepend to description.
	description := req.Description
	specFilePath := strings.TrimSpace(req.SpecFile)
	if specFilePath != "" {
		// Expand ~ to home directory.
		if strings.HasPrefix(specFilePath, "~/") {
			home, err := os.UserHomeDir()
			if err == nil {
				specFilePath = filepath.Join(home, specFilePath[2:])
			}
		}
		specContent, err := os.ReadFile(specFilePath)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("spec_file read error: %v", err))
			return
		}
		if description != "" {
			description = string(specContent) + "\n\n---\n" + description
		} else {
			description = string(specContent)
		}
	}

	// Create task.
	task, err := h.store.CreateTask(model.CreateTaskRequest{
		Title:                  req.Title,
		Description:            description,
		AssignedTo:             req.AssignedTo,
		RequiresReview:         req.RequiresReview,
		DependsOn:              req.DependsOn,
		NotifyCEOOnComplete:    req.NotifyCEOOnComplete,
		Priority:               req.Priority,
		CommitURL:              req.CommitURL,
		TimeoutMinutes:         req.TimeoutMinutes,
		TimeoutAction:          req.TimeoutAction,
		AutoAdvanceTo:          req.AutoAdvanceTo,
		AdvanceTaskTitle:       req.AdvanceTaskTitle,
		AdvanceTaskDescription: req.AdvanceTaskDescription,
		SpecFile:               req.SpecFile,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// V17: broadcast task_created event.
	h.hub.Broadcast(SSEEvent{Type: "task_created", TaskID: task.ID, Status: string(task.Status)})

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

		// V31-P0-2: spec_file support for chain tasks (same logic as handleDispatch).
		description := spec.Description
		if sfPath := strings.TrimSpace(spec.SpecFile); sfPath != "" {
			if strings.HasPrefix(sfPath, "~/") {
				if home, herr := os.UserHomeDir(); herr == nil {
					sfPath = filepath.Join(home, sfPath[2:])
				}
			}
			sfContent, sfErr := os.ReadFile(sfPath)
			if sfErr != nil {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("tasks[%d].spec_file read error: %v", i, sfErr))
				return
			}
			if description != "" {
				description = string(sfContent) + "\n\n---\n" + description
			} else {
				description = string(sfContent)
			}
		}

		task, err := h.store.CreateTask(model.CreateTaskRequest{
			Title:               spec.Title,
			AssignedTo:          spec.AssignedTo,
			Description:         description,
			RequiresReview:      spec.RequiresReview,
			Priority:            spec.Priority,
			SpecFile:            spec.SpecFile,
			DependsOn:           dependsOn,
			ChainID:             chainID,
			NotifyCEOOnComplete: req.NotifyCEOOnComplete,
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
	_ = req.ChainTitle // stored in tasks; not in ChainResponse currently

	// V17: broadcast task_created for each task in the chain.
	for _, t := range created {
		h.hub.Broadcast(SSEEvent{Type: "task_created", TaskID: t.ID, Status: string(t.Status)})
	}

	sessionKey, known := openclaw.SessionKey(first.AssignedTo)
	if !known {
		resp.NotifyError = fmt.Sprintf("unknown agent %q – chain created but no session_send sent", first.AssignedTo)
		log.Printf("[dispatch/chain] unknown agent %q – skipping sessions_send", first.AssignedTo)
	} else if h.oc != nil {
		resp.Notified = true // optimistic: async send in progress
		go func(sk, assignedTo string) {
			msg := "[agent-queue] 你有新的待处理任务。请执行 poll 流程认领。"
			if err := h.oc.SendToSession(sk, msg); err != nil {
				log.Printf("[dispatch/chain] sessions_send to %s failed: %v", assignedTo, err)
			}
		}(sessionKey, first.AssignedTo)
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
	search := q.Get("search")

	var depsMetFilter *bool
	if dm := q.Get("deps_met"); dm != "" {
		b := dm == "true"
		depsMetFilter = &b
	}

	tasks, err := h.store.ListTasksSearch(status, assignedTo, parentID, search, depsMetFilter)
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
			// V29b TASK-A: If the client is a browser (Accept: text/html), serve SPA
			// instead of JSON. This fixes direct URL access to /tasks/:id in Vue Router
			// history mode where Go's /tasks/ handler takes priority over the SPA fallback.
			if strings.Contains(r.Header.Get("Accept"), "text/html") {
				webui.ServeSPA(w, r)
				return
			}
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

	// V31-BrowserRelay: if this failed status is due to Browser Relay not attached,
	// convert to blocked early to avoid failed/autoRetry/stale paths.
	if req.Status != nil && *req.Status == model.StatusFailed {
		payload := ""
		if req.FailureReason != nil {
			payload += *req.FailureReason + "\n"
		}
		if req.Result != nil {
			payload += *req.Result
		}
		if isBrowserRelayNotAttachedText(payload) {
			blocked := model.StatusBlocked
			reason := "matched_rule=browser_relay_not_attached; route_reason=needs_user_attach"
			if req.Result != nil && strings.TrimSpace(*req.Result) != "" {
				combined := *req.Result + "\n" + reason
				req.Result = &combined
			} else {
				req.Result = &reason
			}
			req.Status = &blocked
			req.Note = "browser relay not attached"
		}
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

	// V17: broadcast SSE event to all connected browser clients.
	h.hub.Broadcast(SSEEvent{
		Type:   "task_updated",
		TaskID: task.ID,
		Status: string(task.Status),
	})

	// F6: async notification.
	// done → Discord webhook (user @mention) + V8: triggered dispatch + chain complete check.
	// failed → Discord webhook (user) + SessionNotifier (CEO session).
	if task.Status == model.StatusDone {
		notify.AsyncNotify(h.notifier, task)

		// V8 triggered 缺口修复：遍历 triggered 任务，逐一唤醒下游专家 session。
		if h.sessionN != nil && len(triggered) > 0 {
			go func(ids []string) {
				for _, tid := range ids {
					downstream, err := h.store.GetByID(tid)
					if err != nil {
						log.Printf("[handler] triggered GetByID %s: %v", tid, err)
						continue
					}
					if downstream.AssignedTo != "" {
						h.sessionN.Dispatch(downstream.AssignedTo)
					}
				}
			}(triggered)
		}

		// 单任务完成通知：无 chain_id 且 notify_ceo_on_complete=true 时直接通知 CEO。
		if task.ChainID == "" && task.NotifyCEOOnComplete && h.sessionN != nil {
			go func(t model.Task) {
				if err := h.sessionN.OnTaskComplete(t); err != nil {
					log.Printf("[handler] OnTaskComplete task=%s: %v", t.ID, err)
				}
			}(task)
		}

		// V8 chain complete：若整条链已完成且需要通知 CEO，发送汇总消息。
		if task.ChainID != "" && task.NotifyCEOOnComplete && h.sessionN != nil {
			go func(chainID string) {
				complete, err := h.store.IsChainComplete(chainID)
				if err != nil {
					log.Printf("[handler] IsChainComplete chain=%s: %v", chainID, err)
					return
				}
				if !complete {
					return
				}
				chainTasks, err := h.store.GetChainTasks(chainID)
				if err != nil {
					log.Printf("[handler] GetChainTasks chain=%s: %v", chainID, err)
					return
				}
				if err := h.sessionN.OnChainComplete(chainID, "", chainTasks); err != nil {
					log.Printf("[handler] OnChainComplete chain=%s: %v", chainID, err)
				}
			}(task.ChainID)
		}

		// V13 autoAdvance: success-path dispatch symmetric to autoRetry.
		// When auto_advance_to is set, automatically create and dispatch the next task.
		if task.AutoAdvanceTo != "" {
			h.autoAdvance(task)
		} else {
			// V14 result routing: parse result JSON for next_agent directive.
			// Only fires when auto_advance_to is NOT set (autoAdvance takes priority).
			h.resultRouting(task)
		}
	}

	var blockedDownstream []model.BlockedDownstream
	if task.Status == model.StatusFailed {
		// V31-TEST-SILENCE: test tasks fail silently + auto-cancelled; no retry/stale/notify.
		if isTestTask(task) {
			cancelStatus := model.StatusCancelled
			if cancelled, _, err := h.store.PatchTask(task.ID, model.PatchTaskRequest{
				Status:    &cancelStatus,
				Version:   task.Version,
				ChangedBy: "system",
				Note:      "test task auto-cancel after failure",
			}); err != nil {
				log.Printf("[handler] auto-cancel test task %s failed: %v", task.ID, err)
			} else {
				task = cancelled
			}
		} else {
			notify.AsyncNotify(h.notifier, task) // Discord webhook to user
			h.handleFailedTask(task)             // SessionNotifier to CEO (if no auto-retry)

			// V7: scan blocked downstream tasks (read-only).
			if bd, err := h.store.ScanBlockedDownstream(id); err != nil {
				log.Printf("[handler] ScanBlockedDownstream for %s: %v", id, err)
			} else {
				blockedDownstream = bd
			}
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
//  3. retry_routing table lookup (GetRetryRoute by assigned_to + result keywords)
//
// If a retry agent is identified → auto-create retry task + dispatch (no CEO notify).
// Otherwise → SessionNotifier alerts CEO for human intervention.
func (h *Handler) handleFailedTask(task model.Task) {
	go func() {
		// V31-TEST-SILENCE: test tasks do not enter retry_routing or notify.
		if isTestTask(task) {
			return
		}
		// V31-P0: autoRetry depth cap (retry/fix/re-review) >= 3 → stop and alert CEO.
		retryDepth := strings.Count(task.Title, "retry:") + strings.Count(task.Title, "fix:") + strings.Count(task.Title, "re-review:")
		if retryDepth >= 3 {
			if h.sessionN != nil {
				blocked := task
				blocked.FailureReason = "任务 retry 已达3级上限，需CEO介入"
				if err := h.sessionN.OnFailed(blocked); err != nil {
					log.Printf("[handler] retry depth cap OnFailed for %s: %v", task.ID, err)
				}
			}
			return
		}
		// Priority 1: explicit retry_assigned_to field.
		retryAgent := task.RetryAssignedTo
		// Priority 2: inline result format (legacy/expert shorthand).
		if retryAgent == "" {
			retryAgent, _ = failparser.ParseRetryAgent(task.Result)
		}
		// Priority 3: retry_routing table lookup.
		if retryAgent == "" && task.AssignedTo != "" {
			if route, err := h.store.GetRetryRoute(task.AssignedTo, task.Result); err != nil {
				log.Printf("[handler] GetRetryRoute for task %s: %v", task.ID, err)
			} else {
				retryAgent = route
			}
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

// autoRetry creates retry task(s) for the retry agent and dispatches.
// V7: retry task inherits original.DependsOn; original.superseded_by is set to retry task ID.
// V10: if the original task was assigned to a reviewer (thinker/security) and the retry
//      agent is different, a two-stage chain is created:
//      fix task (retryAgent) → re-review task (original.AssignedTo)
//      superseded_by points to the re-review task so downstream deps wait for re-approval.
// Per spec: does NOT notify CEO.
func (h *Handler) autoRetry(original model.Task, retryAgent string) {
	failureDesc := original.FailureReason
	if failureDesc == "" {
		failureDesc = original.Result
	}

	// Fetch original task details to get DependsOn/ChainID/NotifyCEOOnComplete.
	origDetail, err := h.store.GetByID(original.ID)
	if err != nil {
		log.Printf("[handler] autoRetry: GetByID original %s failed: %v", original.ID, err)
		origDetail = original // fallback
	}

	// V10: detect review-reject scenario.
	// A review reject occurs when the original task was performed by a reviewer
	// (thinker/security) and the retry is directed to a different agent (the implementer).
	isReviewReject := original.AssignedTo != retryAgent &&
		(original.AssignedTo == "thinker" || original.AssignedTo == "security" || original.AssignedTo == "vision")

	if isReviewReject {
		h.autoRetryReviewReject(original, origDetail, retryAgent, failureDesc)
		return
	}

	// Standard single-task retry (existing behaviour).
	newTask, err := h.store.CreateTask(model.CreateTaskRequest{
		Title:               "retry: " + original.Title,
		AssignedTo:          retryAgent,
		Priority:            original.Priority,
		Description:         "failed原因: " + failureDesc,
		DependsOn:           origDetail.DependsOn,           // V7: inherit original deps
		ChainID:             origDetail.ChainID,             // V8: propagate chain membership
		NotifyCEOOnComplete: origDetail.NotifyCEOOnComplete, // V8: propagate notification flag
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

	h.dispatchToAgent(retryAgent, newTask.ID)
}

// autoRetryReviewReject handles the V10 review-reject case.
// Creates a two-stage chain: fix task → re-review task.
// superseded_by of the original review task points to the re-review task,
// so downstream deps (e.g. qa) wait until re-review passes.
func (h *Handler) autoRetryReviewReject(original, origDetail model.Task, retryAgent, failureDesc string) {
	// Stage 1: fix task (assigned to the implementer, e.g. coder/writer).
	fixTask, err := h.store.CreateTask(model.CreateTaskRequest{
		Title:               "fix: " + original.Title,
		AssignedTo:          retryAgent,
		Priority:            original.Priority,
		Description:         "审核退单修改意见:\n" + failureDesc,
		DependsOn:           origDetail.DependsOn,           // inherit original reviewer's deps (e.g. C1)
		ChainID:             origDetail.ChainID,
		NotifyCEOOnComplete: origDetail.NotifyCEOOnComplete,
	})
	if err != nil {
		log.Printf("[handler] autoRetry(review-reject) CreateTask fix failed for original %s: %v", original.ID, err)
		return
	}
	log.Printf("[handler] autoRetry(review-reject): created fix task %s for agent %s (original: %s)",
		fixTask.ID, retryAgent, original.ID)

	// Stage 2: re-review task (assigned back to the original reviewer, e.g. thinker/security).
	reReviewTask, err := h.store.CreateTask(model.CreateTaskRequest{
		Title:               "re-review: " + original.Title,
		AssignedTo:          original.AssignedTo, // thinker/security — the original reviewer
		Priority:            original.Priority,
		Description:         "重新审核修复后的实现。原退单意见:\n" + failureDesc,
		DependsOn:           []string{fixTask.ID}, // depends on the fix being completed first
		ChainID:             origDetail.ChainID,
		NotifyCEOOnComplete: origDetail.NotifyCEOOnComplete,
	})
	if err != nil {
		log.Printf("[handler] autoRetry(review-reject) CreateTask re-review failed (fix %s): %v", fixTask.ID, err)
		return
	}
	log.Printf("[handler] autoRetry(review-reject): created re-review task %s for agent %s",
		reReviewTask.ID, original.AssignedTo)

	// Multi-level reject handling: update any existing superseded_by pointers
	// that pointed at original.ID to now point at reReviewTask.ID.
	// This handles the case where a prior re-review also failed and created new fix/re-review tasks.
	if err := h.store.UpdateSupersededByChain(original.ID, reReviewTask.ID); err != nil {
		log.Printf("[handler] autoRetry UpdateSupersededByChain failed: %v", err)
	}

	// superseded_by of the original review task points to the re-review task (not the fix task).
	// This way, depsMetForID for downstream tasks (e.g. qa) waits for re-review to pass.
	if err := h.store.SetSupersededBy(original.ID, reReviewTask.ID); err != nil {
		log.Printf("[handler] autoRetry SetSupersededBy failed (original %s → re-review %s): %v",
			original.ID, reReviewTask.ID, err)
	}

	// Dispatch only the fix task; re-review will be dispatched automatically
	// when fix task reaches 'done' (unlockDependents → triggered → SessionNotifier.Dispatch).
	h.dispatchToAgent(retryAgent, fixTask.ID)
}

// resultRouting parses task.Result as JSON and dispatches a follow-up task if
// the result contains a next_agent field.
//
// JSON schema (all fields optional except next_agent):
//
//	{ "next_agent": "qa", "next_title": "QA验证", "next_description": "..." }
//
// If next_title is omitted, defaults to "result-route: <original.Title>".
// If next_description is omitted, upstream result is used as context.
// Invalid JSON or missing next_agent → silently skipped (no error).
// autoAdvance (auto_advance_to) takes priority; resultRouting is only called
// when auto_advance_to is empty.
func (h *Handler) resultRouting(original model.Task) {
	if original.Result == "" {
		return
	}
	var parsed struct {
		NextAgent       string `json:"next_agent"`
		NextTitle       string `json:"next_title"`
		NextDescription string `json:"next_description"`
	}
	if err := json.Unmarshal([]byte(original.Result), &parsed); err != nil {
		// Not JSON or malformed – silently skip.
		return
	}
	if parsed.NextAgent == "" {
		return
	}

	go func() {
		title := parsed.NextTitle
		if title == "" {
			title = "result-route: " + original.Title
		}
		desc := parsed.NextDescription
		upstreamResult := original.Result
		if desc != "" {
			desc = "前置结果：" + upstreamResult + "\n\n" + desc
		} else {
			desc = "前置结果：" + upstreamResult
		}

		newTask, err := h.store.CreateTask(model.CreateTaskRequest{
			Title:               title,
			AssignedTo:          parsed.NextAgent,
			Description:         desc,
			Priority:            original.Priority,
			ChainID:             original.ChainID,
			NotifyCEOOnComplete: original.NotifyCEOOnComplete,
		})
		if err != nil {
			log.Printf("[handler] resultRouting CreateTask failed for original %s: %v", original.ID, err)
			return
		}
		log.Printf("[handler] resultRouting: created task %s for agent %s (original: %s)",
			newTask.ID, parsed.NextAgent, original.ID)
		h.dispatchToAgent(parsed.NextAgent, newTask.ID)
	}()
}

// autoAdvance creates and dispatches the next task when a task completes with
// auto_advance_to set. Symmetric to autoRetry (fail path).
//
// Behaviour:
//   - New task title: advance_task_title (or "advance: <original.Title>" as fallback)
//   - New task description: if advance_task_description is set, prepend upstream result:
//     "前置结果：{result}\n\n{advance_task_description}"
//     Otherwise use just the upstream result as context.
//   - Inherits ChainID, Priority, NotifyCEOOnComplete from original.
//   - Does NOT inherit DependsOn (the new task depends only on the original being done,
//     which is implicit by the time autoAdvance fires).
func (h *Handler) autoAdvance(original model.Task) {
	go func() {
		advanceAgent := original.AutoAdvanceTo

		title := original.AdvanceTaskTitle
		if title == "" {
			title = "advance: " + original.Title
		}

		desc := original.AdvanceTaskDescription
		upstreamResult := original.Result
		if upstreamResult == "" {
			upstreamResult = "（无）"
		}
		if desc != "" {
			desc = "前置结果：" + upstreamResult + "\n\n" + desc
		} else {
			desc = "前置结果：" + upstreamResult
		}

		newTask, err := h.store.CreateTask(model.CreateTaskRequest{
			Title:               title,
			AssignedTo:          advanceAgent,
			Description:         desc,
			Priority:            original.Priority,
			ChainID:             original.ChainID,
			NotifyCEOOnComplete: original.NotifyCEOOnComplete,
		})
		if err != nil {
			log.Printf("[handler] autoAdvance CreateTask failed for original %s: %v", original.ID, err)
			return
		}
		log.Printf("[handler] autoAdvance: created task %s for agent %s (original: %s)",
			newTask.ID, advanceAgent, original.ID)

		h.dispatchToAgent(advanceAgent, newTask.ID)
	}()
}

// dispatchToAgent sends a sessions_send nudge to the agent owning taskID.
// Fire-and-forget; errors are logged only.
func (h *Handler) dispatchToAgent(agentName, taskID string) {
	sessionKey, known := openclaw.SessionKey(agentName)
	if !known {
		log.Printf("[handler] dispatchToAgent: unknown agent %q – task %s created but not dispatched", agentName, taskID)
		return
	}
	if h.oc == nil {
		return
	}
	msg := "[agent-queue] 你有新的待处理任务。请执行 poll 流程认领。"
	if err := h.oc.SendToSession(sessionKey, msg); err != nil {
		log.Printf("[handler] dispatchToAgent %s failed: %v", sessionKey, err)
	} else {
		log.Printf("[handler] dispatchToAgent: dispatched task %s to %s", taskID, agentName)
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
func (h *Handler) tasksSummary(w http.ResponseWriter, r *http.Request) {
	assignedTo := r.URL.Query().Get("assigned_to")
	summary, err := h.store.SummaryFiltered(assignedTo)
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

// isTestTaskTitleAssignee returns true for test tasks:
// - title contains "[test]" (case-insensitive)
// - assigned_to starts with "e2e-" (case-insensitive)
func isTestTaskTitleAssignee(title, assignedTo string) bool {
	lt := strings.ToLower(title)
	la := strings.ToLower(assignedTo)
	return strings.Contains(lt, "[test]") || strings.HasPrefix(la, "e2e-")
}

func isTestTask(t model.Task) bool {
	return isTestTaskTitleAssignee(t.Title, t.AssignedTo)
}

// isBrowserRelayNotAttachedText detects Browser Relay not-attached errors (case-insensitive).
// Rules: "browser relay" + (attach/not attached/no connected tab/badge/relay toolbar),
// OR "openclaw" + "browser relay", OR "badge on" + "browser relay".
func isBrowserRelayNotAttachedText(text string) bool {
	lt := strings.ToLower(text)
	hasRelay := strings.Contains(lt, "browser relay")
	if hasRelay {
		if strings.Contains(lt, "attach") || strings.Contains(lt, "not attached") ||
			strings.Contains(lt, "no connected tab") || strings.Contains(lt, "badge") ||
			strings.Contains(lt, "relay toolbar") {
			return true
		}
		if strings.Contains(lt, "badge on") {
			return true
		}
	}
	if strings.Contains(lt, "openclaw") && hasRelay {
		return true
	}
	// extra stable patterns
	if strings.Contains(lt, "no connected tab") {
		return true
	}
	return false
}

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

// -------------------------------------------------------------------
// F11 (V8): /retry-routing
// -------------------------------------------------------------------

func (h *Handler) handleRetryRouting(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		routes, err := h.store.ListRetryRoutes()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"routes": routes, "count": len(routes)})
	case http.MethodPost:
		var req model.RetryRoute
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		if strings.TrimSpace(req.AssignedTo) == "" || strings.TrimSpace(req.RetryAssignedTo) == "" {
			writeError(w, http.StatusBadRequest, "assigned_to and retry_assigned_to are required")
			return
		}
		route, err := h.store.CreateRetryRoute(req)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, route)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (h *Handler) handleRetryRoutingID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	idStr := strings.TrimPrefix(r.URL.Path, "/retry-routing/")
	if idStr == "" {
		writeError(w, http.StatusBadRequest, "missing id")
		return
	}
	var id int
	if _, err := fmt.Sscanf(idStr, "%d", &id); err != nil {
		writeError(w, http.StatusBadRequest, "invalid id: "+idStr)
		return
	}
	if err := h.store.DeleteRetryRoute(id); err != nil {
		handleStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// -------------------------------------------------------------------
// F12 (V8): /chains/:chain_id
// -------------------------------------------------------------------

func (h *Handler) handleChains(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	chainID := strings.TrimPrefix(r.URL.Path, "/chains/")
	if chainID == "" {
		writeError(w, http.StatusBadRequest, "missing chain_id")
		return
	}

	tasks, err := h.store.GetChainTasks(chainID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(tasks) == 0 {
		writeError(w, http.StatusNotFound, "chain not found")
		return
	}

	complete, err := h.store.IsChainComplete(chainID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	doneCount := 0
	for _, t := range tasks {
		if t.Status == model.StatusDone || t.Status == model.StatusCancelled {
			doneCount++
		}
	}

	writeJSON(w, http.StatusOK, model.ChainStatusResponse{
		ChainID:    chainID,
		Total:      len(tasks),
		Done:       doneCount,
		IsComplete: complete,
		Tasks:      tasks,
	})
}

// newChainID generates a unique chain identifier.
func newChainID() string {
	return fmt.Sprintf("chain_%x", time.Now().UnixNano())
}

// =============================================================================
// V12: AI Workbench API Endpoints
// =============================================================================

// handleAPIDashboard serves GET /api/dashboard.
// Returns aggregated stats: pending todo, exceptions (failed/blocked), and counts.
func (h *Handler) handleAPIDashboard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	all, err := h.store.ListTasks("", "", "", nil)
	if err != nil {
		handleStoreError(w, err)
		return
	}

	type Stats struct {
		Total      int `json:"total"`
		Pending    int `json:"pending"`
		InProgress int `json:"in_progress"`
		Done       int `json:"done"`
		Failed     int `json:"failed"`
		Blocked    int `json:"blocked"`
	}

	type Response struct {
		Todo       []model.Task `json:"todo"`
		Exceptions []model.Task `json:"exceptions"`
		Stats      Stats        `json:"stats"`
	}

	var todo, exceptions []model.Task
	var stats Stats
	stats.Total = len(all)
	for _, t := range all {
		switch t.Status {
		case model.StatusPending:
			stats.Pending++
			todo = append(todo, t)
		case model.StatusInProgress, model.StatusClaimed:
			stats.InProgress++
		case model.StatusDone:
			stats.Done++
		case model.StatusFailed:
			stats.Failed++
			exceptions = append(exceptions, t)
		case model.StatusBlocked:
			stats.Blocked++
			exceptions = append(exceptions, t)
		}
	}
	if todo == nil {
		todo = []model.Task{}
	}
	if exceptions == nil {
		exceptions = []model.Task{}
	}

	writeJSON(w, http.StatusOK, Response{Todo: todo, Exceptions: exceptions, Stats: stats})
}

// handleAPITimeline serves GET /api/timeline/:id.
// Returns task details plus full history (task_history table).
func (h *Handler) handleAPITimeline(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/timeline/")
	if id == "" {
		writeError(w, http.StatusBadRequest, "task id required")
		return
	}

	task, err := h.store.GetByID(id)
	if err != nil {
		handleStoreError(w, err)
		return
	}

	type HistoryItem struct {
		ID         int64     `json:"id"`
		TaskID     string    `json:"task_id"`
		FromStatus string    `json:"from_status,omitempty"`
		ToStatus   string    `json:"to_status"`
		ChangedBy  string    `json:"changed_by,omitempty"`
		Note       string    `json:"note,omitempty"`
		ChangedAt  time.Time `json:"changed_at"`
	}
	type Response struct {
		Task    model.Task    `json:"task"`
		History []HistoryItem `json:"history"`
	}

	// Fetch history from store.
	rawHistory, err := h.store.GetHistory(id)
	if err != nil {
		log.Printf("[handler] GetHistory %s: %v", id, err)
		rawHistory = nil
	}
	history := make([]HistoryItem, 0, len(rawHistory))
	for _, h := range rawHistory {
		history = append(history, HistoryItem{
			ID:         h.ID,
			TaskID:     h.TaskID,
			FromStatus: h.FromStatus,
			ToStatus:   h.ToStatus,
			ChangedBy:  h.ChangedBy,
			Note:       h.Note,
			ChangedAt:  h.ChangedAt,
		})
	}

	writeJSON(w, http.StatusOK, Response{Task: task, History: history})
}

// handleAPIChains serves GET /api/chains.
// Returns all chain groups with their tasks (for goal tracking UI).
func (h *Handler) handleAPIChains(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// Get all tasks with a chain_id, then group them.
	all, err := h.store.ListTasks("", "", "", nil)
	if err != nil {
		handleStoreError(w, err)
		return
	}

	type ChainGroup struct {
		ChainID string       `json:"chain_id"`
		Tasks   []model.Task `json:"tasks"`
	}

	chainMap := make(map[string]*ChainGroup)
	chainOrder := []string{}
	for _, t := range all {
		if t.ChainID == "" {
			continue
		}
		if _, ok := chainMap[t.ChainID]; !ok {
			chainMap[t.ChainID] = &ChainGroup{ChainID: t.ChainID}
			chainOrder = append(chainOrder, t.ChainID)
		}
		chainMap[t.ChainID].Tasks = append(chainMap[t.ChainID].Tasks, t)
	}

	chains := make([]ChainGroup, 0, len(chainOrder))
	for _, cid := range chainOrder {
		chains = append(chains, *chainMap[cid])
	}

	type Response struct {
		Chains []ChainGroup `json:"chains"`
	}
	writeJSON(w, http.StatusOK, Response{Chains: chains})
}

// handleAPIConfig serves GET /api/config.
// Returns known agents and server metadata for frontend initialization.
func (h *Handler) handleAPIConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	type AgentInfo struct {
		Name  string `json:"name"`
		Label string `json:"label"`
	}
	type Response struct {
		Agents             []AgentInfo `json:"agents"`
		Version            string      `json:"version"`
		OutboundWebhookURL string      `json:"outbound_webhook_url,omitempty"`
		DBPath             string      `json:"db_path,omitempty"`
		PID                int         `json:"pid,omitempty"`
		Uptime             string      `json:"uptime,omitempty"`
		ListenAddr         string      `json:"listen_addr,omitempty"`
	}

	// Default known agents (can be extended via config in future phases).
	agents := []AgentInfo{
		{"coder", "工程师"},
		{"thinker", "架构师"},
		{"writer", "文档工程师"},
		{"devops", "运维工程师"},
		{"security", "安全工程师"},
		{"qa", "质量工程师"},
		{"vision", "视觉验收"},
		{"uiux", "UI/UX设计师"},
		{"pm", "产品经理"},
		{"ops", "研究员"},
	}

	// V23-A: expose (non-secret) webhook config.
	outboundWebhookURL := os.Getenv("AGENT_QUEUE_WEBHOOK_URL")

	// V28-A: system info metadata
	dbPath := os.Getenv("AGENT_QUEUE_DB_PATH")
	if dbPath == "" {
		dbPath = "data/queue.db"
	}
	if abs, err := filepath.Abs(dbPath); err == nil {
		dbPath = abs
	}
	port := envString("AGENT_QUEUE_PORT", "19827")
	listenAddr := "localhost:" + port
	uptime := formatUptime(time.Since(h.startTime))

	writeJSON(w, http.StatusOK, Response{
		Agents:             agents,
		Version:            "v28",
		OutboundWebhookURL: outboundWebhookURL,
		DBPath:             dbPath,
		PID:                os.Getpid(),
		Uptime:             uptime,
		ListenAddr:         listenAddr,
	})
}

// handleAPIGraph serves GET /api/graph/:chain_id.
// Returns all tasks in the chain with their depends_on lists for DAG visualization.
func (h *Handler) handleAPIGraph(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	chainID := strings.TrimPrefix(r.URL.Path, "/api/graph/")
	if chainID == "" {
		writeError(w, http.StatusBadRequest, "missing chain_id")
		return
	}

	// Fetch tasks for the chain.
	tasks, err := h.store.GetChainTasks(chainID)
	if err != nil {
		handleStoreError(w, err)
		return
	}

	// Enrich each task with its depends_on list.
	enriched := make([]model.Task, 0, len(tasks))
	for _, t := range tasks {
		full, err := h.store.GetByID(t.ID)
		if err != nil {
			enriched = append(enriched, t) // fallback: no deps
			continue
		}
		enriched = append(enriched, full)
	}

	type GraphData struct {
		ChainID string       `json:"chain_id"`
		Tasks   []model.Task `json:"tasks"`
	}
	writeJSON(w, http.StatusOK, GraphData{ChainID: chainID, Tasks: enriched})
}

// handleAPIAgentStats serves GET /api/agents/stats.
// Returns per-agent aggregated task statistics.
func (h *Handler) handleAPIAgentStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	type AgentStat struct {
		Agent               string  `json:"agent"`
		TotalTasks          int     `json:"total_tasks"`
		DoneCount           int     `json:"done_count"`
		FailedCount         int     `json:"failed_count"`
		AvgDurationMinutes  float64 `json:"avg_duration_minutes"`
		SuccessRate         float64 `json:"success_rate"` // done/(done+failed)*100
	}

	rows, err := h.db.QueryContext(r.Context(), `
		SELECT
			assigned_to,
			COUNT(*) AS total,
			SUM(CASE WHEN status = 'done'   THEN 1 ELSE 0 END) AS done,
			SUM(CASE WHEN status = 'failed' THEN 1 ELSE 0 END) AS failed,
			AVG(CASE
				WHEN status IN ('done','failed') AND started_at IS NOT NULL
				THEN (julianday(updated_at) - julianday(started_at)) * 24 * 60
				ELSE NULL
			END) AS avg_min
		FROM tasks
		WHERE assigned_to != ''
		GROUP BY assigned_to
		ORDER BY total DESC
	`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	stats := []AgentStat{}
	for rows.Next() {
		var s AgentStat
		var avgMin *float64
		if err := rows.Scan(&s.Agent, &s.TotalTasks, &s.DoneCount, &s.FailedCount, &avgMin); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if avgMin != nil {
			s.AvgDurationMinutes = *avgMin
		}
		closed := s.DoneCount + s.FailedCount
		if closed > 0 {
			s.SuccessRate = float64(s.DoneCount) / float64(closed) * 100
		}
		stats = append(stats, s)
	}
	if err := rows.Err(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"stats": stats, "count": len(stats)})
}
