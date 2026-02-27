[中文文档](./README.zh-CN.md)

# agent-queue

**Stop babysitting your AI agents.** agent-queue is a lightweight task queue that lets multiple AI agents coordinate autonomously — no central orchestrator required.

Agents poll for work, claim tasks atomically, and report completion via HTTP. Serial chains run end-to-end without human intervention: when task A finishes, task B unlocks automatically; the next agent picks it up on its next poll cycle.

Built with SQLite + Go. Single binary, zero external dependencies, runs on your laptop.

## Why agent-queue?

Without a persistent task queue, multi-agent systems break in predictable ways:

- **The orchestrator bottleneck**: A "CEO" agent must stay online to push each step forward. It sleeps → the chain stalls.
- **Lost state**: Task status lives in LLM context. Context compression or a new session = lost progress.
- **Silent failures**: Agents complete work but no one is notified. Users ask "did it finish?" instead of being told.

agent-queue moves task state out of agent memory and into SQLite. Any agent can crash and recover. Chains advance automatically. Completions notify you directly.

## Features

- **F1 — Task CRUD**: Create/query/update tasks with full lifecycle support
- **F2 — Optimistic lock claim**: Atomic claim with `version` field; concurrent claim → 409 Conflict
- **F3 — Dependency graph**: `depends_on` array; upstream `done` → downstream auto-unlocked
- **F4 — 8-state machine**: `pending → claimed → in_progress → review → done / blocked / failed / cancelled`; `cancelled` is a terminal state that does not trigger autoRetry or unlock downstream deps
- **F5 — Health check**: `GET /health` returns service + database status
- **F6 — Discord webhook**: Task `done`/`failed` → async POST to Discord Incoming Webhook; `failed` also triggers SessionNotifier → CEO (via `Notifier` interface)
- **F7 — Atomic dispatch**: `POST /dispatch` creates task + triggers agent session in one call
- **F8 — Summary panel**: `GET /tasks/summary` returns global counts + active task list
- **F9 — Agent self-poll**: `GET /tasks/poll?assigned_to=X` returns the best available task for an agent (deps-aware, priority-sorted)
- **F10 — Chain dispatch**: `POST /dispatch/chain` creates a full serial chain with `depends_on` set automatically
- **F13 — Review-reject two-stage chain (V10)**: When thinker/security/vision fails a task and routes to a different agent, autoRetry creates a `fix task → re-review task` two-stage chain; downstream deps block until re-review approves. Supports multi-level reject via `UpdateSupersededByChain`
- **F14 — Extended retry_routing seed (V10.1)**: 16 seed rules covering vision/pm/ops default routing; vision added to `isReviewReject` reviewer list
- **F19 — Single-task CEO notification (e2f142a)**: `POST /dispatch` and `POST /tasks` now support `notify_ceo_on_complete: true` for standalone tasks (no chain required); uses the same RetryQueue backoff (30s/60s/120s) as chain completion

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
| `AGENT_QUEUE_DISCORD_WEBHOOK_URL` | Discord Incoming Webhook URL for task completion/failure notifications | No |
| `AGENT_QUEUE_AGENT_WEBHOOKS` | Per-agent webhook URLs (format: `agent1=url1,agent2=url2`); routes done/failed by `assigned_to`, falls back to default URL on miss (V11) | No |
| `AGENT_QUEUE_OPENCLAW_API_URL` | OpenClaw gateway URL for `/dispatch` and SessionNotifier (default: `http://localhost:18789`) | No |
| `AGENT_QUEUE_OPENCLAW_API_KEY` | OpenClaw gateway token for `/dispatch` and SessionNotifier | No |
| `AGENT_QUEUE_DB_PATH` | Override default SQLite database path (recommended: absolute path to avoid WorkingDirectory issues) | No |
| `AGENT_QUEUE_STALE_CHECK_INTERVAL` | Stale task scan interval (default: `10m`) | No |
| `AGENT_QUEUE_STALE_THRESHOLD` | Task is stale if unclaimed beyond this duration (default: `30m`) | No |
| `AGENT_QUEUE_MAX_STALE_DISPATCHES` | Max stale re-dispatch count before alerting CEO (default: `3`) (V11) | No |

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
| `POST` | `/dispatch` | Create task + trigger agent session atomically; supports `notify_ceo_on_complete` (bool) — notifies CEO session via SessionNotifier when task completes |
| `POST` | `/dispatch/chain` | Create full serial chain with auto-set `depends_on` |
| `GET` | `/tasks/poll` | Best available task for agent (`?assigned_to=X`); returns `null` if none |
| `GET` | `/tasks/summary` | Global task counts + active task list |

### Example: Autonomous serial chain

Submit a full chain in one call. Agents pick up their tasks when ready — no manual handoff.

```bash
# CEO submits the full chain
curl -s -X POST localhost:19827/dispatch/chain \
  -H 'Content-Type: application/json' \
  -d '{
    "tasks": [
      {"title": "Implement feature", "assigned_to": "coder"},
      {"title": "Write tests",       "assigned_to": "qa"},
      {"title": "Update docs",       "assigned_to": "writer"}
    ]
  }'
# Returns all task IDs with depends_on set: coder → qa → writer

# Each agent polls on session startup (self-driven)
RESP=$(curl -s "localhost:19827/tasks/poll?assigned_to=coder")
TASK_ID=$(echo $RESP | jq -r '.task.id // empty')

if [ -n "$TASK_ID" ]; then
  VER=$(echo $RESP | jq -r '.task.version')

  curl -s -X POST "localhost:19827/tasks/$TASK_ID/claim" \
    -H 'Content-Type: application/json' \
    -d "{\"version\":$VER,\"agent\":\"coder\"}"

  curl -s -X PATCH "localhost:19827/tasks/$TASK_ID" \
    -H 'Content-Type: application/json' \
    -d "{\"status\":\"in_progress\",\"version\":$((VER+1))}"

  # ... do the work ...

  curl -s -X PATCH "localhost:19827/tasks/$TASK_ID" \
    -H 'Content-Type: application/json' \
    -d "{\"status\":\"done\",\"result\":\"Feature implemented\",\"version\":$((VER+2))}"
  # qa task auto-unlocks; qa agent picks it up on next poll
fi
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
make test      # run all tests (with -race)
make vet       # go vet
make build     # compile
make clean     # remove binary only (safe — does NOT delete database)
make clean-all # remove binary AND database (destructive — clears all task history)
```

> **Note:** Never run `make clean-all` during active task execution — it will permanently delete all task records.

## Architecture

- **Storage**: SQLite WAL mode — single file, zero deployment, ACID transactions; path configurable via `AGENT_QUEUE_DB_PATH`
- **API**: Go `net/http`, no framework, ~800 lines (handler) + ~700 lines (store)
- **Concurrency**: Optimistic locking via `version` field
- **Agent reporting**: Agents only `PATCH /tasks` — no `sessions_send` required. Go server webhook is the sole notification channel.
- **Notifications**: Discord Incoming Webhook (`done`/`failed` → user) + SessionNotifier (`failed` → CEO session, minimal format to prevent LLM misinterpretation), via `Notifier` interface (platform-agnostic)
- **Deployment**: launchd (macOS) / systemd (Linux), KeepAlive auto-restart

Full architecture: [`docs/ARCH.md`](./docs/ARCH.md) | Product spec: [`docs/PRD.md`](./docs/PRD.md)

## License

MIT
