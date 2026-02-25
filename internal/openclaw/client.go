// Package openclaw provides a minimal client for the OpenClaw Gateway
// Tools Invoke HTTP API.
package openclaw

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

// Client wraps the OpenClaw Gateway /tools/invoke endpoint.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// agentSessionKeys maps expert name → canonical session key for sessions_send.
// These are the primary Discord channel sessions for each agent.
var agentSessionKeys = map[string]string{
	"coder":    "agent:coder:discord:channel:1475338640593916045",
	"writer":   "agent:writer:discord:channel:1475339585075548200",
	"thinker":  "agent:thinker:discord:channel:1475338689646297305",
	"devops":   "agent:devops:discord:channel:1475339626872049736",
	"security": "agent:security:discord:channel:1475339809206697984",
	"ops":      "agent:ops:discord:channel:1475339864361664684",
	"qa":       "agent:qa:discord:channel:1475679634442944532",
	"pm":       "agent:pm:discord:channel:1476150796071600242",
	"uiux":     "agent:uiux:discord:channel:1476150914216886395",
	"vision":   "agent:vision:discord:channel:1475680076380110969",
}

// SessionKey returns the session key for the given agent name.
// Returns ("", false) if the agent is unknown.
func SessionKey(agent string) (string, bool) {
	key, ok := agentSessionKeys[agent]
	return key, ok
}

// NewFromEnv builds a Client from environment variables.
//
//	AGENT_QUEUE_OPENCLAW_API_URL  – defaults to http://localhost:18789
//	AGENT_QUEUE_OPENCLAW_API_KEY  – Bearer token
func NewFromEnv() *Client {
	base := os.Getenv("AGENT_QUEUE_OPENCLAW_API_URL")
	if base == "" {
		base = "http://localhost:18789"
	}
	return &Client{
		baseURL: base,
		token:   os.Getenv("AGENT_QUEUE_OPENCLAW_API_KEY"),
		http:    &http.Client{Timeout: 10 * time.Second},
	}
}

// NewWithURL builds a Client with explicit URL and token (useful for tests).
func NewWithURL(baseURL, token string) *Client {
	return &Client{
		baseURL: baseURL,
		token:   token,
		http:    &http.Client{Timeout: 10 * time.Second},
	}
}

// invokeRequest is the body for POST /tools/invoke.
type invokeRequest struct {
	Tool       string         `json:"tool"`
	Args       map[string]any `json:"args"`
	SessionKey string         `json:"sessionKey,omitempty"`
}

type invokeResponse struct {
	OK    bool `json:"ok"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// SendToSession calls sessions_send via /tools/invoke.
// NOTE: sessions_send is in the Gateway's default HTTP deny list.
// To enable, add the following to openclaw.json:
//
//	{ "gateway": { "tools": { "allow": ["sessions_send"] } } }
//
// If the call fails (e.g., tool not available), the error is logged but
// NOT returned as a fatal error – the caller decides whether to propagate.
func (c *Client) SendToSession(sessionKey, message string) error {
	body, err := json.Marshal(invokeRequest{
		Tool: "sessions_send",
		Args: map[string]any{
			"sessionKey":     sessionKey,
			"message":        message,
			"timeoutSeconds": 0, // fire-and-forget
		},
	})
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, c.baseURL+"/tools/invoke", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	var result invokeResponse
	if err = json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	if !result.OK {
		msg := "unknown error"
		if result.Error != nil {
			msg = result.Error.Message
		}
		return fmt.Errorf("tool invoke failed: %s", msg)
	}
	return nil
}

// AsyncSend sends a session message asynchronously, logging any failure.
// Dispatch message format: "[agent-queue] 新任务派发：<title>\ntask_id: <id>"
func AsyncSend(c *Client, sessionKey, message string) {
	go func() {
		if err := c.SendToSession(sessionKey, message); err != nil {
			log.Printf("[openclaw] sessions_send to %s failed: %v (hint: add sessions_send to gateway.tools.allow)", sessionKey, err)
		} else {
			log.Printf("[openclaw] sessions_send to %s: ok", sessionKey)
		}
	}()
}
