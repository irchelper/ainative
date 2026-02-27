package handler

import (
	"fmt"
	"net/http"
)

// handleDocs serves GET /docs — a Scalar-based API documentation page.
// The page loads the OpenAPI spec from GET /openapi.json via CDN Scalar.
func (h *Handler) handleDocs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, docsHTML)
}

// handleOpenAPISpec serves GET /openapi.json — a minimal OpenAPI 3.1 spec
// describing all agent-queue endpoints.
func (h *Handler) handleOpenAPISpec(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, openAPISpec)
}

// docsHTML is a self-contained HTML page that loads Scalar via CDN.
const docsHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>Agent Queue API Docs</title>
  <style>body{margin:0}</style>
</head>
<body>
  <script
    id="api-reference"
    data-url="/openapi.json"
    data-configuration='{"theme":"purple"}'
    src="https://cdn.jsdelivr.net/npm/@scalar/api-reference"></script>
</body>
</html>`

// openAPISpec is a minimal OpenAPI 3.1 spec that describes all agent-queue endpoints.
// Kept inline to avoid embed dependencies and to always reflect the current codebase.
const openAPISpec = `{
  "openapi": "3.1.0",
  "info": {
    "title": "Agent Queue API",
    "version": "1.0.0",
    "description": "Task queue and dispatch system for AI agent workflows"
  },
  "servers": [{"url": "http://localhost:19827", "description": "Local dev"}],
  "paths": {
    "/health": {
      "get": {
        "summary": "Health check",
        "tags": ["System"],
        "responses": {"200": {"description": "OK"}}
      }
    },
    "/tasks": {
      "get": {
        "summary": "List tasks",
        "tags": ["Tasks"],
        "parameters": [
          {"name": "status", "in": "query", "schema": {"type": "string"}},
          {"name": "assigned_to", "in": "query", "schema": {"type": "string"}},
          {"name": "search", "in": "query", "schema": {"type": "string"}, "description": "Fuzzy search on title/description"},
          {"name": "deps_met", "in": "query", "schema": {"type": "boolean"}},
          {"name": "limit", "in": "query", "schema": {"type": "integer"}}
        ],
        "responses": {"200": {"description": "Task list"}}
      },
      "post": {
        "summary": "Create task",
        "tags": ["Tasks"],
        "requestBody": {"required": true, "content": {"application/json": {"schema": {"$ref": "#/components/schemas/CreateTaskRequest"}}}},
        "responses": {"201": {"description": "Created task"}}
      }
    },
    "/tasks/summary": {
      "get": {
        "summary": "Task summary (counts)",
        "tags": ["Tasks"],
        "parameters": [
          {"name": "assigned_to", "in": "query", "schema": {"type": "string"}, "description": "Filter counts by agent"}
        ],
        "responses": {"200": {"description": "Summary"}}
      }
    },
    "/tasks/poll": {
      "get": {
        "summary": "Poll next pending task",
        "tags": ["Tasks"],
        "parameters": [
          {"name": "assigned_to", "in": "query", "required": true, "schema": {"type": "string"}}
        ],
        "responses": {"200": {"description": "Next task or null"}}
      }
    },
    "/tasks/{id}": {
      "get": {
        "summary": "Get task by ID",
        "tags": ["Tasks"],
        "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}],
        "responses": {"200": {"description": "Task"}}
      },
      "patch": {
        "summary": "Update task (FSM transition)",
        "tags": ["Tasks"],
        "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}],
        "requestBody": {"required": true, "content": {"application/json": {"schema": {"$ref": "#/components/schemas/PatchTaskRequest"}}}},
        "responses": {"200": {"description": "Updated task"}}
      }
    },
    "/tasks/{id}/claim": {
      "post": {
        "summary": "Claim a pending task",
        "tags": ["Tasks"],
        "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}],
        "requestBody": {"required": true, "content": {"application/json": {"schema": {"type": "object", "properties": {"agent": {"type": "string"}, "version": {"type": "integer"}}}}}},
        "responses": {"200": {"description": "Claimed task"}}
      }
    },
    "/dispatch": {
      "post": {
        "summary": "Dispatch a single task",
        "tags": ["Dispatch"],
        "requestBody": {"required": true, "content": {"application/json": {"schema": {"$ref": "#/components/schemas/DispatchRequest"}}}},
        "responses": {"201": {"description": "Dispatched task"}}
      }
    },
    "/dispatch/chain": {
      "post": {
        "summary": "Dispatch a linear task chain",
        "tags": ["Dispatch"],
        "requestBody": {"required": true, "content": {"application/json": {"schema": {"$ref": "#/components/schemas/ChainRequest"}}}},
        "responses": {"201": {"description": "Chain created"}}
      }
    },
    "/dispatch/graph": {
      "post": {
        "summary": "Dispatch a DAG task graph",
        "tags": ["Dispatch"],
        "requestBody": {"required": true, "content": {"application/json": {"schema": {"$ref": "#/components/schemas/GraphRequest"}}}},
        "responses": {"201": {"description": "Graph created"}}
      }
    },
    "/dispatch/from-template/{name}": {
      "post": {
        "summary": "Dispatch from a named template",
        "tags": ["Templates", "Dispatch"],
        "parameters": [{"name": "name", "in": "path", "required": true, "schema": {"type": "string"}}],
        "requestBody": {"required": true, "content": {"application/json": {"schema": {"type": "object", "properties": {"vars": {"type": "object"}, "notify_ceo_on_complete": {"type": "boolean"}}}}}},
        "responses": {"201": {"description": "Chain dispatched from template"}}
      }
    },
    "/templates": {
      "get": {"summary": "List templates", "tags": ["Templates"], "responses": {"200": {"description": "Templates"}}},
      "post": {"summary": "Create template", "tags": ["Templates"], "responses": {"201": {"description": "Created"}}}
    },
    "/templates/{name}": {
      "get": {"summary": "Get template", "tags": ["Templates"], "parameters": [{"name": "name", "in": "path", "required": true, "schema": {"type": "string"}}], "responses": {"200": {"description": "Template"}}},
      "put": {"summary": "Update template", "tags": ["Templates"], "parameters": [{"name": "name", "in": "path", "required": true, "schema": {"type": "string"}}], "responses": {"200": {"description": "Updated"}}},
      "delete": {"summary": "Delete template", "tags": ["Templates"], "parameters": [{"name": "name", "in": "path", "required": true, "schema": {"type": "string"}}], "responses": {"204": {"description": "Deleted"}}}
    },
    "/retry-routing": {
      "get": {"summary": "List retry routing rules", "tags": ["Routing"], "responses": {"200": {"description": "Rules"}}},
      "post": {"summary": "Create routing rule", "tags": ["Routing"], "responses": {"201": {"description": "Created"}}}
    },
    "/api/dashboard": {
      "get": {"summary": "Dashboard data (todo + exceptions)", "tags": ["UI API"], "responses": {"200": {"description": "Dashboard"}}}
    },
    "/api/chains": {
      "get": {"summary": "List all chains with tasks", "tags": ["UI API"], "responses": {"200": {"description": "Chains"}}}
    },
    "/api/timeline/{id}": {
      "get": {
        "summary": "Task timeline (task + history)",
        "tags": ["UI API"],
        "parameters": [{"name": "id", "in": "path", "required": true, "schema": {"type": "string"}}],
        "responses": {"200": {"description": "Timeline"}}
      }
    },
    "/api/events": {
      "get": {"summary": "SSE real-time event stream", "tags": ["UI API"], "responses": {"200": {"description": "text/event-stream"}}}
    },
    "/api/config": {
      "get": {"summary": "Frontend config (agents list)", "tags": ["UI API"], "responses": {"200": {"description": "Config"}}}
    }
  },
  "components": {
    "schemas": {
      "CreateTaskRequest": {
        "type": "object",
        "required": ["title", "assigned_to"],
        "properties": {
          "title": {"type": "string"},
          "assigned_to": {"type": "string"},
          "description": {"type": "string"},
          "priority": {"type": "integer", "default": 0, "description": "0=normal, 1=high, 2=urgent"},
          "requires_review": {"type": "boolean"},
          "depends_on": {"type": "array", "items": {"type": "string"}},
          "notify_ceo_on_complete": {"type": "boolean"},
          "timeout_minutes": {"type": "integer"},
          "timeout_action": {"type": "string"}
        }
      },
      "PatchTaskRequest": {
        "type": "object",
        "required": ["version"],
        "properties": {
          "status": {"type": "string", "enum": ["claimed","in_progress","review","done","failed","blocked","pending","cancelled"]},
          "result": {"type": "string"},
          "failure_reason": {"type": "string"},
          "retry_assigned_to": {"type": "string"},
          "commit_url": {"type": "string"},
          "priority": {"type": "integer", "description": "0=normal, 1=high, 2=urgent"},
          "version": {"type": "integer"}
        }
      },
      "DispatchRequest": {
        "type": "object",
        "required": ["title", "assigned_to"],
        "properties": {
          "title": {"type": "string"},
          "assigned_to": {"type": "string"},
          "description": {"type": "string"},
          "notify_ceo_on_complete": {"type": "boolean"}
        }
      },
      "ChainRequest": {
        "type": "object",
        "required": ["tasks"],
        "properties": {
          "tasks": {"type": "array", "items": {"$ref": "#/components/schemas/ChainTaskSpec"}},
          "notify_ceo_on_complete": {"type": "boolean"}
        }
      },
      "ChainTaskSpec": {
        "type": "object",
        "required": ["title", "assigned_to"],
        "properties": {
          "title": {"type": "string"},
          "assigned_to": {"type": "string"},
          "description": {"type": "string"}
        }
      },
      "GraphRequest": {
        "type": "object",
        "required": ["nodes"],
        "properties": {
          "nodes": {"type": "array", "items": {"$ref": "#/components/schemas/GraphNodeSpec"}},
          "edges": {"type": "array", "items": {"$ref": "#/components/schemas/GraphEdge"}},
          "notify_ceo_on_complete": {"type": "boolean"}
        }
      },
      "GraphNodeSpec": {
        "type": "object",
        "required": ["key", "title", "assigned_to"],
        "properties": {
          "key": {"type": "string"},
          "title": {"type": "string"},
          "assigned_to": {"type": "string"},
          "description": {"type": "string"},
          "priority": {"type": "integer"}
        }
      },
      "GraphEdge": {
        "type": "object",
        "required": ["from", "to"],
        "properties": {
          "from": {"type": "string"},
          "to": {"type": "string"}
        }
      }
    }
  }
}`
