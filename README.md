[中文文档](./README.zh-CN.md)

# ainative

[![Release](https://img.shields.io/github/v/release/irchelper/ainative?label=release)](https://github.com/irchelper/ainative/releases/tag/v1.0.0)
[![Go](https://img.shields.io/badge/go-1.22+-00ADD8)](https://go.dev/)
[![License](https://img.shields.io/badge/license-MIT-blue)](./LICENSE)

**Stop babysitting your AI agents.** ainative is a lightweight task queue with a built-in AI-native workbench UI that lets multiple AI agents coordinate autonomously — no central orchestrator required.

Agents poll for work, claim tasks atomically, and report completion via HTTP. Serial chains run end-to-end without human intervention: when task A finishes, task B unlocks automatically. The next agent picks it up on its next poll cycle.

Single binary. Zero external dependencies. Runs on your laptop.

---

## What's in v1.0.0

**Task Engine**
- FSM state machine (8 states: pending → claimed → in_progress → done / failed / cancelled / review / blocked)
- `depends_on` dependency graph — upstream `done` auto-unlocks downstream
- `POST /dispatch/chain` serial chains; `POST /dispatch/graph` arbitrary DAG dispatch
- Dynamic priority (`priority`: 0=normal / 1=high / 2=urgent) — `ORDER BY priority DESC`
- Batch operations (`POST /api/tasks/bulk`: cancel + reassign)
- Task templates (`POST /dispatch/from-template/:name`)
- Human approval nodes in chains

**Reliability**
- RetryQueue with exponential backoff (30s / 60s / 120s)
- Auto-retry routing table (`retry_routing`) — failed tasks route to the right agent
- Stale task recovery — unclaimed tasks re-dispatched after configurable threshold
- Agent timeout auto-fail

**Notifications**
- CEO chain/task completion via SessionNotifier
- Discord per-agent webhook routing
- Outbound webhook (`AGENT_QUEUE_WEBHOOK_URL`) with HMAC-SHA256 signature

**Web UI (AI Workbench)**
- Dashboard (task cards, search, multi-select bulk ops), Kanban, TaskDetail with timeline + chain inline view
- DAG visualization (`/graph`) — Kahn BFS level layout, status-color nodes
- Agent stats panel (`/stats`) — success rate, avg duration, progress bars
- Settings page (`/settings`) — webhook status, system info
- Task comments — threaded, SSE real-time, `Ctrl+Enter` submit
- i18n Chinese/English toggle (vue-i18n@9, localStorage persistence)
- Mobile responsive — hamburger menu, breakpoint grid

**API & Docs**
- Scalar interactive API docs at `GET /docs` (OpenAPI 3.1, 18 endpoints)
- SSE real-time updates (`GET /events`)
- `GET /api/agents/stats` — per-agent aggregated metrics
- `GET /api/graph/:chain_id` — DAG graph data

---

## Quick Start

> Agent flow: agents poll (`GET /tasks/poll?assigned_to=<agent>`), claim atomically, then PATCH results. For serial workflows, use `POST /dispatch/chain` (or `POST /dispatch` for single tasks).

```bash
# 1. Clone and build
git clone https://github.com/irchelper/ainative.git
cd ainative
make build          # compiles Go binary + embeds frontend

# 2. Start the server
./agent-queue       # listening on :19827

# 3. Open the dashboard
open http://localhost:19827/

# 4. Create your first task
curl -s -X POST localhost:19827/tasks \
  -H 'Content-Type: application/json' \
  -d '{"title":"My first task","assigned_to":"coder"}'
```

Go 1.22+ required. No database setup needed — SQLite is embedded.

---

## Why ainative?

Without a persistent task queue, multi-agent systems break in predictable ways:

- **The orchestrator bottleneck**: A "CEO" agent must stay online to push each step forward. It sleeps → the chain stalls.
- **Lost state**: Task status lives in LLM context. Context compression or a new session = lost progress.
- **Silent failures**: Agents complete work but no one is notified. Users ask "did it finish?" instead of being told.

ainative moves task state out of agent memory and into SQLite. Any agent can crash and recover. Chains advance automatically. Completions notify you directly.

---

## Features

- **Task queue** — Full CRUD with optimistic locking (`version` field); concurrent claim → 409 Conflict
- **Dependency graph** — `depends_on` array; upstream `done` → downstream auto-unlocks
- **8-state machine** — `pending → claimed → in_progress → review → done / blocked / failed / cancelled`
- **Atomic dispatch** — `POST /dispatch` creates task + wakes agent session in one call
- **Serial chain dispatch** — `POST /dispatch/chain` creates a full chain with `depends_on` wired automatically
- **CEO notifications** — Task/chain completion notifies CEO session via SessionNotifier (RetryQueue: 30s/60s/120s backoff)
- **Auto retry routing** — Failed tasks route to the right agent automatically via `retry_routing` table
- **Stale task recovery** — Unclaimed tasks are re-dispatched after a configurable threshold
- **Web UI** — Built-in SPA dashboard (Vue 3 + TypeScript + Tailwind); embed.FS, no separate server needed
- **Discord webhooks** — Per-agent or global webhook for `done`/`failed` notifications
- **Health check** — `GET /health` for uptime monitoring and launchd/systemd integration

---

## API Reference

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/health` | Service and database health check |
| `POST` | `/tasks` | Create task (`title`, `assigned_to`, `depends_on`, `notify_ceo_on_complete`, …) |
| `GET` | `/tasks` | List tasks — filter by `status`, `assigned_to`, `parent_id`, `deps_met` |
| `GET` | `/tasks/:id` | Task detail with dependency chain and state history |
| `PATCH` | `/tasks/:id` | Update status/result with optimistic lock (`version` required) |
| `POST` | `/tasks/:id/claim` | Atomic claim — body: `{"version": N, "agent": "name"}` |
| `GET` | `/tasks/poll` | Best available task for agent (`?assigned_to=X`); returns `null` if none |
| `GET` | `/tasks/summary` | Global task counts + active task list |
| `POST` | `/dispatch` | Create task + trigger agent session; supports `notify_ceo_on_complete` (bool) |
| `POST` | `/dispatch/chain` | Create full serial chain with auto-set `depends_on` |

Full API spec: [`docs/api/openapi.yaml`](./docs/api/openapi.yaml)

### Agent poll loop (minimal example)

```bash
# Poll for work
RESP=$(curl -s "localhost:19827/tasks/poll?assigned_to=myagent")
TASK_ID=$(echo $RESP | jq -r '.task.id // empty')

if [ -n "$TASK_ID" ]; then
  VER=$(echo $RESP | jq -r '.task.version')

  # Claim it
  curl -s -X POST "localhost:19827/tasks/$TASK_ID/claim" \
    -H 'Content-Type: application/json' \
    -d "{\"version\":$VER,\"agent\":\"myagent\"}"

  # Mark in progress
  curl -s -X PATCH "localhost:19827/tasks/$TASK_ID" \
    -H 'Content-Type: application/json' \
    -d "{\"status\":\"in_progress\",\"version\":$((VER+1))}"

  # ... do the work ...

  # Report done
  curl -s -X PATCH "localhost:19827/tasks/$TASK_ID" \
    -H 'Content-Type: application/json' \
    -d "{\"status\":\"done\",\"result\":\"Work complete\",\"version\":$((VER+2))}"
fi
```

See [Agent Integration Guide](./docs/guides/agent-integration.md) for patterns, error handling, and OpenClaw setup.

---

## Configuration

ainative works out of the box with no configuration. All settings are optional:

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `AGENT_QUEUE_DISCORD_WEBHOOK_URL` | — | Discord Incoming Webhook for task notifications |
| `AGENT_QUEUE_AGENT_WEBHOOKS` | — | Per-agent webhooks: `agent1=url1,agent2=url2` |
| `AGENT_QUEUE_OPENCLAW_API_URL` | `http://localhost:18789` | OpenClaw gateway URL (for agent dispatch) |
| `AGENT_QUEUE_OPENCLAW_API_KEY` | — | OpenClaw gateway token |
| `AGENT_QUEUE_DB_PATH` | `data/queue.db` | SQLite database path |
| `AGENT_QUEUE_STALE_CHECK_INTERVAL` | `10m` | How often to scan for stale tasks |
| `AGENT_QUEUE_STALE_THRESHOLD` | `30m` | Task is stale if unclaimed beyond this |
| `AGENT_QUEUE_MAX_STALE_DISPATCHES` | `3` | Max re-dispatch attempts before alerting |

### Config file (optional)

Create `config.yaml` in the working directory:

```yaml
server:
  port: 19827
  db: data/queue.db
notifications:
  webhook_url: "https://discord.com/api/webhooks/..."
  openclaw_url: "http://localhost:18789"
timeouts:
  stale_check_interval: 10m
  stale_threshold: 30m
```

See [Configuration Reference](./docs/guides/configuration.md) for all options.

---

## Deployment

### macOS (launchd)

```bash
make build
# Edit launchd/com.irchelper.agent-queue.plist with your env vars
bash scripts/launchd-install.sh
curl http://localhost:19827/health   # verify

Notes:
- Update `launchd/com.irchelper.agent-queue.plist` with your real values (e.g. `AGENT_QUEUE_DISCORD_WEBHOOK_URL`, `AGENT_QUEUE_DISCORD_USER_ID`).
- `KeepAlive: true` means launchd will restart the service if it crashes.
- After editing the plist, reload the service (`launchctl unload` + `launchctl load`) for changes to take effect.
- Logs: `~/Library/Logs/agent-queue/` (e.g. `stdout.log`).
```

### Linux (systemd)

```bash
make build
# Copy agent-queue binary to /usr/local/bin/
# Create /etc/systemd/system/ainative.service (see docs/guides/configuration.md)
systemctl enable --now ainative
```

### Docker

```bash
docker run -p 19827:19827 \
  -e AGENT_QUEUE_DISCORD_WEBHOOK_URL=https://... \
  -v $(pwd)/data:/app/data \
  ghcr.io/irchelper/ainative:latest
```

---

## Development

```bash
make test      # run all tests (with -race)
make vet       # go vet
make build     # compile Go binary (embeds frontend from dist/)
make build-web # build frontend only (cd web && npm run build)
make clean     # remove binary (safe — does NOT delete database)
make clean-all # remove binary + database (destructive)
```

> **Note:** `make clean-all` permanently deletes all task records. Never run during active execution.

Frontend development (hot reload):
```bash
make dev-api          # start Go API server
cd web && npm run dev # start Vite dev server (proxies API to :19827)
```

---

## Architecture

- **Storage**: SQLite WAL mode — single file, zero ops, ACID; path via `AGENT_QUEUE_DB_PATH`
- **Backend**: Go `net/http`, no framework; optimistic locking via `version` field
- **Frontend**: Vue 3 + TypeScript + Vite + Tailwind; embedded via `embed.FS` (single binary)
- **Notifications**: Discord Incoming Webhook (user audit) + SessionNotifier (agent wakeup / CEO alerts)
- **Deployment**: Single binary; launchd (macOS) / systemd (Linux); KeepAlive auto-restart

Full architecture: [`docs/ARCH.md`](./docs/ARCH.md) | Product spec: [`docs/PRD.md`](./docs/PRD.md)

---

## Contributing

See [CONTRIBUTING.md](./CONTRIBUTING.md) for development setup, coding guidelines, and PR process.

## License

MIT
