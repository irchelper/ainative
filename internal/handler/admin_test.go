package handler_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// deleteReq sends a DELETE request to the test server.
func deleteReq(t *testing.T, srv *httptest.Server, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, srv.URL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE %s: %v", path, err)
	}
	return resp
}

// cleanupResp is the JSON response from cleanup-test-tasks.
type cleanupResp struct {
	DeletedCount int    `json:"deleted_count"`
	MaxAge       string `json:"max_age"`
	Error        string `json:"error,omitempty"`
}

func decodeCleanup(t *testing.T, resp *http.Response) cleanupResp {
	t.Helper()
	defer resp.Body.Close()
	var cr cleanupResp
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		t.Fatalf("decode cleanup resp: %v", err)
	}
	return cr
}

// --- Tests ---

func TestCleanup_MethodNotAllowed(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	resp := getJSON(t, srv, "/api/admin/cleanup-test-tasks")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET should be 405, got %d", resp.StatusCode)
	}
}

func TestCleanup_InvalidMaxAge(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	resp := deleteReq(t, srv, "/api/admin/cleanup-test-tasks?max_age=abc")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid max_age should be 400, got %d", resp.StatusCode)
	}
	cr := decodeCleanup(t, resp)
	if cr.Error == "" {
		t.Error("expected error message for invalid max_age")
	}
}

func TestCleanup_NegativeMaxAge(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	resp := deleteReq(t, srv, "/api/admin/cleanup-test-tasks?max_age=-1h")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("negative max_age should be 400, got %d", resp.StatusCode)
	}
	cr := decodeCleanup(t, resp)
	if cr.Error == "" {
		t.Error("expected error message for negative max_age")
	}
}

func TestCleanup_DefaultMaxAge_SkipsRecentTasks(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	// Create a [TEST] task and drive it to done (just completed, < 1h old).
	resp := postJSON(t, srv, "/tasks", map[string]any{
		"title":       "[TEST] recent task",
		"assigned_to": "e2e-qa",
	})
	defer resp.Body.Close()
	var created struct{ ID string `json:"id"` }
	json.NewDecoder(resp.Body).Decode(&created)

	// Drive to done.
	driveTaskToDone(t, srv, created.ID, 1, "e2e-qa")

	// Cleanup with default max_age=1h — recent task should NOT be deleted.
	delResp := deleteReq(t, srv, "/api/admin/cleanup-test-tasks")
	cr := decodeCleanup(t, delResp)
	if cr.DeletedCount != 0 {
		t.Errorf("expected 0 deleted (task is recent), got %d", cr.DeletedCount)
	}
	if cr.MaxAge != "1h0m0s" {
		t.Errorf("expected max_age=1h0m0s, got %s", cr.MaxAge)
	}

	// Verify task still exists.
	getResp := getJSON(t, srv, "/tasks/"+created.ID)
	defer getResp.Body.Close()
	if getResp.StatusCode != 200 {
		t.Error("task should still exist after cleanup with default max_age")
	}
}

func TestCleanup_ZeroMaxAge_DeletesAll(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	// Create a [TEST] task and drive it to done.
	resp := postJSON(t, srv, "/tasks", map[string]any{
		"title":       "[TEST] deletable task",
		"assigned_to": "e2e-qa",
	})
	defer resp.Body.Close()
	var created struct{ ID string `json:"id"` }
	json.NewDecoder(resp.Body).Decode(&created)

	driveTaskToDone(t, srv, created.ID, 1, "e2e-qa")

	// Cleanup with max_age=0 — should delete even brand-new tasks.
	delResp := deleteReq(t, srv, "/api/admin/cleanup-test-tasks?max_age=0")
	cr := decodeCleanup(t, delResp)
	if cr.DeletedCount < 1 {
		t.Errorf("expected >= 1 deleted with max_age=0, got %d", cr.DeletedCount)
	}
	if cr.MaxAge != "0s" {
		t.Errorf("expected max_age=0s, got %s", cr.MaxAge)
	}

	// Verify task is gone.
	getResp := getJSON(t, srv, "/tasks/"+created.ID)
	defer getResp.Body.Close()
	if getResp.StatusCode != 404 {
		t.Errorf("task should be deleted, got status %d", getResp.StatusCode)
	}
}

func TestCleanup_MatchRules(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	// Create test-matching tasks with various title/assignee patterns.
	testCases := []struct {
		title    string
		assignee string
		shouldDel bool
	}{
		{"[TEST] explicit prefix", "qa", true},           // [TEST] in title
		{"some [test] mid title", "qa", true},             // [test] case-insensitive mid
		{"normal task", "e2e-coder", true},                // e2e-* assignee
		{"normal task 2", "e2e-qa", true},                 // e2e-qa assignee
		{"[e2e] tagged", "coder", true},                   // [e2e] prefix
		{"production task", "coder", false},                // normal — should NOT be deleted
		{"production [important]", "qa", false},            // no test marker
	}

	ids := make([]string, len(testCases))
	for i, tc := range testCases {
		resp := postJSON(t, srv, "/tasks", map[string]any{
			"title":       tc.title,
			"assigned_to": tc.assignee,
		})
		var created struct{ ID string `json:"id"` }
		json.NewDecoder(resp.Body).Decode(&created)
		resp.Body.Close()
		ids[i] = created.ID

		// Drive to done (terminal state).
		driveTaskToDone(t, srv, ids[i], 1, tc.assignee)
	}

	// Cleanup with max_age=0.
	delResp := deleteReq(t, srv, "/api/admin/cleanup-test-tasks?max_age=0")
	cr := decodeCleanup(t, delResp)

	expectedDel := 0
	for _, tc := range testCases {
		if tc.shouldDel {
			expectedDel++
		}
	}
	if cr.DeletedCount != expectedDel {
		t.Errorf("expected %d deleted, got %d", expectedDel, cr.DeletedCount)
	}

	// Verify production tasks still exist.
	for i, tc := range testCases {
		getResp := getJSON(t, srv, "/tasks/"+ids[i])
		exists := getResp.StatusCode == 200
		getResp.Body.Close()

		if tc.shouldDel && exists {
			t.Errorf("task %q should be deleted but still exists", tc.title)
		}
		if !tc.shouldDel && !exists {
			t.Errorf("task %q should NOT be deleted but is gone", tc.title)
		}
	}
}

func TestCleanup_NonTerminalNotDeleted(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	// Create a [TEST] task but leave it in_progress (non-terminal).
	resp := postJSON(t, srv, "/tasks", map[string]any{
		"title":       "[TEST] in-progress task",
		"assigned_to": "e2e-qa",
	})
	defer resp.Body.Close()
	var created struct{ ID string `json:"id"` }
	json.NewDecoder(resp.Body).Decode(&created)

	postJSON(t, srv, fmt.Sprintf("/tasks/%s/claim", created.ID), map[string]any{
		"agent": "e2e-qa", "version": 1,
	}).Body.Close()
	// Leave as claimed (non-terminal) — don't drive to done.

	// Cleanup with max_age=0.
	delResp := deleteReq(t, srv, "/api/admin/cleanup-test-tasks?max_age=0")
	cr := decodeCleanup(t, delResp)
	if cr.DeletedCount != 0 {
		t.Errorf("expected 0 deleted (non-terminal), got %d", cr.DeletedCount)
	}

	// Verify task still exists.
	getResp := getJSON(t, srv, "/tasks/"+created.ID)
	defer getResp.Body.Close()
	if getResp.StatusCode != 200 {
		t.Error("non-terminal task should still exist")
	}
}

func TestCleanup_MaxAgeEcho(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	cases := []struct {
		param    string
		expected string
	}{
		{"", "1h0m0s"},        // default
		{"0", "0s"},           // zero
		{"30m", "30m0s"},      // 30 minutes
		{"2h", "2h0m0s"},      // 2 hours
	}
	for _, c := range cases {
		path := "/api/admin/cleanup-test-tasks"
		if c.param != "" {
			path += "?max_age=" + c.param
		}
		resp := deleteReq(t, srv, path)
		cr := decodeCleanup(t, resp)
		if cr.MaxAge != c.expected {
			t.Errorf("max_age=%q: expected echo %q, got %q", c.param, c.expected, cr.MaxAge)
		}
	}
}
