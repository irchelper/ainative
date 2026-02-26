# agent-queue 架构说明

> 版本：v11 | 更新：2026-02-26 | 代码基线：commit `19535b2` — V11 agent_channel_map webhook路由 + stale max_dispatches限制
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
  4. sessions_send → 专家 B session（唤醒下游）    ← V8 修复：triggered 缺口（原代码未实现此步骤）
  5. [V8] 若 task.chain_id != "" && task.notify_ceo_on_complete:
       IsChainComplete(chain_id) → 全部 done/cancelled/superseded
         → SessionNotifier.OnChainComplete(chain_id, chain_title, tasks)
  ↓
专家 B session 启动 → poll → claim → ... → PATCH done
  ↓
... 链路自动流转至终点 ...
  ↓
最后一个 PATCH done → Discord webhook @康熙
                    → [V8 若 notify_ceo_on_complete=true] SessionNotifier.OnChainComplete → CEO session
```

**⚠️ triggered 下游唤醒缺口（V8 修复）：**

ARCH.md §2 一直写着"sessions_send → 专家 B session（唤醒下游）"，但 commit `6d56b5d` 对应代码中 `handler.go` 的 `patchTask` 函数：
- `unlockDependents` 确实返回了 `triggered` 列表
- 但 handler **未遍历 triggered 调用 SessionNotifier 唤醒下游专家**
- 导致串行链依赖自动解锁后，下游专家 session 不会被唤醒，必须靠专家自己 startup poll 才能感知

**V8 修复方案：** `patchTask` done 分支在 `unlockDependents` 返回 triggered 列表后，遍历每个 triggered 任务，调用 `SessionNotifier.Dispatch(assignedTo, taskID)` 唤醒对应专家 session。

### 异常路径（failed）

```
专家 X 执行中遇到无法解决的问题
  ↓
PATCH /tasks/:id  status=failed, result="原因"（或含 "| retry_assigned_to: Y"）
  ↓
Go server V8 retry 路由优先级链：
  1. PATCH body 显式传 retry_assigned_to → 直接用
  2. failparser 从 result 解析 "| retry_assigned_to: Y" → 用
  3. 查 retry_routing 表（按 error_keyword + priority） → 命中则用
  4. 全部未命中 → SessionNotifier 唤醒 CEO session（人工决策）
  ↓
步骤 1-3 命中（有退单目标）→ autoRetry（见下）
步骤 4（无退单目标）→ SessionNotifier CEO
  ↓
同时：Discord webhook → @康熙（❌ 格式，含 blocked_downstream 列表）
```

**autoRetry V8 行为（有 retry_assigned_to 时）：**

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

**autoRetry V10：review-reject 两阶段链（commit 1edb15f + 9f33672）**

当 reviewer（thinker / security / vision）将任务 fail 并退单给不同角色（如 coder）时，普通单任务 retry 不够——需要 reviewer 重新 approve 才能解锁下游。V10 引入两阶段链：

**isReviewReject 判断条件：**
```
original.AssignedTo != retryAgent &&
(original.AssignedTo == "thinker" || original.AssignedTo == "security" || original.AssignedTo == "vision")
```
即：reviewer 失败且退单目标与 reviewer 不同。

**autoRetryReviewReject 执行逻辑：**

```
原任务 X（thinker/security/vision, failed）
  ↓ 创建两阶段链（单事务）
  fix 任务 X-fix（retryAgent, 如 coder）← 立即 dispatch 唤醒
      ↓ depends_on: X-fix
  re-review 任务 X-rr（original.AssignedTo, 如 thinker）← X-fix done 后自动解锁
```

- `X.superseded_by = X-rr.id`（指向 re-review，不是 fix）
- 下游依赖 X 的任务（如 qa）继续被 `depsMetForID` 阻塞，直到 X-rr PATCH done 才解锁
- X-fix 继承 X 的 `depends_on`（维持链路位置）

**UpdateSupersededByChain（多级退单支持）：**

当 re-review 再次 failed（第二次退单），系统创建新一轮两阶段链前，先调用 `store.UpdateSupersededByChain(old, new)` 将所有已存在的 `superseded_by = X-rr.id` 重定向到新 re-review 任务，确保依赖链始终跟随最新 re-review，不出现 double-unlock。

**V10.1 新增：vision + 4 条 seed rules（commit 9f33672）**

- vision 加入 `isReviewReject` 判断列表（UI 设计审核退单给 uiux/coder 时走两阶段链）
- retry_routing seed 新增 4 条规则：

```sql
('vision',  '',  'coder',   0),  -- vision 退单默认给 coder（UI实现问题）
('vision',  'ui','uiux',   10),  -- vision 发现 UI 设计问题退给 uiux
('pm',      '',  'thinker', 0),  -- pm 退单默认给 thinker（需求/架构问题）
('ops',     '',  'devops',  0),  -- ops 退单默认给 devops（运维执行问题）
```

合计 retry_routing seed：16 条（原 12 条 + 新增 4 条）。

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

### 链路完成通知消息（→ CEO session，V8 新增）

```
[agent-queue] ✅ 任务链完成：{chain_title 或 "链路 chain_id"}
完成任务数：{done_count}/{total_count}
链路任务：
  ✅ {task1.title} ({task1.assigned_to}) — {task1.result}
  ✅ {task2.title} ({task2.assigned_to}) — {task2.result}
  ✅ {task3.title} ({task3.assigned_to}) — {task3.result}
chain_id: {chain_id}
```

**触发条件：** 链内任意任务 PATCH done 时，若 `chain_id != "" && notify_ceo_on_complete=true`，server 调 `IsChainComplete(chain_id)`，确认全链完成后触发 `OnChainComplete`。

**设计：** 与 dispatch 极简格式不同，链路完成消息含完整子任务结果——因为 CEO 此时是"结果汇总者"角色，需要知道每步的 result 才能决定下一阶段。

### SessionNotifier 发送目标原则

- 每个事件**只发给需要知道的 1 个 session**，不广播
- dispatch 新任务 → 目标专家 session
- dispatch/chain → 链中第一个专家
- done 触发下游解锁 → 下游专家 session（自动唤醒，V8 triggered 缺口修复）
- failed 需 CEO 介入 → CEO session
- chain 全部完成（notify_ceo_on_complete=true） → CEO session（V8 新增）

---

## §6 CEO 感知机制

### 当前 CEO 感知链路

| 事件 | CEO 如何感知 |
|------|------------|
| 任务 done | 不直接感知（串行链靠 Go server 自动推进，不需要 CEO）|
| 任务 failed（有 retry_assigned_to，含 retry_routing 表命中） | 不感知（自动退单，Go server 处理）|
| 任务 failed（无 retry_assigned_to 且 retry_routing 无匹配） | SessionNotifier 唤醒 CEO session |
| 链路全部完成（notify_ceo_on_complete=true）[V8] | SessionNotifier.OnChainComplete 唤醒 CEO session，含完整子任务结果 |
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
| sessions_send 无法检测投递失败（fire-and-forget，`timeoutSeconds:0` 永远返回 `ok:true`） | 无法区分"成功投递"和"session 不在静默丢失" | **V9 功能 B**：CEO 通知（failed/chain_complete）走内存重试队列（3次/30s-60s-120s）；**V9 功能 C**：stale ticker 兜底 |
| sessions_send 目标 session 不在 | CEO 可能不知道 failed；专家可能不知道有新任务 | **V9 功能 B** 重试 CEO 通知；**V9 功能 C** stale ticker 10min 扫描，30min 无人 claim → 自动 re-dispatch |
| dispatch sessions_send 丢失 | 专家不知道有新任务，任务长时间挂起 | **V9 功能 C** stale ticker：pending+deps_met+30min 无 claim → 自动 re-dispatch（替代原"CEO 手动重派"方案） |
| Go server 宕机 | PATCH 404，专家 PATCH 失败；内存重试队列丢失 | launchd KeepAlive 自动重启（秒级恢复）；重启后 CEO startup 巡检 `GET /tasks/summary` 补漏；专家降级 sessions_send CEO |

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

**per-agent webhook 路由（V11 新增，F15）：**

`DiscordNotifier` 支持按 `task.AssignedTo` 路由到不同 Discord 频道：

- 环境变量 `AGENT_QUEUE_AGENT_WEBHOOKS`，格式：`agent1=url1,agent2=url2,...`
- 路由逻辑：按 `assigned_to` 查表 → miss 时 fallback 默认 `AGENT_QUEUE_DISCORD_WEBHOOK_URL`
- 用途：done/failed 通知投递到各专家专属频道，而非全部汇聚到一个默认频道

**当前部署配置（9专家独立 webhook）：**

```
coder   → #工程师 频道
thinker → #思想家 频道
writer  → #文案师 频道
devops  → #运维 频道
security→ #安全 频道
ops     → #运营 频道
pm      → #产品 频道
vision  → #视觉分析师 频道
qa      → #测试工程师 频道
```

未配置 agent 的任务通知仍投递到默认频道（`AGENT_QUEUE_DISCORD_WEBHOOK_URL`）。

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

**V8 新增请求体字段：**

```json
{
  "tasks": [...],
  "notify_ceo_on_complete": true,   // V8 新增：链路全部完成时是否 SessionNotifier 唤醒 CEO（默认 false）
  "chain_title": "需求→设计→实现"   // V8 新增：链路名称，用于 CEO 通知消息中展示（可选）
}
```

**行为：** `notify_ceo_on_complete` 写入链内所有任务的 `notify_ceo_on_complete` 列；`chain_id` 由 server 自动生成写入全链任务。向后兼容：旧请求不传这两个字段，行为不变（不通知 CEO）。

### F11（V8 新增）：GET/POST/DELETE /retry-routing（retry 路由表管理）

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/retry-routing` | 查询全部 retry 路由规则 |
| `POST` | `/retry-routing` | 添加新规则（assigned_to + error_keyword + retry_assigned_to + priority） |
| `DELETE` | `/retry-routing/:id` | 删除规则 |

**用途：** 在线调整 retry 路由，无需重启 server。初始数据（9 专家映射）在 server 启动时 seed。

### F12（V8 新增，可选）：GET /chains/:chain_id

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/chains/:chain_id` | 查询某条链路的所有任务状态 |

**响应示例：**
```json
{
  "chain_id": "chain_1706271600_abc123",
  "chain_title": "需求→设计→实现",
  "total": 3,
  "done": 2,
  "is_complete": false,
  "tasks": [
    {"id": "xxx", "title": "A任务", "status": "done", "assigned_to": "pm", "result": "..."},
    {"id": "yyy", "title": "B任务", "status": "done", "assigned_to": "thinker", "result": "..."},
    {"id": "zzz", "title": "C任务", "status": "in_progress", "assigned_to": "coder", "result": ""}
  ]
}
```

**用途：** CEO 或用户查询整条链路进度，无需逐个查子任务。

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
  chain_id TEXT NOT NULL DEFAULT '',       -- V8 新增：任务所属链路标识（dispatch/chain 自动填入）
                                           -- 格式：`chain_{timestamp}_{rand}` 或空字符串（非链路任务）
  notify_ceo_on_complete INTEGER NOT NULL DEFAULT 0,  -- V8 新增：链路完成时是否通知 CEO
                                                       -- dispatch/chain 的 notify_ceo_on_complete=true 时全链任务都置 1
  parent_id, mode, requires_review,
  result,                         -- done 时写摘要，failed 时写失败原因（支持 "原因 | retry_assigned_to: X" 格式）
  version, started_at, created_at, updated_at
)

-- 任务依赖关系
task_deps (task_id, depends_on_task_id)

-- 状态变更历史
task_history (id, task_id, from_status, to_status, changed_by, note, changed_at)

-- V8 新增：retry 路由表（服务端自动查表，替代 SOUL.md 硬编码映射）
retry_routing (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  assigned_to TEXT NOT NULL,      -- 遇到问题的专家（from）
  error_keyword TEXT NOT NULL DEFAULT '',  -- 匹配 result 中的关键词（空=匹配所有）
  retry_assigned_to TEXT NOT NULL,         -- 退单目标（to）
  priority INTEGER NOT NULL DEFAULT 0      -- 同 assigned_to 多条规则时的优先级（高优先）
)

-- retry_routing 初始数据（16条 seed，V10.1 更新）
INSERT INTO retry_routing (assigned_to, error_keyword, retry_assigned_to, priority) VALUES
  ('qa',      'bug',        'coder',   10),
  ('qa',      'ui',         'uiux',    10),
  ('qa',      '',           'coder',    0),   -- 兜底：qa 退单默认给 coder
  ('coder',   '架构',        'thinker', 10),
  ('coder',   '需求',        'pm',      10),
  ('coder',   '',           'thinker',  0),   -- 兜底：coder 退单默认给 thinker
  ('writer',  '',           'pm',       0),
  ('devops',  'bug',        'coder',   10),
  ('devops',  '架构',        'thinker', 10),
  ('devops',  '',           'coder',    0),   -- 兜底：devops 退单默认给 coder
  ('thinker', '',           'writer',   0),
  ('uiux',    '',           'pm',       0),
  -- V10.1 新增 4 条
  ('vision',  'ui',         'uiux',    10),   -- vision 发现 UI 设计问题退给 uiux
  ('vision',  '',           'coder',    0),   -- 兜底：vision 退单默认给 coder
  ('pm',      '',           'thinker',  0),   -- pm 退单默认给 thinker
  ('ops',     '',           'devops',   0);   -- ops 退单默认给 devops
```

**retry_routing 查表优先级链：**

```
1. PATCH body 显式传 retry_assigned_to → 直接用
2. failparser 从 result 解析 "| retry_assigned_to: X" → 用
3. 查 retry_routing 表：SELECT retry_assigned_to FROM retry_routing
     WHERE assigned_to = ? AND (error_keyword = '' OR result LIKE '%' || error_keyword || '%')
     ORDER BY priority DESC LIMIT 1
4. 无匹配 → SessionNotifier 唤醒 CEO（人工决策）
```

---

## §7 SessionNotifier 可靠性机制（V9）

### 核心约束：sessions_send 无法检测投递失败

OpenClaw `/tools/invoke` 的 `sessions_send` 使用 `timeoutSeconds: 0`（fire-and-forget），返回 `ok: true` 即使目标 session 不存在。**调用方无法区分成功投递和 session 不在静默丢失。**

这意味着：
- "重试到成功"策略在协议层不可行——你永远不知道什么时候成功
- 重试的价值是"增加概率"——session 可能在两次尝试之间被创建

### 功能 B：内存重试队列（CEO 通知专用）

**新增文件：** `internal/notify/retry.go`（~80 行）

```go
type retryItem struct {
    sendFunc  func() error  // 闭包：captures sessionKey + message
    attempts  int
    nextRetry time.Time
    label     string         // 日志用："OnFailed:task_id" / "OnChainComplete:chain_id"
}

type RetryQueue struct {
    mu    sync.Mutex
    items []retryItem
    stop  chan struct{}
}
```

**行为：**
- `Enqueue(label, sendFunc)`：立即调用一次；若 HTTP 报错（非 ok），加入队列
- background goroutine 每 10s tick，按退避策略重试
- 退避：30s → 60s → 120s，第 3 次失败后放弃，仅 log

**哪些通知走重试队列：**

| 通知类型 | 重试 | 理由 |
|---------|------|------|
| failed → CEO | ✅ | CEO 必须感知，延迟=阻塞 |
| chain_complete → CEO | ✅ | CEO 需要感知链路完成 |
| dispatch → 专家 | ❌ | 任务在 SQLite，专家 startup poll 兜底；功能 C 再兜底 |
| triggered → 下游专家 | ❌ | 同上 |

**放弃后的兜底：** 3 次重试失败 = OpenClaw Gateway 大概率宕机；launchd 秒级重启后 CEO startup 巡检 `GET /tasks/summary` 补漏。不走 Discord webhook 兜底（webhook 无法触发 CEO agent，兜底无意义）。

**server 重启丢失队列：** 可接受。launchd 秒级重启，SQLite 持久化是真正保底——任务状态不丢，CEO startup 巡检会发现 failed 并人工决策。

### 功能 C：stale 任务巡检 ticker

**改动文件：** `internal/store/store.go`（~20 行）+ `internal/handler/handler.go`（~40 行）+ `cmd/server/main.go`

**stale 定义：**
```sql
-- store.ListStaleCandidates
SELECT id, title, assigned_to FROM tasks
WHERE status = 'pending'
  AND assigned_to != ''
  AND updated_at < datetime('now', ?)   -- ? = '-30 minutes'
ORDER BY updated_at ASC
LIMIT 20
```

Go 层 post-filter：逐个调 `depsMetForID`（含 V7 superseded_by 扩展），过滤依赖未满足的任务。

**行为：**
```
StartStaleTicker(interval=10min):
  每 10min tick → checkStaleTasks()
    → ListStaleCandidates(30min) → post-filter deps_met
    → for each stale task:
        SessionNotifier.Dispatch(task.AssignedTo)   // re-dispatch
        store.TouchUpdatedAt(task.ID)                // 重置 30min 倒计时
```

`TouchUpdatedAt`：`UPDATE tasks SET updated_at = ? WHERE id = ?`，**不改 version**（防影响乐观锁）。

**参数可配置（环境变量）：**
- `AGENT_QUEUE_STALE_CHECK_INTERVAL`（默认 10m）
- `AGENT_QUEUE_STALE_THRESHOLD`（默认 30m）
- `AGENT_QUEUE_MAX_STALE_DISPATCHES`（默认 3）：stale re-dispatch 上限（V11 新增）

**stale_dispatch_count 上限机制（V11）：**

`tasks` 表新增 `stale_dispatch_count` 列，每次 stale re-dispatch 时 `TouchUpdatedAt` 同步自增。

当 `stale_dispatch_count >= maxStaleDispatches`：
- 停止 re-dispatch（不再唤醒专家）
- 触发 `SessionNotifier.OnFailed(task)` 向 CEO session 发送告警
- 告警格式：`FailureReason = "stale max dispatches reached (N/N)"`

**意图：** 防止 stale ticker 对无响应专家无限轰炸。达到上限 = 系统认为该专家 session 异常，升级为人工决策。

**与功能 B 的互补关系：**

| 场景 | 功能 B | 功能 C |
|------|-------|-------|
| failed → CEO sessions_send HTTP 报错 | ✅ 30s/60s/120s 重试 | ❌ |
| chain_complete → CEO HTTP 报错 | ✅ 重试 | ❌ |
| dispatch/triggered → 专家 session 不在 | ❌ | ✅ 30min 后 re-dispatch |
| 专家 session 不在（通用） | ❌ | ✅ 任务 pending 30min 无 claim → re-dispatch |

功能 B 覆盖短时瞬态失败（秒级），功能 C 覆盖长时 session 缺失（分钟级）。

---

## 技术选型

| 层 | 选型 | 说明 |
|----|------|------|
| **存储** | SQLite（WAL 模式） | 单文件，零部署，ACID 事务，WAL 支持并发读写 |
| **API 服务** | Go `net/http`（无框架） | 单二进制；handler ~800行 + store ~700行；V8 retry_routing + V9 RetryQueue + stale ticker + V10 review-reject 两阶段链 |
| **通知** | Discord Incoming Webhook + OpenClaw sessions_send | 双通道：webhook=用户审计，sessions_send=agent感知；V9 内存重试队列提升 CEO 通知可靠性 |
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

# V11 新增：per-agent webhook 路由（格式：agent1=url1,agent2=url2,...）
export AGENT_QUEUE_AGENT_WEBHOOKS="coder=https://...,thinker=https://...,writer=https://..."

# V11 新增：stale re-dispatch 上限（默认 3）
export AGENT_QUEUE_MAX_STALE_DISPATCHES=3
```

**全量环境变量说明：**

| 变量 | 说明 | 默认值 |
|------|------|--------|
| `AGENT_QUEUE_DISCORD_WEBHOOK_URL` | 默认 Discord Incoming Webhook URL（done/failed 通知） | — |
| `AGENT_QUEUE_AGENT_WEBHOOKS` | per-agent webhook 路由，格式 `agent1=url1,agent2=url2`；按 `assigned_to` 路由，miss 时 fallback 默认 URL（V11） | — |
| `AGENT_QUEUE_DISCORD_USER_ID` | Discord 用户 ID，用于 failed 消息 @mention | — |
| `AGENT_QUEUE_OPENCLAW_API_URL` | OpenClaw Gateway URL（sessions_send 依赖） | `http://localhost:18789` |
| `AGENT_QUEUE_OPENCLAW_API_KEY` | OpenClaw Gateway token | — |
| `AGENT_QUEUE_DB_PATH` | SQLite 数据库路径（推荐绝对路径） | `data/queue.db` |
| `AGENT_QUEUE_STALE_CHECK_INTERVAL` | stale ticker 扫描间隔 | `10m` |
| `AGENT_QUEUE_STALE_THRESHOLD` | 无 claim 超过此时长视为 stale | `30m` |
| `AGENT_QUEUE_MAX_STALE_DISPATCHES` | stale re-dispatch 上限，达到后告警 CEO（V11） | `3` |

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
- **[V8] 为什么 chain_id 不建 chain 表：** chain_id 是分组标签（`chain_{timestamp}_{rand}`），存在 tasks 表即可；chain 表增加 JOIN 复杂度，无实质收益；`GET /chains/:chain_id` 直接 `SELECT * FROM tasks WHERE chain_id = ?` 实现
- **[V8] 为什么 notify_ceo_on_complete 标记在每个任务而非只标记链尾：** autoRetry 可替换链中任意任务，如只标记链尾，retry 任务不继承标记；全链任务都标记后，任何任务 done 时统一检查 IsChainComplete，逻辑一致
- **[V8] 为什么 retry_routing 在 SQLite 表而非 SOUL.md 硬编码：** SOUL.md 是 prompt 文件，修改退单规则需重部署 9 个专家；Go server 动态查表，在线 CRUD `/retry-routing`，无需重启；且从 SOUL.md 中删除 15-20 行映射规则，降低每次 session 的 context 成本
- **[V8] triggered 缺口为何存在：** unlockDependents 早期设计只是"解锁依赖"，返回 triggered 列表供响应体展示；SessionNotifier 唤醒下游专家的逻辑被遗漏，导致串行链依赖解锁后下游专家不会自动被唤醒。V8 修复：patchTask done 分支遍历 triggered，逐一调 SessionNotifier.Dispatch
- **[V9] 为什么不做 SQLite 持久化重试队列：** sessions_send 协议层无法确认投递（`timeoutSeconds:0` 永远返回 `ok:true`），SQLite 队列无法判断"何时停止重试"，会堆积永不清空的残留记录。内存重试 3 次足够覆盖"session 刚重建"窗口期；重启丢队列由 launchd+CEO startup 巡检兜底
- **[V9] 为什么不加 `notified` 列：** `notified` 只记录创建时的通知状态，不反映后续 triggered dispatch 的状态；逻辑上"30 分钟没人 claim"就是 stale，无需区分原因；避免 schema 膨胀
- **[V9] TouchUpdatedAt 不改 version：** stale re-dispatch 是系统内部操作，不应影响乐观锁；专家 claim 时 version 不变，避免 409 冲突
- **[V9] stale ticker 为何选 Go server 内置而非 OpenClaw cron：** OpenClaw cron 发 systemEvent 给 CEO，但 CEO session 不在时消息丢失——这正是 stale 巡检要解决的问题，形成循环依赖。Go server 自驱 re-dispatch 不依赖任何外部 agent
- **[V10] 为什么 superseded_by 指向 re-review 而非 fix：** fix 任务完成后下游（qa）不应立即解锁——需要 reviewer 二次确认才算通过。若 superseded_by 指向 fix，qa 在 fix done 时就解锁，绕过了 re-review 校验环节。指向 re-review 确保 qa 必须等 reviewer approve 才能执行
- **[V10] 为什么 isReviewReject 只判断特定 reviewer 角色：** 任何 assigned_to != retryAgent 都可以是 review-reject，但只有 thinker/security/vision 的职责是"审核"——这些角色失败后自然需要重新审核。其他角色（如 coder 退单 thinker）是技术求助，不需要原角色二次 approve
- **[V10] UpdateSupersededByChain 为何必要：** 多级退单时，若只 SetSupersededBy 而不更新旧 superseded_by 指针，depsMetForID 仍会跟随旧 re-review，下游任务在旧 re-review done 时就解锁，新一轮 fix+re-review 失去约束力
- **[V10.1] cancelled 终态语义：** `cancelled` = 任务主动放弃，不触发 autoRetry，不解锁下游依赖。`IsChainComplete` 中 cancelled 任务视为 None（链未完成），不等价于 done——这样 chain 中有 cancelled 任务时，链路完成通知不会误触发
- **[V11] per-agent webhook 而非单一全局 webhook：** 单一全局 webhook 所有通知混入同一频道，成员难以辨别哪个任务属于哪个角色；per-agent 路由让 done/failed 通知精准投递到对应专家频道，CEO 频道不被噪音淹没。miss 时 fallback 默认 URL 确保向后兼容
- **[V11] stale_dispatch_count 上限而非无限重试：** 无响应专家可能是 session 异常/网络断连，无限 re-dispatch 只会产生通知风暴；上限（默认 3 次）后升级为 CEO 告警，转人工决策，比无限轰炸更合理

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
- 测试任务 `assigned_to` 统一用 `test` 或 `e2e-*` 前缀（如 `e2e-coder`、`e2e-qa`）
- 禁止使用真实 agent 名（`coder/thinker/qa/devops/writer/ops/vision/pm/uiux/security`）
  - 违规后果：stale ticker 每 10 分钟重新 dispatch，向真实 agent session 发送通知风暴
- **例外**：验证 retry_routing 表路由（如 `coder → thinker`）必须使用真实 agent 名
  - 此类任务 title 必须加 `[TEST]` 前缀（如 `[TEST] V7-coder`）
  - 测试完成后立即 PATCH `status=cancelled`（利用 STEP2 引入的 cancelled 终态清理）
- 没有任何专家的 SOUL.md 配置了 `assigned_to=test` 或 `e2e-*`，测试任务不会被真实 agent claim
- e2e 脚本必须在脚本末尾调用 cleanup 函数，cancel 所有残留的 `[TEST]` 前缀任务

---

## 标准任务链路

### 代码任务标准链路

代码类任务统一使用三段串行链路，通过 `POST /dispatch/chain` 一次提交：

```bash
curl -X POST http://localhost:19827/dispatch/chain \
  -H "Content-Type: application/json" \
  -d '{
    "chain_title": "实现[功能名]",
    "notify_ceo_on_complete": true,
    "tasks": [
      {"title": "实现[功能]",   "assigned_to": "coder",  "description": "..."},
      {"title": "QA测试[功能]", "assigned_to": "qa",     "description": "..."},
      {"title": "reload",       "assigned_to": "devops", "description": "launchd reload + 健康检查"}
    ]
  }'
```

**各节点职责：**

| 节点 | 执行者 | 验收标准 | 失败行为 |
|------|--------|---------|---------|
| 实现 | coder | 编译通过 + lint 无 error；测试文件不得新建/修改 | 架构/需求问题退单 thinker/pm |
| QA gate | qa | 编写/补写测试，全部通过 | 测试失败退单 coder（含失败原因+预期行为）|
| reload | devops | launchd reload + 健康检查通过 | 部署失败退单 coder/thinker |

**QA gate 是硬性卡点：** qa 任务依赖 coder 任务（`depends_on`），只有 coder PATCH done 后 qa 才能被 poll 到。qa 测试全通过才 PATCH done，触发 devops 解锁。测试失败则 PATCH failed + `retry_assigned_to: coder`，自动退单，devops 不会被解锁。

**coder 验收规则（不跑测试）：**
- ✅ 编译通过 + lint 无 error = 验收通过
- 若现有测试 break：列出失败测试名，回执中注明 `⚠️ 测试失败（交QA）: [测试名]`，不修复测试文件
- ❌ 禁止新建/修改 `*_test.go` / `*.test.ts`，测试文件由 QA 独立维护

---

## V11（计划中）：POST /dispatch/graph

### 背景

当前 `/dispatch/chain` 仅支持线性串行链（A→B→C）。复杂任务常需要**并行+汇聚**的 DAG 结构（如：A→[B,C]→D，B/C 并行执行，D 依赖 B+C 都完成）。

现有手动构建方式：
```bash
# 手动多次 POST，逐个设置 depends_on
POST /tasks {"title":"A", ...}                          # A
POST /tasks {"title":"B", "depends_on":["A.id"], ...}   # B depends on A
POST /tasks {"title":"C", "depends_on":["A.id"], ...}   # C depends on A
POST /tasks {"title":"D", "depends_on":["B.id","C.id"]} # D depends on B+C
```

**问题：** 多次 POST 非原子性（部分失败时图状态不一致）；客户端需要记录每个 task_id 并手动穿联；易出错。

### V11 目标：一次 POST 提交完整 DAG

```bash
POST /dispatch/graph
{
  "graph_title": "并行评审流程",
  "notify_ceo_on_complete": true,
  "nodes": [
    {"id": "a", "title": "实现功能",   "assigned_to": "coder"},
    {"id": "b", "title": "架构审核",   "assigned_to": "thinker", "depends_on": ["a"]},
    {"id": "c", "title": "视觉验收",   "assigned_to": "vision",  "depends_on": ["a"]},
    {"id": "d", "title": "QA测试",     "assigned_to": "qa",      "depends_on": ["b","c"]},
    {"id": "e", "title": "部署",       "assigned_to": "devops",  "depends_on": ["d"]}
  ]
}
```

- `nodes[].id`：客户端本地引用 ID（字符串），由 server 转换为 SQLite task_id
- `depends_on`：引用同图内其他 node 的本地 id
- server 在单事务内原子创建所有任务 + 依赖关系
- 返回：所有 node 的 `{local_id → task_id}` 映射 + 链路入口任务（无依赖的 node）的 sessions_send 唤醒

### 当前状态

**V11 尚未实现。** 现阶段使用手动多次 POST 或 `/dispatch/chain`（线性链）替代。V11 实现后，线性链 `/dispatch/chain` 将成为 `/dispatch/graph` 的 DAG 退化形式（仍保留向后兼容）。

**实现预期复杂度：** store.go 新增 `CreateGraph` 事务方法（~60 行）+ handler.go 新增路由（~40 行）+ 客户端 id→task_id 映射逻辑（~30 行）。
