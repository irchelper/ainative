package fsm_test

import (
	"testing"

	"github.com/irchelper/agent-queue/internal/fsm"
	"github.com/irchelper/agent-queue/internal/model"
)

func TestValidate_AllowedTransitions(t *testing.T) {
	cases := []struct {
		name           string
		from           model.Status
		to             model.Status
		requiresReview bool
	}{
		{"pending→claimed", model.StatusPending, model.StatusClaimed, false},
		{"pending→cancelled", model.StatusPending, model.StatusCancelled, false},
		{"claimed→in_progress", model.StatusClaimed, model.StatusInProgress, false},
		{"claimed→pending(release)", model.StatusClaimed, model.StatusPending, false},
		{"in_progress→done (no review)", model.StatusInProgress, model.StatusDone, false},
		{"in_progress→review (requires_review=true)", model.StatusInProgress, model.StatusReview, true},
		{"in_progress→blocked", model.StatusInProgress, model.StatusBlocked, false},
		{"in_progress→failed", model.StatusInProgress, model.StatusFailed, false},
		{"in_progress→pending(timeout)", model.StatusInProgress, model.StatusPending, false},
		{"review→done", model.StatusReview, model.StatusDone, true},
		{"review→in_progress(revise)", model.StatusReview, model.StatusInProgress, true},
		{"review→failed", model.StatusReview, model.StatusFailed, true},
		{"blocked→pending", model.StatusBlocked, model.StatusPending, false},
		{"blocked→in_progress", model.StatusBlocked, model.StatusInProgress, false},
		{"failed→pending(retry)", model.StatusFailed, model.StatusPending, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := fsm.Validate(tc.from, tc.to, tc.requiresReview); err != nil {
				t.Errorf("expected transition to be allowed, got error: %v", err)
			}
		})
	}
}

func TestValidate_ForbiddenTransitions(t *testing.T) {
	cases := []struct {
		name           string
		from           model.Status
		to             model.Status
		requiresReview bool
	}{
		{"done→pending (terminal)", model.StatusDone, model.StatusPending, false},
		{"done→in_progress (terminal)", model.StatusDone, model.StatusInProgress, false},
		{"done→failed (terminal)", model.StatusDone, model.StatusFailed, false},
		{"cancelled→pending (terminal)", model.StatusCancelled, model.StatusPending, false},
		{"pending→in_progress (skip claimed)", model.StatusPending, model.StatusInProgress, false},
		{"pending→done (skip all)", model.StatusPending, model.StatusDone, false},
		{"in_progress→done when requires_review=true", model.StatusInProgress, model.StatusDone, true},
		{"in_progress→review when requires_review=false", model.StatusInProgress, model.StatusReview, false},
		{"failed→in_progress (must go through pending)", model.StatusFailed, model.StatusInProgress, false},
		{"failed→done (must go through pending)", model.StatusFailed, model.StatusDone, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := fsm.Validate(tc.from, tc.to, tc.requiresReview); err == nil {
				t.Errorf("expected transition to be rejected, but got no error")
			}
		})
	}
}
