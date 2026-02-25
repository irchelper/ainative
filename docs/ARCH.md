# agent-queue 架构说明

> 版本：v5 | 更新：2026-02-25
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
  assigned_to, retry_assigned_to,          -- retry_assigned_to: failed 时重试指定的 agent（可与原 assigned_to 不同）
  parent_id, mode, requires_review,
  result, failure_reason,                  -- failure_reason: failed 时专家写入的失败原因
  version, created_at, updated_at
)

-- 任务依赖关系
task_deps (task_id, depends_on_task_id)

-- 状态变更历史
task_history (id, task_id, from_status, to_status, actor, note, timestamp)
```

---

## 核心状态机（8 态）

```
pending → claimed → in_progress → done        （标准完成路径）
                  → in_progress → review → done （requires_review=true 路径）
                  → in_progress → blocked → pending （阻塞后解除）
                  → in_progress → failed       （专家主动上报失败）
                  → in_progress → pending      （超时释放，cron/人工触发）
pending → cancelled                            （直接取消）
claimed → pending                              （释放认领）
failed  → pending                              （CEO 决策后重试）
```

**合法状态转换矩阵：**

| from \ to | pending | claimed | in_progress | review | done | blocked | failed | cancelled |
|-----------|---------|---------|-------------|--------|------|---------|--------|-----------|
| pending | — | ✅ claim | ❌ | ❌ | ❌ | ❌ | ❌ | ✅ |
| claimed | ✅ release | — | ✅ start | ❌ | ❌ | ❌ | ❌ | ❌ |
| in_progress | ✅ timeout | ❌ | — | ✅* | ✅* | ✅ | ✅ | ❌ |
| review | ❌ | ❌ | ✅ revise | — | ✅ | ❌ | ❌ | ❌ |
| blocked | ✅ unblock | ❌ | ❌ | ❌ | ❌ | — | ❌ | ❌ |
| failed | ✅ retry | ❌ | ❌ | ❌ | ❌ | ❌ | — | ❌ |
| done | ❌ | ❌ | ❌ | ❌ | — | ❌ | ❌ | ❌ |
| cancelled | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ | — |

> `✅*`：`in_progress → done` 仅在 `requires_review=false` 时合法；`in_progress → review` 仅在 `requires_review=true` 时合法。非法转换返回 422。

**`in_progress → failed`（专家主动上报）：** 专家执行中遇到无法自行解决的阻塞（依赖缺失、外部服务不可用、需 CEO 决策）时主动调用。`failure_reason` 字段写入失败原因。Go server 在 failed 时**额外触发 SessionNotifier** 唤醒 CEO session（区别于 done 只发 Discord webhook）。

**`failed → pending`（CEO 重试）：** CEO 收到 failed 通知、判断可重试后，`PATCH /tasks/:id` 将状态改回 pending。若指定了 `retry_assigned_to`，则任务重入队列后优先分配给该 agent。

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

### F6：Webhook 通知 + SessionNotifier

**两类通知，触发时机不同：**

| 事件 | 通知方式 | 接收方 |
|------|---------|--------|
| 任务 `done` | Discord Incoming Webhook（`DiscordNotifier`） | 用户（@mention） |
| 任务 `failed` | SessionNotifier（`/tools/invoke sessions_send`） | CEO session |

**done 通知（DiscordNotifier）：**
- 异步 POST Discord Incoming Webhook
- 通知格式：`@用户ID ✅ 任务完成：[task title] (task_id: xxx)`
- 环境变量：`AGENT_QUEUE_DISCORD_WEBHOOK_URL`（未配置时 no-op + log.Info）
- 失败处理：重试 1 次，最终失败记 error log，不阻塞状态变更

**failed 通知（SessionNotifier）：**
- 任务状态变为 `failed` 时，调用 OpenClaw `/tools/invoke` 触发 CEO session
- 通知内容包含：task_id、title、failure_reason、assigned_to
- 环境变量：`AGENT_QUEUE_OPENCLAW_API_URL` + `AGENT_QUEUE_OPENCLAW_API_KEY`（复用 /dispatch 配置）
- 失败处理：重试 1 次，最终失败记 error log + Discord webhook 兜底通知

**设计意图：** 正常路径（done）CEO 不被唤醒，实现零干扰。异常路径（failed）精确唤醒 CEO，由 LLM 判断如何处理（重试/改派/取消）。不使用 Discord webhook 通知 CEO 是因为 CEO 需要结构化数据（task_id、failure_reason）才能决策，而 Discord 消息是非结构化的。

**Notifier 接口扩展：**
```go
type Notifier interface {
    Notify(task Task) error
}

// DiscordNotifier: done 时推送给用户
// SessionNotifier: failed 时唤醒 CEO session
// 两者均实现 Notifier 接口，Go server 根据状态选择对应实现
```

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

### F9：GET /tasks/poll（专家自驱认领）

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/tasks/poll?assigned_to=<agent>` | 返回该 agent 第一个可认领任务（依赖已满足） |

**响应：**
```json
{"task": {...}}   // 有任务
{"task": null}    // 无待处理任务
```

**服务端排序逻辑：** `status=pending AND assigned_to=? ORDER BY priority DESC, created_at ASC`，然后过滤依赖未满足的任务，返回第一个合法任务。

**设计意图：** 专家 session 启动时调用一次即可获得最优任务，无需客户端实现筛选/排序逻辑。与 `GET /tasks?...` 的区别：poll 是主动拉取+自动选优，返回单个任务，专为自驱 claim 场景设计。

### F10：POST /dispatch/chain（串行链原子派发）

| 方法 | 路径 | 说明 |
|------|------|------|
| `POST` | `/dispatch/chain` | 原子创建串行任务链，自动设置 depends_on |

**请求体：**
```json
{
  "tasks": [
    {"title": "步骤A", "assigned_to": "coder", "description": "..."},
    {"title": "步骤B", "assigned_to": "qa",    "description": "..."},
    {"title": "步骤C", "assigned_to": "writer", "description": "..."}
  ]
}
```

**行为：** 按数组顺序创建任务，自动设置 `task[i].depends_on = [task[i-1].id]`，形成 A→B→C 串行链。返回所有子任务对象（含各自 task_id）。

**与 POST /dispatch 的区别：**

| 接口 | 适用场景 |
|------|---------|
| `POST /dispatch` | 单任务：建任务 + 触发专家 session |
| `POST /dispatch/chain` | 串行链：一次性建完整链路，依赖自动设置 |

**全自动串行链运行机制：**
1. CEO 调用 `/dispatch/chain` 建链，拿到各任务 task_id
2. CEO 通过 sessions_send 通知链路第一个专家（注入 task_id）
3. 专家 A 执行完 → `PATCH done` → Go server 自动解锁任务 B（`triggered` 列表）
4. 专家 B session 启动时 `GET /tasks/poll` 发现任务 B → 自驱 claim → 执行
5. 无需 CEO 手动推进，链路自动流转至终点

### F11：异常感知与 CEO 介入（SessionNotifier）

**设计目标：** 正常路径 CEO 零干扰，异常路径精确唤醒 CEO。

**专家上报 failed 流程：**
```bash
# 专家遇到无法自行解决的问题时
curl -X PATCH http://localhost:19827/tasks/{task_id} \
  -H "Content-Type: application/json" \
  -d '{"status":"failed","failure_reason":"依赖服务不可用：X API 返回 503","version":N}'
# Go server 自动触发 SessionNotifier → CEO session 收到唤醒消息
```

**CEO 收到 failed 通知后的处理选项：**

| 选项 | 操作 | 适用场景 |
|------|------|---------|
| 原 agent 重试 | `PATCH /tasks/:id {"status":"pending","version":N}` | 临时故障，原 agent 可重试 |
| 改派其他 agent | `PATCH /tasks/:id {"status":"pending","retry_assigned_to":"other","version":N}` | 原 agent 不适合，换人处理 |
| 取消任务 | `PATCH /tasks/:id {"status":"cancelled","version":N}` | 任务已无意义或被上游阻塞 |
| 修改描述后重试 | 先 PATCH description，再 PATCH status=pending | 任务 spec 有误，需更正 |

**`retry_assigned_to` 字段：** 任务进入 failed 后，CEO 可在 PATCH 中同时设置 `retry_assigned_to`，任务回到 pending 池后优先分配给指定 agent（下次 poll 时 `GET /tasks/poll?assigned_to=retry_assigned_to` 可命中）。

**SessionNotifier 消息格式（发送给 CEO session）：**
```
⚠️ 任务失败：[task title]
task_id: {id}
assigned_to: {agent}
failure_reason: {reason}
请检查并决定：重试 / 改派 / 取消
```

**与 blocked 状态的区别：**

| 状态 | 触发方 | 通知方式 | CEO 响应 |
|------|--------|---------|---------|
| `blocked` | agent（等待外部条件） | 无自动通知 | CEO 定期巡检 |
| `failed` | agent（无法继续执行） | SessionNotifier 精确唤醒 | CEO 立即决策 |

---

## 异常处理与退单

### retry_assigned_to 自动退单机制

专家发现问题时，PATCH failed 并在 result 里声明退单目标：

**result 格式：**
```
"<失败原因描述> | retry_assigned_to: <专家名>"
```

**示例：**
```
# qa 发现 coder bug
result: "登录按钮无响应 | retry_assigned_to: coder"

# writer 发现需求不可实现
result: "第3章登录态存储方案未定义 | retry_assigned_to: pm"

# devops 发现架构问题
result: "容器无法启动，端口设计有误 | retry_assigned_to: thinker"

# coder 发现架构缺陷
result: "auth 模块无法解耦 | retry_assigned_to: thinker"
```

**Go server 处理逻辑：**
1. 解析 result 中的 `retry_assigned_to: {name}`
2. 有值 → 自动建新任务（title: `retry: {原任务title}`，assigned_to = 退单目标专家），**不通知 CEO**
3. 无值 → 触发 CEO SessionNotifier（兜底，需人工判断）

### 退单映射规范（各专家退单合同）

各专家遇到不同类型问题时的标准退单目标：

| 发现问题的专家 | 问题类型 | 退单给 |
|---|---|---|
| qa | 代码 bug | coder |
| qa | UI/交互问题 | uiux |
| coder | 架构设计缺陷 | thinker |
| coder | 需求不明确 | pm |
| writer | 需求不可实现 | pm |
| devops | 代码 bug 导致部署失败 | coder |
| devops | 架构设计问题 | thinker |
| thinker | 文档描述不准确 | writer |
| uiux | 需求模糊 | pm |

**设计原则：** 退单是网状结构，专家间可直接退单，不经过 CEO 中转；仅当无法确定退单目标时才上报 CEO（SessionNotifier 兜底）。

### CEO SessionNotifier（兜底机制）

触发条件：PATCH failed 且 result 中没有 `retry_assigned_to`

**实现：**
```
调用 /tools/invoke sessions_send → CEO session
session key: agent:ceo:discord:channel:1475338424293789877
消息格式：
  [agent-queue] ⚠️ 任务失败需介入：{title}
  result: {result}
  task_id: {id}
```

**决策选项（CEO 收到通知后）：**

| 选项 | 操作 | 适用场景 |
|------|------|---------|
| 原 agent 重试 | `PATCH {"status":"pending","version":N}` | 临时故障 |
| 改派其他 agent | `PATCH {"status":"pending","retry_assigned_to":"other","version":N}` | 换人处理 |
| 取消任务 | `PATCH {"status":"cancelled","version":N}` | 任务无意义 |

---

## 专家自驱 Claim（P3）

**设计目标：** CEO 提交任务链后全程不介入，专家 session 启动即自动认领并执行，串行链自动流转至终点。

### 完整串行链流程

```
CEO POST /dispatch/chain → 一次性提交整条串行链（自动设 depends_on）
  ↓
专家 A session 启动
  → GET /tasks/poll?assigned_to=A  （只返回 deps_met=true 的任务）
  → POST /tasks/{id}/claim
  → PATCH in_progress
  → 执行任务
  → PATCH done
  ↓
Go server 检测 A 的下游依赖满足 → 自动解锁任务 B（triggered 列表）
  ↓
专家 B session 启动
  → GET /tasks/poll?assigned_to=B  （B 现在 deps_met=true，可被 poll 到）
  → POST /tasks/{id}/claim
  → PATCH in_progress
  → 执行任务
  → PATCH done
  ↓
... 依此类推，链路自动推进 ...
  ↓
最后一个任务 PATCH done → Go server webhook → @用户
```

### 关键特性

- **CEO 零介入**：链路提交后，串行推进完全由 Go server 依赖解锁 + 专家 poll 驱动，CEO 不轮询、不手动推进
- **deps_met 过滤**：`GET /tasks/poll` 服务端自动过滤依赖未满足的任务，专家无需判断"轮到我了吗"
- **两阶段查询防死锁**：Poll 实现分两阶段——先关闭候选任务游标，再逐个检查依赖，防止 SQLite 单连接在嵌套查询中死锁
- **乐观锁防重复**：`POST /tasks/:id/claim` 需传 `version`，并发 claim 只有一个成功（409 冲突），无重复执行风险
- **中断恢复**：专家崩溃后任务停在 `in_progress`，超时检测 cron 释放回 `pending`，下次 poll 可重新认领

### 与旧模式对比

| 维度 | 旧模式（CEO 调度） | 新模式（专家自驱） |
|------|-------------------|-------------------|
| 串行链推进 | CEO 收到回执 → 手动派下一步 | Go server 解锁依赖 → 专家 poll 自驱 |
| CEO 在线要求 | 必须在线且有上下文 | 不需要 |
| 步间延迟 | CEO 被唤醒时间（不确定） | 专家 poll 周期（可预期） |
| 状态持久化 | CEO 上下文（崩了就丢） | SQLite（永久持久化） |

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
- 处理 `failed` 任务（收到 SessionNotifier 唤醒 → 决策：重试/改派/取消）
- 处理 `blocked` 任务（决策后 `PATCH /tasks/:id` 解除阻塞）
- 超时检测（cron 检查长时间 `in_progress` 的任务，触发超时释放）
- 最终决策（需求变更、资源分配等人工判断）

**CEO 不再做的事：**
- ❌ 不主动轮询 done 任务
- ❌ 不手动推进串行链下一步
- ❌ 不转发专家完成通知给用户

---

## 专家集成说明

**专家通过 HTTP API 直接操作任务，取代 sessions_send 汇报。**

### 核心交互流程（自驱 claim 模式）

专家 session 启动时**主动 poll**，无需等待 CEO 唤醒：

```
专家 session 启动
  → GET /tasks/poll?assigned_to=agent_name
  │
  ├── task != null（有待执行任务）
  │     → POST /tasks/:id/claim       {"version": N, "agent": "agent_name"}
  │     → PATCH /tasks/:id            {"status": "in_progress", "version": N+1}
  │     → 执行任务（读 task.description 获取完整 spec）
  │     → PATCH /tasks/:id            {"status": "done", "result": "...", "version": N+2}
  │     → Go server 自动 webhook 通知 + 依赖解锁（triggered 列表）
  │
  └── task == null（无待处理任务）
        → 正常处理收到的 sessions_send 消息
```

**`GET /tasks/poll` vs `GET /tasks?...` 的区别：**

| 对比维度 | `GET /tasks?status=pending&deps_met=true` | `GET /tasks/poll?assigned_to=X` |
|---------|------------------------------------------|--------------------------------|
| 返回数量 | 列表（需客户端选择） | 单个最优任务（服务端排序） |
| 依赖检查 | 需客户端传 `deps_met=true` | 服务端自动过滤依赖未满足的任务 |
| 优先级排序 | 需客户端排序 | 服务端按 `priority DESC, created_at ASC` 排好 |
| 适用场景 | 监控/查询全量任务 | 专家启动时自驱认领 |

### 完整 Shell 示例

```bash
# 1. session 启动时 poll
RESP=$(curl -s "http://localhost:19827/tasks/poll?assigned_to=coder")
TASK_ID=$(echo $RESP | jq -r '.task.id // empty')

if [ -n "$TASK_ID" ]; then
  VER=$(echo $RESP | jq -r '.task.version')

  # 2. claim（乐观锁，冲突返回 409）
  curl -s -X POST "http://localhost:19827/tasks/$TASK_ID/claim" \
    -H "Content-Type: application/json" \
    -d "{\"agent\":\"coder\",\"version\":$VER}"

  # 3. in_progress
  curl -s -X PATCH "http://localhost:19827/tasks/$TASK_ID" \
    -H "Content-Type: application/json" \
    -d "{\"status\":\"in_progress\",\"version\":$((VER+1))}"

  # 4. 执行任务...

  # 5. done（触发 webhook + 自动解锁依赖）
  curl -s -X PATCH "http://localhost:19827/tasks/$TASK_ID" \
    -H "Content-Type: application/json" \
    -d "{\"status\":\"done\",\"result\":\"任务摘要\",\"version\":$((VER+2))}"
fi
```

### 专家不再做的事

- ❌ 不调用 `sessions_send` 向 CEO 汇报任务完成（有 task_id 时）
- ❌ 不用 `message` tool 向 #首席ceo 发消息汇报进度
- ❌ 不主动 @CEO（通知由 Go server webhook 自动处理）

**兜底：** 无 task_id 时（旧式 sessions_send 派发），仍需 sessions_send CEO，防止沉默。

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
