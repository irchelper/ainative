# agent-queue 架构说明

> 版本：v7 | 更新：2026-02-26 | 代码基线：commit `7610e5e` + V7 superseded_by
> 对应 PRD：`PRD.md`

---

## §1 系统定位

agent-queue 是一个基于 SQLite + Go 的**轻量级多 agent 任务队列**——把任务状态从 agent 脑子里搬到数据库里，让串行链自动推进、agent 崩了能恢复、不需要调度者在线。

**解决的根本问题：** 基于"调度者模式"的 multi-agent 系统完全依赖中心 agent 在线。调度者崩了/上下文压缩了/用户离线了——整条任务链就断了。

**核心设计原则：**
- 任务状态持久化在 SQLite（不在 agent 脑子里）
- 专家自驱 poll/claim，不依赖 CEO 在线推进
- Discord webhook 作为用户审计通道；sessions_send 作为 agent 间机器感知通道
- 两个通道互补，不互相替代

---

## §2 核心流程：任务生命周期

### 正常路径

```
CEO POST /dispatch/chain → 创建 A→B→C 串行链
  ↓
Go server sessions_send 唤醒专家 A
  ↓
专家 A session 启动 → GET /tasks/poll → POST /claim → PATCH in_progress → 执行 → PATCH done
  ↓
Go server（PATCH done 触发）：
  1. SQLite 写入 done
  2. unlockDependents → B 的 deps_met=true
  3. Discord webhook → @康熙（用户审计）
  4. sessions_send → 专家 B session（唤醒下游）
  ↓
专家 B session 启动 → poll → claim → ... → PATCH done
  ↓
... 链路自动流转至终点 ...
  ↓
最后一个 PATCH done → Discord webhook @康熙
```

### 异常路径（failed）

```
专家 X 执行中遇到无法解决的问题
  ↓
PATCH /tasks/:id  status=failed, result="原因 | retry_assigned_to: Y"
  ↓
Go server 解析 result：
  ├── 有 retry_assigned_to → autoRetry（见下）
  └── 无 retry_assigned_to → SessionNotifier 唤醒 CEO session（人工决策）
  ↓
同时：Discord webhook → @康熙（❌ 格式，含 blocked_downstream 列表）
```

**autoRetry V7 行为（有 retry_assigned_to 时）：**

| 步骤 | 操作 | 说明 |
|------|------|------|
| 1 | 读取原任务 X 的 `depends_on` 列表 | 继承原依赖，让 retry 任务在链路中处于相同位置 |
| 2 | 创建 retry 任务 X'，`depends_on = X.depends_on`，`assigned_to = retry_assigned_to` | X' 继承 X 的前置依赖，保持链路完整性 |
| 3 | 更新 `X.superseded_by = X'.id` | 标记替代关系，`depsMetForID` 扩展判断用 |
| 4 | sessions_send 唤醒 Y（不通知 CEO） | 正常 dispatch 流程 |

**失败链路自动恢复机制（`superseded_by` + `depsMetForID` 扩展）：**

链 A→B→C 中 A failed，autoRetry 创建 A'，`A.superseded_by = A'.id`。

当 A' 执行完 PATCH done 时：
- `unlockDependents(A'.id)` 处理 A' 的直接下游（若 A' 有依赖链则正常解锁）
- `depsMetForID(B)` 检查 B 的依赖 A：A 不是 done，但 `A.superseded_by = A'.id` 且 A' 是 done → **依赖视为满足**
- B 解锁 → poll 可见 → sessions_send 唤醒 B → 链路从断点自动恢复

`depsMetForID` SQL 扩展逻辑：
```sql
SELECT COUNT(*) FROM task_deps td
JOIN tasks t ON t.id = td.depends_on
WHERE td.task_id = ?
  AND t.status != 'done'
  AND (t.superseded_by = '' OR NOT EXISTS (
    SELECT 1 FROM tasks s WHERE s.id = t.superseded_by AND s.status = 'done'
  ))
```
含义：dep 要么自己 done，要么它的 superseder done。两个条件均不满足才算"依赖未满足"。

**`blocked_downstream` 响应字段（PATCH failed 时）：**

PATCH → failed 时，Go server 扫描所有直接 + 间接依赖该任务的下游 pending 任务，在 PATCH response 中返回：

```json
{
  "task": {..., "status": "failed"},
  "triggered": [],
  "blocked_downstream": [
    {"id": "xxx", "title": "B任务", "assigned_to": "writer"},
    {"id": "yyy", "title": "C任务", "assigned_to": "qa"}
  ]
}
```

**这是纯只读扫描，不修改任何下游任务的状态。** 用于 CEO 感知受影响范围，决策是否需要人工干预。

### 状态机（8 态）

```
pending → claimed → in_progress → done          （标准路径）
                  → in_progress → review → done  （requires_review=true）
                  → in_progress → blocked → pending（外部阻塞→解除）
                  → in_progress → failed         （专家主动上报）
                  → in_progress → pending          （超时释放，cron/人工触发）
pending → cancelled                               （取消）
claimed → pending                                  （释放认领）
failed  → pending                                  （CEO 人工干预重试）
```

**终态：** `done` / `cancelled`。`failed` 可被 CEO PATCH 回 `pending`，属于人工干预，不是自动流转。

**`failed → pending`（CEO 重试）补充约束：** PATCH 时必须同时清空 `superseded_by`（置为空字符串），否则若之前已创建 retry 任务，依赖链会出现双重解锁冲突。

**合法状态转换矩阵：**

| from \ to | pending | claimed | in_progress | review | done | blocked | failed | cancelled |
|-----------|---------|---------|-------------|--------|------|---------|--------|-----------|
| pending | — | ✅ | ❌ | ❌ | ❌ | ❌ | ❌ | ✅ |
| claimed | ✅ release | — | ✅ | ❌ | ❌ | ❌ | ❌ | ❌ |
| in_progress | ✅ timeout | ❌ | — | ✅* | ✅* | ✅ | ✅ | ❌ |
| review | ❌ | ❌ | ✅ revise | — | ✅ | ❌ | ❌ | ❌ |
| blocked | ✅ | ❌ | ❌ | ❌ | ❌ | — | ❌ | ❌ |
| failed | ✅ retry | ❌ | ❌ | ❌ | ❌ | ❌ | — | ❌ |
| done | ❌ | ❌ | ❌ | ❌ | — | ❌ | ❌ | ❌ |
| cancelled | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ | — |

> `✅*`：`in_progress → done` 仅在 `requires_review=false` 时合法；`in_progress → review` 仅在 `requires_review=true` 时合法。非法转换返回 422。

---

## §3 专家汇报链路

### 两个通知通道的职责

| 通道 | 技术实现 | 接收方 | 作用 | 触发条件 |
|------|---------|--------|------|---------| 
| Discord Incoming Webhook | HTTP POST → Discord 频道 | 用户（康熙） | **审计 + 可观测性** — 用户看到任务完成/失败 | done / failed |
| SessionNotifier | OpenClaw `/tools/invoke sessions_send` → agent session | agent（专家 / CEO） | **机器感知** — 唤醒 agent 执行后续 | dispatch 唤醒专家 / failed 唤醒 CEO |

### 为什么需要两个通道

**Discord webhook 无法触发 CEO agent。** bot-to-bot 限制：webhook 消息发到 Discord 频道，但 CEO agent 只响应 Inter-session message（sessions_send），不监听 Discord 频道消息。因此：

- **done 时：** 只需要用户知道 → Discord webhook。CEO 不需要感知每个 done（串行链靠 Go server 自动推进下游）
- **failed 时：** 用户需要知道（webhook）+ CEO agent 需要介入（SessionNotifier sessions_send）→ 双推
- **dispatch 时：** 专家 agent 需要被唤醒（sessions_send）→ SessionNotifier

### 专家侧最终规则

| 场景 | 专家操作 | 通知链路 |
|------|---------|---------|
| 有 task_id + 任务成功 | `PATCH /tasks/:id status=done result="摘要"` | Go server → Discord webhook @康熙 |
| 有 task_id + 任务失败 | `PATCH /tasks/:id status=failed result="原因"` | Go server → Discord webhook @康熙 + sessions_send CEO |
| 无 task_id（兜底） | `sessions_send` CEO session | CEO 收到 Inter-session message |

**专家永远不需要：**
- ❌ sessions_send CEO（有 task_id 时）
- ❌ message tool 发 #首席ceo
- ❌ @首席CEO

### 已知局限

**局限 1：Discord webhook 不触发 CEO agent**
- webhook 消息对人可见（审计），但 CEO agent 不读 Discord 频道消息
- 正常 done 不需要 CEO 感知（Go server 自动唤醒下游专家推进链路）
- 只有 failed 需要 CEO → 通过 SessionNotifier 解决

**局限 2：SessionNotifier 依赖 CEO session 存活**
- sessions_send 到 CEO session，如果 CEO session 已关/换新 → 消息静默丢失，无报错
- 缓解（现有）：CEO 每次 session 启动调 `GET /tasks/summary` 主动感知全局状态
- 缓解（建议）：CEO cron 每 10 分钟调 `GET /tasks?status=failed` 巡检
- 长期方案（v2）：Go server 内置重试队列，sessions_send 失败时定期重发

**局限 3：dispatch sessions_send 也可能丢失**
- `/dispatch` 创建任务 + sessions_send 唤醒专家；专家 session 不在 → `notified=false`
- 任务已创建在 SQLite，不丢。专家下次 session 启动时 poll 会发现
- 但如果专家不会自动启动 session，任务可能长时间无人认领
- 缓解：CEO cron 巡检 `GET /tasks?status=pending`，发现 `created_at` 超过 30 分钟的任务手动重派

---

## §4 数据持久化

### 当前状态（commit 7610e5e）

- SQLite WAL 模式（`PRAGMA journal_mode=WAL`），`data/queue.db`
- `CREATE TABLE IF NOT EXISTS`，server 重启不清数据
- `Makefile clean` 只删二进制，`clean-all` 才删 db
- `AGENT_QUEUE_DB_PATH` 环境变量支持绝对路径覆盖默认路径

### 路径配置

**默认路径：** `data/queue.db`（相对于 launchd WorkingDirectory `/Users/kangxi/clawd/agent-queue`）

**推荐：** plist 中显式配置绝对路径，防止 WorkingDirectory 变动导致路径失效：

```xml
<key>EnvironmentVariables</key>
<dict>
  <key>AGENT_QUEUE_DB_PATH</key>
  <string>/Users/kangxi/clawd/agent-queue/data/queue.db</string>
</dict>
```

或命令行：
```bash
./agent-queue --db /Users/kangxi/clawd/agent-queue/data/queue.db
```

### Makefile clean 规范

```makefile
clean:      # 只删二进制（安全）
    rm -f agent-queue

clean-all:  # 删二进制 + 数据库（危险，清空所有任务历史）
    rm -f agent-queue data/queue.db
```

**⚠️ 重要：** 开发迭代时只用 `make clean`。`make clean-all` 会永久删除所有任务记录，仅在需要完全重置时使用。

### Schema 迁移

使用 `ALTER TABLE ... ADD COLUMN`（幂等）模式，server 重启自动应用，不删现有数据。

---

## §5 SessionNotifier 消息格式规范

### dispatch 唤醒消息（→ 专家 session）

```
[agent-queue] 你有新的待处理任务。请执行 poll 流程认领。
```

**设计原则：** 极简，不含 title / task_id / description。防止 LLM 把通知消息误解为任务指令。专家收到后走 SOUL.md 中的 poll 流程（`GET /tasks/poll` → claim → 读 description → 执行）。

**历史教训：** 早期 dispatch 消息包含 `{title}\ntask_id: {id}\n请通过 POST 认领后执行`，导致专家把通知误当 CEO 直接派发的任务来执行（消息里有结构化指令）。修复后改为极简格式，只做唤醒不传内容。

### failed 告警消息（→ CEO session）

```
[agent-queue] ❌ 任务失败需介入：{title}
result: {result}
task_id: {id}
```

含 task_id 是因为 CEO 需要知道哪个任务失败了才能决策。CEO 不会把这个误当任务执行（消息开头有 `[agent-queue]` 前缀，且带 task_id）。

### SessionNotifier 发送目标原则

- 每个事件**只发给需要知道的 1 个 session**，不广播
- dispatch 新任务 → 目标专家 session
- dispatch/chain → 链中第一个专家
- done 触发下游解锁 → 下游专家 session（自动唤醒）
- failed 需 CEO 介入 → CEO session

---

## §6 CEO 感知机制

### 当前 CEO 感知链路

| 事件 | CEO 如何感知 |
|------|------------|
| 任务 done | 不直接感知（串行链靠 Go server 自动推进，不需要 CEO）|
| 任务 failed（有 retry_assigned_to） | 不感知（自动退单，Go server 处理）|
| 任务 failed（无 retry_assigned_to） | SessionNotifier 唤醒 CEO session |
| 用户可见通知 | Discord webhook（done / failed 均有，用于审计）|
| session 启动时 | CEO 主动调 `GET /tasks/summary` 掌握全局 |

### CEO session 启动巡检

每次 CEO session 启动时执行：

```bash
# 1. 全局状态概览
GET http://localhost:19827/tasks/summary

# 2. 检查 failed 计数 > 0 → 逐个读 failure_reason，决策重试/改派/取消
GET http://localhost:19827/tasks/:id   # 每个 failed 任务

# 3. 检查长时间 pending（created_at > 30min）→ dispatch sessions_send 可能丢失，手动重派
GET http://localhost:19827/tasks?status=pending

# 4. 检查长时间 in_progress（started_at > SLA×2）→ agent 可能崩了，触发超时释放
GET http://localhost:19827/tasks?status=in_progress
```

### 已知局限汇总

| 局限 | 影响 | 缓解措施 |
|------|------|---------|
| webhook 不触发 CEO agent | CEO 不自动感知 done | 不需要——串行链靠 Go server 推进，CEO 无需介入 |
| sessions_send 丢失（target session 不在） | CEO 可能不知道 failed | CEO startup GET /tasks/summary + cron 巡检 /tasks?status=failed |
| dispatch sessions_send 丢失 | 专家不知道有新任务 | 专家 startup poll + CEO cron 巡检长时间 pending 任务 |
| Go server 宕机 | PATCH 404，专家 PATCH 失败 | launchd KeepAlive 自动重启（秒级恢复）；专家降级 sessions_send CEO |

---

## 关键 API 端点

### F1：任务 CRUD

| 方法 | 路径 | 说明 |
|------|------|------|
| `POST` | `/tasks` | 创建任务，支持 `depends_on`/`requires_review`/`parent_id` |
| `GET` | `/tasks` | 查询列表，过滤：`status`/`assigned_to`/`parent_id`/`deps_met` |
| `GET` | `/tasks/:id` | 查询详情 + 依赖关系 + 变更历史 |
| `PATCH` | `/tasks/:id` | 更新状态/result，需传 `version`（乐观锁） |

### F2：乐观锁认领

| 方法 | 路径 | 说明 |
|------|------|------|
| `POST` | `/tasks/:id/claim` | 原子认领，需传 `version` + `agent`；冲突返回 409 |

### F3：依赖关系

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/tasks/:id/deps-met` | 查询依赖是否全部满足 |

**自动解锁：** `PATCH done` 时，response 返回 `triggered` 列表（被解锁的后续任务 ID）。Go server 同时 sessions_send 唤醒下游专家。

### F4：状态机

所有状态流转通过 `PATCH /tasks/:id` 执行，服务端校验合法性 + `requires_review` 条件路由。

### F5：健康检查

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/health` | 返回服务状态 + 数据库连接状态 |

### F6：通知（双通道）

**两类通知，触发时机不同：**

| 事件 | Discord webhook（用户审计） | SessionNotifier（agent 感知） |
|------|--------------------------|------------------------------|
| 任务 `done` | ✅ @康熙 ✅ 格式 | ✅ sessions_send 下游专家（自动推进）|
| 任务 `failed` | ✅ @康熙 ❌ 格式 | ✅ sessions_send CEO（无 retry_assigned_to 时）|

**done 消息格式（Discord webhook → 用户）：**
```
<@用户ID> ✅ 任务完成
**任务：** {title}
**专家：** {assigned_to}
**耗时：** {duration}
**结果：** {result}
`task_id: {id}`
```

**failed 消息格式（Discord webhook → 用户）：**
```
<@用户ID> ❌ 任务失败
**任务：** {title}
**专家：** {assigned_to}
**耗时：** {duration}
**失败原因：** {result}
`task_id: {id}`
```

**Notifier 接口：**
```go
type Notifier interface {
    Notify(task Task) error
}
// DiscordNotifier: HTTP POST → Discord Incoming Webhook
// 环境变量未配置时 no-op（不报错，log.Info）
```

### F7：POST /dispatch（原子化派发）

| 方法 | 路径 | 说明 |
|------|------|------|
| `POST` | `/dispatch` | 建任务 + sessions_send 唤醒专家，一步完成 |

**SessionNotifier 消息：** `[agent-queue] 你有新的待处理任务。请执行 poll 流程认领。`（极简，不含 title）

**优雅降级：** sessions_send 失败时任务仍创建成功，响应含 `notified=false` + `notify_error`。

### F8：GET /tasks/summary

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/tasks/summary` | 返回 pending/claimed/in_progress/done_today/failed 计数 + 非 done 任务列表 |

**用途：** CEO session 启动时一次调用掌握全局状态。

### F9：GET /tasks/poll（专家自驱认领）

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/tasks/poll?assigned_to=<agent>` | 返回该 agent 第一个可认领任务（deps_met + 优先级排序） |

**服务端排序：** `status=pending AND assigned_to=? ORDER BY priority DESC, created_at ASC`，过滤依赖未满足的任务，返回第一个合法任务。

**响应：**
```json
{"task": {...}}   // 有任务
{"task": null}    // 无待处理任务
```

### F10：POST /dispatch/chain（串行链原子派发）

| 方法 | 路径 | 说明 |
|------|------|------|
| `POST` | `/dispatch/chain` | 原子创建串行任务链，自动设置 depends_on |

按数组顺序创建任务，`task[i].depends_on = [task[i-1].id]`，形成 A→B→C 串行链。返回所有子任务对象。

---

## 数据库 Schema

```sql
-- 任务主表
tasks (
  id, title, description, status, priority,
  assigned_to,                    -- 任务负责人（claim 时写入，超时释放时清空）
  retry_assigned_to,              -- failed 时指定的重试 agent
  superseded_by TEXT NOT NULL DEFAULT '',  -- retry 替代链指针：autoRetry 创建 X' 后，X.superseded_by = X'.id
                                           -- depsMetForID 扩展判断：dep 自己 done OR dep.superseded_by 对应任务 done
                                           -- failed→pending CEO 重试时必须清空此字段（防双重解锁冲突）
  parent_id, mode, requires_review,
  result,                         -- done 时写摘要，failed 时写失败原因（支持 "原因 | retry_assigned_to: X" 格式）
  version, started_at, created_at, updated_at
)

-- 任务依赖关系
task_deps (task_id, depends_on_task_id)

-- 状态变更历史
task_history (id, task_id, from_status, to_status, changed_by, note, changed_at)
```

---

## 技术选型

| 层 | 选型 | 说明 |
|----|------|------|
| **存储** | SQLite（WAL 模式） | 单文件，零部署，ACID 事务，WAL 支持并发读写 |
| **API 服务** | Go `net/http`（无框架） | 单二进制；handler ~540行 + store ~690行 |
| **通知** | Discord Incoming Webhook + OpenClaw sessions_send | 双通道：webhook=用户审计，sessions_send=agent感知 |
| **并发控制** | 乐观锁（`version` 字段） | `WHERE version = ? AND status = 'pending'` 原子更新 |
| **部署** | launchd（macOS）/ systemd（Linux）| KeepAlive，进程崩溃自动重启 |

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
export AGENT_QUEUE_DB_PATH="/Users/kangxi/clawd/agent-queue/data/queue.db"
```

### launchd plist 环境变量配置

```xml
<key>EnvironmentVariables</key>
<dict>
  <key>AGENT_QUEUE_DISCORD_WEBHOOK_URL</key>
  <string>https://discord.com/api/webhooks/...</string>
  <key>AGENT_QUEUE_OPENCLAW_API_URL</key>
  <string>http://localhost:18789</string>
  <key>AGENT_QUEUE_OPENCLAW_API_KEY</key>
  <string>your-gateway-token</string>
  <key>AGENT_QUEUE_DB_PATH</key>
  <string>/Users/kangxi/clawd/agent-queue/data/queue.db</string>
</dict>
```

### OpenClaw Gateway 配置（F7 /dispatch 依赖）

```json
{
  "gateway": {
    "tools": {
      "allow": ["sessions_send"]
    }
  }
}
```

---

## 设计决策备注

- **为什么不用框架：** handler ~540 行，`net/http` 足够，引入 Gin/Echo 无收益
- **为什么选 SQLite：** 个人工作站场景，10K+ 任务轻松支撑，零部署，单文件备份
- **为什么双通道而非单通道：** Discord webhook 无法触发 agent（bot-to-bot 限制），sessions_send 无法让用户审计；两者互补
- **为什么 SessionNotifier 消息极简：** 早期含 title 导致 LLM 把通知误解为任务指令，改为"有新任务请 poll"后行为稳定
- **乐观锁而非悲观锁：** SQLite 单节点冲突概率低，乐观锁性能更好；悲观锁（FOR UPDATE）在 SQLite 实现复杂
- **为什么 autoRetry 解析 result 字段：** 专家发现问题时自然语言描述失败原因，在同一字段内用 `| retry_assigned_to: X` 声明退单目标，保持接口简单，无需新增字段
- **为什么选 `superseded_by` 而非修改 task_deps：** 修改 task_deps 会丢失原始依赖历史，且需要在事务中原子完成多表更新；`superseded_by` 只新增一列，不改现有数据，历史完整，依赖链语义不变
- **为什么 `blocked_downstream` 是只读扫描而非主动冻结：** 下游任务天然被 `deps_met=false` 屏蔽（A not done → B 依赖未满足 → poll 不返回 B），无需主动冻结；只读扫描提供可观测性，不引入新状态转换复杂度

---

## e2e 测试规范

### 测试任务隔离

验证 agent-queue 功能时，创建的测试任务必须使用专属 `assigned_to`：

```bash
curl -X POST http://localhost:19827/tasks \
  -H "Content-Type: application/json" \
  -d '{"title":"test-xxx","assigned_to":"test"}'
```

**规则：**
- 测试任务 `assigned_to` 统一用 `test`（不用真实专家名如 coder/devops）
- 没有任何专家的 SOUL.md 配置了 `assigned_to=test`，测试任务不会被自驱 claim
- 验证完成后手动 PATCH 为 failed（或等 server 重启清空）
- 禁止用真实专家名创建测试任务，避免干扰专家自驱队列
