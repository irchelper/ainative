package notify

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/irchelper/agent-queue/internal/model"
	"github.com/irchelper/agent-queue/internal/openclaw"
)

// TestDiscordNotifier_Retry_SuccessOnSecondAttempt verifies that Discord
// webhook retries on transient failure and succeeds on subsequent attempt.
func TestDiscordNotifier_Retry_SuccessOnSecondAttempt(t *testing.T) {
	// Override backoff to be instant for testing.
	orig := discordRetryBackoff
	discordRetryBackoff = []time.Duration{0, 0, 0, 0}
	defer func() { discordRetryBackoff = orig }()

	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n <= 2 {
			w.WriteHeader(http.StatusInternalServerError) // fail first 2
			return
		}
		w.WriteHeader(http.StatusOK) // succeed on 3rd
	}))
	defer srv.Close()

	d := &DiscordNotifier{
		webhookURL: srv.URL,
		client:     &http.Client{Timeout: 5 * time.Second},
	}

	task := model.Task{ID: "retry-ok", Title: "test", Status: model.StatusDone}
	err := d.Notify(task)
	if err != nil {
		t.Fatalf("expected success after retry, got: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Errorf("expected 3 attempts, got %d", got)
	}
}

// TestDiscordNotifier_Retry_AllFail verifies that when all retries fail,
// Notify returns an error.
func TestDiscordNotifier_Retry_AllFail(t *testing.T) {
	orig := discordRetryBackoff
	discordRetryBackoff = []time.Duration{0, 0, 0, 0}
	defer func() { discordRetryBackoff = orig }()

	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	d := &DiscordNotifier{
		webhookURL: srv.URL,
		client:     &http.Client{Timeout: 5 * time.Second},
	}

	task := model.Task{ID: "retry-fail", Title: "test", Status: model.StatusDone}
	err := d.Notify(task)
	if err == nil {
		t.Fatal("expected error after all retries exhausted")
	}
	if got := atomic.LoadInt32(&attempts); got != 4 {
		t.Errorf("expected 4 attempts (initial + 3 retries), got %d", got)
	}
}

// TestDiscordNotifier_Retry_ImmediateSuccess verifies no unnecessary retries
// when the first attempt succeeds.
func TestDiscordNotifier_Retry_ImmediateSuccess(t *testing.T) {
	orig := discordRetryBackoff
	discordRetryBackoff = []time.Duration{0, 0, 0, 0}
	defer func() { discordRetryBackoff = orig }()

	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := &DiscordNotifier{
		webhookURL: srv.URL,
		client:     &http.Client{Timeout: 5 * time.Second},
	}

	task := model.Task{ID: "no-retry", Title: "test", Status: model.StatusDone}
	err := d.Notify(task)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("expected 1 attempt (no retry needed), got %d", got)
	}
}

// TestOutboundWebhook_SendWithRetry_SuccessOnSecond verifies outbound webhook
// retries and eventually succeeds.
// Uses default backoff (0/2s/5s) — waits for goroutine via done channel.
func TestOutboundWebhook_SendWithRetry_SuccessOnSecond(t *testing.T) {
	var mu sync.Mutex
	var attempts int
	done := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable) // fail first
			return
		}
		w.WriteHeader(http.StatusOK) // succeed on 2nd
		// Signal done after success.
		select {
		case done <- struct{}{}:
		default:
		}
	}))
	defer srv.Close()

	n := NewOutboundWebhookNotifier(srv.URL, "test-secret")
	task := model.Task{ID: "owh-retry", Title: "webhook retry", Status: model.StatusDone, AssignedTo: "qa"}
	_ = n.Notify(task)

	// Wait for success signal (max 10s).
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for retry success")
	}

	mu.Lock()
	gotAttempts := attempts
	mu.Unlock()

	if gotAttempts != 2 {
		t.Errorf("expected 2 attempts, got %d", gotAttempts)
	}
}

// TestOutboundWebhook_SendWithRetry_AllFail verifies that all retries are
// attempted when every attempt fails.
// Uses default backoff (0/2s/5s) — waits for goroutine via done channel.
func TestOutboundWebhook_SendWithRetry_AllFail(t *testing.T) {
	done := make(chan struct{})
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusInternalServerError)
		// Signal done after last attempt (3 total with default backoff).
		if n == int32(len(outboundRetryBackoff)) {
			close(done)
		}
	}))
	defer srv.Close()

	n := NewOutboundWebhookNotifier(srv.URL, "")
	task := model.Task{ID: "owh-allfail", Title: "t", Status: model.StatusDone}
	_ = n.Notify(task)

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for all retry attempts")
	}

	// Small grace period for goroutine cleanup.
	time.Sleep(50 * time.Millisecond)

	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Errorf("expected 3 attempts, got %d", got)
	}
}

// TestOutboundWebhook_Send_ReturnsError verifies that send() returns an error
// on non-2xx response (方案B: send() now returns error instead of just logging).
func TestOutboundWebhook_Send_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	n := NewOutboundWebhookNotifier(srv.URL, "")
	payload := OutboundWebhookEvent{
		Event:  "task.done",
		TaskID: "err-test",
	}
	err := n.send(payload)
	if err == nil {
		t.Fatal("expected error from send() on 502 response")
	}
}

// TestSessionNotifier_NotifyCEO_WebhookViaRetryQueue verifies that when
// notifyCEO is called with a webhook URL, both the webhook and sessions_send
// are enqueued via RetryQueue (方案C).
func TestSessionNotifier_NotifyCEO_WebhookViaRetryQueue(t *testing.T) {
	var webhookCalls int32
	var sessionCalls int32

	webhookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&webhookCalls, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer webhookSrv.Close()

	sessionSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&sessionCalls, 1)
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer sessionSrv.Close()

	sn := &SessionNotifier{
		client:        newTestOCClient(sessionSrv.URL),
		ceoSessionKey: "agent:ceo:test",
		ceoWebhookURL: webhookSrv.URL,
		retryQ:        NewRetryQueue(),
		httpClient:    &http.Client{Timeout: 5 * time.Second},
	}
	sn.Start()
	defer sn.Stop()

	sn.notifyCEO("test-label", "webhook content", "session msg")

	// Wait for RetryQueue to process both items.
	time.Sleep(200 * time.Millisecond)

	wc := atomic.LoadInt32(&webhookCalls)
	sc := atomic.LoadInt32(&sessionCalls)

	if wc != 1 {
		t.Errorf("expected 1 webhook call, got %d", wc)
	}
	if sc != 1 {
		t.Errorf("expected 1 session call, got %d", sc)
	}
}

// TestSessionNotifier_NotifyCEO_WebhookFailEnqueuesRetry verifies that webhook
// failures in notifyCEO are enqueued into RetryQueue for later retry (方案C).
// We verify the retry item is queued (without waiting for the 10s ticker).
func TestSessionNotifier_NotifyCEO_WebhookFailEnqueuesRetry(t *testing.T) {
	var webhookCalls int32

	webhookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&webhookCalls, 1)
		w.WriteHeader(http.StatusInternalServerError) // always fail
	}))
	defer webhookSrv.Close()

	sessionSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer sessionSrv.Close()

	rq := NewRetryQueue()

	sn := &SessionNotifier{
		client:        newTestOCClient(sessionSrv.URL),
		ceoSessionKey: "agent:ceo:test",
		ceoWebhookURL: webhookSrv.URL,
		retryQ:        rq,
		httpClient:    &http.Client{Timeout: 5 * time.Second},
	}
	sn.Start()
	defer sn.Stop()

	sn.notifyCEO("test-retry", "webhook fail", "session msg")

	// Wait for initial attempt (synchronous in Enqueue).
	time.Sleep(200 * time.Millisecond)

	wc := atomic.LoadInt32(&webhookCalls)
	if wc != 1 {
		t.Errorf("expected 1 initial webhook call, got %d", wc)
	}

	// Verify retry item was enqueued.
	if rq.Len() == 0 {
		t.Error("expected retry item to be queued after webhook failure")
	}
}

// newTestOCClient creates an openclaw.Client pointing to the given mock URL.
func newTestOCClient(url string) *openclaw.Client {
	return openclaw.NewWithURL(url, "test-token")
}
