# agent-queue 架构说明

> 版本：v4 | 更新：2026-02-25
> 对应 PRD：`PRD.md`

---

## 技术选型

| 层 | 选型 | 说明 |
|----|------|------|
| **存储** | SQLite（WAL 模式） | 单文件，零部署，ACID 事务，WAL 支持并发读写 |
| **API 服务** | Go `net/http`（无框架） | 单二进制，~350 行，无外部依赖 |
| **通知** | Discord Incoming Webhook | 标准 HTTP POST，通过 `Notifier` 接口抽象，可扩展 |
| **并发控制** | 乐观锁（`version` 字段） | `WHERE version = ? AND status = 'pending'` 原子更新 |
| **部署** | launchd（macOS）/ systemd（Linux）| KeepAlive，进程崩溃自动重启 |

**数据库：** `data/queue.db`（可通过 `--db` 参数自定义路径）
**监听地址：** `localhost:19827`（仅本机，可通过 `--port` 参数自定义）

---

## 数据库 Schema（3 张表）

```sql
-- 任务主表
tasks (
  id, title, description, status, priority,
  assigned_to, parent_id, mode, requires_review,
  result, version, created_at, updated_at
)

-- 任务依赖关系
task_deps (task_id, depends_on_task_id)

-- 状态变更历史
task_history (id, task_id, from_status, to_status, actor, note, timestamp)
```

---

## 核心状态机（7 态）

```
pending → claimed → in_progress → done        （标准完成路径）
                  → in_progress → review → done （requires_review=true 路径）
                  → in_progress → blocked → pending （阻塞后解除）
                  → in_progress → pending      （超时释放，cron/人工触发）
pending → cancelled                            （直接取消）
claimed → pending                              （释放认领）
```

**合法状态转换矩阵：**

| from \ to | pending | claimed | in_progress | review | done | blocked | cancelled |
|-----------|---------|---------|-------------|--------|------|---------|-----------|
| pending | — | ✅ claim | ❌ | ❌ | ❌ | ❌ | ✅ |
| claimed | ✅ release | — | ✅ start | ❌ | ❌ | ❌ | ❌ |
| in_progress | ✅ timeout | ❌ | — | ✅* | ✅* | ✅ | ❌ |
| review | ❌ | ❌ | ✅ revise | — | ✅ | ❌ | ❌ |
| blocked | ✅ unblock | ❌ | ❌ | ❌ | ❌ | — | ❌ |
| done | ❌ | ❌ | ❌ | ❌ | — | ❌ | ❌ |
| cancelled | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ | — |

> `✅*`：`in_progress → done` 仅在 `requires_review=false` 时合法；`in_progress → review` 仅在 `requires_review=true` 时合法。非法转换返回 422。

**`in_progress → pending`（超时释放）：** 由超时检测 cron 或人工触发，清空 `assigned_to`，任务回到可认领池。agent 不主动调用此转换。

---

## 关键 API 端点（F1-F6）

### F1：任务 CRUD

| 方法 | 路径 | 说明 |
|------|------|------|
| `POST` | `/tasks` | 创建任务，支持 `depends_on`/`requires_review`/`parent_id` |
| `GET` | `/tasks` | 查询列表，过滤：`status`/`assigned_to`/`parent_id`/`deps_met` |
| `GET` | `/tasks/:id` | 查询详情 + 依赖关系 + 变更历史 |
| `PATCH` | `/tasks/:id` | 更新状态/result，需传 `version`（乐观锁校验） |

### F2：乐观锁认领

| 方法 | 路径 | 说明 |
|------|------|------|
| `POST` | `/tasks/:id/claim` | 原子认领，需传 `version` + `agent`；冲突返回 409 |

### F3：依赖关系

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/tasks/:id/deps-met` | 查询依赖是否全部满足 |

**自动解锁：** `PATCH /tasks/:id` 设为 `done` 时，response 返回 `triggered` 列表（被解锁的后续任务 ID）。

### F4：状态机（内置于 PATCH 接口）

所有状态流转通过 `PATCH /tasks/:id` 执行，服务端自动校验合法性 + `requires_review` 条件路由。

### F5：健康检查

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/health` | 返回服务状态 + 数据库连接状态 |

### F6：Webhook 通知

- 任务状态变为 `done` 时，异步 POST Discord Incoming Webhook
- 通知格式：`@用户ID ✅ 任务完成：[task title] (task_id: xxx)`
- 环境变量：`AGENT_QUEUE_DISCORD_WEBHOOK_URL`（未配置时 no-op + log.Info）
- 失败处理：重试 1 次，最终失败记 error log，不阻塞状态变更

### F7：POST /dispatch（原子化派发）

| 方法 | 路径 | 说明 |
|------|------|------|
| `POST` | `/dispatch` | 一步完成"建任务 + 触发专家 session"，替代 POST /tasks + sessions_send 两步 |

**行为流程：**
1. 创建 SQLite 任务记录（status=pending）
2. 调用 OpenClaw `/tools/invoke`（sessions_send）触发专家 session
3. 返回 task 对象 + `notified` 状态

**优雅降级：** sessions_send 失败时任务仍创建成功，响应含 `notified=false` + `notify_error` 字段，不阻断任务创建。

**环境变量：**
- `AGENT_QUEUE_OPENCLAW_API_URL`（默认 `http://localhost:18789`）
- `AGENT_QUEUE_OPENCLAW_API_KEY`（OpenClaw gateway token）

**专家 session key 映射：** 已硬编码在 Go server `internal/openclaw` 包中（agent name → sessionKey）。

### F8：GET /tasks/summary（全局状态面板）

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/tasks/summary` | CEO 启动时一次调用获取全局任务状态 |

**响应结构：**
- 计数：`pending` / `claimed` / `in_progress` / `done_today`
- 非 done 任务列表（按 `updated_at` 倒序）

**用途：** 替代 CEO 逐个查询任务状态，session 启动时一次调用即可掌握全局进度。

---

## CEO 集成说明

**CEO 角色：监控者（不再是推进者）**

| 操作 | v3（旧） | v4（新） |
|------|---------|---------|
| 感知任务完成 | cron 轮询 `GET /tasks?status=done&unack=true` | webhook 推送被动接收 |
| 推进串行链 | CEO 手动派下一步 | Go server 自动解锁依赖，agent cron 自行认领 |
| 通知用户 | CEO 转发 | Go server webhook 直推 Discord |

**CEO 仍负责的事：**
- 创建任务链（`POST /tasks` + `depends_on`）
- 处理 blocked 任务（决策后 `PATCH /tasks/:id` 解除阻塞）
- 超时检测（cron 检查长时间 `in_progress` 的任务，触发超时释放）
- 最终决策（需求变更、资源分配等人工判断）

**CEO 不再做的事：**
- ❌ 不主动轮询 done 任务
- ❌ 不手动推进串行链下一步
- ❌ 不转发专家完成通知给用户

---

## 专家集成说明

**专家通过 HTTP API 直接操作任务，取代 sessions_send 汇报。**

### 核心交互流程

```
专家 cron 触发
  → GET /tasks?status=pending&deps_met=true&assigned_to=agent_name
  → POST /tasks/:id/claim  {"version": N, "agent": "agent_name"}
  → 执行任务
  → PATCH /tasks/:id       {"status": "done", "result": "...", "version": N+1}
  → Go server 自动 webhook 通知 + 依赖解锁
```

### 专家不再做的事

- ❌ 不调用 `sessions_send` 向 CEO 汇报任务完成
- ❌ 不用 `message` tool 向 #首席ceo 发消息汇报进度
- ❌ 不主动 @CEO（通知由 Go server webhook 自动处理）

### 过渡方案（两阶段）

| 阶段 | 模式 | 切换条件 |
|------|------|---------|
| Phase 1 | `PATCH /tasks` + `sessions_send` 双写 | webhook 连续 7 天无漏发 |
| Phase 2 | 纯 `PATCH /tasks` | 删除 sessions_send 相关代码 |

---

## 部署说明

```bash
# 构建
go build -o agent-queue .

# 运行（默认 localhost:19827，数据库 data/queue.db）
./agent-queue

# 自定义参数
./agent-queue --port 8080 --db /path/to/queue.db

# 环境变量（可选）
export AGENT_QUEUE_DISCORD_WEBHOOK_URL="https://discord.com/api/webhooks/..."
export AGENT_QUEUE_OPENCLAW_API_URL="http://localhost:18789"
export AGENT_QUEUE_OPENCLAW_API_KEY="<gateway_token>"
```

**持久化运行：** 配合 launchd（macOS）或 systemd（Linux）设置 KeepAlive，进程崩溃自动重启，状态不丢失（SQLite 持久化）。

## 部署配置

### OpenClaw Gateway 配置（F7 /dispatch 依赖）

在 `openclaw.json` 的 gateway 节点添加 tools 权限：

```json
{
  "gateway": {
    "tools": {
      "allow": ["sessions_send"]
    }
  }
}
```

### launchd plist 环境变量配置

F7 `/dispatch` 调用 OpenClaw API，需在 launchd plist 中配置：

```xml
<key>EnvironmentVariables</key>
<dict>
  <key>AGENT_QUEUE_OPENCLAW_API_URL</key>
  <string>http://localhost:<gateway_port></string>
  <key>AGENT_QUEUE_OPENCLAW_API_KEY</key>
  <string><gateway_token></string>
</dict>
```

**未配置时：** `/dispatch` 接口任务仍可创建，`notified=false`，降级行为与 `POST /tasks` 相同。

---

## 设计决策备注

- **为什么不用框架：** ~350 行 Go，`net/http` 足够，引入 Gin/Echo 增加依赖无收益
- **为什么选 SQLite：** 个人工作站场景，10K+ 任务轻松支撑，零部署成本，单文件备份
- **为什么 webhook 而非 cron 拉：** 秒级触达 vs 最长 3min 延迟；webhook 无需去重状态管理；不依赖 CEO 在线
- **为什么 `Notifier` 接口：** 保持平台无关性，未来扩展 Telegram/Slack 无需改核心逻辑
- **乐观锁而非悲观锁：** SQLite 单节点，冲突概率低，乐观锁性能更好；悲观锁（FOR UPDATE）在 SQLite 实现复杂
