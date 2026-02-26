// Package fsm implements the 7-state task state machine.
package fsm

import (
	"fmt"

	"github.com/irchelper/agent-queue/internal/model"
)

// validTransitions lists the globally allowed from→to pairs.
// requires_review conditions are enforced separately.
var validTransitions = map[model.Status]map[model.Status]bool{
	model.StatusPending: {
		model.StatusClaimed:   true,
		model.StatusCancelled: true,
	},
	model.StatusClaimed: {
		model.StatusInProgress: true,
		model.StatusPending:    true, // release
	},
	model.StatusInProgress: {
		model.StatusReview:  true,
		model.StatusDone:    true,
		model.StatusBlocked: true,
		model.StatusFailed:  true, // execution error – terminal
		model.StatusPending: true, // timeout/release
	},
	model.StatusReview: {
		model.StatusDone:       true,
		model.StatusInProgress: true, // revise/send back
	},
	model.StatusBlocked: {
		model.StatusPending:    true, // unblock
		model.StatusInProgress: true, // direct resume
	},
	// failed → pending: CEO decides to retry (with optional retry_assigned_to).
	// failed → cancelled: CEO cancels a failed task (no retry, no downstream unlock).
	model.StatusFailed: {
		model.StatusPending:    true,
		model.StatusCancelled: true,
	},
	model.StatusDone:      {}, // terminal
	model.StatusCancelled: {}, // terminal
}

// Validate checks whether the from→to transition is allowed given the task's
// requires_review setting.  Returns a descriptive error on failure.
func Validate(from, to model.Status, requiresReview bool) error {
	targets, ok := validTransitions[from]
	if !ok {
		return fmt.Errorf("unknown source status %q", from)
	}
	if !targets[to] {
		return fmt.Errorf("transition %s → %s is not allowed", from, to)
	}

	// requires_review conditional routing (only applies to in_progress)
	if from == model.StatusInProgress {
		switch to {
		case model.StatusDone:
			if requiresReview {
				return fmt.Errorf("task requires review: in_progress → done is not allowed; must transition to review first")
			}
		case model.StatusReview:
			if !requiresReview {
				return fmt.Errorf("task does not require review: in_progress → review is not allowed")
			}
		}
	}

	return nil
}
