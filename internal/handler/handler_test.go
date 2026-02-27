package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
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

// TestFailed_RetryRouting_TableLookup verifies that when no explicit retry_assigned_to
// is given, the retry_routing table is queried and the matching agent is used.
// coder's catch-all rule routes to thinker.
func TestFailed_RetryRouting_TableLookup(t *testing.T) {
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

	// Fail a coder task with no explicit retry directive.
	// V8: retry_routing catch-all rule coder→thinker should fire.
	driveToFailed(t, srv, "重要任务", "完全崩溃，没有重试方案")

	time.Sleep(150 * time.Millisecond)

	// A retry task should be created for thinker (coder catch-all → thinker).
	listResp := getJSON(t, srv, "/tasks?assigned_to=thinker")
	var listBody struct {
		Tasks []model.Task `json:"tasks"`
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
		t.Fatal("retry_routing catch-all should have created a retry task for thinker")
	}
	if retryTask.AssignedTo != "thinker" {
		t.Fatalf("expected retry task assigned to thinker, got %q", retryTask.AssignedTo)
	}
}

// TestFailed_NoRetryDirective_CEOAlert verifies that when assigned_to is not in
// retry_routing, the CEO session receives a ❌ alert.
func TestFailed_NoRetryDirective_CEOAlert(t *testing.T) {
	var mu sync.Mutex
	var ceoMessages []string
	mockOC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		if args, ok := body["args"].(map[string]any); ok {
			if msg, ok := args["message"].(string); ok {
				mu.Lock()
				ceoMessages = append(ceoMessages, msg)
				mu.Unlock()
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true}) //nolint:errcheck
	}))
	defer mockOC.Close()

	oc := openclaw.NewWithURL(mockOC.URL, "")
	srv := newTestServer(t, oc)
	defer srv.Close()

	// Use an agent not in retry_routing to ensure no table match → CEO alert.
	// "scaffold" has no catch-all in seed data (vision/pm/ops were added in V10).
	var task model.Task
	r := postJSON(t, srv, "/tasks", map[string]any{"title": "scaffold-task", "assigned_to": "scaffold"})
	json.NewDecoder(r.Body).Decode(&task) //nolint:errcheck
	r.Body.Close()

	claimR := postJSON(t, srv, "/tasks/"+task.ID+"/claim",
		map[string]any{"version": task.Version, "agent": "scaffold"})
	var claimed model.Task
	json.NewDecoder(claimR.Body).Decode(&claimed) //nolint:errcheck
	claimR.Body.Close()

	ipTask := patchTaskTo(t, srv, task.ID, "in_progress", claimed.Version)

	body, _ := json.Marshal(map[string]any{"status": "failed", "result": "完全崩溃，无法处理", "version": ipTask.Version})
	req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/tasks/"+task.ID, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()

	time.Sleep(200 * time.Millisecond)

	// CEO should have been notified.
	mu.Lock()
	snap := make([]string, len(ceoMessages))
	copy(snap, ceoMessages)
	mu.Unlock()

	found := false
	for _, msg := range snap {
		if strings.Contains(msg, "❌") && strings.Contains(msg, "scaffold-task") {
			found = true
		}
	}
	if !found {
		t.Errorf("CEO should have received ❌ alert for failed task, got: %v", snap)
	}
}

// -------------------------------------------------------------------
// V8: chain complete notification + triggered dispatch fix
// -------------------------------------------------------------------

func TestV8_ChainComplete_NotifiesCEO(t *testing.T) {
	var mu sync.Mutex
	var ceoMessages []string
	mockOC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		if args, ok := body["args"].(map[string]any); ok {
			if msg, ok := args["message"].(string); ok {
				mu.Lock()
				ceoMessages = append(ceoMessages, msg)
				mu.Unlock()
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true}) //nolint:errcheck
	}))
	defer mockOC.Close()

	oc := openclaw.NewWithURL(mockOC.URL, "")
	srv := newTestServer(t, oc)
	defer srv.Close()

	// Create chain A→B with notify_ceo_on_complete=true.
	chainResp := postJSON(t, srv, "/dispatch/chain", map[string]any{
		"tasks": []map[string]any{
			{"title": "chain-A", "assigned_to": "coder"},
			{"title": "chain-B", "assigned_to": "writer"},
		},
		"notify_ceo_on_complete": true,
		"chain_title":            "测试链路",
	})
	if chainResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", chainResp.StatusCode)
	}
	var cr model.ChainResponse
	json.NewDecoder(chainResp.Body).Decode(&cr) //nolint:errcheck
	chainResp.Body.Close()

	taskA := cr.Tasks[0]
	taskB := cr.Tasks[1]

	// Drive A to done.
	claimA := postJSON(t, srv, "/tasks/"+taskA.ID+"/claim",
		map[string]any{"version": taskA.Version, "agent": "coder"})
	var claimedA model.Task
	json.NewDecoder(claimA.Body).Decode(&claimedA) //nolint:errcheck
	claimA.Body.Close()
	ipA := patchTaskTo(t, srv, taskA.ID, "in_progress", claimedA.Version)
	patchTaskTo(t, srv, taskA.ID, "done", ipA.Version)

	// CEO should NOT be notified yet (chain not complete).
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	foundChain := false
	for _, msg := range ceoMessages {
		if strings.Contains(msg, "✅") && strings.Contains(msg, "任务链") {
			foundChain = true
		}
	}
	mu.Unlock()
	if foundChain {
		t.Error("CEO should NOT be notified until chain is fully complete")
	}

	// Drive B to done → chain complete.
	// B was unlocked by A done; claim it.
	claimB := postJSON(t, srv, "/tasks/"+taskB.ID+"/claim",
		map[string]any{"version": taskB.Version, "agent": "writer"})
	var claimedB model.Task
	json.NewDecoder(claimB.Body).Decode(&claimedB) //nolint:errcheck
	claimB.Body.Close()
	ipB := patchTaskTo(t, srv, taskB.ID, "in_progress", claimedB.Version)
	patchTaskTo(t, srv, taskB.ID, "done", ipB.Version)

	time.Sleep(200 * time.Millisecond)

	// CEO should now be notified.
	mu.Lock()
	snap := make([]string, len(ceoMessages))
	copy(snap, ceoMessages)
	mu.Unlock()

	foundChain = false
	for _, msg := range snap {
		if strings.Contains(msg, "✅") && strings.Contains(msg, "任务链") {
			foundChain = true
		}
	}
	if !foundChain {
		t.Errorf("CEO should have received chain complete notification, got: %v", snap)
	}
}

func TestV8_TriggeredDispatch_WakesDownstream(t *testing.T) {
	// Capture all sessions_send calls to verify downstream dispatch.
	var mu sync.Mutex
	var dispatched []string
	mockOC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		if args, ok := body["args"].(map[string]any); ok {
			if key, ok := args["sessionKey"].(string); ok {
				mu.Lock()
				dispatched = append(dispatched, key)
				mu.Unlock()
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true}) //nolint:errcheck
	}))
	defer mockOC.Close()

	oc := openclaw.NewWithURL(mockOC.URL, "")
	srv := newTestServer(t, oc)
	defer srv.Close()

	// Create A→B chain.
	chainResp := postJSON(t, srv, "/dispatch/chain", map[string]any{
		"tasks": []map[string]any{
			{"title": "trigger-A", "assigned_to": "coder"},
			{"title": "trigger-B", "assigned_to": "writer"},
		},
	})
	var cr model.ChainResponse
	json.NewDecoder(chainResp.Body).Decode(&cr) //nolint:errcheck
	chainResp.Body.Close()
	taskA := cr.Tasks[0]

	// Clear captured dispatches (first one is from chain dispatch).
	time.Sleep(30 * time.Millisecond)
	mu.Lock()
	dispatched = nil
	mu.Unlock()

	// Drive A to done → should trigger dispatch to writer.
	claimA := postJSON(t, srv, "/tasks/"+taskA.ID+"/claim",
		map[string]any{"version": taskA.Version, "agent": "coder"})
	var claimedA model.Task
	json.NewDecoder(claimA.Body).Decode(&claimedA) //nolint:errcheck
	claimA.Body.Close()
	ipA := patchTaskTo(t, srv, taskA.ID, "in_progress", claimedA.Version)
	patchTaskTo(t, srv, taskA.ID, "done", ipA.Version)

	time.Sleep(200 * time.Millisecond)

	// Verify writer session was dispatched.
	writerKey := "agent:writer:discord:channel:1475339585075548200"
	mu.Lock()
	snap := make([]string, len(dispatched))
	copy(snap, dispatched)
	mu.Unlock()

	found := false
	for _, key := range snap {
		if key == writerKey {
			found = true
		}
	}
	if !found {
		t.Errorf("writer session should have been dispatched after A done, got: %v", snap)
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

// -------------------------------------------------------------------
// V9: StaleTicker re-dispatch
// -------------------------------------------------------------------

// newTestServerWithHandler creates a test server and returns both server and handler
// for direct method access (e.g. CheckStaleTasks).
func newTestServerWithHandler(t *testing.T, oc *openclaw.Client) (*httptest.Server, *handler.Handler) {
	t.Helper()
	f, err := os.CreateTemp("", "handler-stale-test-*.db")
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
	return httptest.NewServer(mux), h
}

// TestV9_StaleTicker_ReDispatchesPendingTask verifies that CheckStaleTasks
// re-dispatches a task whose updated_at is beyond the stale threshold.
func TestV9_StaleTicker_ReDispatchesPendingTask(t *testing.T) {
	var mu sync.Mutex
	var dispatched []string

	mockOC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		if args, ok := body["args"].(map[string]any); ok {
			if key, ok := args["sessionKey"].(string); ok {
				mu.Lock()
				dispatched = append(dispatched, key)
				mu.Unlock()
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true}) //nolint:errcheck
	}))
	defer mockOC.Close()

	oc := openclaw.NewWithURL(mockOC.URL, "")
	srv, h := newTestServerWithHandler(t, oc)
	defer srv.Close()

	// Create a pending task for coder BEFORE setting threshold,
	// so the task's updated_at is genuinely in the past by the time we check.
	var task model.Task
	r := postJSON(t, srv, "/tasks", map[string]any{"title": "stale-task", "assigned_to": "coder"})
	json.NewDecoder(r.Body).Decode(&task) //nolint:errcheck
	r.Body.Close()

	// Wait briefly so updated_at is strictly before 'now'.
	time.Sleep(50 * time.Millisecond)

	// Set stale threshold to 1ms so the task (created 50ms ago) is immediately stale.
	h.SetStaleThresholdForTesting(time.Millisecond)

	// Clear any dispatch from task creation.
	mu.Lock()
	dispatched = nil
	mu.Unlock()

	// Trigger stale check directly (no waiting for 10min ticker).
	h.CheckStaleTasks()

	time.Sleep(50 * time.Millisecond)

	// Verify coder session was re-dispatched.
	coderKey := "agent:coder:discord:channel:1475338640593916045"
	mu.Lock()
	snap := make([]string, len(dispatched))
	copy(snap, dispatched)
	mu.Unlock()

	found := false
	for _, key := range snap {
		if key == coderKey {
			found = true
		}
	}
	if !found {
		t.Errorf("stale ticker should have re-dispatched coder task, got: %v", snap)
	}
}

// TestV9_StaleTicker_SkipsDepsPendingTasks verifies that tasks with unmet deps
// are NOT re-dispatched by the stale ticker.
func TestV9_StaleTicker_SkipsDepsPendingTasks(t *testing.T) {
	var mu sync.Mutex
	var dispatched []string

	mockOC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		if args, ok := body["args"].(map[string]any); ok {
			if key, ok := args["sessionKey"].(string); ok {
				mu.Lock()
				dispatched = append(dispatched, key)
				mu.Unlock()
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true}) //nolint:errcheck
	}))
	defer mockOC.Close()

	oc := openclaw.NewWithURL(mockOC.URL, "")
	srv, h := newTestServerWithHandler(t, oc)
	defer srv.Close()

	// Create A→B chain. B depends on A (not done).
	chainResp := postJSON(t, srv, "/dispatch/chain", map[string]any{
		"tasks": []map[string]any{
			{"title": "stale-A", "assigned_to": "coder"},
			{"title": "stale-B", "assigned_to": "writer"},
		},
	})
	var cr model.ChainResponse
	json.NewDecoder(chainResp.Body).Decode(&cr) //nolint:errcheck
	chainResp.Body.Close()

	taskB := cr.Tasks[1]

	// Wait so tasks are genuinely stale (updated_at in the past).
	time.Sleep(50 * time.Millisecond)

	// Set stale threshold to 1ms.
	h.SetStaleThresholdForTesting(time.Millisecond)

	mu.Lock()
	dispatched = nil
	mu.Unlock()

	h.CheckStaleTasks()
	time.Sleep(50 * time.Millisecond)

	// B should NOT be re-dispatched because its dep (A) is not done.
	writerKey := "agent:writer:discord:channel:1475339585075548200"
	mu.Lock()
	snap := make([]string, len(dispatched))
	copy(snap, dispatched)
	mu.Unlock()

	for _, key := range snap {
		if key == writerKey {
			t.Errorf("writer task %s should not be re-dispatched (deps not met), dispatched: %v", taskB.ID, snap)
		}
	}
}

// -------------------------------------------------------------------
// V10: review-reject two-stage chain (autoRetryReviewReject)
// -------------------------------------------------------------------

// patchTaskToFailed drives a task through claim→in_progress→failed with given result.
func patchTaskToFailed(t *testing.T, srv *httptest.Server, taskID string, version int, agent, result string) model.Task {
	t.Helper()
	claimR := postJSON(t, srv, "/tasks/"+taskID+"/claim",
		map[string]any{"version": version, "agent": agent})
	var claimed model.Task
	json.NewDecoder(claimR.Body).Decode(&claimed) //nolint:errcheck
	claimR.Body.Close()

	ipTask := patchTaskTo(t, srv, taskID, "in_progress", claimed.Version)

	body, _ := json.Marshal(map[string]any{
		"status":  "failed",
		"result":  result,
		"version": ipTask.Version,
	})
	req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/tasks/"+taskID, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	var pr struct{ Task model.Task }
	json.NewDecoder(resp.Body).Decode(&pr) //nolint:errcheck
	resp.Body.Close()
	return pr.Task
}

// driveTaskToDone drives a task through claim→in_progress→done.
func driveTaskToDone(t *testing.T, srv *httptest.Server, taskID string, version int, agent string) model.Task {
	t.Helper()
	claimR := postJSON(t, srv, "/tasks/"+taskID+"/claim",
		map[string]any{"version": version, "agent": agent})
	var claimed model.Task
	json.NewDecoder(claimR.Body).Decode(&claimed) //nolint:errcheck
	claimR.Body.Close()

	ipTask := patchTaskTo(t, srv, taskID, "in_progress", claimed.Version)
	return patchTaskTo(t, srv, taskID, "done", ipTask.Version)
}

// TestV10_ReviewReject_TwoStageChain verifies that when thinker fails a review
// task with retry_assigned_to pointing to a different agent (coder), a two-stage
// chain is created: fix task (coder) → re-review task (thinker).
// The original task's superseded_by should point to re-review (not fix),
// so downstream deps (e.g. qa) wait until re-review passes.
func TestV10_ReviewReject_TwoStageChain(t *testing.T) {
	mockOC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"ok": true}) //nolint:errcheck
	}))
	defer mockOC.Close()

	oc := openclaw.NewWithURL(mockOC.URL, "")
	srv := newTestServer(t, oc)
	defer srv.Close()

	// Create chain: coder → thinker(review) → qa
	chainResp := postJSON(t, srv, "/dispatch/chain", map[string]any{
		"tasks": []map[string]any{
			{"title": "impl-task", "assigned_to": "coder"},
			{"title": "review-task", "assigned_to": "thinker"},
			{"title": "qa-task", "assigned_to": "qa"},
		},
	})
	if chainResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", chainResp.StatusCode)
	}
	var cr model.ChainResponse
	json.NewDecoder(chainResp.Body).Decode(&cr) //nolint:errcheck
	chainResp.Body.Close()

	implTask := cr.Tasks[0]
	reviewTask := cr.Tasks[1]
	qaTask := cr.Tasks[2]

	// Drive impl to done.
	driveTaskToDone(t, srv, implTask.ID, implTask.Version, "coder")
	time.Sleep(50 * time.Millisecond)

	// Drive review to failed with REQUEST_CHANGES → triggers review-reject two-stage.
	// thinker rejects → retry goes to coder (different agent = review-reject).
	patchTaskToFailed(t, srv, reviewTask.ID, reviewTask.Version, "thinker",
		"REQUEST_CHANGES: 代码有问题 | retry_assigned_to: coder")

	// Allow autoRetry goroutine to complete.
	time.Sleep(200 * time.Millisecond)

	// Find fix and re-review tasks.
	listResp := getJSON(t, srv, "/tasks")
	var listBody struct {
		Tasks []model.Task `json:"tasks"`
	}
	json.NewDecoder(listResp.Body).Decode(&listBody) //nolint:errcheck
	listResp.Body.Close()

	var fixTask, reReviewTask *model.Task
	for i := range listBody.Tasks {
		t2 := &listBody.Tasks[i]
		if strings.HasPrefix(t2.Title, "fix:") {
			fixTask = t2
		}
		if strings.HasPrefix(t2.Title, "re-review:") {
			reReviewTask = t2
		}
	}

	if fixTask == nil {
		t.Fatal("expected a 'fix: ...' task to be created")
	}
	if reReviewTask == nil {
		t.Fatal("expected a 're-review: ...' task to be created")
	}

	// Verify fix task is assigned to coder (the implementer).
	if fixTask.AssignedTo != "coder" {
		t.Fatalf("fix task should be assigned to coder, got %q", fixTask.AssignedTo)
	}
	// Verify re-review task is assigned to thinker (original reviewer).
	if reReviewTask.AssignedTo != "thinker" {
		t.Fatalf("re-review task should be assigned to thinker, got %q", reReviewTask.AssignedTo)
	}

	// Verify re-review depends on fix.
	reReviewDetail := getJSON(t, srv, "/tasks/"+reReviewTask.ID)
	var rrDetail model.Task
	json.NewDecoder(reReviewDetail.Body).Decode(&rrDetail) //nolint:errcheck
	reReviewDetail.Body.Close()
	if len(rrDetail.DependsOn) != 1 || rrDetail.DependsOn[0] != fixTask.ID {
		t.Fatalf("re-review should depend on fix task, got %v", rrDetail.DependsOn)
	}

	// Verify original review task's superseded_by → re-review (NOT fix).
	reviewDetail := getJSON(t, srv, "/tasks/"+reviewTask.ID)
	var rvDetail model.Task
	json.NewDecoder(reviewDetail.Body).Decode(&rvDetail) //nolint:errcheck
	reviewDetail.Body.Close()
	if rvDetail.SupersededBy != reReviewTask.ID {
		t.Fatalf("original review.superseded_by should point to re-review %q, got %q",
			reReviewTask.ID, rvDetail.SupersededBy)
	}

	// qa task should NOT be deps_met yet (re-review not done).
	depsResp := getJSON(t, srv, "/tasks/"+qaTask.ID+"/deps-met")
	var dm model.DepsMet
	json.NewDecoder(depsResp.Body).Decode(&dm) //nolint:errcheck
	depsResp.Body.Close()
	if dm.DepsMet {
		t.Fatal("qa task should NOT be deps_met while re-review is pending")
	}

	// Drive fix task to done.
	driveTaskToDone(t, srv, fixTask.ID, fixTask.Version, "coder")
	time.Sleep(100 * time.Millisecond)

	// qa still NOT deps_met (re-review still pending).
	depsResp2 := getJSON(t, srv, "/tasks/"+qaTask.ID+"/deps-met")
	var dm2 model.DepsMet
	json.NewDecoder(depsResp2.Body).Decode(&dm2) //nolint:errcheck
	depsResp2.Body.Close()
	if dm2.DepsMet {
		t.Fatal("qa task should NOT be deps_met while re-review is still pending (fix done but re-review not)")
	}

	// Drive re-review to done.
	// Re-fetch re-review to get latest version.
	rrLatest := getJSON(t, srv, "/tasks/"+reReviewTask.ID)
	var rrLatestTask model.Task
	json.NewDecoder(rrLatest.Body).Decode(&rrLatestTask) //nolint:errcheck
	rrLatest.Body.Close()
	driveTaskToDone(t, srv, reReviewTask.ID, rrLatestTask.Version, "thinker")
	time.Sleep(100 * time.Millisecond)

	// NOW qa should be deps_met.
	depsResp3 := getJSON(t, srv, "/tasks/"+qaTask.ID+"/deps-met")
	var dm3 model.DepsMet
	json.NewDecoder(depsResp3.Body).Decode(&dm3) //nolint:errcheck
	depsResp3.Body.Close()
	if !dm3.DepsMet {
		t.Fatal("qa task should be deps_met after re-review is done")
	}
}

// TestV10_ReviewReject_SecurityAgent verifies that security agent also triggers
// the two-stage review-reject flow (not just thinker).
func TestV10_ReviewReject_SecurityAgent(t *testing.T) {
	mockOC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"ok": true}) //nolint:errcheck
	}))
	defer mockOC.Close()

	oc := openclaw.NewWithURL(mockOC.URL, "")
	srv := newTestServer(t, oc)
	defer srv.Close()

	// Create a security review task directly.
	var secTask model.Task
	r := postJSON(t, srv, "/tasks", map[string]any{
		"title":       "security-review",
		"assigned_to": "security",
	})
	json.NewDecoder(r.Body).Decode(&secTask) //nolint:errcheck
	r.Body.Close()

	// Drive to failed with retry to coder.
	patchTaskToFailed(t, srv, secTask.ID, secTask.Version, "security",
		"安全漏洞 | retry_assigned_to: coder")

	time.Sleep(200 * time.Millisecond)

	// Should create fix + re-review (two-stage), not a single retry.
	listResp := getJSON(t, srv, "/tasks")
	var listBody struct {
		Tasks []model.Task `json:"tasks"`
	}
	json.NewDecoder(listResp.Body).Decode(&listBody) //nolint:errcheck
	listResp.Body.Close()

	var fixTask, reReviewTask *model.Task
	for i := range listBody.Tasks {
		t2 := &listBody.Tasks[i]
		if strings.HasPrefix(t2.Title, "fix:") {
			fixTask = t2
		}
		if strings.HasPrefix(t2.Title, "re-review:") {
			reReviewTask = t2
		}
	}

	if fixTask == nil {
		t.Fatal("security review-reject should create a fix task")
	}
	if reReviewTask == nil {
		t.Fatal("security review-reject should create a re-review task")
	}
	if fixTask.AssignedTo != "coder" {
		t.Fatalf("fix task should be assigned to coder, got %q", fixTask.AssignedTo)
	}
	if reReviewTask.AssignedTo != "security" {
		t.Fatalf("re-review task should be assigned back to security, got %q", reReviewTask.AssignedTo)
	}
}

// TestV10_NonReviewReject_SingleRetry verifies that when the original task is NOT
// assigned to a reviewer (thinker/security) OR the retry agent is the same,
// the standard single-retry path is used (no two-stage chain).
func TestV10_NonReviewReject_SingleRetry(t *testing.T) {
	mockOC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"ok": true}) //nolint:errcheck
	}))
	defer mockOC.Close()

	oc := openclaw.NewWithURL(mockOC.URL, "")
	srv := newTestServer(t, oc)
	defer srv.Close()

	// coder task fails → retry to thinker (standard single retry, not review-reject).
	driveToFailed(t, srv, "coder-task-fail", "编译失败 | retry_assigned_to: thinker")

	time.Sleep(200 * time.Millisecond)

	listResp := getJSON(t, srv, "/tasks")
	var listBody struct {
		Tasks []model.Task `json:"tasks"`
	}
	json.NewDecoder(listResp.Body).Decode(&listBody) //nolint:errcheck
	listResp.Body.Close()

	var retryTask *model.Task
	var fixTask *model.Task
	for i := range listBody.Tasks {
		t2 := &listBody.Tasks[i]
		if strings.HasPrefix(t2.Title, "retry:") {
			retryTask = t2
		}
		if strings.HasPrefix(t2.Title, "fix:") {
			fixTask = t2
		}
	}

	// Should use single retry, NOT two-stage.
	if retryTask == nil {
		t.Fatal("expected a 'retry: ...' task (single retry)")
	}
	if fixTask != nil {
		t.Fatal("should NOT create a 'fix: ...' task for non-review-reject scenario")
	}
	if retryTask.AssignedTo != "thinker" {
		t.Fatalf("retry task should be assigned to thinker, got %q", retryTask.AssignedTo)
	}
}

// TestV10_ReviewReject_SameAgent_SingleRetry verifies that when thinker fails
// and retries to itself (same agent), it uses single retry, not two-stage.
func TestV10_ReviewReject_SameAgent_SingleRetry(t *testing.T) {
	mockOC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"ok": true}) //nolint:errcheck
	}))
	defer mockOC.Close()

	oc := openclaw.NewWithURL(mockOC.URL, "")
	srv := newTestServer(t, oc)
	defer srv.Close()

	// Create a thinker task that retries to itself.
	var tTask model.Task
	r := postJSON(t, srv, "/tasks", map[string]any{
		"title":       "thinker-self-retry",
		"assigned_to": "thinker",
	})
	json.NewDecoder(r.Body).Decode(&tTask) //nolint:errcheck
	r.Body.Close()

	patchTaskToFailed(t, srv, tTask.ID, tTask.Version, "thinker",
		"需要重新思考 | retry_assigned_to: thinker")

	time.Sleep(200 * time.Millisecond)

	listResp := getJSON(t, srv, "/tasks")
	var listBody struct {
		Tasks []model.Task `json:"tasks"`
	}
	json.NewDecoder(listResp.Body).Decode(&listBody) //nolint:errcheck
	listResp.Body.Close()

	var retryTask *model.Task
	var fixTask *model.Task
	for i := range listBody.Tasks {
		t2 := &listBody.Tasks[i]
		if strings.HasPrefix(t2.Title, "retry:") {
			retryTask = t2
		}
		if strings.HasPrefix(t2.Title, "fix:") {
			fixTask = t2
		}
	}

	// Same agent → single retry, not two-stage.
	if retryTask == nil {
		t.Fatal("expected single 'retry: ...' task when thinker retries to itself")
	}
	if fixTask != nil {
		t.Fatal("should NOT create fix/re-review for same-agent retry")
	}
}

// TestV10_MultiLevelReject_UpdateSupersededByChain verifies that when a re-review
// itself fails again (multi-level reject), UpdateSupersededByChain correctly
// redirects existing superseded_by pointers so downstream deps follow the latest chain.
func TestV10_MultiLevelReject_UpdateSupersededByChain(t *testing.T) {
	mockOC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"ok": true}) //nolint:errcheck
	}))
	defer mockOC.Close()

	oc := openclaw.NewWithURL(mockOC.URL, "")
	srv := newTestServer(t, oc)
	defer srv.Close()

	// Create chain: coder → thinker → qa
	chainResp := postJSON(t, srv, "/dispatch/chain", map[string]any{
		"tasks": []map[string]any{
			{"title": "impl", "assigned_to": "coder"},
			{"title": "review", "assigned_to": "thinker"},
			{"title": "test", "assigned_to": "qa"},
		},
	})
	var cr model.ChainResponse
	json.NewDecoder(chainResp.Body).Decode(&cr) //nolint:errcheck
	chainResp.Body.Close()

	implTask := cr.Tasks[0]
	reviewTask := cr.Tasks[1]
	qaTask := cr.Tasks[2]

	// Drive impl to done.
	driveTaskToDone(t, srv, implTask.ID, implTask.Version, "coder")
	time.Sleep(50 * time.Millisecond)

	// === First reject ===
	patchTaskToFailed(t, srv, reviewTask.ID, reviewTask.Version, "thinker",
		"REQUEST_CHANGES: 第一次退单 | retry_assigned_to: coder")
	time.Sleep(200 * time.Millisecond)

	// Find first fix and re-review tasks.
	listResp := getJSON(t, srv, "/tasks")
	var listBody struct {
		Tasks []model.Task `json:"tasks"`
	}
	json.NewDecoder(listResp.Body).Decode(&listBody) //nolint:errcheck
	listResp.Body.Close()

	var fix1, reReview1 *model.Task
	for i := range listBody.Tasks {
		t2 := &listBody.Tasks[i]
		if strings.HasPrefix(t2.Title, "fix:") && fix1 == nil {
			fix1 = t2
		}
		if strings.HasPrefix(t2.Title, "re-review:") && reReview1 == nil {
			reReview1 = t2
		}
	}
	if fix1 == nil || reReview1 == nil {
		t.Fatal("first reject should create fix + re-review tasks")
	}

	// Drive fix1 to done so re-review1 becomes available.
	driveTaskToDone(t, srv, fix1.ID, fix1.Version, "coder")
	time.Sleep(100 * time.Millisecond)

	// === Second reject: re-review1 also fails ===
	rr1Latest := getJSON(t, srv, "/tasks/"+reReview1.ID)
	var rr1Detail model.Task
	json.NewDecoder(rr1Latest.Body).Decode(&rr1Detail) //nolint:errcheck
	rr1Latest.Body.Close()

	patchTaskToFailed(t, srv, reReview1.ID, rr1Detail.Version, "thinker",
		"REQUEST_CHANGES: 第二次退单 | retry_assigned_to: coder")
	time.Sleep(200 * time.Millisecond)

	// Find second fix and re-review tasks.
	listResp2 := getJSON(t, srv, "/tasks")
	var listBody2 struct {
		Tasks []model.Task `json:"tasks"`
	}
	json.NewDecoder(listResp2.Body).Decode(&listBody2) //nolint:errcheck
	listResp2.Body.Close()

	var fix2, reReview2 *model.Task
	for i := range listBody2.Tasks {
		t2 := &listBody2.Tasks[i]
		// Skip first-round tasks.
		if t2.ID == fix1.ID || t2.ID == reReview1.ID {
			continue
		}
		if strings.HasPrefix(t2.Title, "fix:") && fix2 == nil {
			fix2 = t2
		}
		if strings.HasPrefix(t2.Title, "re-review:") && reReview2 == nil {
			reReview2 = t2
		}
	}
	if fix2 == nil || reReview2 == nil {
		t.Fatal("second reject should create a new fix + re-review pair")
	}

	// Key assertion: original review task's superseded_by should now point to
	// reReview2 (updated by UpdateSupersededByChain), not reReview1.
	origReview := getJSON(t, srv, "/tasks/"+reviewTask.ID)
	var origDetail model.Task
	json.NewDecoder(origReview.Body).Decode(&origDetail) //nolint:errcheck
	origReview.Body.Close()

	if origDetail.SupersededBy != reReview2.ID {
		t.Fatalf("original review.superseded_by should be updated to reReview2 %q by UpdateSupersededByChain, got %q",
			reReview2.ID, origDetail.SupersededBy)
	}

	// qa should NOT be deps_met (reReview2 not done).
	depsResp := getJSON(t, srv, "/tasks/"+qaTask.ID+"/deps-met")
	var dm model.DepsMet
	json.NewDecoder(depsResp.Body).Decode(&dm) //nolint:errcheck
	depsResp.Body.Close()
	if dm.DepsMet {
		t.Fatal("qa should NOT be deps_met while second re-review is pending")
	}

	// Drive fix2 + reReview2 to done → qa should finally be deps_met.
	driveTaskToDone(t, srv, fix2.ID, fix2.Version, "coder")
	time.Sleep(100 * time.Millisecond)

	rr2Latest := getJSON(t, srv, "/tasks/"+reReview2.ID)
	var rr2Detail model.Task
	json.NewDecoder(rr2Latest.Body).Decode(&rr2Detail) //nolint:errcheck
	rr2Latest.Body.Close()

	driveTaskToDone(t, srv, reReview2.ID, rr2Detail.Version, "thinker")
	time.Sleep(100 * time.Millisecond)

	depsResp2 := getJSON(t, srv, "/tasks/"+qaTask.ID+"/deps-met")
	var dm2 model.DepsMet
	json.NewDecoder(depsResp2.Body).Decode(&dm2) //nolint:errcheck
	depsResp2.Body.Close()
	if !dm2.DepsMet {
		t.Fatal("qa should be deps_met after second re-review is done")
	}
}

// TestV10_ReviewReject_FixDispatchedReReviewAutoTriggered verifies that only the
// fix task is dispatched immediately; the re-review task is auto-triggered when
// the fix task completes (via the standard deps-met + triggered dispatch flow).
func TestV10_ReviewReject_FixDispatchedReReviewAutoTriggered(t *testing.T) {
	var mu sync.Mutex
	var dispatched []string
	mockOC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		if args, ok := body["args"].(map[string]any); ok {
			if key, ok := args["sessionKey"].(string); ok {
				mu.Lock()
				dispatched = append(dispatched, key)
				mu.Unlock()
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true}) //nolint:errcheck
	}))
	defer mockOC.Close()

	oc := openclaw.NewWithURL(mockOC.URL, "")
	srv := newTestServer(t, oc)
	defer srv.Close()

	// Create a thinker task.
	var tTask model.Task
	r := postJSON(t, srv, "/tasks", map[string]any{
		"title":       "thinker-review",
		"assigned_to": "thinker",
	})
	json.NewDecoder(r.Body).Decode(&tTask) //nolint:errcheck
	r.Body.Close()

	// Fail it → triggers two-stage.
	patchTaskToFailed(t, srv, tTask.ID, tTask.Version, "thinker",
		"REQUEST_CHANGES | retry_assigned_to: coder")
	time.Sleep(200 * time.Millisecond)

	// After the review-reject, coder should have been dispatched (for fix task).
	coderKey := "agent:coder:discord:channel:1475338640593916045"
	thinkerKey := "agent:thinker:discord:channel:1475338689646297305"

	mu.Lock()
	dispatchedAfterReject := make([]string, len(dispatched))
	copy(dispatchedAfterReject, dispatched)
	dispatched = nil // reset for next phase
	mu.Unlock()

	coderDispatched := false
	for _, key := range dispatchedAfterReject {
		if key == coderKey {
			coderDispatched = true
		}
	}
	if !coderDispatched {
		t.Errorf("coder should have been dispatched for fix task, got: %v", dispatchedAfterReject)
	}

	// Find and drive fix task to done → should trigger dispatch to thinker for re-review.
	listResp := getJSON(t, srv, "/tasks")
	var listBody struct {
		Tasks []model.Task `json:"tasks"`
	}
	json.NewDecoder(listResp.Body).Decode(&listBody) //nolint:errcheck
	listResp.Body.Close()

	var fixTask *model.Task
	for i := range listBody.Tasks {
		if strings.HasPrefix(listBody.Tasks[i].Title, "fix:") {
			fixTask = &listBody.Tasks[i]
			break
		}
	}
	if fixTask == nil {
		t.Fatal("fix task not found")
	}

	driveTaskToDone(t, srv, fixTask.ID, fixTask.Version, "coder")
	time.Sleep(200 * time.Millisecond)

	// Thinker should be dispatched for the re-review.
	mu.Lock()
	dispatchedAfterFix := make([]string, len(dispatched))
	copy(dispatchedAfterFix, dispatched)
	mu.Unlock()

	thinkerDispatched := false
	for _, key := range dispatchedAfterFix {
		if key == thinkerKey {
			thinkerDispatched = true
		}
	}
	if !thinkerDispatched {
		t.Errorf("thinker should have been dispatched for re-review after fix done, got: %v", dispatchedAfterFix)
	}
}

// TestV10_ReviewReject_ChainIDPropagated verifies that fix and re-review tasks
// inherit the chain_id from the original review task.
func TestV10_ReviewReject_ChainIDPropagated(t *testing.T) {
	mockOC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"ok": true}) //nolint:errcheck
	}))
	defer mockOC.Close()

	oc := openclaw.NewWithURL(mockOC.URL, "")
	srv := newTestServer(t, oc)
	defer srv.Close()

	// Create chain with known chain_id.
	chainResp := postJSON(t, srv, "/dispatch/chain", map[string]any{
		"tasks": []map[string]any{
			{"title": "step-1", "assigned_to": "coder"},
			{"title": "step-2-review", "assigned_to": "thinker"},
		},
	})
	var cr model.ChainResponse
	json.NewDecoder(chainResp.Body).Decode(&cr) //nolint:errcheck
	chainResp.Body.Close()

	chainID := cr.ChainID
	implTask := cr.Tasks[0]
	reviewTask := cr.Tasks[1]

	// Drive impl to done.
	driveTaskToDone(t, srv, implTask.ID, implTask.Version, "coder")
	time.Sleep(50 * time.Millisecond)

	// Fail review → two-stage.
	patchTaskToFailed(t, srv, reviewTask.ID, reviewTask.Version, "thinker",
		"REQUEST_CHANGES | retry_assigned_to: coder")
	time.Sleep(200 * time.Millisecond)

	// Verify fix and re-review have the same chain_id.
	listResp := getJSON(t, srv, "/tasks")
	var listBody struct {
		Tasks []model.Task `json:"tasks"`
	}
	json.NewDecoder(listResp.Body).Decode(&listBody) //nolint:errcheck
	listResp.Body.Close()

	for _, t2 := range listBody.Tasks {
		if strings.HasPrefix(t2.Title, "fix:") || strings.HasPrefix(t2.Title, "re-review:") {
			if t2.ChainID != chainID {
				t.Errorf("task %q (title=%q) should have chain_id=%q, got %q",
					t2.ID, t2.Title, chainID, t2.ChainID)
			}
		}
	}
}

// -------------------------------------------------------------------
// V10.1: vision/pm/ops retry_routing coverage
// -------------------------------------------------------------------

// TestV10_RetryRouting_VisionDefault_Coder verifies vision catch-all routes to coder.
func TestV10_RetryRouting_VisionDefault_Coder(t *testing.T) {
	mockOC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"ok": true}) //nolint:errcheck
	}))
	defer mockOC.Close()

	oc := openclaw.NewWithURL(mockOC.URL, "")
	srv := newTestServer(t, oc)
	defer srv.Close()

	// vision task fails with no "设计" keyword → catch-all → coder.
	var task model.Task
	r := postJSON(t, srv, "/tasks", map[string]any{"title": "vision-check", "assigned_to": "vision"})
	json.NewDecoder(r.Body).Decode(&task) //nolint:errcheck
	r.Body.Close()

	patchTaskToFailed(t, srv, task.ID, task.Version, "vision",
		"实现有问题 | retry_assigned_to: coder")
	time.Sleep(200 * time.Millisecond)

	// vision != coder → isReviewReject → two-stage chain (fix + re-review).
	listResp := getJSON(t, srv, "/tasks")
	var listBody struct {
		Tasks []model.Task `json:"tasks"`
	}
	json.NewDecoder(listResp.Body).Decode(&listBody) //nolint:errcheck
	listResp.Body.Close()

	var fixTask *model.Task
	for i := range listBody.Tasks {
		if strings.HasPrefix(listBody.Tasks[i].Title, "fix:") {
			fixTask = &listBody.Tasks[i]
			break
		}
	}
	if fixTask == nil {
		t.Fatal("vision default route should create a fix task (via review-reject)")
	}
	if fixTask.AssignedTo != "coder" {
		t.Fatalf("fix task should be assigned to coder, got %q", fixTask.AssignedTo)
	}
}

// TestV10_RetryRouting_VisionDesign_UIUX verifies vision + "设计" keyword routes to uiux.
func TestV10_RetryRouting_VisionDesign_UIUX(t *testing.T) {
	mockOC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"ok": true}) //nolint:errcheck
	}))
	defer mockOC.Close()

	oc := openclaw.NewWithURL(mockOC.URL, "")
	srv := newTestServer(t, oc)
	defer srv.Close()

	// vision task fails with "设计" keyword → priority match → uiux.
	var task model.Task
	r := postJSON(t, srv, "/tasks", map[string]any{"title": "vision-design", "assigned_to": "vision"})
	json.NewDecoder(r.Body).Decode(&task) //nolint:errcheck
	r.Body.Close()

	patchTaskToFailed(t, srv, task.ID, task.Version, "vision",
		"设计风格不对 | retry_assigned_to: uiux")
	time.Sleep(200 * time.Millisecond)

	// vision != uiux → isReviewReject → two-stage chain.
	listResp := getJSON(t, srv, "/tasks")
	var listBody struct {
		Tasks []model.Task `json:"tasks"`
	}
	json.NewDecoder(listResp.Body).Decode(&listBody) //nolint:errcheck
	listResp.Body.Close()

	var fixTask, reReviewTask *model.Task
	for i := range listBody.Tasks {
		t2 := &listBody.Tasks[i]
		if strings.HasPrefix(t2.Title, "fix:") {
			fixTask = t2
		}
		if strings.HasPrefix(t2.Title, "re-review:") {
			reReviewTask = t2
		}
	}
	if fixTask == nil {
		t.Fatal("vision+设计 route should create a fix task")
	}
	if fixTask.AssignedTo != "uiux" {
		t.Fatalf("fix task should be assigned to uiux, got %q", fixTask.AssignedTo)
	}
	if reReviewTask == nil {
		t.Fatal("vision+设计 route should create a re-review task")
	}
	if reReviewTask.AssignedTo != "vision" {
		t.Fatalf("re-review should be assigned back to vision, got %q", reReviewTask.AssignedTo)
	}
}

// TestV10_RetryRouting_PM_Thinker verifies pm catch-all routes to thinker.
func TestV10_RetryRouting_PM_Thinker(t *testing.T) {
	mockOC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"ok": true}) //nolint:errcheck
	}))
	defer mockOC.Close()

	oc := openclaw.NewWithURL(mockOC.URL, "")
	srv := newTestServer(t, oc)
	defer srv.Close()

	// pm task fails → catch-all → thinker.
	var task model.Task
	r := postJSON(t, srv, "/tasks", map[string]any{"title": "pm-task", "assigned_to": "pm"})
	json.NewDecoder(r.Body).Decode(&task) //nolint:errcheck
	r.Body.Close()

	patchTaskToFailed(t, srv, task.ID, task.Version, "pm", "需求不清晰")
	time.Sleep(200 * time.Millisecond)

	// pm is NOT in isReviewReject list → standard single retry.
	listResp := getJSON(t, srv, "/tasks?assigned_to=thinker")
	var listBody struct {
		Tasks []model.Task `json:"tasks"`
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
		t.Fatal("pm catch-all should create a retry task for thinker")
	}
	if retryTask.AssignedTo != "thinker" {
		t.Fatalf("retry task should be assigned to thinker, got %q", retryTask.AssignedTo)
	}
}

// TestV10_RetryRouting_Ops_Devops verifies ops catch-all routes to devops.
func TestV10_RetryRouting_Ops_Devops(t *testing.T) {
	mockOC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"ok": true}) //nolint:errcheck
	}))
	defer mockOC.Close()

	oc := openclaw.NewWithURL(mockOC.URL, "")
	srv := newTestServer(t, oc)
	defer srv.Close()

	// ops task fails → catch-all → devops.
	var task model.Task
	r := postJSON(t, srv, "/tasks", map[string]any{"title": "ops-task", "assigned_to": "ops"})
	json.NewDecoder(r.Body).Decode(&task) //nolint:errcheck
	r.Body.Close()

	patchTaskToFailed(t, srv, task.ID, task.Version, "ops", "部署失败")
	time.Sleep(200 * time.Millisecond)

	// ops is NOT in isReviewReject list → standard single retry.
	listResp := getJSON(t, srv, "/tasks?assigned_to=devops")
	var listBody struct {
		Tasks []model.Task `json:"tasks"`
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
		t.Fatal("ops catch-all should create a retry task for devops")
	}
	if retryTask.AssignedTo != "devops" {
		t.Fatalf("retry task should be assigned to devops, got %q", retryTask.AssignedTo)
	}
}

// TestV10_VisionReviewReject_TwoStageChain verifies that vision as a reviewer
// triggers isReviewReject two-stage chain (fix + re-review) when retry goes to
// a different agent.
func TestV10_VisionReviewReject_TwoStageChain(t *testing.T) {
	mockOC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"ok": true}) //nolint:errcheck
	}))
	defer mockOC.Close()

	oc := openclaw.NewWithURL(mockOC.URL, "")
	srv := newTestServer(t, oc)
	defer srv.Close()

	// Create chain: coder → vision(review) → qa
	chainResp := postJSON(t, srv, "/dispatch/chain", map[string]any{
		"tasks": []map[string]any{
			{"title": "impl-feature", "assigned_to": "coder"},
			{"title": "visual-review", "assigned_to": "vision"},
			{"title": "final-qa", "assigned_to": "qa"},
		},
	})
	if chainResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", chainResp.StatusCode)
	}
	var cr model.ChainResponse
	json.NewDecoder(chainResp.Body).Decode(&cr) //nolint:errcheck
	chainResp.Body.Close()

	implTask := cr.Tasks[0]
	reviewTask := cr.Tasks[1]
	qaTask := cr.Tasks[2]

	// Drive impl to done.
	driveTaskToDone(t, srv, implTask.ID, implTask.Version, "coder")
	time.Sleep(50 * time.Millisecond)

	// vision rejects → retry to coder (vision != coder → isReviewReject).
	patchTaskToFailed(t, srv, reviewTask.ID, reviewTask.Version, "vision",
		"UI实现有偏差 | retry_assigned_to: coder")
	time.Sleep(200 * time.Millisecond)

	// Find fix and re-review tasks.
	listResp := getJSON(t, srv, "/tasks")
	var listBody struct {
		Tasks []model.Task `json:"tasks"`
	}
	json.NewDecoder(listResp.Body).Decode(&listBody) //nolint:errcheck
	listResp.Body.Close()

	var fixTask, reReviewTask *model.Task
	for i := range listBody.Tasks {
		t2 := &listBody.Tasks[i]
		if strings.HasPrefix(t2.Title, "fix:") {
			fixTask = t2
		}
		if strings.HasPrefix(t2.Title, "re-review:") {
			reReviewTask = t2
		}
	}

	// Verify two tasks created.
	if fixTask == nil {
		t.Fatal("vision review-reject should create a fix task")
	}
	if reReviewTask == nil {
		t.Fatal("vision review-reject should create a re-review task")
	}

	// fix → coder, re-review → vision.
	if fixTask.AssignedTo != "coder" {
		t.Fatalf("fix task should be assigned to coder, got %q", fixTask.AssignedTo)
	}
	if reReviewTask.AssignedTo != "vision" {
		t.Fatalf("re-review should be assigned back to vision, got %q", reReviewTask.AssignedTo)
	}

	// Verify original review's superseded_by → re-review (not fix).
	reviewDetail := getJSON(t, srv, "/tasks/"+reviewTask.ID)
	var rvDetail model.Task
	json.NewDecoder(reviewDetail.Body).Decode(&rvDetail) //nolint:errcheck
	reviewDetail.Body.Close()
	if rvDetail.SupersededBy != reReviewTask.ID {
		t.Fatalf("original review.superseded_by should point to re-review %q, got %q",
			reReviewTask.ID, rvDetail.SupersededBy)
	}

	// qa should NOT be deps_met yet.
	depsResp := getJSON(t, srv, "/tasks/"+qaTask.ID+"/deps-met")
	var dm model.DepsMet
	json.NewDecoder(depsResp.Body).Decode(&dm) //nolint:errcheck
	depsResp.Body.Close()
	if dm.DepsMet {
		t.Fatal("qa should NOT be deps_met while vision re-review is pending")
	}

	// Drive fix → done, then re-review → done.
	driveTaskToDone(t, srv, fixTask.ID, fixTask.Version, "coder")
	time.Sleep(100 * time.Millisecond)

	rrLatest := getJSON(t, srv, "/tasks/"+reReviewTask.ID)
	var rrDetail model.Task
	json.NewDecoder(rrLatest.Body).Decode(&rrDetail) //nolint:errcheck
	rrLatest.Body.Close()

	driveTaskToDone(t, srv, reReviewTask.ID, rrDetail.Version, "vision")
	time.Sleep(100 * time.Millisecond)

	// NOW qa should be deps_met.
	depsResp2 := getJSON(t, srv, "/tasks/"+qaTask.ID+"/deps-met")
	var dm2 model.DepsMet
	json.NewDecoder(depsResp2.Body).Decode(&dm2) //nolint:errcheck
	depsResp2.Body.Close()
	if !dm2.DepsMet {
		t.Fatal("qa should be deps_met after vision re-review is done")
	}
}

// -------------------------------------------------------------------
// Cancelled state tests
// -------------------------------------------------------------------

// TestCancelled_FromFailed verifies failed→cancelled transition succeeds.
func TestCancelled_FromFailed(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	// Create + drive task to failed.
	r := postJSON(t, srv, "/tasks", map[string]any{"title": "cancel-me-failed", "assigned_to": "coder"})
	var task model.Task
	json.NewDecoder(r.Body).Decode(&task) //nolint:errcheck
	r.Body.Close()

	claimR := postJSON(t, srv, "/tasks/"+task.ID+"/claim", map[string]any{"version": task.Version, "agent": "coder"})
	var claimed model.Task
	json.NewDecoder(claimR.Body).Decode(&claimed) //nolint:errcheck
	claimR.Body.Close()

	ipTask := patchTaskTo(t, srv, task.ID, "in_progress", claimed.Version)

	body, _ := json.Marshal(map[string]any{"status": "failed", "result": "broken", "version": ipTask.Version})
	req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/tasks/"+task.ID, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	var pr struct{ Task model.Task }
	json.NewDecoder(resp.Body).Decode(&pr) //nolint:errcheck
	resp.Body.Close()
	failedTask := pr.Task

	if failedTask.Status != "failed" {
		t.Fatalf("expected failed, got %s", failedTask.Status)
	}

	// Now cancel it.
	cancelledTask := patchTaskTo(t, srv, task.ID, "cancelled", failedTask.Version)
	if cancelledTask.Status != "cancelled" {
		t.Fatalf("expected cancelled, got %s", cancelledTask.Status)
	}
}

// TestCancelled_FromPending verifies pending→cancelled transition succeeds.
func TestCancelled_FromPending(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	r := postJSON(t, srv, "/tasks", map[string]any{"title": "cancel-pending", "assigned_to": "coder"})
	var task model.Task
	json.NewDecoder(r.Body).Decode(&task) //nolint:errcheck
	r.Body.Close()

	cancelledTask := patchTaskTo(t, srv, task.ID, "cancelled", task.Version)
	if cancelledTask.Status != "cancelled" {
		t.Fatalf("expected cancelled, got %s", cancelledTask.Status)
	}
}

// TestCancelled_FromDone_Fails verifies done→cancelled is rejected (422).
func TestCancelled_FromDone_Fails(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	r := postJSON(t, srv, "/tasks", map[string]any{"title": "done-task-no-cancel", "assigned_to": "coder"})
	var task model.Task
	json.NewDecoder(r.Body).Decode(&task) //nolint:errcheck
	r.Body.Close()

	claimR := postJSON(t, srv, "/tasks/"+task.ID+"/claim", map[string]any{"version": task.Version, "agent": "coder"})
	var claimed model.Task
	json.NewDecoder(claimR.Body).Decode(&claimed) //nolint:errcheck
	claimR.Body.Close()

	ipTask := patchTaskTo(t, srv, task.ID, "in_progress", claimed.Version)
	doneTask := patchTaskTo(t, srv, task.ID, "done", ipTask.Version)
	if doneTask.Status != "done" {
		t.Fatalf("expected done, got %s", doneTask.Status)
	}

	// Attempt cancel from done – should fail.
	body, _ := json.Marshal(map[string]any{"status": "cancelled", "version": doneTask.Version})
	req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/tasks/"+task.ID, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d", resp.StatusCode)
	}
}

// TestCancelled_NoAutoRetry verifies that cancelling a task does NOT trigger autoRetry.
func TestCancelled_NoAutoRetry(t *testing.T) {
	var mu sync.Mutex
	var received []string
	mockOC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		if args, ok := body["args"].(map[string]any); ok {
			if msg, ok := args["message"].(string); ok {
				mu.Lock()
				received = append(received, msg)
				mu.Unlock()
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true}) //nolint:errcheck
	}))
	defer mockOC.Close()

	oc := openclaw.NewWithURL(mockOC.URL, "")
	srv := newTestServer(t, oc)
	defer srv.Close()

	// Create task and drive to failed (which normally triggers autoRetry for coder→thinker via routing table).
	r := postJSON(t, srv, "/tasks", map[string]any{"title": "no-retry-on-cancel", "assigned_to": "coder"})
	var task model.Task
	json.NewDecoder(r.Body).Decode(&task) //nolint:errcheck
	r.Body.Close()

	claimR := postJSON(t, srv, "/tasks/"+task.ID+"/claim", map[string]any{"version": task.Version, "agent": "coder"})
	var claimed model.Task
	json.NewDecoder(claimR.Body).Decode(&claimed) //nolint:errcheck
	claimR.Body.Close()

	ipTask := patchTaskTo(t, srv, task.ID, "in_progress", claimed.Version)

	// Cancel directly from in_progress... but FSM doesn't allow that. Go pending→cancelled instead.
	// Reset to pending first, then cancel.
	pendingTask := patchTaskTo(t, srv, task.ID, "pending", ipTask.Version)
	cancelledTask := patchTaskTo(t, srv, task.ID, "cancelled", pendingTask.Version)
	if cancelledTask.Status != "cancelled" {
		t.Fatalf("expected cancelled, got %s", cancelledTask.Status)
	}

	time.Sleep(200 * time.Millisecond)

	// Check no tasks were auto-created (no retry task for coder).
	listResp := getJSON(t, srv, "/tasks?assigned_to=coder&status=pending")
	var listResult struct {
		Tasks []model.Task `json:"tasks"`
	}
	json.NewDecoder(listResp.Body).Decode(&listResult) //nolint:errcheck
	listResp.Body.Close()
	for _, t2 := range listResult.Tasks {
		if strings.Contains(t2.Title, "retry") {
			t.Errorf("unexpected retry task created after cancel: %s", t2.Title)
		}
	}
}

// TestCancelled_NoDownstreamUnlock verifies that cancelling a task does NOT unlock downstream deps.
func TestCancelled_NoDownstreamUnlock(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	// Create parent task.
	r := postJSON(t, srv, "/tasks", map[string]any{"title": "parent-task", "assigned_to": "coder"})
	var parent model.Task
	json.NewDecoder(r.Body).Decode(&parent) //nolint:errcheck
	r.Body.Close()

	// Create downstream task depending on parent.
	r2 := postJSON(t, srv, "/tasks", map[string]any{
		"title":       "downstream-task",
		"assigned_to": "qa",
		"depends_on":  []string{parent.ID},
	})
	var downstream model.Task
	json.NewDecoder(r2.Body).Decode(&downstream) //nolint:errcheck
	r2.Body.Close()

	// Cancel parent.
	cancelledParent := patchTaskTo(t, srv, parent.ID, "cancelled", parent.Version)
	if cancelledParent.Status != "cancelled" {
		t.Fatalf("expected parent cancelled, got %s", cancelledParent.Status)
	}

	time.Sleep(100 * time.Millisecond)

	// Downstream should still be pending (deps NOT met, since parent is cancelled not done).
	depsResp := getJSON(t, srv, "/tasks/"+downstream.ID+"/deps-met")
	var dm model.DepsMet
	json.NewDecoder(depsResp.Body).Decode(&dm) //nolint:errcheck
	depsResp.Body.Close()
	if dm.DepsMet {
		t.Fatal("downstream should NOT be deps_met after parent is cancelled (not done)")
	}
}

// -------------------------------------------------------------------
// V11: stale ticker max_dispatches
// -------------------------------------------------------------------

// TestV11_StaleTicker_MaxDispatches verifies that when stale_dispatch_count
// reaches maxStaleDispatches, the CEO is alerted instead of re-dispatching.
func TestV11_StaleTicker_MaxDispatches(t *testing.T) {
	var mu sync.Mutex
	var ceoMessages []string

	mockOC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		if args, ok := body["args"].(map[string]any); ok {
			if msg, ok := args["message"].(string); ok {
				mu.Lock()
				ceoMessages = append(ceoMessages, msg)
				mu.Unlock()
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true}) //nolint:errcheck
	}))
	defer mockOC.Close()

	oc := openclaw.NewWithURL(mockOC.URL, "")
	srv, h := newTestServerWithHandler(t, oc)
	defer srv.Close()

	// Set max to 2 for fast testing.
	h.SetMaxStaleDispatchesForTesting(2)
	h.SetStaleThresholdForTesting(50 * time.Millisecond)

	// Create a task with e2e-coder (not real agent → no session_send).
	r := postJSON(t, srv, "/tasks", map[string]any{"title": "stale-maxdisp", "assigned_to": "e2e-coder"})
	var task model.Task
	json.NewDecoder(r.Body).Decode(&task) //nolint:errcheck
	r.Body.Close()

	// Manually set stale_dispatch_count = 2 (at limit) by running ticker twice normally.
	// Simpler: directly manipulate via two CheckStaleTasks calls after threshold passes.
	time.Sleep(100 * time.Millisecond)

	// First tick: dispatch count goes 0→1 (below limit)
	h.CheckStaleTasks()
	time.Sleep(20 * time.Millisecond)

	// Second tick: dispatch count goes 1→2 (now AT limit)
	h.CheckStaleTasks()
	time.Sleep(20 * time.Millisecond)

	// Third tick: count is 2 = maxStaleDispatches → should alert CEO instead of dispatching
	h.CheckStaleTasks()
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	snap := make([]string, len(ceoMessages))
	copy(snap, ceoMessages)
	mu.Unlock()

	// Should have received a CEO alert containing "stale" or "❌"
	found := false
	for _, msg := range snap {
		if strings.Contains(msg, "stale-maxdisp") || strings.Contains(msg, "❌") {
			found = true
		}
	}
	if !found {
		t.Errorf("CEO should have been alerted for stale max dispatches, got: %v", snap)
	}
}

// TestV11_StaleTicker_BelowMax_NoCEOAlert verifies that below max dispatches,
// the CEO is not alerted (normal re-dispatch).
func TestV11_StaleTicker_BelowMax_NoCEOAlert(t *testing.T) {
	var mu sync.Mutex
	var ceoMessages []string

	mockOC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		if args, ok := body["args"].(map[string]any); ok {
			if msg, ok := args["message"].(string); ok {
				mu.Lock()
				ceoMessages = append(ceoMessages, msg)
				mu.Unlock()
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true}) //nolint:errcheck
	}))
	defer mockOC.Close()

	oc := openclaw.NewWithURL(mockOC.URL, "")
	srv, h := newTestServerWithHandler(t, oc)
	defer srv.Close()

	h.SetMaxStaleDispatchesForTesting(5) // high limit
	h.SetStaleThresholdForTesting(50 * time.Millisecond)

	r := postJSON(t, srv, "/tasks", map[string]any{"title": "stale-below-max", "assigned_to": "e2e-coder"})
	var task model.Task
	json.NewDecoder(r.Body).Decode(&task) //nolint:errcheck
	r.Body.Close()

	time.Sleep(100 * time.Millisecond)
	h.CheckStaleTasks()
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	snap := make([]string, len(ceoMessages))
	copy(snap, ceoMessages)
	mu.Unlock()

	for _, msg := range snap {
		if strings.Contains(msg, "stale-below-max") {
			t.Errorf("CEO should NOT be alerted for below-max stale dispatch, got: %v", snap)
		}
	}
}

// TestOnTaskComplete_SingleTask_NotifiesCEO verifies that when a single task
// (no chain_id) with notify_ceo_on_complete=true is patched to done,
// OnTaskComplete sends a CEO notification.
func TestOnTaskComplete_SingleTask_NotifiesCEO(t *testing.T) {
	var mu sync.Mutex
	var ceoMessages []string
	mockOC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		if args, ok := body["args"].(map[string]any); ok {
			if msg, ok := args["message"].(string); ok {
				mu.Lock()
				ceoMessages = append(ceoMessages, msg)
				mu.Unlock()
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true}) //nolint:errcheck
	}))
	defer mockOC.Close()

	oc := openclaw.NewWithURL(mockOC.URL, "")
	srv := newTestServer(t, oc)
	defer srv.Close()

	// Create a single task (no chain) with notify_ceo_on_complete=true.
	r := postJSON(t, srv, "/tasks", map[string]any{
		"title":                 "single-notify-task",
		"assigned_to":           "coder",
		"notify_ceo_on_complete": true,
	})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", r.StatusCode)
	}
	var task model.Task
	json.NewDecoder(r.Body).Decode(&task) //nolint:errcheck
	r.Body.Close()

	// Drive to done.
	claimR := postJSON(t, srv, "/tasks/"+task.ID+"/claim",
		map[string]any{"version": task.Version, "agent": "coder"})
	var claimed model.Task
	json.NewDecoder(claimR.Body).Decode(&claimed) //nolint:errcheck
	claimR.Body.Close()
	ip := patchTaskTo(t, srv, task.ID, "in_progress", claimed.Version)
	patchTaskTo(t, srv, task.ID, "done", ip.Version)

	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	snap := make([]string, len(ceoMessages))
	copy(snap, ceoMessages)
	mu.Unlock()

	found := false
	for _, msg := range snap {
		if strings.Contains(msg, "single-notify-task") && strings.Contains(msg, "✅") {
			found = true
		}
	}
	if !found {
		t.Errorf("CEO should have received OnTaskComplete notification, got: %v", snap)
	}
}

// TestOnTaskComplete_SingleTask_NoChainNoNotify verifies that a single task
// without notify_ceo_on_complete does NOT notify CEO on done.
func TestOnTaskComplete_SingleTask_NoChainNoNotify(t *testing.T) {
	var mu sync.Mutex
	var ceoMessages []string
	mockOC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		if args, ok := body["args"].(map[string]any); ok {
			if msg, ok := args["message"].(string); ok {
				mu.Lock()
				ceoMessages = append(ceoMessages, msg)
				mu.Unlock()
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true}) //nolint:errcheck
	}))
	defer mockOC.Close()

	oc := openclaw.NewWithURL(mockOC.URL, "")
	srv := newTestServer(t, oc)
	defer srv.Close()

	// Create single task WITHOUT notify_ceo_on_complete.
	r := postJSON(t, srv, "/tasks", map[string]any{
		"title":       "no-notify-single-task",
		"assigned_to": "coder",
	})
	var task model.Task
	json.NewDecoder(r.Body).Decode(&task) //nolint:errcheck
	r.Body.Close()

	claimR := postJSON(t, srv, "/tasks/"+task.ID+"/claim",
		map[string]any{"version": task.Version, "agent": "coder"})
	var claimed model.Task
	json.NewDecoder(claimR.Body).Decode(&claimed) //nolint:errcheck
	claimR.Body.Close()
	ip := patchTaskTo(t, srv, task.ID, "in_progress", claimed.Version)
	patchTaskTo(t, srv, task.ID, "done", ip.Version)

	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	snap := make([]string, len(ceoMessages))
	copy(snap, ceoMessages)
	mu.Unlock()

	for _, msg := range snap {
		if strings.Contains(msg, "no-notify-single-task") {
			t.Errorf("CEO should NOT be notified for task without notify_ceo_on_complete, got: %v", snap)
		}
	}
}

// ---------------------------------------------------------------------------
// V13: autoAdvance tests
// ---------------------------------------------------------------------------

// TestV13_AutoAdvance_NoField verifies that when auto_advance_to is empty,
// no downstream advance task is created on PATCH done.
func TestV13_AutoAdvance_NoField(t *testing.T) {
	mockOC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"ok": true}) //nolint:errcheck
	}))
	defer mockOC.Close()
	oc := openclaw.NewWithURL(mockOC.URL, "")
	srv := newTestServer(t, oc)
	defer srv.Close()

	// Create task without auto_advance_to
	dispR := postJSON(t, srv, "/dispatch", map[string]any{
		"title":       "v13-no-advance",
		"assigned_to": "coder",
	})
	if dispR.StatusCode != http.StatusCreated {
		t.Fatalf("dispatch: want 201, got %d", dispR.StatusCode)
	}
	var dispResp struct{ Task model.Task }
	json.NewDecoder(dispR.Body).Decode(&dispResp)
	dispR.Body.Close()
	task := dispResp.Task

	// claim → in_progress → done
	claimR := postJSON(t, srv, "/tasks/"+task.ID+"/claim",
		map[string]any{"version": task.Version, "agent": "coder"})
	var claimed model.Task
	json.NewDecoder(claimR.Body).Decode(&claimed)
	claimR.Body.Close()

	ip := patchTaskTo(t, srv, task.ID, "in_progress", claimed.Version)
	patchTaskTo(t, srv, task.ID, "done", ip.Version)

	time.Sleep(200 * time.Millisecond)

	// No advance tasks should exist
	listR := getJSON(t, srv, "/tasks?assigned_to=coder")
	var listResp struct{ Tasks []model.Task }
	json.NewDecoder(listR.Body).Decode(&listResp)
	listR.Body.Close()
	for _, tt := range listResp.Tasks {
		if strings.HasPrefix(tt.Title, "advance:") {
			t.Errorf("unexpected advance task: %q", tt.Title)
		}
	}
}

// TestV13_AutoAdvance_CreatesNextTask verifies that when auto_advance_to is set,
// PATCH done creates and dispatches a new task to the target agent with upstream
// result injected into description.
func TestV13_AutoAdvance_CreatesNextTask(t *testing.T) {
	mockOC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"ok": true}) //nolint:errcheck
	}))
	defer mockOC.Close()
	oc := openclaw.NewWithURL(mockOC.URL, "")
	srv := newTestServer(t, oc)
	defer srv.Close()

	const advanceAgent = "qa"
	const advanceTitle = "QA验证阶段"
	const advanceDesc = "请验证coder的实现"
	const upstreamResult = "实现完成：commit abc123"

	// Create task with auto_advance_to
	dispR := postJSON(t, srv, "/dispatch", map[string]any{
		"title":                    "v13-advance-source",
		"assigned_to":              "coder",
		"auto_advance_to":          advanceAgent,
		"advance_task_title":       advanceTitle,
		"advance_task_description": advanceDesc,
	})
	if dispR.StatusCode != http.StatusCreated {
		t.Fatalf("dispatch: want 201, got %d", dispR.StatusCode)
	}
	var dispResp struct{ Task model.Task }
	json.NewDecoder(dispR.Body).Decode(&dispResp)
	dispR.Body.Close()
	task := dispResp.Task

	// Verify auto_advance_to was stored correctly
	if task.AutoAdvanceTo != advanceAgent {
		t.Fatalf("auto_advance_to: want %q, got %q", advanceAgent, task.AutoAdvanceTo)
	}

	// claim → in_progress → done (with result)
	claimR := postJSON(t, srv, "/tasks/"+task.ID+"/claim",
		map[string]any{"version": task.Version, "agent": "coder"})
	var claimed model.Task
	json.NewDecoder(claimR.Body).Decode(&claimed)
	claimR.Body.Close()

	ip := patchTaskTo(t, srv, task.ID, "in_progress", claimed.Version)

	// PATCH done with result
	doneBody, _ := json.Marshal(map[string]any{
		"status":  "done",
		"result":  upstreamResult,
		"version": ip.Version,
	})
	doneReq, _ := http.NewRequest(http.MethodPatch, srv.URL+"/tasks/"+task.ID, bytes.NewReader(doneBody))
	doneReq.Header.Set("Content-Type", "application/json")
	doneR, err := http.DefaultClient.Do(doneReq)
	if err != nil {
		t.Fatalf("PATCH done: %v", err)
	}
	doneR.Body.Close()

	// Wait for async autoAdvance goroutine
	time.Sleep(400 * time.Millisecond)

	// Find the advance task for qa
	listR := getJSON(t, srv, "/tasks?assigned_to="+advanceAgent)
	var listResp struct{ Tasks []model.Task }
	json.NewDecoder(listR.Body).Decode(&listResp)
	listR.Body.Close()

	var found *model.Task
	for i := range listResp.Tasks {
		if listResp.Tasks[i].Title == advanceTitle {
			found = &listResp.Tasks[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("advance task %q not found in tasks: %v", advanceTitle, listResp.Tasks)
	}

	// Verify assigned_to
	if found.AssignedTo != advanceAgent {
		t.Errorf("advance task assigned_to=%q, want %q", found.AssignedTo, advanceAgent)
	}

	// Verify description contains upstream result and advance_task_description
	if !strings.Contains(found.Description, upstreamResult) {
		t.Errorf("description missing upstream result; desc=%q", found.Description)
	}
	if !strings.Contains(found.Description, advanceDesc) {
		t.Errorf("description missing advance_task_description; desc=%q", found.Description)
	}
	if !strings.Contains(found.Description, "前置结果：") {
		t.Errorf("description missing 前置结果 prefix; desc=%q", found.Description)
	}
}

// ---------------------------------------------------------------------------
// V14: resultRouting tests
// ---------------------------------------------------------------------------

// TestV14_ResultRouting_PlainText verifies that plain-text result does not trigger routing.
func TestV14_ResultRouting_PlainText(t *testing.T) {
	mockOC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"ok": true}) //nolint:errcheck
	}))
	defer mockOC.Close()
	oc := openclaw.NewWithURL(mockOC.URL, "")
	srv := newTestServer(t, oc)
	defer srv.Close()

	dispR := postJSON(t, srv, "/dispatch", map[string]any{
		"title":       "v14-plain-text",
		"assigned_to": "coder",
	})
	var dispResp struct{ Task model.Task }
	json.NewDecoder(dispR.Body).Decode(&dispResp)
	dispR.Body.Close()
	task := dispResp.Task

	claimR := postJSON(t, srv, "/tasks/"+task.ID+"/claim",
		map[string]any{"version": task.Version, "agent": "coder"})
	var claimed model.Task
	json.NewDecoder(claimR.Body).Decode(&claimed)
	claimR.Body.Close()
	ip := patchTaskTo(t, srv, task.ID, "in_progress", claimed.Version)

	// PATCH done with plain-text result
	body, _ := json.Marshal(map[string]any{
		"status":  "done",
		"result":  "完成了，无需路由",
		"version": ip.Version,
	})
	req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/tasks/"+task.ID, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r, _ := http.DefaultClient.Do(req)
	r.Body.Close()

	time.Sleep(200 * time.Millisecond)

	// No result-route tasks should exist
	listR := getJSON(t, srv, "/tasks")
	var listResp struct{ Tasks []model.Task }
	json.NewDecoder(listR.Body).Decode(&listResp)
	listR.Body.Close()
	for _, tt := range listResp.Tasks {
		if strings.HasPrefix(tt.Title, "result-route:") {
			t.Errorf("unexpected result-route task: %q", tt.Title)
		}
	}
}

// TestV14_ResultRouting_InvalidJSON verifies that invalid JSON in result does not trigger routing.
func TestV14_ResultRouting_InvalidJSON(t *testing.T) {
	mockOC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"ok": true}) //nolint:errcheck
	}))
	defer mockOC.Close()
	oc := openclaw.NewWithURL(mockOC.URL, "")
	srv := newTestServer(t, oc)
	defer srv.Close()

	dispR := postJSON(t, srv, "/dispatch", map[string]any{
		"title":       "v14-invalid-json",
		"assigned_to": "coder",
	})
	var dispResp struct{ Task model.Task }
	json.NewDecoder(dispR.Body).Decode(&dispResp)
	dispR.Body.Close()
	task := dispResp.Task

	claimR := postJSON(t, srv, "/tasks/"+task.ID+"/claim",
		map[string]any{"version": task.Version, "agent": "coder"})
	var claimed model.Task
	json.NewDecoder(claimR.Body).Decode(&claimed)
	claimR.Body.Close()
	ip := patchTaskTo(t, srv, task.ID, "in_progress", claimed.Version)

	body, _ := json.Marshal(map[string]any{
		"status":  "done",
		"result":  `{"broken json`,
		"version": ip.Version,
	})
	req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/tasks/"+task.ID, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r, _ := http.DefaultClient.Do(req)
	r.Body.Close()

	time.Sleep(200 * time.Millisecond)

	listR := getJSON(t, srv, "/tasks")
	var listResp struct{ Tasks []model.Task }
	json.NewDecoder(listR.Body).Decode(&listResp)
	listR.Body.Close()
	for _, tt := range listResp.Tasks {
		if strings.HasPrefix(tt.Title, "result-route:") {
			t.Errorf("unexpected result-route task: %q", tt.Title)
		}
	}
}

// TestV14_ResultRouting_NextAgent verifies that JSON result with next_agent triggers routing.
func TestV14_ResultRouting_NextAgent(t *testing.T) {
	mockOC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"ok": true}) //nolint:errcheck
	}))
	defer mockOC.Close()
	oc := openclaw.NewWithURL(mockOC.URL, "")
	srv := newTestServer(t, oc)
	defer srv.Close()

	const nextAgent = "thinker"
	const nextTitle = "架构审核"
	const nextDesc = "请审核实现方案"
	resultJSON := `{"next_agent":"thinker","next_title":"架构审核","next_description":"请审核实现方案","summary":"coder完成了实现"}`

	dispR := postJSON(t, srv, "/dispatch", map[string]any{
		"title":       "v14-result-route-source",
		"assigned_to": "coder",
	})
	var dispResp struct{ Task model.Task }
	json.NewDecoder(dispR.Body).Decode(&dispResp)
	dispR.Body.Close()
	task := dispResp.Task

	claimR := postJSON(t, srv, "/tasks/"+task.ID+"/claim",
		map[string]any{"version": task.Version, "agent": "coder"})
	var claimed model.Task
	json.NewDecoder(claimR.Body).Decode(&claimed)
	claimR.Body.Close()
	ip := patchTaskTo(t, srv, task.ID, "in_progress", claimed.Version)

	body, _ := json.Marshal(map[string]any{
		"status":  "done",
		"result":  resultJSON,
		"version": ip.Version,
	})
	req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/tasks/"+task.ID, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r, _ := http.DefaultClient.Do(req)
	r.Body.Close()

	time.Sleep(400 * time.Millisecond)

	// Find the routed task
	listR := getJSON(t, srv, "/tasks?assigned_to="+nextAgent)
	var listResp struct{ Tasks []model.Task }
	json.NewDecoder(listR.Body).Decode(&listResp)
	listR.Body.Close()

	var found *model.Task
	for i := range listResp.Tasks {
		if listResp.Tasks[i].Title == nextTitle {
			found = &listResp.Tasks[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("result-route task %q not found; tasks=%v", nextTitle, listResp.Tasks)
	}
	if found.AssignedTo != nextAgent {
		t.Errorf("assigned_to=%q, want %q", found.AssignedTo, nextAgent)
	}
	if !strings.Contains(found.Description, resultJSON) {
		t.Errorf("description missing upstream result; desc=%q", found.Description)
	}
	if !strings.Contains(found.Description, nextDesc) {
		t.Errorf("description missing next_description; desc=%q", found.Description)
	}
	if !strings.Contains(found.Description, "前置结果：") {
		t.Errorf("description missing 前置结果 prefix; desc=%q", found.Description)
	}
}

// TestV14_ResultRouting_AutoAdvancePriority verifies that autoAdvance takes priority
// over result routing when auto_advance_to is set.
func TestV14_ResultRouting_AutoAdvancePriority(t *testing.T) {
	mockOC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"ok": true}) //nolint:errcheck
	}))
	defer mockOC.Close()
	oc := openclaw.NewWithURL(mockOC.URL, "")
	srv := newTestServer(t, oc)
	defer srv.Close()

	// Task with both auto_advance_to and JSON result with next_agent
	dispR := postJSON(t, srv, "/dispatch", map[string]any{
		"title":              "v14-priority-test",
		"assigned_to":        "coder",
		"auto_advance_to":    "qa",
		"advance_task_title": "QA阶段",
	})
	var dispResp struct{ Task model.Task }
	json.NewDecoder(dispR.Body).Decode(&dispResp)
	dispR.Body.Close()
	task := dispResp.Task

	claimR := postJSON(t, srv, "/tasks/"+task.ID+"/claim",
		map[string]any{"version": task.Version, "agent": "coder"})
	var claimed model.Task
	json.NewDecoder(claimR.Body).Decode(&claimed)
	claimR.Body.Close()
	ip := patchTaskTo(t, srv, task.ID, "in_progress", claimed.Version)

	// Result also contains next_agent (should be ignored)
	body, _ := json.Marshal(map[string]any{
		"status":  "done",
		"result":  `{"next_agent":"thinker","next_title":"不应该被创建"}`,
		"version": ip.Version,
	})
	req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/tasks/"+task.ID, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r, _ := http.DefaultClient.Do(req)
	r.Body.Close()

	time.Sleep(400 * time.Millisecond)

	// Only the autoAdvance task (QA阶段) should exist, NOT the result-route task
	listR := getJSON(t, srv, "/tasks")
	var listResp struct{ Tasks []model.Task }
	json.NewDecoder(listR.Body).Decode(&listResp)
	listR.Body.Close()

	foundAdvance := false
	for _, tt := range listResp.Tasks {
		if tt.Title == "QA阶段" {
			foundAdvance = true
		}
		if tt.Title == "不应该被创建" || strings.HasPrefix(tt.Title, "result-route:") {
			t.Errorf("result-route task should NOT be created when auto_advance_to is set; found: %q", tt.Title)
		}
	}
	if !foundAdvance {
		t.Errorf("autoAdvance task 'QA阶段' not found; tasks=%v", listResp.Tasks)
	}
}
