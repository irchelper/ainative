// Package config loads agent-queue configuration from YAML file and environment variables.
// Priority (highest first): flag > env var > config file > default value.
// No external dependencies (no Viper); uses gopkg.in/yaml.v3.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Agent defines a known agent with its display label and session key.
type Agent struct {
	Name       string `yaml:"name"`
	Label      string `yaml:"label"`
	SessionKey string `yaml:"session_key"`
}

// ServerConfig holds HTTP server settings.
type ServerConfig struct {
	Port int    `yaml:"port"`
	DB   string `yaml:"db"`
}

// TimeoutConfig holds timing-related settings.
type TimeoutConfig struct {
	AgentMinutes         int           `yaml:"agent_minutes"`
	HeartbeatMiss        int           `yaml:"heartbeat_miss"`
	StaleCheckInterval   time.Duration `yaml:"stale_check_interval"`
	StaleThreshold       time.Duration `yaml:"stale_threshold"`
	MaxStaleDispatches   int           `yaml:"max_stale_dispatches"`
}

// NotificationConfig holds notification channel settings.
type NotificationConfig struct {
	WebhookURL    string            `yaml:"webhook_url"`
	AgentWebhooks map[string]string `yaml:"agent_webhooks"`
	OpenClawURL   string            `yaml:"openclaw_url"`
	OpenClawKey   string            `yaml:"openclaw_key"`
}

// WebConfig holds frontend serving settings.
type WebConfig struct {
	StaticDir string `yaml:"static_dir"`
}

// Config is the top-level configuration structure.
type Config struct {
	Server        ServerConfig       `yaml:"server"`
	Agents        []Agent            `yaml:"agents"`
	Timeouts      TimeoutConfig      `yaml:"timeouts"`
	Notifications NotificationConfig `yaml:"notifications"`
	Web           WebConfig          `yaml:"web"`
}

// Defaults returns a Config populated with sensible default values.
func Defaults() Config {
	return Config{
		Server: ServerConfig{
			Port: 19827,
			DB:   "data/queue.db",
		},
		Agents: []Agent{
			{Name: "coder", Label: "工程师"},
			{Name: "thinker", Label: "架构师"},
			{Name: "writer", Label: "文档工程师"},
			{Name: "devops", Label: "运维工程师"},
			{Name: "security", Label: "安全工程师"},
			{Name: "qa", Label: "质量工程师"},
			{Name: "vision", Label: "视觉验收"},
			{Name: "uiux", Label: "UI/UX设计师"},
			{Name: "pm", Label: "产品经理"},
			{Name: "ops", Label: "研究员"},
		},
		Timeouts: TimeoutConfig{
			AgentMinutes:       30,
			HeartbeatMiss:      2,
			StaleCheckInterval: 10 * time.Minute,
			StaleThreshold:     30 * time.Minute,
			MaxStaleDispatches: 3,
		},
		Notifications: NotificationConfig{
			OpenClawURL: "http://localhost:18789",
		},
	}
}

// Load reads configuration from the YAML file at path (if it exists),
// then overlays environment variables on top. Returns defaults if no file found.
func Load(path string) (Config, error) {
	cfg := Defaults()

	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				return cfg, fmt.Errorf("read config %q: %w", path, err)
			}
			// file not found: use defaults + env
		} else {
			if err := yaml.Unmarshal(data, &cfg); err != nil {
				return cfg, fmt.Errorf("parse config %q: %w", path, err)
			}
		}
	}

	// Overlay environment variables (backward compat with existing env vars).
	applyEnv(&cfg)
	return cfg, nil
}

// LoadAuto discovers config.yaml in the current directory (or uses defaults).
func LoadAuto() (Config, error) {
	for _, candidate := range []string{"config.yaml", "config.yml"} {
		if _, err := os.Stat(candidate); err == nil {
			return Load(candidate)
		}
	}
	cfg := Defaults()
	applyEnv(&cfg)
	return cfg, nil
}

// applyEnv overlays known environment variables onto cfg.
func applyEnv(cfg *Config) {
	if v := os.Getenv("AGENT_QUEUE_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			cfg.Server.Port = p
		}
	}
	if v := os.Getenv("AGENT_QUEUE_DB"); v != "" {
		cfg.Server.DB = v
	}
	if v := os.Getenv("AGENT_QUEUE_DISCORD_WEBHOOK_URL"); v != "" {
		cfg.Notifications.WebhookURL = v
	}
	if v := os.Getenv("AGENT_QUEUE_OPENCLAW_URL"); v != "" {
		cfg.Notifications.OpenClawURL = v
	}
	if v := os.Getenv("AGENT_QUEUE_OPENCLAW_KEY"); v != "" {
		cfg.Notifications.OpenClawKey = v
	}
	if v := os.Getenv("AGENT_QUEUE_STATIC_DIR"); v != "" {
		cfg.Web.StaticDir = v
	}
	if v := os.Getenv("AGENT_QUEUE_MAX_STALE_DISPATCHES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Timeouts.MaxStaleDispatches = n
		}
	}
	if v := os.Getenv("AGENT_QUEUE_AGENT_WEBHOOKS"); v != "" {
		cfg.Notifications.AgentWebhooks = parseAgentWebhooksEnv(v)
	}
}

// parseAgentWebhooksEnv parses "agent1=url1,agent2=url2" format.
func parseAgentWebhooksEnv(v string) map[string]string {
	m := make(map[string]string)
	for _, pair := range strings.Split(v, ",") {
		parts := strings.SplitN(strings.TrimSpace(pair), "=", 2)
		if len(parts) == 2 {
			m[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
		}
	}
	return m
}
