[中文文档](./README.zh-CN.md)

# agent-queue

A SQLite-backed task queue for OpenClaw multi-agent workflows. Coordinates multiple AI agents via HTTP — persistent state, optimistic locking, automatic dependency resolution, and Discord webhook notifications.

## Features

- **F1 — Task CRUD**: Create/query/update tasks with full lifecycle support
- **F2 — Optimistic lock claim**: Atomic claim with `version` field; concurrent claim → 409 Conflict
- **F3 — Dependency graph**: `depends_on` array; upstream `done` → downstream auto-unlocked
- **F4 — 7-state machine**: `pending → claimed → in_progress → review → done / blocked / cancelled`
- **F5 — Health check**: `GET /health` returns service + database status
- **F6 — Discord webhook**: Task `done` → async POST to Discord Incoming Webhook (via `Notifier` interface)
- **F7 — Atomic dispatch**: `POST /dispatch` creates task + triggers agent session in one call
- **F8 — Summary panel**: `GET /tasks/summary` returns global counts + active task list

## Quick Start

```bash
# Build
go build -o agent-queue .

# Run (default: localhost:19827, data/queue.db)
./agent-queue

# Custom port and database path
./agent-queue --port 8080 --db /path/to/queue.db
```

### Environment Variables

| Variable | Description | Required |
|----------|-------------|----------|
| `AGENT_QUEUE_DISCORD_WEBHOOK_URL` | Discord Incoming Webhook URL for task completion notifications | No |
| `AGENT_QUEUE_OPENCLAW_API_URL` | OpenClaw gateway URL for `/dispatch` (default: `http://localhost:18789`) | No |
| `AGENT_QUEUE_OPENCLAW_API_KEY` | OpenClaw gateway token for `/dispatch` | No |

## API Reference

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/health` | Service and database health check |
| `POST` | `/tasks` | Create task (supports `depends_on`, `requires_review`, `parent_id`) |
| `GET` | `/tasks` | List tasks — filter by `status`, `assigned_to`, `parent_id`, `deps_met` |
| `GET` | `/tasks/:id` | Task detail with dependency chain and state history |
| `PATCH` | `/tasks/:id` | Update status/result with optimistic lock (`version` required) |
| `POST` | `/tasks/:id/claim` | Atomic claim — body: `{"version": N, "agent": "name"}` |
| `GET` | `/tasks/:id/deps-met` | Check if all dependencies are satisfied |
| `POST` | `/dispatch` | Create task + trigger agent session atomically |
| `GET` | `/tasks/summary` | Global task counts + active task list |

### Example: Serial task chain

```bash
# Create task A
A=$(curl -s -X POST localhost:19827/tasks \
  -H 'Content-Type: application/json' \
  -d '{"title":"Step A","assigned_to":"coder"}' | jq -r .id)

# Create task B depending on A
curl -s -X POST localhost:19827/tasks \
  -H 'Content-Type: application/json' \
  -d "{\"title\":\"Step B\",\"assigned_to\":\"qa\",\"depends_on\":[\"$A\"]}"

# Claim and complete A (unlocks B automatically)
VER=$(curl -s localhost:19827/tasks/$A | jq .version)
curl -s -X POST localhost:19827/tasks/$A/claim \
  -H 'Content-Type: application/json' \
  -d "{\"version\":$VER,\"agent\":\"coder\"}"

curl -s -X PATCH localhost:19827/tasks/$A \
  -H 'Content-Type: application/json' \
  -d "{\"status\":\"done\",\"result\":\"Step A complete\",\"version\":$((VER+1))}"
```

## Deployment Config

### macOS launchd

Edit `launchd/com.irchelper.agent-queue.plist` with your values, then:

```bash
make build
bash scripts/launchd-install.sh    # install and start
curl http://localhost:19827/health  # verify
```

### Environment variables in plist

```xml
<key>EnvironmentVariables</key>
<dict>
  <key>AGENT_QUEUE_DISCORD_WEBHOOK_URL</key>
  <string>https://discord.com/api/webhooks/...</string>
  <key>AGENT_QUEUE_OPENCLAW_API_URL</key>
  <string>http://localhost:18789</string>
  <key>AGENT_QUEUE_OPENCLAW_API_KEY</key>
  <string>your-gateway-token</string>
</dict>
```

### OpenClaw Gateway config (required for `/dispatch`)

Add to `openclaw.json`:

```json
{
  "gateway": {
    "tools": {
      "allow": ["sessions_send"]
    }
  }
}
```

## Development

```bash
make test    # run all tests (with -race)
make vet     # go vet
make build   # compile
```

## Architecture

- **Storage**: SQLite WAL mode — single file, zero deployment, ACID transactions
- **API**: Go `net/http`, no framework, ~350 lines
- **Concurrency**: Optimistic locking via `version` field
- **Notifications**: Discord Incoming Webhook via `Notifier` interface (platform-agnostic)
- **Deployment**: launchd (macOS) / systemd (Linux), KeepAlive auto-restart

Full architecture: [`docs/ARCH.md`](./docs/ARCH.md) | Product spec: [`docs/PRD.md`](./docs/PRD.md)

## License

MIT
