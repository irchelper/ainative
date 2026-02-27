package handler_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/irchelper/agent-queue/internal/model"
)

// ------------------------------------------------------------------
// POST /dispatch/graph tests
// ------------------------------------------------------------------

func TestDispatchGraph_Linear(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	body := `{
		"nodes": [
			{"key":"a","title":"Task A","assigned_to":"coder"},
			{"key":"b","title":"Task B","assigned_to":"qa"},
			{"key":"c","title":"Task C","assigned_to":"devops"}
		],
		"edges": [
			{"from":"a","to":"b"},
			{"from":"b","to":"c"}
		],
		"notify_ceo_on_complete": false
	}`
	resp, err := http.Post(srv.URL+"/dispatch/graph", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}

	var gr model.GraphResponse
	if err := json.NewDecoder(resp.Body).Decode(&gr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(gr.Tasks) != 3 {
		t.Fatalf("expected 3 tasks, got %d", len(gr.Tasks))
	}
	if len(gr.NodeIDMap) != 3 {
		t.Fatalf("expected 3 node_id_map entries, got %d", len(gr.NodeIDMap))
	}
	// Only root (a) should be in first_dispatched.
	if len(gr.FirstDispatched) != 1 {
		t.Fatalf("expected 1 first_dispatched, got %d: %v", len(gr.FirstDispatched), gr.FirstDispatched)
	}
	if gr.FirstDispatched[0] != gr.NodeIDMap["a"] {
		t.Errorf("first_dispatched should be node a's task ID")
	}
}

func TestDispatchGraph_Diamond(t *testing.T) {
	// Diamond: a → b, a → c, b → d, c → d
	srv := newTestServer(t, nil)
	defer srv.Close()

	body := `{
		"nodes": [
			{"key":"a","title":"Root","assigned_to":"thinker"},
			{"key":"b","title":"Branch B","assigned_to":"coder"},
			{"key":"c","title":"Branch C","assigned_to":"security"},
			{"key":"d","title":"Merge","assigned_to":"qa"}
		],
		"edges": [
			{"from":"a","to":"b"},
			{"from":"a","to":"c"},
			{"from":"b","to":"d"},
			{"from":"c","to":"d"}
		]
	}`
	resp, err := http.Post(srv.URL+"/dispatch/graph", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}

	var gr model.GraphResponse
	if err := json.NewDecoder(resp.Body).Decode(&gr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(gr.Tasks) != 4 {
		t.Fatalf("expected 4 tasks, got %d", len(gr.Tasks))
	}
	if len(gr.FirstDispatched) != 1 || gr.FirstDispatched[0] != gr.NodeIDMap["a"] {
		t.Errorf("expected single root a, got %v", gr.FirstDispatched)
	}
}

func TestDispatchGraph_Cycle(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	body := `{
		"nodes": [
			{"key":"a","title":"A","assigned_to":"coder"},
			{"key":"b","title":"B","assigned_to":"coder"}
		],
		"edges": [
			{"from":"a","to":"b"},
			{"from":"b","to":"a"}
		]
	}`
	resp, err := http.Post(srv.URL+"/dispatch/graph", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for cycle, got %d", resp.StatusCode)
	}
}

func TestDispatchGraph_MissingEdgeKey(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	body := `{
		"nodes": [{"key":"a","title":"A","assigned_to":"coder"}],
		"edges": [{"from":"a","to":"z"}]
	}`
	resp, err := http.Post(srv.URL+"/dispatch/graph", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for unknown edge key, got %d", resp.StatusCode)
	}
}

func TestDispatchGraph_NoEdges(t *testing.T) {
	// All nodes are roots when no edges are specified.
	srv := newTestServer(t, nil)
	defer srv.Close()

	body := `{
		"nodes": [
			{"key":"x","title":"X","assigned_to":"coder"},
			{"key":"y","title":"Y","assigned_to":"qa"}
		],
		"edges": []
	}`
	resp, err := http.Post(srv.URL+"/dispatch/graph", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}

	var gr model.GraphResponse
	if err := json.NewDecoder(resp.Body).Decode(&gr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(gr.FirstDispatched) != 2 {
		t.Fatalf("expected 2 roots, got %d", len(gr.FirstDispatched))
	}
	got := append([]string{}, gr.FirstDispatched...)
	sort.Strings(got)
	want := []string{gr.NodeIDMap["x"], gr.NodeIDMap["y"]}
	sort.Strings(want)
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("first_dispatched mismatch: want %v got %v", want, got)
		}
	}
}

// ------------------------------------------------------------------
// GET /tasks?search= tests
// ------------------------------------------------------------------

func TestListTasks_Search(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	// Create tasks.
	postGraph := func(title, agent string) {
		b, _ := json.Marshal(map[string]any{"title": title, "assigned_to": agent})
		resp, err := http.Post(srv.URL+"/dispatch", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatalf("dispatch: %v", err)
		}
		resp.Body.Close()
	}
	postGraph("fix login bug", "coder")
	postGraph("write documentation", "writer")
	postGraph("fix navbar styling", "coder")

	check := func(query string, wantCount int) {
		t.Helper()
		resp, err := http.Get(srv.URL + "/tasks?search=" + query)
		if err != nil {
			t.Fatalf("GET /tasks: %v", err)
		}
		defer resp.Body.Close()
		var result struct {
			Tasks []model.Task `json:"tasks"`
			Count int          `json:"count"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if result.Count != wantCount {
			t.Errorf("search=%s: expected %d tasks, got %d", query, wantCount, result.Count)
		}
	}

	check("fix", 2)
	check("documentation", 1)
	check("nonexistent", 0)
}

func TestListTasks_SearchCombinedWithAssignedTo(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	post := func(title, agent string) {
		b, _ := json.Marshal(map[string]any{"title": title, "assigned_to": agent})
		resp, _ := http.Post(srv.URL+"/dispatch", "application/json", bytes.NewReader(b))
		resp.Body.Close()
	}
	post("fix login bug", "coder")
	post("fix test failures", "qa")

	// search=fix + assigned_to=coder → 1 result.
	resp, err := http.Get(srv.URL + "/tasks?search=fix&assigned_to=coder")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	var result struct {
		Tasks []model.Task `json:"tasks"`
		Count int          `json:"count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.Count != 1 {
		t.Errorf("expected 1, got %d", result.Count)
	}
}
