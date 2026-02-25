package failparser_test

import (
	"testing"

	"github.com/irchelper/agent-queue/internal/failparser"
)

func TestParseRetryAgent(t *testing.T) {
	cases := []struct {
		name      string
		result    string
		wantAgent string
		wantOK    bool
	}{
		{
			name:      "standard pipe format",
			result:    "bug: 登录按钮无响应 | retry_assigned_to: coder",
			wantAgent: "coder",
			wantOK:    true,
		},
		{
			name:      "no space after colon",
			result:    "OOM crash | retry_assigned_to:devops",
			wantAgent: "devops",
			wantOK:    true,
		},
		{
			name:      "multiple spaces after colon",
			result:    "retry_assigned_to:   thinker",
			wantAgent: "thinker",
			wantOK:    true,
		},
		{
			name:      "agent with hyphen",
			result:    "retry_assigned_to: code-review",
			wantAgent: "code-review",
			wantOK:    true,
		},
		{
			name:      "at end of string",
			result:    "deployment failed retry_assigned_to: ops",
			wantAgent: "ops",
			wantOK:    true,
		},
		{
			name:   "no directive",
			result: "everything fine but failed anyway",
			wantOK: false,
		},
		{
			name:   "empty result",
			result: "",
			wantOK: false,
		},
		{
			name:   "partial keyword no agent",
			result: "retry_assigned_to:",
			wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agent, ok := failparser.ParseRetryAgent(tc.result)
			if ok != tc.wantOK {
				t.Fatalf("ok=%v, want %v (result=%q)", ok, tc.wantOK, tc.result)
			}
			if ok && agent != tc.wantAgent {
				t.Fatalf("agent=%q, want %q", agent, tc.wantAgent)
			}
		})
	}
}
