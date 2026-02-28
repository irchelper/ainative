// Package notify provides the Notifier interface and a Discord Incoming
// Webhook implementation.
package notify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/irchelper/agent-queue/internal/model"
)

// Notifier sends a notification when a task changes state.
type Notifier interface {
	Notify(task model.Task) error
}

// NoOp is a no-operation notifier used when no webhook URL is configured.
type NoOp struct{}

func (NoOp) Notify(task model.Task) error {
	log.Printf("[notify] no-op: task %s (%s) done – webhook not configured", task.ID, task.Title)
	return nil
}

// DiscordNotifier sends a message to a Discord Incoming Webhook.
// Per-agent webhook routing: if AGENT_QUEUE_AGENT_WEBHOOKS is set (format:
// "agentId1=url1,agentId2=url2,..."), Notify uses the agent-specific URL for
// task.AssignedTo, falling back to the default webhookURL on a miss.
type DiscordNotifier struct {
	webhookURL    string
	userID        string
	agentWebhooks map[string]string // agentId → webhookURL
	client        *http.Client
}

// NewFromEnv returns a DiscordNotifier if AGENT_QUEUE_DISCORD_WEBHOOK_URL is
// set, otherwise a NoOp notifier.
// Also parses AGENT_QUEUE_AGENT_WEBHOOKS for per-agent routing.
func NewFromEnv() Notifier {
	url := os.Getenv("AGENT_QUEUE_DISCORD_WEBHOOK_URL")
	if url == "" {
		return NoOp{}
	}
	return &DiscordNotifier{
		webhookURL:    url,
		userID:        os.Getenv("AGENT_QUEUE_DISCORD_USER_ID"),
		agentWebhooks: parseAgentWebhooks(os.Getenv("AGENT_QUEUE_AGENT_WEBHOOKS")),
		client:        &http.Client{Timeout: 10 * time.Second},
	}
}

// parseAgentWebhooks parses "agent1=url1,agent2=url2,..." into a map.
// Malformed entries are silently skipped.
func parseAgentWebhooks(raw string) map[string]string {
	m := make(map[string]string)
	if raw == "" {
		return m
	}
	for _, entry := range strings.Split(raw, ",") {
		parts := strings.SplitN(strings.TrimSpace(entry), "=", 2)
		if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
			m[parts[0]] = parts[1]
		}
	}
	return m
}

// resolveWebhookURL returns the agent-specific URL for the given agentID,
// falling back to the default webhookURL if no entry is found.
func (d *DiscordNotifier) resolveWebhookURL(agentID string) string {
	if url, ok := d.agentWebhooks[agentID]; ok {
		return url
	}
	return d.webhookURL
}

// Notify sends a Discord webhook message.  It retries once on failure.
// discordRetryBackoff defines wait durations between Discord webhook retries.
// attempt 0 is immediate, 1→5s, 2→15s, 3→30s.
var discordRetryBackoff = []time.Duration{0, 5 * time.Second, 15 * time.Second, 30 * time.Second}

// Notify sends a Discord webhook message with up to 3 retries (exponential backoff).
// A final failure is logged but never blocks the caller.
func (d *DiscordNotifier) Notify(task model.Task) error {
	var err error
	for attempt, wait := range discordRetryBackoff {
		if attempt > 0 {
			log.Printf("[notify] attempt %d for task %s, waiting %v", attempt+1, task.ID, wait)
			time.Sleep(wait)
		}
		err = d.send(task)
		if err == nil {
			if attempt > 0 {
				log.Printf("[notify] task %s succeeded on attempt %d", task.ID, attempt+1)
			}
			return nil
		}
		log.Printf("[notify] attempt %d failed for task %s: %v", attempt+1, task.ID, err)
	}
	log.Printf("[notify] giving up on task %s after %d attempts: %v", task.ID, len(discordRetryBackoff), err)
	return err
}

func (d *DiscordNotifier) send(task model.Task) error {
	content := FormatMessage(task, d.userID, time.Now())
	if task.Status == model.StatusFailed {
		content = FormatFailedMessage(task, d.userID, time.Now())
	}

	body, err := json.Marshal(map[string]string{"content": content})
	if err != nil {
		return fmt.Errorf("marshal body: %w", err)
	}

	// Route to agent-specific webhook if configured; fallback to default.
	webhookURL := d.resolveWebhookURL(task.AssignedTo)
	resp, err := d.client.Post(webhookURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("http post: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// FormatMessage builds the Discord notification content for a completed task.
// userID may be empty (no @mention in that case).
// doneAt is the timestamp used for duration calculation (pass time.Now() in production).
func FormatMessage(task model.Task, userID string, doneAt time.Time) string {
	mention := ""
	if userID != "" {
		mention = fmt.Sprintf("<@%s> ", userID)
	}

	expert := task.AssignedTo
	if expert == "" {
		expert = "未知"
	}

	result := task.Result
	if result == "" {
		result = "（无）"
	}

	duration := FormatDuration(task.StartedAt, doneAt)

	return fmt.Sprintf(
		"%s✅ 任务完成\n**任务：** %s\n**专家：** %s\n**耗时：** %s\n**结果：** %s\n`task_id: %s`",
		mention, task.Title, expert, duration, result, task.ID,
	)
}

// FormatFailedMessage builds the Discord notification content for a failed task.
// Per docs/ARCH.md F6.
func FormatFailedMessage(task model.Task, userID string, doneAt time.Time) string {
	mention := ""
	if userID != "" {
		mention = fmt.Sprintf("<@%s> ", userID)
	}

	expert := task.AssignedTo
	if expert == "" {
		expert = "未知"
	}

	reason := task.FailureReason
	if reason == "" {
		reason = task.Result
	}
	if reason == "" {
		reason = "（无）"
	}

	duration := FormatDuration(task.StartedAt, doneAt)

	return fmt.Sprintf(
		"%s❌ 任务失败\n**任务：** %s\n**专家：** %s\n**耗时：** %s\n**失败原因：** %s\n`task_id: %s`",
		mention, task.Title, expert, duration, reason, task.ID,
	)
}

// FormatDuration computes a human-readable duration from startedAt to doneAt.
// If startedAt is nil, returns "未知".
// Duration < 1 min → "< 1 分钟"; otherwise → "约 X 分钟" (ceiling).
func FormatDuration(startedAt *time.Time, doneAt time.Time) string {
	if startedAt == nil {
		return "未知"
	}
	elapsed := doneAt.Sub(*startedAt)
	minutes := elapsed.Minutes()
	if minutes < 1 {
		return "< 1 分钟"
	}
	return fmt.Sprintf("约 %d 分钟", int(math.Ceil(minutes)))
}

// AsyncNotify runs n.Notify in a goroutine so the caller is never blocked.
func AsyncNotify(n Notifier, task model.Task) {
	go func() {
		if err := n.Notify(task); err != nil {
			log.Printf("[notify] async notification failed for task %s: %v", task.ID, err)
		}
	}()
}
