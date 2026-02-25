package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/irchelper/agent-queue/internal/db"
	"github.com/irchelper/agent-queue/internal/handler"
	"github.com/irchelper/agent-queue/internal/model"
	"github.com/irchelper/agent-queue/internal/notify"
	"github.com/irchelper/agent-queue/internal/openclaw"
	"github.com/irchelper/agent-queue/internal/store"
)

// -------------------------------------------------------------------
// Test helpers
// -------------------------------------------------------------------

func newTestServer(t *testing.T, oc *openclaw.Client) *httptest.Server {
	t.Helper()
	f, err := os.CreateTemp("", "handler-test-*.db")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	f.Close()
	t.Cleanup(func() { os.Remove(f.Name()) })

	database, err := db.Open(f.Name())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	s := store.New(database)
	h := handler.New(database, s, notify.NoOp{}, oc)
	mux := http.NewServeMux()
	h.Register(mux)
	return httptest.NewServer(mux)
}

func postJSON(t *testing.T, srv *httptest.Server, path string, body any) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(srv.URL+path, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

func getJSON(t *testing.T, srv *httptest.Server, path string) *http.Response {
	t.Helper()
	resp, err := http.Get(srv.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return resp
}

// -------------------------------------------------------------------
// GET /tasks/summary
// -------------------------------------------------------------------

func TestSummary_Empty(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	resp := getJSON(t, srv, "/tasks/summary")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var s model.SummaryResponse
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		t.Fatalf("decode: %v", err)
	}
	resp.Body.Close()
	if s.Pending != 0 || s.DoneToday != 0 || len(s.Tasks) != 0 {
		t.Fatalf("expected empty summary, got %+v", s)
	}
}

func TestSummary_WithTasks(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	// Create 2 pending, 1 claimed.
	postJSON(t, srv, "/tasks", map[string]any{"title": "t1"})
	t2 := postJSON(t, srv, "/tasks", map[string]any{"title": "t2"})
	t2.Body.Close()
	postJSON(t, srv, "/tasks", map[string]any{"title": "t3"})

	// Claim the second task.
	var task2 model.Task
	resp2 := postJSON(t, srv, "/tasks", map[string]any{"title": "t2claim"})
	json.NewDecoder(resp2.Body).Decode(&task2) //nolint:errcheck
	resp2.Body.Close()

	claimResp := postJSON(t, srv, "/tasks/"+task2.ID+"/claim",
		map[string]any{"version": task2.Version, "agent": "coder"})
	claimResp.Body.Close()

	resp := getJSON(t, srv, "/tasks/summary")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var s model.SummaryResponse
	json.NewDecoder(resp.Body).Decode(&s) //nolint:errcheck
	resp.Body.Close()

	// 3 pending (t1/t3/t3-extra) + 1 claimed (task2).
	if s.Claimed != 1 {
		t.Fatalf("expected 1 claimed, got %d", s.Claimed)
	}
	if len(s.Tasks) == 0 {
		t.Fatal("expected non-empty tasks list")
	}
}

func TestSummary_DoneToday_NotInList(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	// Create + advance to done.
	var task model.Task
	r := postJSON(t, srv, "/tasks", map[string]any{"title": "done-task"})
	json.NewDecoder(r.Body).Decode(&task) //nolint:errcheck
	r.Body.Close()

	claimR := postJSON(t, srv, "/tasks/"+task.ID+"/claim",
		map[string]any{"version": task.Version, "agent": "a"})
	var claimed model.Task
	json.NewDecoder(claimR.Body).Decode(&claimed) //nolint:errcheck
	claimR.Body.Close()

	patchTo := func(status string, version int) model.Task {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"status": status, "version": version})
		req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/tasks/"+task.ID, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("PATCH %s: %v", status, err)
		}
		defer r.Body.Close()
		if r.StatusCode != http.StatusOK {
			t.Fatalf("PATCH %s returned %d", status, r.StatusCode)
		}
		var pr struct{ Task model.Task }
		json.NewDecoder(r.Body).Decode(&pr) //nolint:errcheck
		return pr.Task
	}

	ipTask := patchTo("in_progress", claimed.Version)
	patchTo("done", ipTask.Version)

	sumResp := getJSON(t, srv, "/tasks/summary")
	var s model.SummaryResponse
	json.NewDecoder(sumResp.Body).Decode(&s) //nolint:errcheck
	sumResp.Body.Close()

	if s.DoneToday != 1 {
		t.Fatalf("expected done_today=1, got %d", s.DoneToday)
	}
	// done tasks must NOT appear in the active task list.
	for _, st := range s.Tasks {
		if st.ID == task.ID {
			t.Fatalf("done task should not appear in summary.tasks list")
		}
	}
}

// -------------------------------------------------------------------
// POST /dispatch
// -------------------------------------------------------------------

func TestDispatch_CreatesTask(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	resp := postJSON(t, srv, "/dispatch", map[string]any{
		"title":       "implement login",
		"assigned_to": "coder",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	var dr model.DispatchResponse
	json.NewDecoder(resp.Body).Decode(&dr) //nolint:errcheck
	resp.Body.Close()

	if dr.Task.ID == "" {
		t.Fatal("expected non-empty task ID")
	}
	if dr.Task.Title != "implement login" {
		t.Fatalf("title mismatch: %s", dr.Task.Title)
	}
	if dr.Task.AssignedTo != "coder" {
		t.Fatalf("assigned_to mismatch: %s", dr.Task.AssignedTo)
	}
}

func TestDispatch_MissingTitle_Returns400(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	resp := postJSON(t, srv, "/dispatch", map[string]any{"assigned_to": "coder"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestDispatch_MissingAssignedTo_Returns400(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	resp := postJSON(t, srv, "/dispatch", map[string]any{"title": "test"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestDispatch_UnknownAgent_TaskCreatedButNotifyError(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	resp := postJSON(t, srv, "/dispatch", map[string]any{
		"title":       "test unknown agent",
		"assigned_to": "unknown-agent-xyz",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 (task still created), got %d", resp.StatusCode)
	}
	var dr model.DispatchResponse
	json.NewDecoder(resp.Body).Decode(&dr) //nolint:errcheck
	resp.Body.Close()

	if dr.Task.ID == "" {
		t.Fatal("expected non-empty task ID even for unknown agent")
	}
	if dr.NotifyError == "" {
		t.Fatal("expected notify_error for unknown agent")
	}
	if dr.Notified {
		t.Fatal("notified should be false for unknown agent")
	}
}

func TestDispatch_MockOpenClaw_NotifySent(t *testing.T) {
	// Spin up a mock OpenClaw server that records the sessions_send call.
	var capturedBody map[string]any
	mockOC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/tools/invoke" {
			json.NewDecoder(r.Body).Decode(&capturedBody) //nolint:errcheck
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": "ok"}) //nolint:errcheck
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mockOC.Close()

	oc := openclaw.NewWithURL(mockOC.URL, "test-token")

	srv := newTestServer(t, oc)
	defer srv.Close()

	resp := postJSON(t, srv, "/dispatch", map[string]any{
		"title":       "review PR",
		"assigned_to": "coder",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	var dr model.DispatchResponse
	json.NewDecoder(resp.Body).Decode(&dr) //nolint:errcheck
	resp.Body.Close()

	if !dr.Notified {
		t.Fatalf("expected notified=true, got false. notify_error=%q", dr.NotifyError)
	}
	if capturedBody == nil {
		t.Fatal("mock OpenClaw received no request")
	}
	if capturedBody["tool"] != "sessions_send" {
		t.Fatalf("expected tool=sessions_send, got %v", capturedBody["tool"])
	}
	args, _ := capturedBody["args"].(map[string]any)
	if args == nil {
		t.Fatal("args is nil")
	}
	if args["sessionKey"] != "agent:coder:discord:channel:1475338640593916045" {
		t.Fatalf("unexpected sessionKey: %v", args["sessionKey"])
	}
}

func TestDispatch_MockOpenClaw_NotifyFails_TaskStillCreated(t *testing.T) {
	// Mock that always returns error.
	mockOC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"ok":    false,
			"error": map[string]any{"message": "tool not available"},
		})
	}))
	defer mockOC.Close()

	oc := openclaw.NewWithURL(mockOC.URL, "")
	srv := newTestServer(t, oc)
	defer srv.Close()

	resp := postJSON(t, srv, "/dispatch", map[string]any{
		"title":       "critical task",
		"assigned_to": "devops",
	})
	// Task must still be created even if notify fails.
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	var dr model.DispatchResponse
	json.NewDecoder(resp.Body).Decode(&dr) //nolint:errcheck
	resp.Body.Close()

	if dr.Task.ID == "" {
		t.Fatal("task should be created even when notify fails")
	}
	if dr.Notified {
		t.Fatal("notified should be false when mock returns error")
	}
	if dr.NotifyError == "" {
		t.Fatal("notify_error should be non-empty when mock returns error")
	}
}

// -------------------------------------------------------------------
// POST /dispatch/chain
// -------------------------------------------------------------------

func TestDispatchChain_CreatesSerialTasks(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	resp := postJSON(t, srv, "/dispatch/chain", map[string]any{
		"tasks": []map[string]any{
			{"title": "step-A", "assigned_to": "coder"},
			{"title": "step-B", "assigned_to": "writer"},
			{"title": "step-C", "assigned_to": "thinker"},
		},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	var cr model.ChainResponse
	json.NewDecoder(resp.Body).Decode(&cr) //nolint:errcheck
	resp.Body.Close()

	if cr.ChainID == "" {
		t.Fatal("chain_id should be non-empty")
	}
	if len(cr.Tasks) != 3 {
		t.Fatalf("expected 3 tasks, got %d", len(cr.Tasks))
	}
	if cr.FirstDispatched != cr.Tasks[0].ID {
		t.Fatalf("first_dispatched %q != tasks[0].id %q", cr.FirstDispatched, cr.Tasks[0].ID)
	}

	// Verify depends_on chain: B depends on A, C depends on B.
	taskA := cr.Tasks[0]
	taskB := cr.Tasks[1]
	taskC := cr.Tasks[2]

	// Fetch details to get depends_on populated.
	detailB := getJSON(t, srv, "/tasks/"+taskB.ID)
	var bDetail model.Task
	json.NewDecoder(detailB.Body).Decode(&bDetail) //nolint:errcheck
	detailB.Body.Close()
	if len(bDetail.DependsOn) != 1 || bDetail.DependsOn[0] != taskA.ID {
		t.Fatalf("task B should depend on A, got %v", bDetail.DependsOn)
	}

	detailC := getJSON(t, srv, "/tasks/"+taskC.ID)
	var cDetail model.Task
	json.NewDecoder(detailC.Body).Decode(&cDetail) //nolint:errcheck
	detailC.Body.Close()
	if len(cDetail.DependsOn) != 1 || cDetail.DependsOn[0] != taskB.ID {
		t.Fatalf("task C should depend on B, got %v", cDetail.DependsOn)
	}
}

func TestDispatchChain_FirstTaskHasNoDeps(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	resp := postJSON(t, srv, "/dispatch/chain", map[string]any{
		"tasks": []map[string]any{
			{"title": "only-task", "assigned_to": "coder"},
		},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	var cr model.ChainResponse
	json.NewDecoder(resp.Body).Decode(&cr) //nolint:errcheck
	resp.Body.Close()

	if len(cr.Tasks) != 1 {
		t.Fatalf("expected 1 task, got %d", len(cr.Tasks))
	}
	// First task must be immediately pollable (no deps).
	detailResp := getJSON(t, srv, "/tasks/"+cr.Tasks[0].ID+"/deps-met")
	var dm model.DepsMet
	json.NewDecoder(detailResp.Body).Decode(&dm) //nolint:errcheck
	detailResp.Body.Close()
	if !dm.DepsMet {
		t.Fatal("first task should have deps_met=true")
	}
}

func TestDispatchChain_EmptyTasks_Returns400(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	resp := postJSON(t, srv, "/dispatch/chain", map[string]any{"tasks": []any{}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestDispatchChain_MissingTitle_Returns400(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	resp := postJSON(t, srv, "/dispatch/chain", map[string]any{
		"tasks": []map[string]any{
			{"assigned_to": "coder"}, // missing title
		},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestDispatchChain_AutoUnlock_SecondTaskAfterFirstDone(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	// Create chain A → B.
	chainResp := postJSON(t, srv, "/dispatch/chain", map[string]any{
		"tasks": []map[string]any{
			{"title": "step-A", "assigned_to": "coder"},
			{"title": "step-B", "assigned_to": "coder"},
		},
	})
	if chainResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", chainResp.StatusCode)
	}
	var cr model.ChainResponse
	json.NewDecoder(chainResp.Body).Decode(&cr) //nolint:errcheck
	chainResp.Body.Close()

	taskA := cr.Tasks[0]
	taskB := cr.Tasks[1]

	// Before A done: B should not be pollable (deps not met).
	pollResp := getJSON(t, srv, "/tasks/poll?assigned_to=coder")
	var pr model.PollResponse
	json.NewDecoder(pollResp.Body).Decode(&pr) //nolint:errcheck
	pollResp.Body.Close()
	if pr.Task == nil || pr.Task.ID != taskA.ID {
		t.Fatalf("poll should return task A first, got %v", pr.Task)
	}

	// Drive task A to done via FSM.
	claimA := postJSON(t, srv, "/tasks/"+taskA.ID+"/claim",
		map[string]any{"version": taskA.Version, "agent": "coder"})
	var claimedA model.Task
	json.NewDecoder(claimA.Body).Decode(&claimedA) //nolint:errcheck
	claimA.Body.Close()

	ipA := patchTaskTo(t, srv, taskA.ID, "in_progress", claimedA.Version)
	patchTaskTo(t, srv, taskA.ID, "done", ipA.Version)

	// After A done: B should be deps_met.
	depsB := getJSON(t, srv, "/tasks/"+taskB.ID+"/deps-met")
	var dmB model.DepsMet
	json.NewDecoder(depsB.Body).Decode(&dmB) //nolint:errcheck
	depsB.Body.Close()
	if !dmB.DepsMet {
		t.Fatal("task B should have deps_met=true after A is done")
	}

	// Poll should now return task B.
	pollResp2 := getJSON(t, srv, "/tasks/poll?assigned_to=coder")
	var pr2 model.PollResponse
	json.NewDecoder(pollResp2.Body).Decode(&pr2) //nolint:errcheck
	pollResp2.Body.Close()
	if pr2.Task == nil || pr2.Task.ID != taskB.ID {
		t.Fatalf("poll should return task B after A done, got %v", pr2.Task)
	}
}

// -------------------------------------------------------------------
// failed 状态（终态）+ autoRetry + SessionNotifier
// -------------------------------------------------------------------

// driveToFailed creates a task, claims it, moves it to in_progress, then failed.
func driveToFailed(t *testing.T, srv *httptest.Server, title, result string) model.Task {
	t.Helper()
	var task model.Task
	r := postJSON(t, srv, "/tasks", map[string]any{"title": title, "assigned_to": "coder"})
	json.NewDecoder(r.Body).Decode(&task) //nolint:errcheck
	r.Body.Close()

	claimR := postJSON(t, srv, "/tasks/"+task.ID+"/claim",
		map[string]any{"version": task.Version, "agent": "coder"})
	var claimed model.Task
	json.NewDecoder(claimR.Body).Decode(&claimed) //nolint:errcheck
	claimR.Body.Close()

	ipTask := patchTaskTo(t, srv, task.ID, "in_progress", claimed.Version)

	body, _ := json.Marshal(map[string]any{"status": "failed", "result": result, "version": ipTask.Version})
	req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/tasks/"+task.ID, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	var pr struct{ Task model.Task }
	json.NewDecoder(resp.Body).Decode(&pr) //nolint:errcheck
	resp.Body.Close()
	return pr.Task
}

func TestFailed_PendingAllowed_CanRetry(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	failed := driveToFailed(t, srv, "fail-me", "no retry directive")

	if failed.Status != model.StatusFailed {
		t.Fatalf("expected failed, got %s", failed.Status)
	}

	// failed → pending should be 200 (CEO retry).
	retried := patchTaskTo(t, srv, failed.ID, "pending", failed.Version)
	if retried.Status != model.StatusPending {
		t.Fatalf("expected pending after CEO retry, got %s", retried.Status)
	}
}

func TestFailed_CannotTransitionToInProgress(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	failed := driveToFailed(t, srv, "fail-me2", "no retry")

	// failed → in_progress should be 422.
	body, _ := json.Marshal(map[string]any{"status": "in_progress", "version": failed.Version})
	req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/tasks/"+failed.ID, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for failed→in_progress, got %d", resp.StatusCode)
	}
}

func TestFailed_RetryWithRetryAssignedTo(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	// Fail a task, then CEO retries with retry_assigned_to override.
	failed := driveToFailed(t, srv, "改派任务", "原 coder 无法处理")

	body, _ := json.Marshal(map[string]any{
		"status":           "pending",
		"retry_assigned_to": "thinker",
		"version":          failed.Version,
	})
	req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/tasks/"+failed.ID, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	var pr struct{ Task model.Task }
	json.NewDecoder(resp.Body).Decode(&pr) //nolint:errcheck
	resp.Body.Close()

	if pr.Task.Status != model.StatusPending {
		t.Fatalf("expected pending, got %s", pr.Task.Status)
	}
	if pr.Task.AssignedTo != "thinker" {
		t.Fatalf("expected assigned_to=thinker after retry, got %q", pr.Task.AssignedTo)
	}
}

func TestFailed_FailureReasonField(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	var task model.Task
	r := postJSON(t, srv, "/tasks", map[string]any{"title": "t", "assigned_to": "coder"})
	json.NewDecoder(r.Body).Decode(&task) //nolint:errcheck
	r.Body.Close()

	claimR := postJSON(t, srv, "/tasks/"+task.ID+"/claim",
		map[string]any{"version": task.Version, "agent": "coder"})
	var claimed model.Task
	json.NewDecoder(claimR.Body).Decode(&claimed) //nolint:errcheck
	claimR.Body.Close()

	ipTask := patchTaskTo(t, srv, task.ID, "in_progress", claimed.Version)

	// PATCH with explicit failure_reason field.
	body, _ := json.Marshal(map[string]any{
		"status":         "failed",
		"failure_reason": "依赖服务 API 返回 503",
		"version":        ipTask.Version,
	})
	req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/tasks/"+task.ID, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	var pr struct{ Task model.Task }
	json.NewDecoder(resp.Body).Decode(&pr) //nolint:errcheck
	resp.Body.Close()

	if pr.Task.FailureReason != "依赖服务 API 返回 503" {
		t.Fatalf("failure_reason not saved: got %q", pr.Task.FailureReason)
	}
}

func TestFailed_DoneToFailed_Returns422(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	var task model.Task
	r := postJSON(t, srv, "/tasks", map[string]any{"title": "done-task", "assigned_to": "coder"})
	json.NewDecoder(r.Body).Decode(&task) //nolint:errcheck
	r.Body.Close()

	claimR := postJSON(t, srv, "/tasks/"+task.ID+"/claim",
		map[string]any{"version": task.Version, "agent": "coder"})
	var claimed model.Task
	json.NewDecoder(claimR.Body).Decode(&claimed) //nolint:errcheck
	claimR.Body.Close()

	ipTask := patchTaskTo(t, srv, task.ID, "in_progress", claimed.Version)
	doneTask := patchTaskTo(t, srv, task.ID, "done", ipTask.Version)

	body, _ := json.Marshal(map[string]any{"status": "failed", "version": doneTask.Version})
	req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/tasks/"+task.ID, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 for done→failed, got %d", resp.StatusCode)
	}
}

func TestFailed_AutoRetry_CreatesNewTask(t *testing.T) {
	// Mock OpenClaw to capture sessions_send.
	var dispatched []map[string]any
	mockOC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		dispatched = append(dispatched, body)
		json.NewEncoder(w).Encode(map[string]any{"ok": true}) //nolint:errcheck
	}))
	defer mockOC.Close()

	oc := openclaw.NewWithURL(mockOC.URL, "")
	srv := newTestServer(t, oc)
	defer srv.Close()

	// Fail with retry_assigned_to in result.
	driveToFailed(t, srv, "登录实现", "登录按钮无响应 | retry_assigned_to: coder")

	// Allow goroutine to complete.
	time.Sleep(100 * time.Millisecond)

	// Verify a retry task was created.
	listResp := getJSON(t, srv, "/tasks?assigned_to=coder")
	var listBody struct {
		Tasks []model.Task `json:"tasks"`
		Count int          `json:"count"`
	}
	json.NewDecoder(listResp.Body).Decode(&listBody) //nolint:errcheck
	listResp.Body.Close()

	var retryTask *model.Task
	for i := range listBody.Tasks {
		if strings.HasPrefix(listBody.Tasks[i].Title, "retry:") {
			retryTask = &listBody.Tasks[i]
			break
		}
	}
	if retryTask == nil {
		t.Fatal("expected a 'retry: ...' task to be created")
	}
	if retryTask.AssignedTo != "coder" {
		t.Fatalf("retry task assigned_to: want coder, got %q", retryTask.AssignedTo)
	}
	if !strings.Contains(retryTask.Description, "failed原因:") {
		t.Errorf("retry task description should contain original failure: %q", retryTask.Description)
	}
}

func TestFailed_NoRetryDirective_NoNewTask(t *testing.T) {
	// Mock CEO session notifier capture.
	var ceoMessages []string
	mockOC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		if args, ok := body["args"].(map[string]any); ok {
			if msg, ok := args["message"].(string); ok {
				ceoMessages = append(ceoMessages, msg)
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true}) //nolint:errcheck
	}))
	defer mockOC.Close()

	oc := openclaw.NewWithURL(mockOC.URL, "")
	srv := newTestServer(t, oc)
	defer srv.Close()

	driveToFailed(t, srv, "重要任务", "完全崩溃，没有重试方案")

	time.Sleep(100 * time.Millisecond)

	// No new task should be created.
	listResp := getJSON(t, srv, "/tasks?assigned_to=coder")
	var listBody struct {
		Tasks []model.Task `json:"tasks"`
	}
	json.NewDecoder(listResp.Body).Decode(&listBody) //nolint:errcheck
	listResp.Body.Close()

	for _, t2 := range listBody.Tasks {
		if strings.HasPrefix(t2.Title, "retry:") {
			t.Fatal("no retry task should be created when no retry_assigned_to directive")
		}
	}

	// CEO should have been notified.
	found := false
	for _, msg := range ceoMessages {
		if strings.Contains(msg, "❌") && strings.Contains(msg, "重要任务") {
			found = true
		}
	}
	if !found {
		t.Errorf("CEO should have received ❌ alert for failed task, got: %v", ceoMessages)
	}
}

// patchTaskTo is a reusable helper for PATCH status transitions.
func patchTaskTo(t *testing.T, srv *httptest.Server, taskID, status string, version int) model.Task {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"status": status, "version": version})
	req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/tasks/"+taskID, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH %s %s: %v", taskID, status, err)
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		t.Fatalf("PATCH %s %s returned %d", taskID, status, r.StatusCode)
	}
	var pr struct{ Task model.Task }
	json.NewDecoder(r.Body).Decode(&pr) //nolint:errcheck
	return pr.Task
}

// -------------------------------------------------------------------
// V7: superseded_by + deps扩展 — 链路自动恢复
// -------------------------------------------------------------------

// TestV7_ChainAutoRecovery: 链 A→B→C，A failed，autoRetry 创建 A'
// A' done 后，B 的 deps_met=true（通过 superseded_by 扩展逻辑）
func TestV7_ChainAutoRecovery(t *testing.T) {
	// Mock OpenClaw for autoRetry dispatch.
	mockOC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"ok": true}) //nolint:errcheck
	}))
	defer mockOC.Close()

	oc := openclaw.NewWithURL(mockOC.URL, "")
	srv := newTestServer(t, oc)
	defer srv.Close()

	// Create chain A→B→C via dispatch/chain.
	chainResp := postJSON(t, srv, "/dispatch/chain", map[string]any{
		"tasks": []map[string]any{
			{"title": "A-task", "assigned_to": "coder"},
			{"title": "B-task", "assigned_to": "writer"},
			{"title": "C-task", "assigned_to": "thinker"},
		},
	})
	if chainResp.StatusCode != http.StatusCreated {
		t.Fatalf("chain create: expected 201, got %d", chainResp.StatusCode)
	}
	var cr model.ChainResponse
	json.NewDecoder(chainResp.Body).Decode(&cr) //nolint:errcheck
	chainResp.Body.Close()

	taskA := cr.Tasks[0]
	taskB := cr.Tasks[1]

	// Before A done/failed: B deps_met should be false.
	depsResp := getJSON(t, srv, "/tasks/"+taskB.ID+"/deps-met")
	var dm model.DepsMet
	json.NewDecoder(depsResp.Body).Decode(&dm) //nolint:errcheck
	depsResp.Body.Close()
	if dm.DepsMet {
		t.Fatal("B should not be deps_met before A done")
	}

	// Drive A to failed (with retry_assigned_to: coder in result).
	claimA := postJSON(t, srv, "/tasks/"+taskA.ID+"/claim",
		map[string]any{"version": taskA.Version, "agent": "coder"})
	var claimedA model.Task
	json.NewDecoder(claimA.Body).Decode(&claimedA) //nolint:errcheck
	claimA.Body.Close()

	ipA := patchTaskTo(t, srv, taskA.ID, "in_progress", claimedA.Version)

	// PATCH A to failed with retry_assigned_to in result → triggers autoRetry.
	body, _ := json.Marshal(map[string]any{
		"status":  "failed",
		"result":  "接口报错 | retry_assigned_to: coder",
		"version": ipA.Version,
	})
	req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/tasks/"+taskA.ID, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	failResp, _ := http.DefaultClient.Do(req)
	var failPR struct {
		Task              model.Task               `json:"task"`
		BlockedDownstream []model.BlockedDownstream `json:"blocked_downstream"`
	}
	json.NewDecoder(failResp.Body).Decode(&failPR) //nolint:errcheck
	failResp.Body.Close()

	if failPR.Task.Status != model.StatusFailed {
		t.Fatalf("expected A to be failed, got %s", failPR.Task.Status)
	}

	// blocked_downstream should include B (and C).
	if len(failPR.BlockedDownstream) == 0 {
		t.Error("expected blocked_downstream to be non-empty when A fails")
	}
	foundB := false
	for _, bd := range failPR.BlockedDownstream {
		if bd.ID == taskB.ID {
			foundB = true
		}
	}
	if !foundB {
		t.Errorf("B should be in blocked_downstream, got: %+v", failPR.BlockedDownstream)
	}

	// Allow autoRetry goroutine to complete.
	time.Sleep(150 * time.Millisecond)

	// Find the retry task A'.
	listResp := getJSON(t, srv, "/tasks?assigned_to=coder")
	var listBody struct {
		Tasks []model.Task `json:"tasks"`
	}
	json.NewDecoder(listResp.Body).Decode(&listBody) //nolint:errcheck
	listResp.Body.Close()

	var taskAPrime *model.Task
	for i := range listBody.Tasks {
		if strings.HasPrefix(listBody.Tasks[i].Title, "retry:") {
			taskAPrime = &listBody.Tasks[i]
			break
		}
	}
	if taskAPrime == nil {
		t.Fatal("autoRetry should have created a retry task A'")
	}

	// Verify A has superseded_by = A'.id
	aDetail := getJSON(t, srv, "/tasks/"+taskA.ID)
	var aTask model.Task
	json.NewDecoder(aDetail.Body).Decode(&aTask) //nolint:errcheck
	aDetail.Body.Close()
	if aTask.SupersededBy != taskAPrime.ID {
		t.Fatalf("A.superseded_by should be A'.id=%q, got %q", taskAPrime.ID, aTask.SupersededBy)
	}

	// B should still NOT be deps_met (A' is still pending).
	depsResp2 := getJSON(t, srv, "/tasks/"+taskB.ID+"/deps-met")
	var dm2 model.DepsMet
	json.NewDecoder(depsResp2.Body).Decode(&dm2) //nolint:errcheck
	depsResp2.Body.Close()
	if dm2.DepsMet {
		t.Fatal("B should not be deps_met while A' is still pending")
	}

	// Drive A' to done: claim → in_progress → done.
	claimAPrime := postJSON(t, srv, "/tasks/"+taskAPrime.ID+"/claim",
		map[string]any{"version": taskAPrime.Version, "agent": "coder"})
	var claimedAPrime model.Task
	json.NewDecoder(claimAPrime.Body).Decode(&claimedAPrime) //nolint:errcheck
	claimAPrime.Body.Close()

	ipAPrime := patchTaskTo(t, srv, taskAPrime.ID, "in_progress", claimedAPrime.Version)
	patchTaskTo(t, srv, taskAPrime.ID, "done", ipAPrime.Version)

	// NOW B should be deps_met (A failed but its superseder A' is done).
	depsResp3 := getJSON(t, srv, "/tasks/"+taskB.ID+"/deps-met")
	var dm3 model.DepsMet
	json.NewDecoder(depsResp3.Body).Decode(&dm3) //nolint:errcheck
	depsResp3.Body.Close()
	if !dm3.DepsMet {
		t.Fatal("B should be deps_met after A' (superseder) is done")
	}

	// Poll as writer should now return B.
	pollResp := getJSON(t, srv, "/tasks/poll?assigned_to=writer")
	var pr model.PollResponse
	json.NewDecoder(pollResp.Body).Decode(&pr) //nolint:errcheck
	pollResp.Body.Close()
	if pr.Task == nil || pr.Task.ID != taskB.ID {
		t.Fatalf("poll for writer should return B after chain recovery, got %v", pr.Task)
	}
}

// -------------------------------------------------------------------
// GET /tasks/poll
// -------------------------------------------------------------------

func TestPoll_NoTasks_ReturnsNullTask(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	resp := getJSON(t, srv, "/tasks/poll?assigned_to=coder")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var pr model.PollResponse
	json.NewDecoder(resp.Body).Decode(&pr) //nolint:errcheck
	resp.Body.Close()
	if pr.Task != nil {
		t.Fatalf("expected nil task, got %+v", pr.Task)
	}
}

func TestPoll_MissingAssignedTo_Returns400(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	resp := getJSON(t, srv, "/tasks/poll")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestPoll_ReturnsOnlyDepsMetTasks(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	// Create task A (no deps).
	var taskA model.Task
	r := postJSON(t, srv, "/tasks", map[string]any{"title": "A", "assigned_to": "coder"})
	json.NewDecoder(r.Body).Decode(&taskA) //nolint:errcheck
	r.Body.Close()

	// Create task B (depends on A).
	var taskB model.Task
	r2 := postJSON(t, srv, "/tasks", map[string]any{
		"title": "B", "assigned_to": "coder", "depends_on": []string{taskA.ID},
	})
	json.NewDecoder(r2.Body).Decode(&taskB) //nolint:errcheck
	r2.Body.Close()

	// Poll should return A (deps met), not B.
	pollResp := getJSON(t, srv, "/tasks/poll?assigned_to=coder")
	var pr model.PollResponse
	json.NewDecoder(pollResp.Body).Decode(&pr) //nolint:errcheck
	pollResp.Body.Close()

	if pr.Task == nil {
		t.Fatal("expected a task from poll")
	}
	if pr.Task.ID != taskA.ID {
		t.Fatalf("poll returned %q, want task A %q", pr.Task.ID, taskA.ID)
	}
}

func TestPoll_PriorityOrdering(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	// Create low-priority task first.
	// Note: priority field is not in CreateTaskRequest yet, so both default to 0.
	// This test verifies created_at ASC fallback ordering (first created = first returned).
	var t1, t2 model.Task
	r1 := postJSON(t, srv, "/tasks", map[string]any{"title": "first", "assigned_to": "coder"})
	json.NewDecoder(r1.Body).Decode(&t1) //nolint:errcheck
	r1.Body.Close()

	r2 := postJSON(t, srv, "/tasks", map[string]any{"title": "second", "assigned_to": "coder"})
	json.NewDecoder(r2.Body).Decode(&t2) //nolint:errcheck
	r2.Body.Close()

	pollResp := getJSON(t, srv, "/tasks/poll?assigned_to=coder")
	var pr model.PollResponse
	json.NewDecoder(pollResp.Body).Decode(&pr) //nolint:errcheck
	pollResp.Body.Close()

	if pr.Task == nil || pr.Task.ID != t1.ID {
		t.Fatalf("expected first-created task (oldest), got %v", pr.Task)
	}
}

func TestPoll_WrongAgent_ReturnsNothing(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	postJSON(t, srv, "/tasks", map[string]any{"title": "coder-task", "assigned_to": "coder"}).Body.Close()

	// Poll as a different agent.
	resp := getJSON(t, srv, "/tasks/poll?assigned_to=devops")
	var pr model.PollResponse
	json.NewDecoder(resp.Body).Decode(&pr) //nolint:errcheck
	resp.Body.Close()

	if pr.Task != nil {
		t.Fatalf("devops should not see coder's task, got %+v", pr.Task)
	}
}
