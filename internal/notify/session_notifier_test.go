package notify_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/irchelper/agent-queue/internal/model"
	"github.com/irchelper/agent-queue/internal/notify"
	"github.com/irchelper/agent-queue/internal/openclaw"
)

func TestSessionNotifier_OnFailed_SendsAlert(t *testing.T) {
	var received map[string]any
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&received) //nolint:errcheck
		json.NewEncoder(w).Encode(map[string]any{"ok": true}) //nolint:errcheck
	}))
	defer mock.Close()

	oc := openclaw.NewWithURL(mock.URL, "tok")
	sn := notify.NewSessionNotifier(oc, "agent:ceo:discord:channel:123")

	task := model.Task{
		ID:     "f001",
		Title:  "部署服务",
		Status: model.StatusFailed,
		Result: "OOM on pod startup",
	}
	if err := sn.OnFailed(task); err != nil {
		t.Fatalf("OnFailed error: %v", err)
	}
	if received == nil {
		t.Fatal("mock received no request")
	}
	args, _ := received["args"].(map[string]any)
	if args["sessionKey"] != "agent:ceo:discord:channel:123" {
		t.Fatalf("wrong sessionKey: %v", args["sessionKey"])
	}
	msg, _ := args["message"].(string)
	if !strings.Contains(msg, "⚠️") {
		t.Errorf("message should contain warning icon")
	}
	if !strings.Contains(msg, "f001") {
		t.Errorf("message should contain task_id")
	}
	if !strings.Contains(msg, "OOM on pod startup") {
		t.Errorf("message should contain result")
	}
}

func TestSessionNotifier_OnFailed_DefaultCEOKey(t *testing.T) {
	var capturedKey string
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body) //nolint:errcheck
		args, _ := body["args"].(map[string]any)
		capturedKey, _ = args["sessionKey"].(string)
		json.NewEncoder(w).Encode(map[string]any{"ok": true}) //nolint:errcheck
	}))
	defer mock.Close()

	oc := openclaw.NewWithURL(mock.URL, "")
	// Empty ceoSessionKey → uses CEOSessionKey constant.
	sn := notify.NewSessionNotifier(oc, "")

	sn.OnFailed(model.Task{ID: "x", Title: "t", Status: model.StatusFailed}) //nolint:errcheck

	if capturedKey != notify.CEOSessionKey {
		t.Fatalf("expected default CEO key %q, got %q", notify.CEOSessionKey, capturedKey)
	}
}

func TestSessionNotifier_OnFailed_NetworkError_ReturnsErr(t *testing.T) {
	// Point to a port that refuses connections.
	oc := openclaw.NewWithURL("http://127.0.0.1:1", "")
	sn := notify.NewSessionNotifier(oc, "agent:ceo:test")

	err := sn.OnFailed(model.Task{ID: "e1", Status: model.StatusFailed})
	if err == nil {
		t.Fatal("expected error from unreachable server")
	}
}
