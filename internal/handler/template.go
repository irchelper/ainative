package handler

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/irchelper/agent-queue/internal/model"
	"github.com/irchelper/agent-queue/internal/openclaw"
	"github.com/irchelper/agent-queue/internal/store"
)

// registerTemplateRoutes registers template and dispatch/from-template endpoints.
func (h *Handler) registerTemplateRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/templates", h.handleTemplates)
	mux.HandleFunc("/templates/", h.handleTemplatesName)
	mux.HandleFunc("/dispatch/from-template/", h.handleDispatchFromTemplate)
}

// POST /templates   → createTemplate
// GET  /templates   → listTemplates
func (h *Handler) handleTemplates(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.listTemplates(w, r)
	case http.MethodPost:
		h.createTemplate(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

// GET    /templates/:name → getTemplate
// DELETE /templates/:name → deleteTemplate
func (h *Handler) handleTemplatesName(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/templates/")
	if name == "" {
		writeError(w, http.StatusBadRequest, "missing template name")
		return
	}
	switch r.Method {
	case http.MethodGet:
		tmpl, err := h.store.GetTemplate(name)
		if err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "template not found")
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, tmpl)
	case http.MethodDelete:
		if err := h.store.DeleteTemplate(name); err == store.ErrNotFound {
			writeError(w, http.StatusNotFound, "template not found")
			return
		} else if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"deleted": name})
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (h *Handler) listTemplates(w http.ResponseWriter, r *http.Request) {
	templates, err := h.store.ListTemplates()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if templates == nil {
		templates = []model.Template{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"templates": templates, "count": len(templates)})
}

func (h *Handler) createTemplate(w http.ResponseWriter, r *http.Request) {
	var req model.CreateTemplateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	if len(req.Tasks) == 0 {
		writeError(w, http.StatusBadRequest, "tasks must be non-empty")
		return
	}
	for i, t := range req.Tasks {
		if strings.TrimSpace(t.AssignedTo) == "" {
			writeError(w, http.StatusBadRequest, "tasks["+string(rune('0'+i))+"]. assigned_to is required")
			return
		}
		if strings.TrimSpace(t.Title) == "" {
			writeError(w, http.StatusBadRequest, "tasks["+string(rune('0'+i))+"]. title is required")
			return
		}
	}
	tmpl, err := h.store.CreateTemplate(req)
	if err != nil {
		// Check for SQLite UNIQUE constraint
		if strings.Contains(err.Error(), "UNIQUE constraint") {
			writeError(w, http.StatusConflict, "template name already exists: "+req.Name)
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, tmpl)
}

// POST /dispatch/from-template/:name
// Body: { "vars": {"goal": "..."}, "notify_ceo_on_complete": true, "chain_title": "..." }
// Instantiates the template by replacing {var} placeholders in title/description.
// Multi-task → chain; single-task → plain dispatch.
func (h *Handler) handleDispatchFromTemplate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/dispatch/from-template/")
	if name == "" {
		writeError(w, http.StatusBadRequest, "missing template name")
		return
	}
	tmpl, err := h.store.GetTemplate(name)
	if err == store.ErrNotFound {
		writeError(w, http.StatusNotFound, "template not found: "+name)
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	var req model.DispatchFromTemplateRequest
	// Body is optional (empty body = no vars)
	_ = json.NewDecoder(r.Body).Decode(&req)

	// Apply variable substitution to each task spec.
	specs := applyVars(tmpl.Tasks, req.Vars)

	if len(specs) == 1 {
		// Single task → plain dispatch
		task, err := h.store.CreateTask(model.CreateTaskRequest{
			Title:               specs[0].Title,
			AssignedTo:          specs[0].AssignedTo,
			Description:         specs[0].Description,
			RequiresReview:      specs[0].RequiresReview,
			Priority:            specs[0].Priority,
			NotifyCEOOnComplete: req.NotifyCEOOnComplete,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		resp := model.DispatchResponse{Task: task}
		if sessionKey, known := openclaw.SessionKey(task.AssignedTo); known && h.oc != nil {
			msg := "[agent-queue] 你有新的待处理任务。请执行 poll 流程认领。"
			if err := h.oc.SendToSession(sessionKey, msg); err == nil {
				resp.Notified = true
			}
		}
		writeJSON(w, http.StatusCreated, resp)
		return
	}

	// Multi-task → chain
	chainID := newChainID()
	created := make([]model.Task, 0, len(specs))
	for i, spec := range specs {
		var dependsOn []string
		if i > 0 {
			dependsOn = []string{created[i-1].ID}
		}
		task, err := h.store.CreateTask(model.CreateTaskRequest{
			Title:               spec.Title,
			AssignedTo:          spec.AssignedTo,
			Description:         spec.Description,
			RequiresReview:      spec.RequiresReview,
			Priority:            spec.Priority,
			DependsOn:           dependsOn,
			ChainID:             chainID,
			NotifyCEOOnComplete: req.NotifyCEOOnComplete,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		created = append(created, task)
	}

	// Dispatch first task.
	first := created[0]
	chainResp := model.ChainResponse{
		ChainID:         chainID,
		Tasks:           created,
		FirstDispatched: first.ID,
	}
	if sessionKey, known := openclaw.SessionKey(first.AssignedTo); known && h.oc != nil {
		msg := "[agent-queue] 你有新的待处理任务。请执行 poll 流程认领。"
		if err := h.oc.SendToSession(sessionKey, msg); err == nil {
			chainResp.Notified = true
		}
	}
	writeJSON(w, http.StatusCreated, chainResp)
}

// applyVars substitutes {key} placeholders in title/description for each spec.
// vars may be nil (no substitution performed).
func applyVars(specs []model.TemplateTaskSpec, vars map[string]string) []model.TemplateTaskSpec {
	if len(vars) == 0 {
		return specs
	}
	out := make([]model.TemplateTaskSpec, len(specs))
	for i, s := range specs {
		out[i] = s
		for k, v := range vars {
			placeholder := "{" + k + "}"
			out[i].Title = strings.ReplaceAll(out[i].Title, placeholder, v)
			out[i].Description = strings.ReplaceAll(out[i].Description, placeholder, v)
		}
	}
	return out
}
