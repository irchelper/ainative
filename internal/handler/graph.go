package handler

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/irchelper/agent-queue/internal/model"
	"github.com/irchelper/agent-queue/internal/openclaw"
)

// registerGraphRoutes adds the /dispatch/graph endpoint to mux.
func (h *Handler) registerGraphRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/dispatch/graph", h.handleDispatchGraph)
}

// handleDispatchGraph handles POST /dispatch/graph.
//
// Request body: {nodes, edges, notify_ceo_on_complete}
// Nodes are keyed by caller-chosen strings; edges express dependencies.
// The handler performs Kahn's topological sort, then inserts tasks in
// topological order so each task's depends_on IDs already exist.
// Root tasks (no incoming edges) are dispatched immediately.
func (h *Handler) handleDispatchGraph(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req model.GraphRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if len(req.Nodes) == 0 {
		writeError(w, http.StatusBadRequest, "nodes must be non-empty")
		return
	}

	// Validate node keys are unique and required fields are set.
	keySet := make(map[string]struct{}, len(req.Nodes))
	for i, n := range req.Nodes {
		if strings.TrimSpace(n.Key) == "" {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("nodes[%d].key is required", i))
			return
		}
		if strings.TrimSpace(n.Title) == "" {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("nodes[%d].title is required", i))
			return
		}
		if strings.TrimSpace(n.AssignedTo) == "" {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("nodes[%d].assigned_to is required", i))
			return
		}
		if _, dup := keySet[n.Key]; dup {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("duplicate node key %q", n.Key))
			return
		}
		keySet[n.Key] = struct{}{}
	}

	// Validate edge endpoints reference known keys.
	for _, e := range req.Edges {
		if _, ok := keySet[e.From]; !ok {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("edge.from %q not in nodes", e.From))
			return
		}
		if _, ok := keySet[e.To]; !ok {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("edge.to %q not in nodes", e.To))
			return
		}
	}

	// Kahn's topological sort.
	order, err := topoSort(req.Nodes, req.Edges)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Build a map from key → GraphNodeSpec for quick lookup.
	nodeByKey := make(map[string]model.GraphNodeSpec, len(req.Nodes))
	for _, n := range req.Nodes {
		nodeByKey[n.Key] = n
	}

	// Build inbound-edges map: key → list of keys it depends on (from).
	inboundKeys := make(map[string][]string, len(req.Nodes))
	for _, e := range req.Edges {
		inboundKeys[e.To] = append(inboundKeys[e.To], e.From)
	}

	// Insert tasks in topological order; accumulate key→taskID mapping.
	chainID := newChainID()
	keyToTaskID := make(map[string]string, len(req.Nodes))
	var allTasks []model.Task

	for _, key := range order {
		spec := nodeByKey[key]

		// Resolve depends_on: the task IDs of all nodes this one depends on.
		var dependsOn []string
		for _, depKey := range inboundKeys[key] {
			if tid, ok := keyToTaskID[depKey]; ok {
				dependsOn = append(dependsOn, tid)
			}
		}

		task, err := h.store.CreateTask(model.CreateTaskRequest{
			Title:               spec.Title,
			AssignedTo:          spec.AssignedTo,
			Description:         spec.Description,
			Priority:            spec.Priority,
			RequiresReview:      spec.RequiresReview,
			DependsOn:           dependsOn,
			ChainID:             chainID,
			NotifyCEOOnComplete: req.NotifyCEOOnComplete,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("create node %q: %v", key, err))
			return
		}
		keyToTaskID[key] = task.ID
		allTasks = append(allTasks, task)

		// V17: broadcast task_created.
		h.hub.Broadcast(SSEEvent{Type: "task_created", TaskID: task.ID, Status: string(task.Status)})
	}

	// Root tasks: nodes with no incoming edges → dispatch immediately.
	incomingKeys := make(map[string]bool, len(req.Edges))
	for _, e := range req.Edges {
		incomingKeys[e.To] = true
	}

	var firstDispatched []string
	for _, key := range order {
		if !incomingKeys[key] {
			taskID := keyToTaskID[key]
			firstDispatched = append(firstDispatched, taskID)

			// Notify root agent via sessions_send.
			spec := nodeByKey[key]
			sessionKey, known := openclaw.SessionKey(spec.AssignedTo)
			if known && h.oc != nil {
				msg := "[agent-queue] 你有新的待处理任务。请执行 poll 流程认领。"
				if err := h.oc.SendToSession(sessionKey, msg); err != nil {
					log.Printf("[dispatch/graph] sessions_send to %s failed: %v", sessionKey, err)
				}
			}
		}
	}

	writeJSON(w, http.StatusCreated, model.GraphResponse{
		NodeIDMap:       keyToTaskID,
		Tasks:           allTasks,
		FirstDispatched: firstDispatched,
	})
}

// topoSort performs Kahn's algorithm on the DAG defined by nodes/edges.
// Returns a slice of node keys in topological order (roots first).
// Returns an error if the graph contains a cycle.
func topoSort(nodes []model.GraphNodeSpec, edges []model.GraphEdge) ([]string, error) {
	// Build adjacency list (from → to) and in-degree map.
	inDegree := make(map[string]int, len(nodes))
	adj := make(map[string][]string, len(nodes))

	for _, n := range nodes {
		inDegree[n.Key] = 0
		adj[n.Key] = nil
	}
	for _, e := range edges {
		adj[e.From] = append(adj[e.From], e.To)
		inDegree[e.To]++
	}

	// Initialise queue with zero in-degree nodes (preserve insertion order).
	queue := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if inDegree[n.Key] == 0 {
			queue = append(queue, n.Key)
		}
	}

	order := make([]string, 0, len(nodes))
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		order = append(order, cur)
		for _, neighbor := range adj[cur] {
			inDegree[neighbor]--
			if inDegree[neighbor] == 0 {
				queue = append(queue, neighbor)
			}
		}
	}

	if len(order) != len(nodes) {
		return nil, fmt.Errorf("graph contains a cycle — cannot dispatch")
	}
	return order, nil
}
