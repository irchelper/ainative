package config_test

import (
	"os"
	"testing"

	"github.com/irchelper/agent-queue/internal/config"
)

func TestDefaults(t *testing.T) {
	cfg := config.Defaults()
	if cfg.Server.Port != 19827 {
		t.Errorf("expected port 19827, got %d", cfg.Server.Port)
	}
	if cfg.Server.DB != "data/queue.db" {
		t.Errorf("expected db data/queue.db, got %s", cfg.Server.DB)
	}
	if len(cfg.Agents) == 0 {
		t.Error("expected non-empty default agents")
	}
	if cfg.Timeouts.MaxStaleDispatches != 3 {
		t.Errorf("expected MaxStaleDispatches=3, got %d", cfg.Timeouts.MaxStaleDispatches)
	}
}

func TestLoadNonExistent(t *testing.T) {
	// Non-existent file → use defaults.
	cfg, err := config.Load("/tmp/nonexistent-agent-queue-config-xyz.yaml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Server.Port != 19827 {
		t.Errorf("expected default port, got %d", cfg.Server.Port)
	}
}

func TestLoadFromYAML(t *testing.T) {
	f, err := os.CreateTemp("", "aq-config-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())

	_, _ = f.WriteString(`
server:
  port: 12345
  db: /tmp/test.db
`)
	f.Close()

	cfg, err := config.Load(f.Name())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Server.Port != 12345 {
		t.Errorf("expected port 12345, got %d", cfg.Server.Port)
	}
	if cfg.Server.DB != "/tmp/test.db" {
		t.Errorf("expected db /tmp/test.db, got %s", cfg.Server.DB)
	}
}

func TestEnvOverride(t *testing.T) {
	t.Setenv("AGENT_QUEUE_PORT", "9999")
	t.Setenv("AGENT_QUEUE_DB", "/tmp/env.db")

	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Server.Port != 9999 {
		t.Errorf("expected port 9999, got %d", cfg.Server.Port)
	}
	if cfg.Server.DB != "/tmp/env.db" {
		t.Errorf("expected db /tmp/env.db, got %s", cfg.Server.DB)
	}
}

func TestAgentWebhooksParsing(t *testing.T) {
	t.Setenv("AGENT_QUEUE_AGENT_WEBHOOKS", "coder=https://discord.com/api/webhooks/1,thinker=https://discord.com/api/webhooks/2")
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Notifications.AgentWebhooks["coder"] != "https://discord.com/api/webhooks/1" {
		t.Errorf("unexpected coder webhook: %s", cfg.Notifications.AgentWebhooks["coder"])
	}
	if cfg.Notifications.AgentWebhooks["thinker"] != "https://discord.com/api/webhooks/2" {
		t.Errorf("unexpected thinker webhook: %s", cfg.Notifications.AgentWebhooks["thinker"])
	}
}
