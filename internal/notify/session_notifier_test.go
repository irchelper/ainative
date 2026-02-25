package notify_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/irchelper/agent-queue/internal/model"
	"github.com/irchelper/agent-queue/internal/notify"
	"github.com/irchelper/agent-queue/internal/openclaw"
)

// -------------------------------------------------------------------
// FormatSessionMessage
// -------------------------------------------------------------------

func TestFormatSessionMessage_Done(t *testing.T) {
	now := time.Unix(1700000000, 0)
	start := time.Unix(1700000000-90, 0) // 90s earlier = 2 min
	task := model.Task{
		ID:        "abc123",
		Title:     "实现登录",
		Status:    model.StatusDone,
		AssignedTo: "coder",
		Result:    "PR #42 merged",
		StartedAt: &start,
	}
	msg := notify.FormatSessionMessage(task, now)
	if !strings.Contains(msg, "✅") {
		t.Error("done message should contain ✅")
	}
	if !strings.Contains(msg, "任务完成") {
		t.Error("done message should say 任务完成")
	}
	if !strings.Contains(msg, "PR #42 merged") {
		t.Error("message should contain result")
	}
	if !strings.Contains(msg, "abc123") {
		t.Error("message should contain task_id")
	}
	if !strings.Contains(msg, "约 2 分钟") {
		t.Errorf("message should contain duration '约 2 分钟', got: %s", msg)
	}
}

func TestFormatSessionMessage_Failed(t *testing.T) {
	task := model.Task{
		ID:              "xyz789",
		Title:           "部署服务",
		Status:          model.StatusFailed,
		AssignedTo:      "devops",
		Result:          "OOM on pod startup",
		RetryAssignedTo: "ops",
	}
	msg := notify.FormatSessionMessage(task, time.Now())
	if !strings.Contains(msg, "❌") {
		t.Error("failed message should contain ❌")
	}
	if !strings.Contains(msg, "任务失败") {
		t.Error("failed message should say 任务失败")
	}
	if !strings.Contains(msg, "ops") {
		t.Error("failed message with retry_assigned_to should mention retry agent")
	}
	if !strings.Contains(msg, "OOM on pod startup") {
		t.Error("message should contain failure result")
	}
}

func TestFormatSessionMessage_Failed_NoRetry(t *testing.T) {
	task := model.Task{
		ID:     "nrt001",
		Title:  "任务X",
		Status: model.StatusFailed,
	}
	msg := notify.FormatSessionMessage(task, time.Now())
	if strings.Contains(msg, "重试专家") {
		t.Error("no retry_assigned_to should not show retry info")
	}
}

// -------------------------------------------------------------------
// SessionNotifier.Notify (mock OpenClaw server)
// -------------------------------------------------------------------

func TestSessionNotifier_Notify_Success(t *testing.T) {
	var received map[string]any
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&received) //nolint:errcheck
		json.NewEncoder(w).Encode(map[string]any{"ok": true}) //nolint:errcheck
	}))
	defer mock.Close()

	oc := openclaw.NewWithURL(mock.URL, "tok")
	sn := notify.NewSessionNotifier(oc, "agent:ceo:discord:channel:123")

	task := model.Task{
		ID:     "t001",
		Title:  "测试通知",
		Status: model.StatusDone,
		Result: "ok",
	}
	if err := sn.Notify(task); err != nil {
		t.Fatalf("Notify returned error: %v", err)
	}
	if received == nil {
		t.Fatal("mock server received no request")
	}
	if received["tool"] != "sessions_send" {
		t.Fatalf("expected tool=sessions_send, got %v", received["tool"])
	}
	args, _ := received["args"].(map[string]any)
	if args["sessionKey"] != "agent:ceo:discord:channel:123" {
		t.Fatalf("wrong sessionKey: %v", args["sessionKey"])
	}
	msg, _ := args["message"].(string)
	if !strings.Contains(msg, "t001") {
		t.Errorf("message should contain task_id, got: %s", msg)
	}
}

func TestSessionNotifier_Notify_RetryOnFail(t *testing.T) {
	calls := 0
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			// First call fails.
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"ok":    false,
				"error": map[string]any{"message": "transient error"},
			})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true}) //nolint:errcheck
	}))
	defer mock.Close()

	oc := openclaw.NewWithURL(mock.URL, "")
	sn := notify.NewSessionNotifier(oc, "agent:ceo:discord:channel:456")
	task := model.Task{ID: "t002", Title: "retry test", Status: model.StatusDone}
	if err := sn.Notify(task); err != nil {
		t.Fatalf("Notify should succeed on retry, got: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 calls (1 fail + 1 retry), got %d", calls)
	}
}

// -------------------------------------------------------------------
// MultiNotifier
// -------------------------------------------------------------------

func TestMultiNotifier_CallsAll(t *testing.T) {
	calls := map[string]int{}

	makeN := func(name string) notify.Notifier {
		return &testNotifier{name: name, calls: calls}
	}

	mn := notify.NewMultiNotifier(makeN("A"), makeN("B"), makeN("C"))
	task := model.Task{ID: "m1", Status: model.StatusDone}
	if err := mn.Notify(task); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, name := range []string{"A", "B", "C"} {
		if calls[name] != 1 {
			t.Errorf("notifier %s: expected 1 call, got %d", name, calls[name])
		}
	}
}

type testNotifier struct {
	name  string
	calls map[string]int
}

func (n *testNotifier) Notify(_ model.Task) error {
	n.calls[n.name]++
	return nil
}
