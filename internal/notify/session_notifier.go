// Package notify – SessionNotifier sends CEO alerts via OpenClaw sessions_send.
package notify

import (
	"fmt"
	"log"

	"github.com/irchelper/agent-queue/internal/model"
	"github.com/irchelper/agent-queue/internal/openclaw"
)

// CEOSessionKey is the default CEO session for alerts.
const CEOSessionKey = "agent:ceo:discord:channel:1475338424293789877"

// SessionNotifier notifies the CEO session when a task fails without a
// retry_assigned_to directive (i.e., human intervention is needed).
type SessionNotifier struct {
	client       *openclaw.Client
	ceoSessionKey string
}

// NewSessionNotifier creates a SessionNotifier targeting the given CEO session.
// If ceoSessionKey is empty, CEOSessionKey is used.
func NewSessionNotifier(client *openclaw.Client, ceoSessionKey string) *SessionNotifier {
	if ceoSessionKey == "" {
		ceoSessionKey = CEOSessionKey
	}
	return &SessionNotifier{client: client, ceoSessionKey: ceoSessionKey}
}

// OnFailed sends a CEO alert for a failed task that has no retry directive.
// Called asynchronously from handler; errors are logged, not propagated.
func (s *SessionNotifier) OnFailed(task model.Task) error {
	msg := fmt.Sprintf(
		"[agent-queue] ⚠️ 任务失败需介入：%s\nresult: %s\ntask_id: %s",
		task.Title, task.Result, task.ID,
	)
	if err := s.client.SendToSession(s.ceoSessionKey, msg); err != nil {
		log.Printf("[session_notifier] OnFailed → %s failed: %v", s.ceoSessionKey, err)
		return err
	}
	log.Printf("[session_notifier] OnFailed → %s: notified CEO", s.ceoSessionKey)
	return nil
}

// AsyncOnFailed runs OnFailed in a goroutine so the handler is never blocked.
func AsyncOnFailed(sn *SessionNotifier, task model.Task) {
	go func() {
		if err := sn.OnFailed(task); err != nil {
			log.Printf("[session_notifier] async OnFailed error for task %s: %v", task.ID, err)
		}
	}()
}
