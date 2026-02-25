// Package failparser extracts structured metadata from task result strings.
//
// Convention: when a task fails, the expert may embed a retry directive inside
// the result field:
//
//	"bug: 登录按钮无响应 | retry_assigned_to: coder"
//
// ParseRetryAgent extracts the agent name after "retry_assigned_to:" and
// returns ("", false) when the directive is absent.
package failparser

import (
	"regexp"
	"strings"
)

// retryRe matches "retry_assigned_to: <agent>" anywhere in the result string.
// Agent name: word characters only (letters, digits, underscore, hyphen).
var retryRe = regexp.MustCompile(`retry_assigned_to:\s*([A-Za-z0-9_-]+)`)

// ParseRetryAgent returns the agent name embedded in result, if any.
func ParseRetryAgent(result string) (agent string, ok bool) {
	m := retryRe.FindStringSubmatch(result)
	if len(m) < 2 {
		return "", false
	}
	return strings.TrimSpace(m[1]), true
}
