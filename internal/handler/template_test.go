package handler_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/irchelper/agent-queue/internal/model"
	"github.com/irchelper/agent-queue/internal/openclaw"
)

// ---------------------------------------------------------------------------
// V15: Template CRUD tests
// ---------------------------------------------------------------------------

// TestV15_Template_Seed verifies that the 3 built-in templates are seeded on startup.
func TestV15_Template_Seed(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	r := getJSON(t, srv, "/templates")
	if r.StatusCode != http.StatusOK {
		t.Fatalf("GET /templates: want 200, got %d", r.StatusCode)
	}
	var resp struct {
		Templates []model.Template `json:"templates"`
		Count     int              `json:"count"`
	}
	json.NewDecoder(r.Body).Decode(&resp)
	r.Body.Close()

	if resp.Count < 3 {
		t.Fatalf("expected at least 3 seeded templates, got %d: %v", resp.Count, resp.Templates)
	}
	names := map[string]bool{}
	for _, tmpl := range resp.Templates {
		names[tmpl.Name] = true
	}
	for _, want := range []string{"fix-qa-deploy", "doc-review", "feature"} {
		if !names[want] {
			t.Errorf("seed template %q not found in list", want)
		}
	}
}

// TestV15_Template_CRUD verifies create / get / delete lifecycle.
func TestV15_Template_CRUD(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	// Create
	createR := postJSON(t, srv, "/templates", map[string]any{
		"name":        "test-tmpl",
		"description": "test template",
		"tasks": []map[string]any{
			{"assigned_to": "coder", "title": "实现 {goal}"},
			{"assigned_to": "qa", "title": "测试 {goal}"},
		},
	})
	if createR.StatusCode != http.StatusCreated {
		t.Fatalf("POST /templates: want 201, got %d", createR.StatusCode)
	}
	var created model.Template
	json.NewDecoder(createR.Body).Decode(&created)
	createR.Body.Close()
	if created.Name != "test-tmpl" {
		t.Errorf("name=%q, want test-tmpl", created.Name)
	}
	if len(created.Tasks) != 2 {
		t.Errorf("tasks len=%d, want 2", len(created.Tasks))
	}

	// Get by name
	getR := getJSON(t, srv, "/templates/test-tmpl")
	if getR.StatusCode != http.StatusOK {
		t.Fatalf("GET /templates/test-tmpl: want 200, got %d", getR.StatusCode)
	}
	var fetched model.Template
	json.NewDecoder(getR.Body).Decode(&fetched)
	getR.Body.Close()
	if fetched.Name != "test-tmpl" {
		t.Errorf("fetched name=%q", fetched.Name)
	}

	// Duplicate name → 409
	dupR := postJSON(t, srv, "/templates", map[string]any{
		"name":  "test-tmpl",
		"tasks": []map[string]any{{"assigned_to": "coder", "title": "x"}},
	})
	if dupR.StatusCode != http.StatusConflict {
		t.Errorf("duplicate POST: want 409, got %d", dupR.StatusCode)
	}
	dupR.Body.Close()

	// Delete
	delReq, _ := http.NewRequest(http.MethodDelete, srv.URL+"/templates/test-tmpl", nil)
	delR, err := http.DefaultClient.Do(delReq)
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	if delR.StatusCode != http.StatusOK {
		t.Errorf("DELETE: want 200, got %d", delR.StatusCode)
	}
	delR.Body.Close()

	// Get after delete → 404
	getR2 := getJSON(t, srv, "/templates/test-tmpl")
	if getR2.StatusCode != http.StatusNotFound {
		t.Errorf("GET after delete: want 404, got %d", getR2.StatusCode)
	}
	getR2.Body.Close()
}

// TestV15_DispatchFromTemplate_Chain verifies multi-task template creates a chain.
func TestV15_DispatchFromTemplate_Chain(t *testing.T) {
	mockOC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"ok": true}) //nolint:errcheck
	}))
	defer mockOC.Close()
	oc := openclaw.NewWithURL(mockOC.URL, "")
	srv := newTestServer(t, oc)
	defer srv.Close()

	// Use built-in "doc-review" template (writer → thinker)
	dispR := postJSON(t, srv, "/dispatch/from-template/doc-review", map[string]any{
		"vars": map[string]string{"goal": "API设计"},
	})
	if dispR.StatusCode != http.StatusCreated {
		t.Fatalf("dispatch/from-template: want 201, got %d", dispR.StatusCode)
	}
	var chainResp model.ChainResponse
	json.NewDecoder(dispR.Body).Decode(&chainResp)
	dispR.Body.Close()

	if chainResp.ChainID == "" {
		t.Fatal("chain_id is empty")
	}
	if len(chainResp.Tasks) != 2 {
		t.Fatalf("want 2 tasks, got %d", len(chainResp.Tasks))
	}
	// Variables should be substituted
	if chainResp.Tasks[0].AssignedTo != "writer" {
		t.Errorf("task[0] assigned_to=%q, want writer", chainResp.Tasks[0].AssignedTo)
	}
	if chainResp.Tasks[0].Title != "撰写文档：API设计" {
		t.Errorf("task[0] title=%q, want 撰写文档：API设计", chainResp.Tasks[0].Title)
	}
	if chainResp.Tasks[1].AssignedTo != "thinker" {
		t.Errorf("task[1] assigned_to=%q, want thinker", chainResp.Tasks[1].AssignedTo)
	}
	// task[1] depends on task[0]
	if len(chainResp.Tasks[1].DependsOn) == 0 || chainResp.Tasks[1].DependsOn[0] != chainResp.Tasks[0].ID {
		t.Errorf("task[1] should depend on task[0]; deps=%v", chainResp.Tasks[1].DependsOn)
	}
}

// TestV15_DispatchFromTemplate_Single verifies single-task template creates plain task.
func TestV15_DispatchFromTemplate_Single(t *testing.T) {
	mockOC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"ok": true}) //nolint:errcheck
	}))
	defer mockOC.Close()
	oc := openclaw.NewWithURL(mockOC.URL, "")
	srv := newTestServer(t, oc)
	defer srv.Close()

	// Create a single-task template
	postJSON(t, srv, "/templates", map[string]any{
		"name": "single-task",
		"tasks": []map[string]any{
			{"assigned_to": "coder", "title": "完成 {goal}"},
		},
	}).Body.Close()

	dispR := postJSON(t, srv, "/dispatch/from-template/single-task", map[string]any{
		"vars": map[string]string{"goal": "登录功能"},
	})
	if dispR.StatusCode != http.StatusCreated {
		t.Fatalf("dispatch/from-template: want 201, got %d", dispR.StatusCode)
	}
	var dispResp model.DispatchResponse
	json.NewDecoder(dispR.Body).Decode(&dispResp)
	dispR.Body.Close()

	if dispResp.Task.Title != "完成 登录功能" {
		t.Errorf("title=%q, want '完成 登录功能'", dispResp.Task.Title)
	}
	if dispResp.Task.AssignedTo != "coder" {
		t.Errorf("assigned_to=%q, want coder", dispResp.Task.AssignedTo)
	}
	// Single task should have no chain_id
	if dispResp.Task.ChainID != "" {
		t.Errorf("single task should have empty chain_id, got %q", dispResp.Task.ChainID)
	}
}

// TestV15_DispatchFromTemplate_NotFound verifies 404 for unknown template.
func TestV15_DispatchFromTemplate_NotFound(t *testing.T) {
	srv := newTestServer(t, nil)
	defer srv.Close()

	r := postJSON(t, srv, "/dispatch/from-template/no-such-template", map[string]any{})
	if r.StatusCode != http.StatusNotFound {
		t.Errorf("want 404, got %d", r.StatusCode)
	}
	r.Body.Close()
}


