package notify

import (
	"fmt"
	"log"
	"time"

	"github.com/irchelper/agent-queue/internal/model"
	"github.com/irchelper/agent-queue/internal/openclaw"
)

// SessionNotifier sends task notifications to an OpenClaw agent session via
// sessions_send. It implements the Notifier interface.
//
// Typical use: notify the CEO session or the assigned expert when a task
// transitions to done or failed.
type SessionNotifier struct {
	client     *openclaw.Client
	sessionKey string // target session (e.g. "agent:ceo:discord:channel:xxx")
}

// NewSessionNotifier builds a SessionNotifier targeting the given session.
func NewSessionNotifier(client *openclaw.Client, sessionKey string) *SessionNotifier {
	return &SessionNotifier{client: client, sessionKey: sessionKey}
}

// Notify sends a sessions_send message for the given task.
// Retries once on failure. Non-blocking when called via AsyncNotify.
func (s *SessionNotifier) Notify(task model.Task) error {
	msg := FormatSessionMessage(task, time.Now())
	err := s.client.SendToSession(s.sessionKey, msg)
	if err != nil {
		log.Printf("[notify/session] first attempt failed for task %s → %s: %v – retrying", task.ID, s.sessionKey, err)
		err = s.client.SendToSession(s.sessionKey, msg)
		if err != nil {
			log.Printf("[notify/session] retry failed for task %s → %s: %v", task.ID, s.sessionKey, err)
		}
	}
	return err
}

// FormatSessionMessage builds the sessions_send message for a task state change.
// Covers both done and failed states.
func FormatSessionMessage(task model.Task, doneAt time.Time) string {
	icon := "✅"
	verb := "任务完成"
	if task.Status == model.StatusFailed {
		icon = "❌"
		verb = "任务失败"
	}

	expert := task.AssignedTo
	if expert == "" {
		expert = "未知"
	}

	result := task.Result
	if result == "" {
		if task.Status == model.StatusFailed {
			result = "（无失败原因）"
		} else {
			result = "（无）"
		}
	}

	duration := FormatDuration(task.StartedAt, doneAt)

	retry := ""
	if task.Status == model.StatusFailed && task.RetryAssignedTo != "" {
		retry = fmt.Sprintf("\n**重试专家：** %s（PATCH status=pending 触发重试）", task.RetryAssignedTo)
	}

	return fmt.Sprintf(
		"%s %s\n**任务：** %s\n**专家：** %s\n**耗时：** %s\n**结果：** %s%s\n`task_id: %s`",
		icon, verb, task.Title, expert, duration, result, retry, task.ID,
	)
}

// MultiNotifier fans out to multiple notifiers. All are called; errors are
// logged but do not short-circuit subsequent notifiers.
type MultiNotifier struct {
	notifiers []Notifier
}

// NewMultiNotifier returns a Notifier that calls all provided notifiers.
func NewMultiNotifier(notifiers ...Notifier) *MultiNotifier {
	return &MultiNotifier{notifiers: notifiers}
}

func (m *MultiNotifier) Notify(task model.Task) error {
	var lastErr error
	for _, n := range m.notifiers {
		if err := n.Notify(task); err != nil {
			log.Printf("[notify/multi] notifier %T failed for task %s: %v", n, task.ID, err)
			lastErr = err
		}
	}
	return lastErr
}
