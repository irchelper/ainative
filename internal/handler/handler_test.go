package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

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
