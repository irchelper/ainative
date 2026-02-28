// Package notify – SessionNotifier sends CEO alerts via Discord webhook and
// OpenClaw sessions_send.
package notify

import (	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/irchelper/agent-queue/internal/model"
	"github.com/irchelper/agent-queue/internal/openclaw"
)

// CEOSessionKey is the default CEO session for alerts.
const CEOSessionKey = "agent:ceo:discord:channel:1475338424293789877"

// SessionNotifier notifies the CEO via:
//  1. Discord webhook (primary – always visible in #首席ceo channel)
//  2. OpenClaw sessions_send (secondary – injects into CEO session context)
//
// The webhook path is reliable regardless of whether CEO session is active.
// sessions_send is kept as a best-effort supplement.
type SessionNotifier struct {
	client         *openclaw.Client
	ceoSessionKey  string
	ceoWebhookURL  string       // POST CEO alerts directly to Discord channel
	ceoUserID      string       // Discord user ID for @mention
	retryQ         *RetryQueue  // CEO-critical notifications walk this queue
	httpClient     *http.Client
}

// NewSessionNotifier creates a SessionNotifier targeting the given CEO session.
// If ceoSessionKey is empty, CEOSessionKey is used.
// CEO webhook URL and user ID are read from env:
//   - AGENT_QUEUE_CEO_WEBHOOK_URL  (direct Discord webhook for #首席ceo)
//   - AGENT_QUEUE_DISCORD_USER_ID  (for @mention in webhook messages)
//
// Call Start() / Stop() on the embedded RetryQueue via the returned notifier.
func NewSessionNotifier(client *openclaw.Client, ceoSessionKey string) *SessionNotifier {
	if ceoSessionKey == "" {
		ceoSessionKey = CEOSessionKey
	}
	return &SessionNotifier{
		client:        client,
		ceoSessionKey: ceoSessionKey,
		ceoWebhookURL: os.Getenv("AGENT_QUEUE_CEO_WEBHOOK_URL"),
		ceoUserID:     os.Getenv("AGENT_QUEUE_DISCORD_USER_ID"),
		retryQ:        NewRetryQueue(),
		httpClient:    &http.Client{Timeout: 10 * time.Second},
	}
}

// Start launches the background retry goroutine. Must be called once at server startup.
func (s *SessionNotifier) Start() { s.retryQ.Start() }

// Stop gracefully shuts down the retry goroutine. Call during server shutdown.
func (s *SessionNotifier) Stop() { s.retryQ.Stop() }

// RetryQueueLen returns the number of pending retries (for testing/monitoring).
func (s *SessionNotifier) RetryQueueLen() int { return s.retryQ.Len() }

// sendWebhook posts a message directly to the CEO Discord channel via webhook.
// Returns nil if no webhook URL is configured (silent skip).
func (s *SessionNotifier) sendWebhook(content string) error {
	if s.ceoWebhookURL == "" {
		return nil
	}
	body, err := json.Marshal(map[string]string{"content": content})
	if err != nil {
		return fmt.Errorf("marshal webhook body: %w", err)
	}
	resp, err := s.httpClient.Post(s.ceoWebhookURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("webhook post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook HTTP %d", resp.StatusCode)
	}
	return nil
}

// mention returns a Discord @mention prefix if ceoUserID is set, else "".
func (s *SessionNotifier) mention() string {
	if s.ceoUserID != "" {
		return fmt.Sprintf("<@%s> ", s.ceoUserID)
	}
	return ""
}

// notifyCEO sends via webhook (primary) + sessions_send (secondary).
// Both paths walk RetryQueue for resilience (30s/60s/120s backoff, up to 3 retries).
// 方案A: webhook Enqueue returns immediately after first attempt; sessions_send
// Enqueue starts without waiting for the webhook result, minimising head-of-line delay.
func (s *SessionNotifier) notifyCEO(label, webhookContent, sessionMsg string) {
	// Primary: Discord webhook via RetryQueue.
	wc := webhookContent // capture for closure
	webhookLabel := label + ":webhook"
	s.retryQ.Enqueue(webhookLabel, func() error {
		if err := s.sendWebhook(wc); err != nil {
			log.Printf("[session_notifier] %s webhook failed: %v", label, err)
			return err
		}
		if s.ceoWebhookURL != "" {
			log.Printf("[session_notifier] %s webhook: CEO notified via Discord", label)
		}
		return nil
	})

	// Secondary: sessions_send via RetryQueue (best-effort context injection).
	// Enqueued immediately after webhook — does not wait for webhook result (方案A).
	sessionKey := s.ceoSessionKey
	sendFn := func() error {
		if err := s.client.SendToSession(sessionKey, sessionMsg); err != nil {
			log.Printf("[session_notifier] %s sessions_send → %s failed: %v", label, sessionKey, err)
			return err
		}
		log.Printf("[session_notifier] %s sessions_send → %s: ok", label, sessionKey)
		return nil
	}
	s.retryQ.Enqueue(label, sendFn)
}

// OnFailed sends a CEO alert for a failed task that has no retry directive.
// Message format matches docs/ARCH.md §5 spec.
// Uses RetryQueue: up to 3 retries with 30s/60s/120s backoff.
func (s *SessionNotifier) OnFailed(task model.Task) error {
	reason := task.FailureReason
	if reason == "" {
		reason = task.Result
	}
	if reason == "" {
		reason = "（无）"
	}
	sessionMsg := fmt.Sprintf(
		"[agent-queue] ❌ 任务失败需介入：%s\nresult: %s\ntask_id: %s",
		task.Title, reason, task.ID,
	)
	webhookContent := fmt.Sprintf(
		"%s❌ **任务失败需介入**\n**任务：** %s\n**失败原因：** %s\n`task_id: %s`",
		s.mention(), task.Title, reason, task.ID,
	)
	label := "OnFailed:" + task.ID
	s.notifyCEO(label, webhookContent, sessionMsg)
	return nil
}

// AsyncOnFailed runs OnFailed in a goroutine so the handler is never blocked.
func AsyncOnFailed(sn *SessionNotifier, task model.Task) {
	go func() {
		sn.OnFailed(task) //nolint:errcheck
	}()
}

// Dispatch sends a "有新任务请 poll" nudge to the session for the given agent.
// It resolves the sessionKey via openclaw.SessionKey.
// Returns ("", false) when the agent is unknown.
func (s *SessionNotifier) Dispatch(assignedTo string) (string, bool) {
	sessionKey, known := openclaw.SessionKey(assignedTo)
	if !known {
		log.Printf("[session_notifier] Dispatch: unknown agent %q – skipping", assignedTo)
		return "", false
	}
	msg := "[agent-queue] 你有新的待处理任务。请执行 poll 流程认领。"
	if err := s.client.SendToSession(sessionKey, msg); err != nil {
		log.Printf("[session_notifier] Dispatch → %s failed: %v", sessionKey, err)
		return sessionKey, false
	}
	log.Printf("[session_notifier] Dispatch → %s: notified", sessionKey)
	return sessionKey, true
}

// OnChainComplete sends a CEO notification when all tasks in a chain are done.
// Message format matches docs/ARCH.md §5 chain complete spec.
// Uses RetryQueue: up to 3 retries with 30s/60s/120s backoff.
func (s *SessionNotifier) OnChainComplete(chainID, chainTitle string, tasks []model.Task) error {
	if chainTitle == "" {
		chainTitle = "链路 " + chainID
	}
	total := len(tasks)
	done := 0
	for _, t := range tasks {
		if t.Status == model.StatusDone || t.Status == model.StatusCancelled {
			done++
		}
	}

	lines := fmt.Sprintf("[agent-queue] ✅ 任务链完成：%s\n完成任务数：%d/%d\n链路任务：",
		chainTitle, done, total)
	for _, t := range tasks {
		result := t.Result
		if result == "" {
			result = "（无）"
		}
		lines += fmt.Sprintf("\n  ✅ %s (%s) — %s", t.Title, t.AssignedTo, result)
	}
	lines += fmt.Sprintf("\nchain_id: %s", chainID)

	// Webhook content (richer Discord formatting)
	taskLines := ""
	for _, t := range tasks {
		result := t.Result
		if result == "" {
			result = "（无）"
		}
		taskLines += fmt.Sprintf("\n  • %s (%s) — %s", t.Title, t.AssignedTo, result)
	}
	webhookContent := fmt.Sprintf(
		"%s✅ **任务链完成：%s**\n完成：%d/%d%s\n`chain_id: %s`",
		s.mention(), chainTitle, done, total, taskLines, chainID,
	)

	label := "OnChainComplete:" + chainID
	s.notifyCEO(label, webhookContent, lines)
	return nil
}

// OnTaskComplete sends a CEO notification when a single task (no chain) completes.
// Triggered when task.NotifyCEOOnComplete == true && task.ChainID == "".
// Uses RetryQueue: up to 3 retries with 30s/60s/120s backoff.
func (s *SessionNotifier) OnTaskComplete(task model.Task) error {
	result := task.Result
	if result == "" {
		result = "（无）"
	}
	sessionMsg := fmt.Sprintf("[agent-queue] ✅ 任务完成：%s\n执行人：%s\n结果：%s\ntask_id: %s",
		task.Title, task.AssignedTo, result, task.ID)
	webhookContent := fmt.Sprintf(
		"%s✅ **任务完成：%s**\n**执行人：** %s\n**结果：** %s\n`task_id: %s`",
		s.mention(), task.Title, task.AssignedTo, result, task.ID,
	)

	label := "OnTaskComplete:" + task.ID
	s.notifyCEO(label, webhookContent, sessionMsg)
	return nil
}
