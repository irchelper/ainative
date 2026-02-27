[English](./README.md)

# ainative

**让 AI agent 自己跑，不需要人盯着。** ainative 是一个轻量级任务队列，内置 AI-native 工作台 UI，让多个 AI agent 自主协调——不依赖任何中心调度者在线。

Agent 主动 poll 任务、原子认领、通过 HTTP 汇报完成。串行链全程自动推进：任务 A 完成后，任务 B 自动解锁；下一个 agent 在下次 poll 时认领并执行，无需人工传递接力棒。

基于 SQLite + Go 构建，单二进制，零外部依赖，本机直接运行。

## 为什么需要 ainative？

没有持久化任务队列的 multi-agent 系统会以可预期的方式崩溃：

- **调度者单点故障**："CEO" agent 必须一直在线才能推进下一步。它一睡着，整条链就卡住。
- **状态不持久**：任务状态存在 LLM 上下文里。上下文压缩或开新 session = 进度丢失。
- **沉默失败**：Agent 完成了工作，但没人收到通知。用户只能来问"做完了吗？"

ainative 把任务状态从 agent 记忆搬到 SQLite。任何 agent 崩溃后都能恢复。串行链自动推进。任务完成直接通知你。

## 功能列表

- **F1 — 任务 CRUD**：创建/查询/更新任务，完整生命周期管理
- **F2 — 乐观锁认领**：`version` 字段原子认领；并发冲突 → 409 Conflict
- **F3 — 依赖关系图**：`depends_on` 数组；前置任务 `done` → 后续任务自动解锁
- **F4 — 8 态状态机**：`pending → claimed → in_progress → review → done / blocked / failed / cancelled`
- **F5 — 健康检查**：`GET /health` 返回服务状态 + 数据库连接状态
- **F6 — Discord webhook**：任务 `done`/`failed` → 异步 POST Discord Incoming Webhook；`failed` 同时触发 SessionNotifier → CEO（通过 `Notifier` 接口抽象）
- **F7 — 原子派发**：`POST /dispatch` 一步完成建任务 + 触发 agent session
- **F8 — 全局状态面板**：`GET /tasks/summary` 返回计数 + 当前活跃任务列表
- **F9 — Agent 自驱 poll**：`GET /tasks/poll?assigned_to=X` 返回该 agent 当前最优任务（依赖感知 + 优先级排序）
- **F10 — 串行链派发**：`POST /dispatch/chain` 一次性创建完整串行链，自动设置 `depends_on`
- **F19 — 单任务 CEO 通知（e2f142a）**：`POST /dispatch` 和 `POST /tasks` 支持 `notify_ceo_on_complete: true`，单任务完成后自动通知 CEO session（无需串行链），同样走 RetryQueue（30s/60s/120s backoff）
- **Web UI（v12）** — 内置 SPA 工作台（Vue 3 + TypeScript + Vite + Tailwind），单二进制通过 embed.FS 内嵌前端；页面包括：Dashboard、目标追踪、看板、任务时间线

## 快速开始

```bash
# 构建
go build -o agent-queue .

# 运行（默认：localhost:19827，数据库 data/queue.db）
./agent-queue

# 自定义端口和数据库路径
./agent-queue --port 8080 --db /path/to/queue.db
```

### 环境变量

| 变量 | 说明 | 是否必须 |
|------|------|---------|
| `AGENT_QUEUE_DISCORD_WEBHOOK_URL` | 任务完成/失败通知的 Discord Incoming Webhook URL | 否 |
| `AGENT_QUEUE_OPENCLAW_API_URL` | `/dispatch` 和 SessionNotifier 使用的 OpenClaw gateway URL（默认 `http://localhost:18789`） | 否 |
| `AGENT_QUEUE_OPENCLAW_API_KEY` | OpenClaw gateway token，供 `/dispatch` 和 SessionNotifier 使用 | 否 |
| `AGENT_QUEUE_DB_PATH` | 覆盖默认 SQLite 数据库路径（推荐配置绝对路径，避免 WorkingDirectory 变动导致找不到 db） | 否 |

## API 参考

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/health` | 服务 + 数据库健康检查 |
| `POST` | `/tasks` | 创建任务（支持 `depends_on`、`requires_review`、`parent_id`） |
| `GET` | `/tasks` | 查询任务列表，支持 `status`/`assigned_to`/`parent_id`/`deps_met` 过滤 |
| `GET` | `/tasks/:id` | 任务详情（含依赖链 + 状态变更历史） |
| `PATCH` | `/tasks/:id` | 更新状态/result，需传 `version`（乐观锁校验） |
| `POST` | `/tasks/:id/claim` | 原子认领，body：`{"version": N, "agent": "名称"}` |
| `GET` | `/tasks/:id/deps-met` | 查询依赖是否全部满足 |
| `POST` | `/dispatch` | 原子化建任务 + 触发 agent session；支持 `notify_ceo_on_complete`（bool）— 任务完成时通过 SessionNotifier 通知 CEO session |
| `POST` | `/dispatch/chain` | 创建完整串行链，自动设置 `depends_on` |
| `GET` | `/tasks/poll` | 返回该 agent 最优可认领任务（`?assigned_to=X`），无任务返回 `null` |
| `GET` | `/tasks/summary` | 全局任务计数 + 活跃任务列表 |

### 示例：自驱串行链

一次提交完整链路，agent 各自 poll 认领，无需人工传递接力棒。

```bash
# CEO 一次性提交完整链路
curl -s -X POST localhost:19827/dispatch/chain \
  -H 'Content-Type: application/json' \
  -d '{
    "tasks": [
      {"title": "实现功能", "assigned_to": "coder"},
      {"title": "编写测试", "assigned_to": "qa"},
      {"title": "更新文档", "assigned_to": "writer"}
    ]
  }'
# 返回所有任务 ID，depends_on 已自动设置：coder → qa → writer

# 每个 agent session 启动时自驱 poll（以 coder 为例）
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

  # ... 执行任务 ...

  curl -s -X PATCH "localhost:19827/tasks/$TASK_ID" \
    -H 'Content-Type: application/json' \
    -d "{\"status\":\"done\",\"result\":\"功能实现完成\",\"version\":$((VER+2))}"
  # qa 任务自动解锁，qa agent 下次 poll 时认领
fi
```

## 部署配置

### macOS launchd

编辑 `launchd/com.irchelper.agent-queue.plist` 填入实际值，然后：

```bash
make build
bash scripts/launchd-install.sh    # 安装并启动
curl http://localhost:19827/health  # 验证
```

### plist 环境变量配置

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

### OpenClaw Gateway 配置（`/dispatch` 必需）

在 `openclaw.json` 中添加：

```json
{
  "gateway": {
    "tools": {
      "allow": ["sessions_send"]
    }
  }
}
```

## 开发

```bash
make test      # 运行全部测试（含 -race）
make vet       # go vet
make build     # 编译
make clean     # 只删二进制（安全，不删数据库）
make clean-all # 删二进制 + 数据库（危险，会清空全部任务历史）
```

> **注意：** 任务执行中禁止运行 `make clean-all`，否则会永久删除所有任务记录。

## 架构说明

- **存储**：SQLite WAL 模式，单文件，零部署，ACID 事务；路径通过 `AGENT_QUEUE_DB_PATH` 配置
- **API**：Go `net/http`，无框架，约 350 行
- **并发控制**：乐观锁（`version` 字段）
- **专家汇报**：专家只需 `PATCH /tasks`，无需 sessions_send。Go server webhook 是唯一通知通道。
- **通知**：Discord Incoming Webhook（`done`/`failed` → 用户）+ SessionNotifier（`failed` → CEO session，极简格式防 LLM 误解），通过 `Notifier` 接口抽象（平台无关）
- **部署**：launchd（macOS）/ systemd（Linux），KeepAlive 自动重启

完整架构文档：[`docs/ARCH.md`](./docs/ARCH.md) | 产品需求：[`docs/PRD.md`](./docs/PRD.md)

## License

MIT
