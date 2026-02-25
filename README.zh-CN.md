[English](./README.md)

# agent-queue

面向 OpenClaw multi-agent 工作流的 SQLite 任务队列服务。通过 HTTP 协调多个 AI agent——持久化状态、乐观锁防重复认领、依赖关系自动解锁、Discord webhook 通知。

## 功能列表

- **F1 — 任务 CRUD**：创建/查询/更新任务，完整生命周期管理
- **F2 — 乐观锁认领**：`version` 字段原子认领；并发冲突 → 409 Conflict
- **F3 — 依赖关系图**：`depends_on` 数组；前置任务 `done` → 后续任务自动解锁
- **F4 — 7 态状态机**：`pending → claimed → in_progress → review → done / blocked / cancelled`
- **F5 — 健康检查**：`GET /health` 返回服务状态 + 数据库连接状态
- **F6 — Discord webhook**：任务 `done` → 异步 POST Discord Incoming Webhook（通过 `Notifier` 接口抽象）
- **F7 — 原子派发**：`POST /dispatch` 一步完成建任务 + 触发 agent session
- **F8 — 全局状态面板**：`GET /tasks/summary` 返回计数 + 当前活跃任务列表

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
| `AGENT_QUEUE_DISCORD_WEBHOOK_URL` | 任务完成通知的 Discord Incoming Webhook URL | 否 |
| `AGENT_QUEUE_OPENCLAW_API_URL` | `/dispatch` 使用的 OpenClaw gateway URL（默认 `http://localhost:18789`） | 否 |
| `AGENT_QUEUE_OPENCLAW_API_KEY` | OpenClaw gateway token，供 `/dispatch` 使用 | 否 |

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
| `POST` | `/dispatch` | 原子化建任务 + 触发 agent session |
| `GET` | `/tasks/summary` | 全局任务计数 + 活跃任务列表 |

### 示例：串行任务链

```bash
# 创建任务 A
A=$(curl -s -X POST localhost:19827/tasks \
  -H 'Content-Type: application/json' \
  -d '{"title":"步骤 A","assigned_to":"coder"}' | jq -r .id)

# 创建依赖 A 的任务 B
curl -s -X POST localhost:19827/tasks \
  -H 'Content-Type: application/json' \
  -d "{\"title\":\"步骤 B\",\"assigned_to\":\"qa\",\"depends_on\":[\"$A\"]}"

# 认领并完成 A（自动解锁 B）
VER=$(curl -s localhost:19827/tasks/$A | jq .version)
curl -s -X POST localhost:19827/tasks/$A/claim \
  -H 'Content-Type: application/json' \
  -d "{\"version\":$VER,\"agent\":\"coder\"}"

curl -s -X PATCH localhost:19827/tasks/$A \
  -H 'Content-Type: application/json' \
  -d "{\"status\":\"done\",\"result\":\"步骤 A 完成\",\"version\":$((VER+1))}"
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
make test    # 运行全部测试（含 -race）
make vet     # go vet
make build   # 编译
```

## 架构说明

- **存储**：SQLite WAL 模式，单文件，零部署，ACID 事务
- **API**：Go `net/http`，无框架，约 350 行
- **并发控制**：乐观锁（`version` 字段）
- **通知**：通过 `Notifier` 接口抽象的 Discord Incoming Webhook（平台无关）
- **部署**：launchd（macOS）/ systemd（Linux），KeepAlive 自动重启

完整架构文档：[`docs/ARCH.md`](./docs/ARCH.md) | 产品需求：[`docs/PRD.md`](./docs/PRD.md)

## License

MIT
