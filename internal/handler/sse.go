package handler

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"
)

// SSEEvent represents a server-sent event payload.
type SSEEvent struct {
	Type   string `json:"type"`
	TaskID string `json:"task_id,omitempty"`
	Status string `json:"status,omitempty"`
}

// sseClient represents a connected SSE client.
type sseClient struct {
	ch chan SSEEvent
	id uint64
}

// SSEHub manages SSE client connections and broadcasts events.
type SSEHub struct {
	mu      sync.RWMutex
	clients map[uint64]*sseClient
	nextID  uint64
}

// NewSSEHub creates a new SSEHub.
func NewSSEHub() *SSEHub {
	return &SSEHub{
		clients: make(map[uint64]*sseClient),
	}
}

// register adds a new client and returns it.
func (h *SSEHub) register() *sseClient {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextID++
	c := &sseClient{
		ch: make(chan SSEEvent, 16),
		id: h.nextID,
	}
	h.clients[c.id] = c
	return c
}

// unregister removes a client.
func (h *SSEHub) unregister(c *sseClient) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.clients, c.id)
	close(c.ch)
}

// Broadcast sends an event to all connected clients (non-blocking; slow clients are dropped).
func (h *SSEHub) Broadcast(event SSEEvent) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, c := range h.clients {
		select {
		case c.ch <- event:
		default:
			// Client buffer full; skip this client for this event.
			log.Printf("[sse] client %d buffer full, dropping event %s", c.id, event.Type)
		}
	}
}

// ServeHTTP handles GET /api/events — upgrades to SSE stream.
func (h *SSEHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// Verify the client supports streaming.
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	// Set SSE headers.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx buffering

	// Add CORS header so the browser's EventSource can connect cross-origin.
	w.Header().Set("Access-Control-Allow-Origin", "*")

	client := h.register()
	defer h.unregister(client)

	// Send an initial comment to confirm the connection.
	fmt.Fprintf(w, ": connected\n\n")
	flusher.Flush()

	// Keep-alive ticker (every 15 s).
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-client.ch:
			if !ok {
				return
			}
			data, err := json.Marshal(ev)
			if err != nil {
				log.Printf("[sse] marshal error: %v", err)
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		case <-ticker.C:
			// SSE heartbeat comment.
			fmt.Fprintf(w, ": heartbeat\n\n")
			flusher.Flush()
		}
	}
}
