# agent-queue 架构说明

> 版本：v11 | 更新：2026-02-27 | 代码基线：commit `e2f142a` — OnTaskComplete 单任务 notify_ceo_on_complete 支持
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

**触发条件（两个分支）：**
- **链路完成：** 链内任意任务 PATCH done 时，若 `chain_id != "" && notify_ceo_on_complete=true`，server 调 `IsChainComplete(chain_id)`，确认全链完成后触发 `OnChainComplete`（含完整子任务结果）
- **单任务完成（commit e2f142a，2026-02-27）：** 任务 PATCH done 时，若 `chain_id == "" && notify_ceo_on_complete=true`，触发 `OnTaskComplete(task)`；消息格式与 OnChainComplete 相同，走 RetryQueue（30s/60s/120s backoff）

**设计：** 与 dispatch 极简格式不同，完成通知含任务 result——CEO 此时是"结果接收者"角色，需要知道 result 才能决定下一阶段。

### SessionNotifier 发送目标原则

- 每个事件**只发给需要知道的 1 个 session**，不广播
- dispatch 新任务 → 目标专家 session
- dispatch/chain → 链中第一个专家
- done 触发下游解锁 → 下游专家 session（自动唤醒，V8 triggered 缺口修复）
- failed 需 CEO 介入 → CEO session
- chain 全部完成（notify_ceo_on_complete=true） → CEO session（V8 新增）
- 单任务完成（chain_id="" && notify_ceo_on_complete=true） → CEO session（e2f142a 新增）

---

## §6 CEO 感知机制

### 当前 CEO 感知链路

| 事件 | CEO 如何感知 |
|------|------------|
| 任务 done | 不直接感知（串行链靠 Go server 自动推进，不需要 CEO）|
| 任务 failed（有 retry_assigned_to，含 retry_routing 表命中） | 不感知（自动退单，Go server 处理）|
| 任务 failed（无 retry_assigned_to 且 retry_routing 无匹配） | SessionNotifier 唤醒 CEO session |
| 链路全部完成（notify_ceo_on_complete=true）[V8] | SessionNotifier.OnChainComplete 唤醒 CEO session，含完整子任务结果 |
| 单任务完成（chain_id="" && notify_ceo_on_complete=true）[e2f142a] | SessionNotifier.OnTaskComplete 唤醒 CEO session，含任务 result |
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

> Note: F7–F10 form the “dispatch → poll → claim → patch” baseline for autonomous expert execution.

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

**retry_routing 幂等性修复（commit 34b43a0，2026-02-26）：**
server 重启时 seed 函数重复插入 retry_routing 记录，导致记录数从 16 增长到无限大，ticker 反复消费重复路由规则。
**修复：** 为 `(assigned_to, error_keyword, retry_assigned_to)` 添加 UNIQUE INDEX；seed 函数改用 `INSERT OR IGNORE`；追加 migration 去重现有重复数据。
重启后记录数稳定在 19（16 条标准 seed + 3 条历史补充），不再增长。

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
| single_task_complete → CEO | ✅ | CEO 需要感知单任务完成（e2f142a 新增）|
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

---

## V12 — AI-native 工作台架构

> 设计文档：`~/.openclaw/workspaces/thinker/docs/2026-02-27-ai-workbench-arch.md`
> 代码基线：commit `6b3d9f6`（当前）→ AI Workbench 新增
> 架构师：🧠 thinker | 日期：2026-02-27

### 设计目标

从 agent-queue（纯 API server, ~7000 行 Go）扩展为全栈 AI-native 工作台：

- 前端 SPA 嵌入 Go 二进制（`embed.FS`），单二进制部署不变
- 开源就绪：陌生人 clone → build → run，5 分钟上手
- 不破坏现有 API（100% 后向兼容）
- 不绑定 OpenClaw——可配置化（config.yaml 控制开关）

### 核心技术决策

| 决策点 | 选择 | 否决备选 |
|--------|------|---------|
| Repo 策略 | Monorepo（留在 agent-queue） | Multi-repo（发布流程复杂，开源体验差）|
| 前端框架 | Vue 3 + TypeScript | React; htmx（htmx 扩展性差）|
| UI 库 | Tailwind CSS + 自建组件 | Element Plus; Naive UI（开源不绑 UI 库，bundle 小）|
| 构建工具 | Vite | Webpack |
| 前端嵌入 | embed.FS | 分离部署（单二进制是核心卖点）|
| 配置系统 | YAML + 环境变量 | Viper（依赖重，30+ 传递依赖）|
| OpenAPI | 手写 `docs/api/openapi.yaml` | swaggo 注解（handler 代码膨胀 30%+）|
| API client | openapi-typescript-codegen 生成 | 手写（类型安全 + 减少维护量）|

### 新增目录结构

```
agent-queue/
├── web/                        # 前端 SPA 源码
│   ├── src/
│   │   ├── api/                # API client（从 OpenAPI 生成）
│   │   ├── components/         # UI 组件（dashboard/timeline/kanban/forms/common）
│   │   ├── composables/        # usePolling, useTask 等
│   │   ├── pages/              # DashboardPage/GoalInputPage/KanbanPage/TaskDetailPage
│   │   ├── stores/             # Pinia 状态管理（task/dashboard/chain/config）
│   │   └── types/              # TypeScript 类型定义
│   ├── package.json
│   ├── vite.config.ts          # API 代理（开发模式）+ outDir=../dist
│   └── tsconfig.json
├── dist/                       # 构建产物（gitignore），embed.FS 读取点
├── docs/api/
│   └── openapi.yaml            # OpenAPI 3.0 手写 spec（全部端点）
├── internal/config/            # YAML 配置加载（新增）
│   ├── config.go               # Config struct + Load()（~120 行）
│   └── config_test.go
└── .github/workflows/          # CI/CD
    ├── test.yml                # push/PR → Go test + 前端 test
    ├── build.yml               # push main → 跨平台构建（4 平台）
    └── release.yml             # tag v* → GitHub Release
```

### 前端技术栈

| 层 | 选型 | 版本 |
|----|------|------|
| 框架 | Vue 3 | ^3.5 |
| 语言 | TypeScript | ^5.5 |
| 构建 | Vite | ^6 |
| 样式 | Tailwind CSS | ^4 |
| 状态管理 | Pinia | ^2.2 |
| 路由 | Vue Router | ^4.4 |
| HTTP | fetch + 生成的 client | 原生 |
| 测试 | Vitest + Vue Test Utils | ^3 |

### Go Server 扩展

**Schema 新增 3 字段（幂等 ALTER TABLE）：**
```sql
ALTER TABLE tasks ADD COLUMN timeout_minutes INTEGER NULL;
ALTER TABLE tasks ADD COLUMN timeout_action VARCHAR NULL;  -- 'escalate' | 'skip'
ALTER TABLE tasks ADD COLUMN commit_url VARCHAR NULL;
```

**新增 API 端点（/api/ 前缀，现有端点路径不变）：**

| 端点 | 说明 | 改动量 |
|------|------|--------|
| `GET /` | SPA 入口（embed.FS + SPA fallback） | ~30 行 |
| `GET /api/dashboard` | 聚合查询：待办+异常+目标概览 | ~60 行 |
| `GET /api/timeline/:id` | 任务时间线（task_history 格式化） | ~40 行 |
| `GET /api/chains` | 链路列表（目标追踪用） | ~30 行 |
| `GET /api/config` | 前端获取可配项（已知 agents 等） | ~15 行 |
| `PATCH /tasks/:id` | 扩展 commit_url / timeout_* 字段 | ~15 行修改 |

**embed.FS 集成（cmd/server/main.go）：**
```go
//go:embed all:dist
var webFS embed.FS

// --static-dir=path 开发模式，读本地文件
// 不传 → 用 embed.FS（生产模式）
// 非 API 路径 + 非静态文件 → 返回 index.html（SPA 路由兜底）
```

**后端改动总量：~545 行**（config 新增 ~200 行 + handler/store/model/db 修改 ~345 行）

### 配置系统（internal/config）

加载优先级：命令行 flag > 环境变量 > config.yaml > 默认值

```yaml
server:
  port: 19827
  db: data/queue.db
agents:
  known:
    - {name: coder, label: 工程师}  # 从配置读取，不硬编码
timeouts:
  agent_minutes: 30
  stale_check_interval: 10m
  stale_threshold: 30m
notifications:
  webhook_url: ""
  openclaw_url: http://localhost:18789
  openclaw_key: ""
web:
  static_dir: ""  # 空=embed.FS；非空=开发模式本地路径
```

**向后兼容：** 所有现有环境变量继续生效；无配置文件时行为不变。不引入 Viper（依赖重）。

### 分期实施方案

| 阶段 | 内容 | 估时 | 主要执行者 |
|------|------|------|-----------|
| P1 骨架 | web/ 脚手架 + config 包 + embed.FS + schema +3 字段 + 新 API 端点 | ~3 天 | coder + qa |
| P2 核心页面 | Dashboard/Goal/Kanban/Timeline + 超时 ticker + 前端测试 | ~5 天 | coder + uiux + qa |
| P3 开源就绪 | README 重写 + CONTRIBUTING.md + OpenAPI spec + CI/CD | ~2 天 | writer + devops + qa |

**总估算：~10 天**（P1/P2 coder 与 uiux 可并行；P3 writer 与 coder 可并行）

### 风险与缓解

| 风险 | 级别 | 缓解 |
|------|------|------|
| embed.FS 增大二进制 | 低 | 前端 gzip 后 <100KB，二进制增加 <1MB |
| Go embed 行为变化 | 低 | embed.FS 从 Go 1.16 稳定，无 breaking change 历史 |
| 前端构建增加 CI 时间 | 低 | npm ci ~30s + vite build ~10s，总增加 <1min |
| OpenAPI spec 与代码不同步 | 中 | CI 中加 spec 校验步骤（响应 schema 匹配测试）|
| handler.go 膨胀 | 低 | 当前 962→~1160 行，<1500 行红线不拆；超线再拆 |
| Tailwind v4 breaking change | 低 | v4 已稳定；锁 package-lock.json 版本 |

---

## V13 — autoAdvance 条件分支（2026-02-27）

> commit: `319f948`

### 设计动机

autoRetry 解决了失败路径的自动路由（fail → retry agent），但成功路径仍需 CEO 手动推进下一步。V13 补全成功路径：任务完成后自动派发下一任务，无需 CEO 介入。

**与 autoRetry 的对称关系：**

| 机制 | 触发条件 | 行为 |
|------|---------|------|
| autoRetry | PATCH status=failed | 自动创建 retry task → dispatch 到 retry_assigned_to |
| autoAdvance | PATCH status=done | 自动创建 next task → dispatch 到 auto_advance_to |

### 新增 Schema 字段（+3）

```sql
ALTER TABLE tasks ADD COLUMN auto_advance_to VARCHAR NULL;
    -- 目标 agent 名，非空时触发 autoAdvance
ALTER TABLE tasks ADD COLUMN advance_task_title VARCHAR NULL;
    -- 下一任务标题（空则自动生成："{原标题} [auto-advance]"）
ALTER TABLE tasks ADD COLUMN advance_task_description TEXT NULL;
    -- 下一任务描述模板（上游 result 自动注入前缀）
```

### 触发逻辑（handler.go patchTask）

```
PATCH status=done
  ↓
task.auto_advance_to != "" ?
  ↓ Yes
  创建新 task：
    assigned_to = auto_advance_to
    title       = advance_task_title ??（原 title + " [auto-advance]"）
    description = "上游结果：{task.result}\n\n" + advance_task_description
    chain_id    = 原 chain_id（继承链路）
    notify_ceo_on_complete = 原值（继承）
  ↓
  SessionNotifier.Dispatch(auto_advance_to)  // 唤醒目标 agent
```

**上游 result 注入：** 下一任务 description 自动以 `"上游结果：{result}\n\n"` 开头，确保下游 agent 拿到完整上下文，无需 CEO 手动传递。

### 与 dispatch/chain 的关系

| 场景 | 推荐方式 |
|------|---------|
| 完整链路已知（A→B→C） | `POST /dispatch/chain`（一次性创建，最清晰）|
| 动态分支（A 完成后根据 result 决定下一步）| autoAdvance / result routing（V14）|
| 单步任务 | `POST /dispatch` 或 `POST /tasks` |

---

## V14 — 结构化 result 路由（2026-02-27）

> commit: `8e9b5a3`

### 设计动机

autoAdvance（V13）在任务创建时静态指定下一 agent。V14 允许专家在 PATCH done 时通过 result 动态决定路由，适合需要"根据结果决定下一步"的场景。

### 触发机制

专家 PATCH done 时，`result` 字段包含合法 JSON 且含 `next_agent` 字段：

```json
{
  "summary": "架构方案完成，建议立即进入实现阶段",
  "next_agent": "coder",
  "next_title": "实现 autoAdvance feature",
  "next_description": "基于架构方案实现 V13 autoAdvance..."
}
```

Go server 解析 result：
1. `next_agent` 存在且为已知 agent → 自动创建 next task + dispatch
2. `next_title` / `next_description` 可选，缺省时自动生成
3. 原 result 原样保存（JSON 字符串），不丢失可读内容

### 优先级规则

```
autoAdvance（V13）优先 > result routing（V14）
```

若任务同时设置了 `auto_advance_to` 且 result 含 `next_agent`：
- 执行 autoAdvance（静态配置优先）
- result routing 被忽略
- 日志记录：`[handler] autoAdvance takes precedence over result routing for task {id}`

### 两者可共存的场景

| 任务配置 | 行为 |
|---------|------|
| 只有 auto_advance_to | 固定路由到指定 agent |
| 只有 result JSON | 动态路由（由专家决定）|
| 两者都有 | autoAdvance 优先，result routing 忽略 |
| 两者都没有 | 不自动推进，依赖 CEO 或 dispatch/chain |

### result 字段兼容性

非 JSON result（普通字符串）完全兼容，Go server 尝试 JSON parse 失败后直接跳过 result routing，行为与 V13 前一致。

---

## V15 — 任务模板 /dispatch/template（2026-02-27）

> commit: `9fe2104`

### 设计动机

频繁使用的多步骤任务链（如"修复→QA→部署"）需要每次手动构造 chain 请求，重复且易出错。V15 引入任务模板系统，将常用链路固化为模板，一次调用即可展开。

### 新增 Schema

```sql
CREATE TABLE IF NOT EXISTS templates (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,       -- 模板唯一名称（如 "fix-qa-deploy"）
    description TEXT NOT NULL,              -- 模板说明
    tasks       TEXT NOT NULL,              -- JSON 数组：[{title, assigned_to, description}, ...]
    created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
```

### 新增 API 端点（5个）

| 端点 | 说明 |
|------|------|
| `POST /templates` | 创建模板（name + description + tasks 数组）|
| `GET /templates` | 列出所有模板 |
| `GET /templates/:name` | 获取单个模板详情 |
| `DELETE /templates/:name` | 删除模板 |
| `POST /dispatch/from-template/:name` | 按模板创建并派发任务（支持变量替换）|

### 内置子模板（3种）

| 模板名 | 链路 | 用途 |
|--------|------|------|
| `fix-qa-deploy` | coder → qa → devops | 代码修复后的标准验证+部署流程 |
| `doc-review` | writer → thinker | 文档起草后的架构师审核 |
| `feature` | pm → coder → thinker → qa → devops | 完整功能开发链路 |

### from-template 变量替换

`POST /dispatch/from-template/:name` 请求体支持变量注入：

```json
{
  "variables": {
    "goal": "修复登录超时 bug",
    "context": "复现步骤：...",
    "branch": "fix/login-timeout"
  },
  "notify_ceo_on_complete": true,
  "chain_title": "登录超时修复链路"
}
```

模板中的 `{goal}`、`{context}` 等占位符自动替换为变量值。

**展开规则：**
- 模板含多个 task → 自动创建 chain（等同 `POST /dispatch/chain`）
- 模板含单个 task → 创建普通任务（等同 `POST /dispatch`）
- 变量未提供时占位符保留原样（不报错）

---

## V16 — agent 任务超时自动处理（2026-02-27）

> commit: `2e36f38`

### 设计动机

agent 任务进入 `in_progress` 后可能因 session 崩溃、网络中断等原因长期不汇报。现有 stale ticker 只处理 `pending` 任务（重派）；V16 扩展为也处理 `in_progress` 超时，自动标记失败并通知 CEO。

### 实现：扩展现有 stale ticker

复用 `internal/handler/stale_ticker.go` 的定时扫描机制，追加 agent 超时检测逻辑：

```
每 10min tick（AGENT_QUEUE_STALE_CHECK_INTERVAL）：
  1. 现有：pending + unclaimed > stale_threshold → re-dispatch
  2. 新增：in_progress + started_at 超过 agent_timeout_minutes → PATCH failed
```

### 超时触发流程

```
扫描 in_progress 任务：
  started_at + agent_timeout_minutes < now
  AND assigned_to != "human"          // 人工任务不自动超时
  AND timeout_action != "skip"        // 配置为 skip 时不自动处理
  ↓
PATCH status=failed
  failure_reason="agent_timeout: exceeded {N}min SLA"
  ↓
触发 handleFailedTask：
  retry_routing 匹配 → 自动改派
  无匹配 → SessionNotifier.OnFailed → CEO session
```

### 配置

| 配置项 | 默认值 | 说明 |
|--------|--------|------|
| `config.agent_timeout_minutes` | `90` | agent 任务超时阈值（分钟）；0 = 禁用 |
| 任务级 `timeout_minutes` 字段 | — | 覆盖全局配置（V12 schema 新增字段）|
| 任务级 `timeout_action` 字段 | `escalate` | `escalate`=通知CEO；`skip`=静默丢弃 |

**配置文件示例：**
```yaml
timeouts:
  agent_minutes: 90     # 全局 agent 超时（V16 新增）
  stale_check_interval: 10m
  stale_threshold: 30m
```

### 与 stale 任务的区别

| 场景 | 状态 | 处理方式 |
|------|------|---------|
| 任务未被认领（pending 超时）| pending | re-dispatch（已有，V11）|
| agent 认领后无响应（in_progress 超时）| in_progress | PATCH failed → retry/CEO（V16 新增）|
| 人工任务超时 | in_progress（human）| 不处理（CEO 负责）|

---

## V17 — SSE 实时更新（2026-02-27）

> commit: `9d6b3c9`

### 设计背景

Web UI 初始版本依赖客户端轮询（每 10-60s 一次）获取任务状态变更。轮询存在两个问题：
1. **延迟**：最多需要等待一个完整轮询周期才能感知变更
2. **无效请求**：无事件时仍产生大量 HTTP 请求

V17 引入 SSE（Server-Sent Events）推送机制，Go server 在任务状态变更时主动推送事件，前端实时更新 UI。

### Go 后端：SSEHub

**新增文件：** `internal/handler/sse.go`（~80 行）

```go
type SSEHub struct {
    clients    map[chan []byte]struct{}  // 已连接的 SSE 客户端
    register   chan chan []byte          // 注册新客户端
    unregister chan chan []byte          // 注销断开的客户端
    broadcast  chan []byte              // 广播事件数据
}

func NewSSEHub() *SSEHub
func (h *SSEHub) Run()                             // goroutine：管理 clients map
func (h *SSEHub) Broadcast(event string, data any) // 序列化并广播 SSE 事件
```

**SSE 端点：** `GET /api/events`

```
Response headers:
  Content-Type: text/event-stream
  Cache-Control: no-cache
  Connection: keep-alive

事件格式：
  event: task_updated
  data: {"id":"...","status":"done","result":"...","updated_at":"..."}
```

**4 种事件类型：**

| 事件名 | 触发时机 | data 字段 |
|--------|---------|-----------|
| `task_updated` | PATCH /tasks/:id 任何状态变更 | task 完整对象 |
| `task_created` | POST /tasks 或 POST /dispatch 创建 | task 完整对象 |
| `task_failed` | PATCH status=failed | task + failure_reason |
| `chain_completed` | chain 所有任务 done | chain_id + tasks 摘要数组 |

**广播插入点（handler.go）：**

```go
// PATCH /tasks/:id done → task_updated
h.hub.Broadcast("task_updated", task)

// PATCH /tasks/:id failed → task_failed
h.hub.Broadcast("task_failed", task)

// POST /dispatch → task_created
h.hub.Broadcast("task_created", task)

// POST /dispatch/chain → task_created（每个子任务）
for _, t := range created {
    h.hub.Broadcast("task_created", t)
}

// chain_completed（OnChainComplete 触发）
h.hub.Broadcast("chain_completed", chainSummary)
```

### Vue 前端：useSSE Composable

**新增文件：** `web/src/composables/useSSE.ts`

```ts
function useSSE(url: string, options?: SSEOptions) {
  // 1. 建立 EventSource 连接
  // 2. 监听各类事件，更新 Pinia store
  // 3. 断线自动重连（指数退避：1s → 2s → 4s → 8s，最大 30s）
  // 4. fallback：SSE 不可用时降级为轮询（保持原有 usePolling 逻辑）
  // 5. 页面隐藏时关闭连接，可见时重连
}
```

**集成页面：**

| 页面 | 集成方式 | 降级行为 |
|------|---------|---------|
| DashboardPage | useSSE 替换 usePolling（任务状态实时刷新）| 降级为 10s 轮询 |
| KanbanPage | useSSE 更新列状态（任务移列动画）| 降级为 60s 轮询 |

**Pinia store 更新逻辑（taskStore.ts）：**

```ts
// SSE 事件处理
onSSEEvent('task_updated', (task) => {
    taskStore.upsert(task)         // 原地更新，不刷新整个列表
})
onSSEEvent('task_created', (task) => {
    taskStore.prepend(task)        // 插入到列表顶部
})
onSSEEvent('chain_completed', (summary) => {
    chainStore.markComplete(summary.chain_id)
})
```

### 连接管理

**并发连接限制：** 无显式上限（SSEHub 使用 Go channel，天然背压）；预期并发用户极少（自用工具），不需要限流。

**连接心跳：** Go server 每 30s 发送一次 SSE comment（`: heartbeat`），防止代理/负载均衡断开空闲连接。

**断线重连策略（前端）：**

```
连接断开 → 等待 1s → 重连
再次断开 → 等待 2s → 重连
... 指数退避（最大 30s）
连接成功 → 重置退避计数
```

重连期间自动切换为轮询，确保数据不中断。

### 与轮询的共存策略

SSE 和轮询不互斥，作为补充：

| 机制 | 触发时机 | 覆盖场景 |
|------|---------|---------|
| SSE（push） | 服务端状态变更时立即推送 | 正常情况，低延迟更新 |
| 轮询（pull） | SSE 断线期间 / 页面加载初始化 | 断线恢复 / 全量数据同步 |

**初始加载仍用 REST API**（GET /tasks）获取全量数据，SSE 只推送增量变更。

---

## V18 — /dispatch/graph DAG API + 任务搜索过滤（2026-02-27）

> commit: `b82b369`

### 功能 A：POST /dispatch/graph（DAG 任务图派发）

#### 设计动机

`POST /dispatch/chain` 只支持线性串行链（A→B→C）。真实项目中常有并行+汇聚结构，例如"先 coder+security 并行，两者都完成后 qa 开始"。V18 引入 DAG API，支持任意有向无环图拓扑。

#### 请求格式

```json
POST /dispatch/graph
{
  "nodes": [
    {"id": "n1", "title": "实现登录功能", "assigned_to": "coder"},
    {"id": "n2", "title": "安全审计", "assigned_to": "security"},
    {"id": "n3", "title": "集成测试", "assigned_to": "qa"}
  ],
  "edges": [
    {"from": "n1", "to": "n3"},
    {"from": "n2", "to": "n3"}
  ],
  "notify_ceo_on_complete": true,
  "chain_title": "登录功能开发+安全审计"
}
```

- `nodes[].id`：客户端本地引用 ID（字符串）
- `edges`：有向边，`from → to` 表示"from 完成后 to 才能开始"
- `notify_ceo_on_complete` + `chain_title`：与 dispatch/chain 一致

#### 响应格式

```json
{
  "chain_id": "chain_xxx",
  "task_map": {
    "n1": "1897xxxx",
    "n2": "1897yyyy",
    "n3": "1897zzzz"
  },
  "dispatched": ["1897xxxx", "1897yyyy"]  // 无前置依赖、已唤醒的节点
}
```

#### 实现：Kahn 拓扑排序

```
1. 验证 DAG（无环检测）：
   - 构建入度表：每个节点统计 in_degree
   - BFS（Kahn）：从 in_degree=0 节点开始逐层展开
   - 若最终处理节点数 < 总节点数 → 存在环，返回 400

2. 事务内原子创建所有任务（store.CreateGraph）：
   - 本地 id → 生成 task_id（ULID）
   - edges → 转换为 task_id 级 depends_on 关系
   - 所有任务同一 chain_id

3. Dispatch 入度=0 的节点（无前置依赖）：
   - SessionNotifier.Dispatch(assignedTo, taskID) 唤醒各 agent
   - 后续节点由 unlockDependents 机制自动解锁（现有逻辑）
```

#### 与 dispatch/chain 的关系

| API | 拓扑结构 | 适用场景 |
|-----|---------|---------|
| `POST /dispatch/chain` | 线性串行（A→B→C） | 已知顺序的标准流程 |
| `POST /dispatch/graph` | 任意 DAG | 含并行/汇聚的复杂任务图 |

`dispatch/chain` 是 `dispatch/graph` 的线性退化形式，两者底层共用 `CreateGraph` 事务方法。

---

### 功能 B：GET /tasks 搜索过滤扩展

#### 新增过滤参数

| 参数 | 类型 | 说明 |
|------|------|------|
| `status` | string | 按状态过滤（已有）|
| `assigned_to` | string | 按 agent 名过滤（已有）|
| `search` | string | **新增**：全文搜索 title + description（SQLite LIKE）|

#### 搜索实现

```sql
-- GET /tasks?search=登录&status=in_progress
SELECT * FROM tasks
WHERE (title LIKE '%登录%' OR description LIKE '%登录%')
  AND status = 'in_progress'
ORDER BY priority DESC, created_at DESC
```

- 大小写不敏感（SQLite LIKE 默认行为）
- `search` 为空时跳过 LIKE 子句，行为与原来完全一致（向后兼容）
- 不引入 FTS5（任务数量小，LIKE 足够；FTS5 增加维护复杂度）

#### 已知缺陷（后续修复）

**`GET /tasks/summary?assigned_to=` 过滤未实现：**

`/tasks/summary` 返回全局统计（各状态计数 + 活跃任务列表），当前不支持按 `assigned_to` 过滤。前端"按 agent 查看统计"功能需等待后续修复。

临时方案：客户端用 `GET /tasks?assigned_to=X` 列表接口自行聚合统计。

---

## V19 Summary 过滤修复 + 动态优先级 (commit: 57c8fe1)

### 背景

V18 引入搜索过滤时，发现 `GET /tasks/summary` 不支持 `assigned_to` 过滤（已在 V18 末尾记录为"已知缺陷"）。V19 完成该修复，并同步新增动态优先级功能（P1）。

---

### 功能 A（P0）：GET /tasks/summary?assigned_to= 过滤修复

#### 问题

`/tasks/summary` 原实现调用 `store.Summary()`，返回全局统计：
- 各状态计数（pending/claimed/in_progress/done/failed）
- 今日完成数（done_today）
- 活跃任务列表（active tasks）

Handler 忽略 HTTP 请求参数（`_ *http.Request`），无法按 agent 过滤。

#### 修复方案

**新增 `store.SummaryFiltered(assignedTo string)`：**

```go
// store.go
func (s *Store) SummaryFiltered(assignedTo string) (SummaryResult, error) {
    // 所有计数查询均附加 WHERE assigned_to = ?（assignedTo 非空时）
    // assignedTo = "" 时退化为全局统计（兼容原 Summary() 行为）
}
```

- 原 `store.Summary()` 改为调用 `SummaryFiltered("")`，行为不变（向后兼容）
- 各状态计数、done_today、active tasks 列表均支持 assigned_to 过滤

**Handler 读取请求参数：**

```go
// handler.go
func (h *Handler) tasksSummary(w http.ResponseWriter, r *http.Request) {
    assignedTo := r.URL.Query().Get("assigned_to")
    result, err := h.store.SummaryFiltered(assignedTo)
    // ...
}
```

#### API 示例

```http
# 全局统计（原行为）
GET /tasks/summary

# 按 agent 过滤
GET /tasks/summary?assigned_to=coder
GET /tasks/summary?assigned_to=qa
```

响应结构不变，仅数据范围缩小为指定 agent 的任务。

---

### 功能 B（P1）：PATCH /tasks/:id 动态优先级

#### 设计

新增优先级字段，支持任务派发后动态调整：

| 值 | 常量 | 含义 |
|----|------|------|
| `0` | normal | 普通（默认）|
| `1` | high | 高优先级 |
| `2` | urgent | 紧急 |

#### 数据层

```go
// model.go
type PatchTaskRequest struct {
    Status      string `json:"status,omitempty"`
    Result      string `json:"result,omitempty"`
    Version     int    `json:"version"`
    Priority    *int   `json:"priority,omitempty"` // 指针：nil 表示不更新
}
```

`store.PatchTask()` 检测 `req.Priority != nil`，追加 `SET priority = ?`：
- **绕过 FSM 状态检查**：优先级更新不经过状态机，任意状态均可更新
- `PATCH /tasks/:id` 响应体包含完整 task 对象（含新 priority + 最新 version）

#### 排序变更

```sql
-- GET /tasks 及内部列表查询
ORDER BY priority DESC, created_at ASC
```

高优先级任务在列表头部；相同优先级时按创建时间升序（FIFO）。

#### 前端（TaskDetailPage.vue）

三档优先级按钮组：

```
[ 普通 ]  [ 高 ]  [ 紧急 ]
```

- 点击后 PATCH `{priority: 0|1|2}`，响应体刷新本地 `task.version`
- 按钮高亮当前选中档位

---

### 验证

```
go test -race ./... ✅
npm run build ✅
```

---

## V20 Scalar API 文档 + DAG 可视化 (commit: 30eefb3)

### 背景

V20 新增两个独立功能：
- **P0**：Scalar API 文档页，让外部开发者无需查阅源码即可了解所有接口
- **P1**：DAG 可视化页面，让 CEO/agent 直观看到任务链路的拓扑结构与执行状态

---

### 功能 A（P0）：GET /docs Scalar API 文档集成

#### 实现方案

通过 CDN 集成 [Scalar](https://scalar.com/)，无需在项目中维护额外 UI 依赖：

```
GET /docs        → 返回 Scalar HTML（CDN 加载，data-url=/openapi.json）
GET /openapi.json → 返回内联 OpenAPI 3.1 spec
```

实现文件：`internal/handler/docs.go`（284 行），包含：
1. OpenAPI 3.1 spec 硬编码（内联，无外部文件依赖）
2. Scalar HTML 模板（CDN 引用 `@scalar/api-reference`）

#### OpenAPI Spec 覆盖范围

共 **18 个端点路径**，按模块分组：

| 模块 | 端点 |
|------|------|
| Tasks | `GET /tasks`, `POST /tasks`, `GET /tasks/{id}`, `PATCH /tasks/{id}`, `POST /tasks/{id}/claim`, `GET /tasks/poll`, `GET /tasks/summary` |
| Dispatch | `POST /dispatch`, `POST /dispatch/chain`, `POST /dispatch/graph` |
| Templates | `GET /templates`, `POST /templates`, `GET /templates/{id}`, `DELETE /templates/{id}` |
| Routing | `GET /routing`, `POST /routing`, `DELETE /routing/{agent}` |
| SSE | `GET /events` |
| Graph | `GET /api/graph/{chain_id}` |

**components/schemas** 定义 6 种请求体结构：
- `CreateTaskRequest`
- `PatchTaskRequest`（含 `priority *int`）
- `ClaimTaskRequest`
- `DispatchRequest`
- `DispatchChainRequest`
- `DispatchGraphRequest`

#### 设计决策

- **内联 spec vs 文件**：spec 硬编码在 Go 源文件中，通过 `embed.FS` 或直接 `[]byte` 返回，不引入额外构建步骤
- **Scalar vs Swagger UI**：Scalar 提供更现代的 UI，CDN 引入零配置，与项目无 npm 依赖耦合

---

### 功能 B（P1）：GET /api/graph/:chain_id + DAG 可视化页面

#### 新端点：GET /api/graph/:chain_id

返回指定链路的任务节点与依赖关系：

```json
{
  "tasks": [
    {
      "id": "abc123",
      "title": "coder 实现登录",
      "status": "done",
      "assigned_to": "coder",
      "depends_on": []
    },
    {
      "id": "def456",
      "title": "qa 验证登录",
      "status": "in_progress",
      "assigned_to": "qa",
      "depends_on": ["abc123"]
    }
  ]
}
```

`Task` 类型新增 `depends_on?: string[]` 字段（`web/src/types/index.ts`）。

#### 前端：GraphVisualizationPage.vue

**布局结构：**
```
┌─────────────────────────────────┐
│  顶部统计栏                       │
│  总数 N / 完成 X / 进行中 Y / ...  │
├─────────────────────────────────┤
│  链路选择器（chain_id 下拉/输入）   │
├─────────────────────────────────┤
│  DAG 拓扑图（左→右分层布局）        │
│                                 │
│  [节点] → [节点] → [节点]          │
├─────────────────────────────────┤
│  底部图例                         │
│  ● 灰=待处理 ● 黄=已认领 ...       │
└─────────────────────────────────┘
```

**层级计算（Kahn BFS 算法）：**

```
1. 构建入度表（in-degree map）
2. 将入度=0 的节点加入队列，层级设为 0
3. BFS 展开：每处理一个节点，将其后继节点入度减 1
4. 入度降为 0 时，后继节点层级 = 当前节点层级 + 1
5. 同层节点在同一列渲染
```

**节点颜色（按状态）：**

| 状态 | 颜色 |
|------|------|
| `pending` | 灰色 |
| `claimed` | 黄色 |
| `in_progress` | 蓝色 |
| `done` | 绿色 |
| `failed` | 红色 |

**交互：**
- 点击节点 → 跳转 `TaskDetailPage`（`/tasks/:id`）

**路由与导航：**
- 路由：`/graph` → `GraphVisualizationPage`（`web/src/router/index.ts`）
- `AppLayout.vue` 导航栏新增 `🕸 DAG` 入口

---

### 验证

```
go test -race ./... ✅
npm run build ✅（8 chunk）
```

---

## V21 搜索 UI + Agent 统计面板 (commit: db19a8b)

### 背景

V21 在已有搜索后端能力（V18 `GET /tasks?search=`）的基础上，补全前端搜索交互；同时新增 Agent 统计面板，让 CEO 可实时查看各 agent 的任务完成率与效率。

---

### 功能 A（P0）：DashboardPage 搜索栏

#### 交互设计

搜索栏插入位置：Error banner 与主任务 grid 之间。

```
┌────────────────────────────────────┐
│  Error banner（如有）               │
├────────────────────────────────────┤
│  🔍 搜索任务...           [✕]       │  ← 新增
├────────────────────────────────────┤
│  任务卡片 grid                      │
└────────────────────────────────────┘
```

**搜索结果 overlay（绝对定位）：**

```
┌──────────────────────────────┐
│  [done] coder 实现登录功能    │  ← 状态 badge + 标题 + agent
│  [in_progress] qa 验证登录   │
│  ...                         │
│  共 N 个结果                  │  ← 页脚
└──────────────────────────────┘
```

- 点击结果项 → 跳转 `/tasks/:id`，搜索框自动清空
- `[✕]` 清空按钮；无结果时显示"未找到相关任务"提示

#### 实现细节

**debounce 300ms（客户端过滤）：**

```js
// DashboardPage.vue
watch(searchQuery, (val) => {
  clearTimeout(searchTimer)
  if (!val.trim()) { searchResults.value = []; return }
  searchTimer = setTimeout(() => doSearch(val), 300)
})

function doSearch(query) {
  // 复用已拉取的本地任务列表，过滤 title + description
  // 无需后端额外改动（V18 后端搜索能力已存在）
  searchResults.value = tasks.value.filter(t =>
    t.title.includes(query) || (t.description ?? '').includes(query)
  )
}
```

**设计决策**：客户端过滤（而非调用 `GET /tasks?search=`）可减少网络请求，适合任务数量有限的场景；列表已在 DashboardPage 缓存，无额外开销。

---

### 功能 B（P1）：GET /api/agents/stats + AgentStatsPage

#### 新端点：GET /api/agents/stats

按 `assigned_to` 聚合任务统计：

```sql
SELECT
  assigned_to,
  COUNT(*) AS total_tasks,
  SUM(CASE WHEN status='done' THEN 1 ELSE 0 END) AS done_count,
  SUM(CASE WHEN status='failed' THEN 1 ELSE 0 END) AS failed_count,
  AVG(
    (julianday(updated_at) - julianday(started_at)) * 24 * 60
  ) AS avg_duration_minutes,
  ROUND(
    done_count * 100.0 / NULLIF(done_count + failed_count, 0), 1
  ) AS success_rate
FROM tasks
WHERE started_at IS NOT NULL
GROUP BY assigned_to
ORDER BY total_tasks DESC
```

响应示例：

```json
[
  {
    "agent": "coder",
    "total_tasks": 42,
    "done_count": 38,
    "failed_count": 2,
    "avg_duration_minutes": 12.5,
    "success_rate": 95.0
  }
]
```

实际生产验收：`/api/agents/stats` 返回 15 个 agents ✅

#### 前端：AgentStatsPage.vue

**布局：** 响应式卡片网格（小屏1列 / 中屏2列 / 大屏3列）

每张 agent 卡片结构：

```
┌────────────────────────┐
│  🤖 coder              │
│  总任务：42             │
│  ████████████░░  95%   │  ← 完成率进度条
│  完成: 38  失败: 2      │
│  平均耗时: 12.5 分钟    │
└────────────────────────┘
```

**进度条颜色阈值：**

| 完成率 | 颜色 |
|--------|------|
| ≥ 80% | 绿色 |
| ≥ 50% | 黄色 |
| < 50% | 红色 |

**路由与导航：**
- 路由：`/stats` → `AgentStatsPage`（`web/src/router/index.ts`）
- `AppLayout.vue` 导航栏新增 `📊 统计` 入口

---

### 验证

```
go test -race ./... ✅
npm run build ✅（9 chunk）
make build + restart：/api/agents/stats 返回 15 个 agents ✅
```

---

## V22 批量操作 + 移动端响应式 (commit: d2d33b4)

### 背景

V22 针对两个独立需求：
- **P0**：CEO 需要对异常任务（failed/stuck）进行批量运维操作（取消/重新分配），逐条操作效率太低
- **P1**：Web UI 在移动端布局破损，需要补全响应式支持

---

### 功能 A（P0）：POST /api/tasks/bulk 批量操作

#### 端点设计

```http
POST /api/tasks/bulk
Content-Type: application/json

{
  "action": "cancel" | "reassign",
  "task_ids": ["id1", "id2", ...],
  "agent": "coder"   // 仅 reassign 时需要
}
```

响应：

```json
{
  "succeeded": ["id1", "id3"],
  "failed": ["id2"],
  "errors": { "id2": "task already terminal" }
}
```

#### 实现细节（internal/handler/bulk.go，129行）

**action=cancel（绕过 FSM）：**

```sql
UPDATE tasks
SET status = 'cancelled', updated_at = NOW()
WHERE id IN (...)
  AND status NOT IN ('done', 'failed', 'cancelled')
```

- 语义：运维强制取消，不经过 FSM 状态检查
- 写入 `task_history` 记录变更（与普通状态流转一致）
- 已 terminal 的任务（done/failed/cancelled）跳过并记入 `failed`

**action=reassign：**

```sql
-- 逐条执行（支持不同 task 分配给不同 agent）
UPDATE tasks SET assigned_to = ?, updated_at = NOW() WHERE id = ?
```

- 任意状态均可重新分配（不限于 pending）
- 逐条处理，部分失败不影响其他条目

#### 前端：DashboardPage 异常面板多选工具栏

布局：

```
┌──────────────────────────────────────────────┐
│  ☑ 全选   已选 3 个任务                        │
│  [批量取消]  [重新分配 → coder ▼]  [清空选择]   │  ← 工具栏（选中时显示）
├──────────────────────────────────────────────┤
│  ☑ [failed] coder  实现登录功能        蓝色高亮 │
│  ☑ [stuck]  qa     验证登录接口        蓝色高亮 │
│  ☐ [done]   devops 部署生产环境                │
└──────────────────────────────────────────────┘
```

- 选中任务时蓝色高亮
- reassign 操作需输入目标 agent 名称
- 操作完成后自动刷新任务列表

---

### 功能 B（P1）：移动端响应式布局

#### AppLayout.vue 改动

**导航栏（`md` 以下）：**

| 尺寸 | 行为 |
|------|------|
| `≥ md`（768px） | 显示完整桌面导航链接 |
| `< md` | 隐藏桌面 nav，显示汉堡菜单（☰）按钮 |

移动端下拉菜单：
- `fixed` 定位，覆盖全屏
- 点击菜单项后自动关闭
- 顶栏 badge 在 `sm` 以下简化为数字圆点（节省空间）

**Sidebar：**
- `lg` 以下（1024px）隐藏
- 主内容区：`lg:ml-52` 补偿侧边栏宽度（大屏有 sidebar 时向右偏移）

#### 各页面断点调整

| 页面 | 改动 |
|------|------|
| DashboardPage | `grid-cols-1 md:grid-cols-2`（移动端单列，桌面双列）|
| KanbanPage | `p-3 md:p-6`（移动端减少内边距；已有 `overflow-x-auto` 横向滚动）|

---

### 验证

```
go test -race ./... ✅
npm run build ✅（9 chunk）
```

---

## V23 Webhook 外发通知 + Timeline 增强 (commits: 205cb09, 4ba8934)

### 背景

V23 由两个并行提交组成：
- **V23-A（P0）**：OutboundWebhookNotifier — 任务完成/失败时向外部系统推送 HTTP 通知（带 HMAC-SHA256 签名）
- **V23-B（P1）**：TaskDetailPage Timeline 增强 — 每个状态变更显示持续时长；同链路任务内联展示

---

### 功能 A（P0）：Outbound Webhook 通知 (commit: 205cb09)

#### 架构

```
任务状态变更（done/failed/cancelled）
        │
        ▼
MultiNotifier（fan-out）
   ├── DiscordNotifier（原有）
   └── OutboundWebhookNotifier（新增）
              │
              ▼（goroutine, best-effort）
        POST <WEBHOOK_URL>
        X-Signature: sha256=<HMAC>
        5s timeout
```

**MultiNotifier**：将多个 `Notifier` 接口实现聚合为 fan-out，对每个子 Notifier 顺序调用 `Notify()`。

#### OutboundWebhookNotifier (internal/notify/outbound_webhook.go，153行)

**触发条件：** `done` / `failed` / `cancelled`（其他状态跳过）

**请求格式：**

```http
POST <AGENT_QUEUE_WEBHOOK_URL>
Content-Type: application/json
X-Signature: sha256=<hex(HMAC-SHA256(secret, body))>

{
  "event": "task.done",
  "task": { ...完整 task 对象... },
  "timestamp": "2026-02-27T14:43:53Z"
}
```

**设计决策：**
- **best-effort**：在 goroutine 中异步执行，5s 超时；失败仅 log，不影响主流程
- **HMAC-SHA256 签名**：接收方可用 `SignatureValid(secret, body, header)` 工具函数验证签名真实性

#### 配置 (internal/config/config.go)

| 环境变量 | 说明 |
|----------|------|
| `AGENT_QUEUE_WEBHOOK_URL` | 外发目标 URL（空则不启用）|
| `AGENT_QUEUE_WEBHOOK_SECRET` | HMAC 签名密钥（空则不签名）|

启动逻辑（cmd/server/main.go）：
```go
if cfg.OutboundWebhookURL != "" {
    notifier = MultiNotifier{discordNotifier, outboundWebhookNotifier}
} else {
    notifier = discordNotifier  // 原有行为
}
```

#### API 版本标记

`GET /api/config` 响应新增：
```json
{
  "version": "v23",
  "outbound_webhook_url": "https://..." // 已配置时返回（遮蔽敏感部分）
}
```

#### 前端：SettingsPage.vue (路由 /settings)

```
⚙️ 系统设置
├── 系统信息（版本 v23）
├── Webhook 状态
│   ├── 已启用：显示遮蔽 URL + 触发事件列表 + 请求格式说明
│   └── 未配置：显示环境变量设置方法
└── Agents 列表（来自 GET /api/agents/stats）
```

导航栏新增 `⚙️ 设置` 入口（AppLayout.vue）。

#### 测试（5个新增）

| 测试名 | 验证点 |
|--------|--------|
| `TestOutboundWebhookNotifier_Done` | 正常 done 事件 + HMAC 签名验证 |
| `TestOutboundWebhookNotifier_Failed` | failed 事件触发 |
| `TestOutboundWebhookNotifier_SkipsNonTerminal` | 非 terminal 状态不触发 |
| `TestOutboundWebhookNotifier_BestEffortOnServerError` | 服务端 500 时不 panic |
| `TestMultiNotifier` | fan-out 到多个 Notifier |

```
go test -race ./... ✅ 全绿
```

---

### 功能 B（P1）：TaskDetailPage Timeline 增强 (commit: 4ba8934)

#### Duration Badge

Timeline 每个状态变更条目新增持续时长 badge：

```
┌──────────────────────────────────────────────┐
│  ● pending → claimed        2026-02-27 14:30  │
│                                               │
│  ● claimed → in_progress    2026-02-27 14:31  │  2m 15s
│                                               │
│  ● in_progress → done       2026-02-27 14:45  │  14m 3s
└──────────────────────────────────────────────┘
```

**实现：**

```js
// historyWithDuration computed
// 将 history 按时间升序排列（oldest → newest）
// 每条 entry 的 duration = 下一条 changed_at - 当前 changed_at
// 首条无 duration（无前置时间点）
```

Badge 样式：`bg-gray-800 font-mono`（等宽字体，深色背景）

格式：`Xm Ys`（如 `2m 15s`，不足 1 分钟时仅显示 `Xs`）

#### Chain 内联展示

若 `task.chain_id` 存在，TaskDetailPage 顶部加载并渲染同链路任务列表：

```
当前任务：[in_progress] qa 验证登录接口

── 所属链路 ──────────────────────────────
  ● [done]        coder  实现登录功能       ← 可点击跳转
  ● [in_progress] qa     验证登录接口       ← 当前任务（蓝色高亮）
  ↓
  ● [pending]     devops 部署到生产         ← 可点击跳转
──────────────────────────────────────────
```

**实现细节：**
- 调用 `GET /api/graph/:chain_id` 获取链路任务
- 仅当 `chainTasks.length > 1` 时展示（单任务不显示）
- 当前任务用蓝色高亮区分
- 加载失败 `try/catch` 静默处理（best-effort，不影响主页面）

---

### 验证

```
go test -race ./... ✅（含5个新 webhook 测试）
npm run build ✅（10 chunks）
```

---

## V24 i18n 中英双语 + 任务评论 (commits: a334cbf, d84c705)

### 背景

V24 包含两个并行功能：
- **V24-A**：Web UI 国际化（中英双语），让非中文用户也能使用 ainative 工作台
- **V24-B**：任务评论系统，支持 CEO 和 agents 在任务详情页留下协作记录

---

### 功能 A（V24-A）：i18n 中英双语 (commit: a334cbf)

#### 技术选型

`vue-i18n@9`，`legacy: false`（Composition API 模式）。

**文件结构：**

```
web/src/i18n/
├── zh.ts      # 中文翻译（~40 keys）
├── en.ts      # 英文翻译（~40 keys）
└── index.ts   # createI18n + toggleLocale + localStorage 持久化
```

**6 个命名空间：**

| 命名空间 | 覆盖内容 |
|----------|---------|
| `nav` | 导航链接名称 |
| `dashboard` | 搜索框、批量操作、无数据提示 |
| `kanban` | 看板标题、loading、human badge |
| `stats` | Agent 统计页文字 |
| `status` | 任务状态名称 |
| `common` | 通用文字（加载中、确认等）|

#### 语言切换

**导航栏（`hidden md:flex`）：** `🌐 EN / 中` 按钮，桌面端显示  
**侧边栏底部：** `🌐 English / 中文` 切换按钮，始终可见

```ts
// i18n/index.ts
function toggleLocale() {
  locale.value = locale.value === 'zh' ? 'en' : 'zh'
  localStorage.setItem('locale', locale.value)
}
```

启动时从 `localStorage` 恢复上次选择的语言。

#### navItems 动态化

`AppLayout.vue` 中 `navItems` 改为 `computed`：

```ts
const navItems = computed(() => [
  { path: '/', label: t('nav.dashboard') },
  { path: '/kanban', label: t('nav.kanban') },
  // ...
])
```

语言切换后导航标签实时更新，无需刷新页面。

---

### 功能 B（V24-B）：任务评论系统 (commit: d84c705)

#### 数据库

```sql
CREATE TABLE IF NOT EXISTS task_comments (
  id         TEXT PRIMARY KEY,
  task_id    TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
  author     TEXT NOT NULL,
  content    TEXT NOT NULL,
  created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
)
```

`ON DELETE CASCADE`：任务删除时评论同步清理。

#### API (internal/handler/comments.go，126行)

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/api/tasks/:id/comments` | 列表（按 `created_at ASC`）|
| `POST` | `/api/tasks/:id/comments` | 新增评论（验证 task 存在）|

`POST` 成功后调用 `SSEHub.Broadcast("comment_created")`，已连接的前端实时收到更新。

请求体：

```json
{
  "author": "ceo",     // 可选，默认 "human"
  "content": "请重新检查登录边界条件"
}
```

#### 前端：TaskDetailPage.vue 评论区

```
┌──────────────────────────────────────┐
│  💬 评论                              │
├──────────────────────────────────────┤
│  [C] ceo · 14:30                     │  ← 头像首字母圆圈 + 作者/时间
│  请重新检查登录边界条件                │  ← 内容卡片
│                                      │
│  [h] human · 14:35                   │
│  已修复，请重新验证                    │
├──────────────────────────────────────┤
│  作者（可选）: [human        ]        │
│  [评论内容...                      ]  │
│  [Ctrl+Enter 发送]        [发送 →]   │
└──────────────────────────────────────┘
```

- `onMounted` 时调用 `loadComments()` 加载历史评论
- `Ctrl+Enter` 快捷键提交
- 提交中禁用按钮（`commentSubmitting`）；失败显示 `commentError`
- SSE `comment_created` 事件触发自动刷新评论列表

---

## V25 收版：FSM Cancel 补全 + v1.0.0 Release (commit: 61b7cde, b8330b3)

### V25-A：FSM Cancel 补全 (commit: 61b7cde)

#### 问题

V22 `POST /api/tasks/bulk` 的 `cancel` 操作直接执行 SQL（绕过 FSM），已支持 `claimed` 和 `in_progress` 状态的任务取消。

但 `PATCH /tasks/:id {status: "cancelled"}` 走 FSM 路径，`fsm.go` 和 `store.go` 的 `validateTransition` 中均未定义这两条规则，导致 PATCH 路径无法取消进行中的任务。

#### 修复

在两处同步新增转换规则：

```go
// internal/fsm/fsm.go
claimed     → cancelled  ✅（新增）
in_progress → cancelled  ✅（新增）

// internal/store/store.go validateTransition
// 同步加入两条规则
```

**影响：**
- `PATCH /tasks/:id {status: "cancelled"}` 现在对 `claimed` 和 `in_progress` 任务有效
- bulk cancel 与 PATCH cancel 行为完全对齐

### V25-B/C：CHANGELOG + v1.0.0 Tag (commit: b8330b3)

- `CHANGELOG.md` 生成（Keep a Changelog 格式，英文，84行）
  - `[Unreleased]` 留空
  - `[1.0.0] - 2026-02-27`：Added/Changed/Fixed，覆盖 V1–V25 全功能
- `git tag v1.0.0 && git push origin main --tags`
- Release URL：https://github.com/irchelper/ainative/releases/tag/v1.0.0

---

### ainative v1.0.0 功能全景

| 版本 | 核心功能 |
|------|---------|
| V1–V6 | MVP：任务 CRUD、FSM、SQLite、Discord 通知 |
| V7 | superseded_by、depends_on、blocked_downstream |
| V8 | chain dispatch、retry_routing、notify_ceo_on_complete |
| V9 | RetryQueue backoff、stale ticker |
| V10–V11 | review-reject chain、per-agent webhook、cancelled 终态 |
| V12–V13 | AI Workbench UI 骨架、autoAdvance |
| V14–V16 | result routing、task templates、agent timeout |
| V17–V18 | SSE 实时更新、human approval、DAG dispatch |
| V19 | summary 过滤、动态优先级 |
| V20 | Scalar API 文档、DAG 可视化 |
| V21 | DashboardPage 搜索、Agent 统计面板 |
| V22 | 批量操作、移动端响应式 |
| V23 | Webhook 外发（HMAC）、Timeline 增强 |
| V24 | i18n 中英双语、任务评论 |
| V25 | FSM cancel 补全、CHANGELOG、v1.0.0 Release |


---

## V29a UX 紧急修复 (commit: 0f88ad2)

### 背景

v1.0.0 发布后发现两个 UX 问题：待办面板始终空白、看板缺少 cancelled 列。V29a 紧急修复。

### 修复 A：待办面板过滤逻辑修正

**问题：** `humanTodos` 使用 `requires_review === true` 过滤，但大多数任务不设置此标志，导致待办面板永远为空。

**修复（DashboardPage.vue + AppLayout.vue）：**

```ts
// 修复前
const humanTodos = computed(() =>
  (store.data?.todo ?? []).filter((t) => t.requires_review === true)
)

// 修复后：改为按状态过滤活跃任务
const humanTodos = computed(() =>
  (store.data?.todo ?? []).filter(
    (t) => t.status === 'claimed' || t.status === 'in_progress'
  )
)
```

AppLayout.vue 的 `todoCount` badge 同步修正，与面板语义一致。

### 修复 B：看板新增 cancelled 列

```ts
// KanbanPage.vue columns 数组新增
{ key: 'cancelled', label: '已取消', color: 'text-gray-500' }
```

---

## V29b TaskDetailPage SPA 修复 + 异常面板增强 (commit: 9879654)

### 背景

直接访问 `/tasks/:id` URL 时，Go 的 `/tasks/` handler 优先于 Vue Router，导致 SPA 返回 JSON 而非 HTML。同时异常面板分类逻辑和工作台文案需要改进。

### 功能 A：TaskDetailPage SPA 路由修复

**问题根因：** Vue Router 使用 history mode，`/tasks/:id` 直接访问时 Go handler 拦截返回 JSON。

**修复（internal/handler/handler.go + internal/webui/embed.go）：**

```go
// handler.go: GET /tasks/:id 时检测 Accept header
case http.MethodGet:
    if strings.Contains(r.Header.Get("Accept"), "text/html") {
        webui.ServeSPA(w, r)  // 浏览器请求 → 返回 SPA HTML
        return
    }
    h.getTask(w, r, id)  // API 请求 → 返回 JSON
```

```go
// webui/embed.go: 新增公开的 ServeSPA 函数
func ServeSPA(w http.ResponseWriter, r *http.Request) {
    serveIndex(w, r, StaticDir())
}
```

### 功能 B：异常面板增强

**异常分类（DashboardPage.vue）：**

```ts
function isRetryable(task: Task): boolean {
  const r = task.failure_reason ?? task.result ?? ''
  return r.startsWith('agent_timeout') || r.startsWith('stale max')
}

const retryableExceptions = computed(() => exceptions.value.filter(isRetryable))
const needsHumanExceptions = computed(() => exceptions.value.filter(t => !isRetryable(t)))
```

- **可自动重试**（agent_timeout / stale max）：UI 提示"系统处理中"
- **需人工介入**：展示 agent pill 过滤器，按 agent 分组查看

**导航文案更新（i18n）：**
- 中文：`仪表盘` → `工作台`
- 英文：`Dashboard` → `Workbench`

---

## V30 DAG 重设计 + 跟踪页增强 + spec_file 支持 (commits: 2a48e96, 12489a0, 84d40c0, 3d56b5c)

### 背景

V30 分4个提交迭代完成前端 UX 全面升级，并新增后端 `spec_file` 字段解决长 description 传参问题。

---

### V30-v1：DAG 重设计 + 跟踪页 Tab 筛选 + 分页 (commit: 2a48e96)

#### DAG 可视化重设计（GraphVisualizationPage.vue）

引入 `dagre` + `d3-selection` + `d3-zoom` 替换原有手写 Kahn BFS 布局：

| 对比项 | 旧实现 | 新实现 |
|--------|--------|--------|
| 布局算法 | 手写 Kahn BFS | dagre（自动计算节点坐标）|
| 渲染方式 | HTML div | SVG + d3 |
| 缩放/平移 | 无 | d3-zoom（滚轮缩放 + 拖拽平移）|
| 节点间连线 | CSS border | SVG path（贝塞尔曲线）|

新增依赖（web/package.json）：dagre@^0.8.5 / d3-selection@^3.0.0 / d3-zoom@^3.0.0

新增文件：
- `web/src/components/Pagination.vue`（通用分页组件）
- `web/src/composables/usePagination.ts`

#### 跟踪页 Tab 筛选 + 分页（GoalTrackingPage.vue）

- Tab 筛选：全部 / 进行中 / 已完成 / 失败 四个 tab
- 分页：每页默认10条，支持跳页

---

### V30-v2：仪表盘限流 + Badge 修复 + 进度条四色 + 看板 done 列折叠分页 (commit: 12489a0)

#### DashboardPage 限流

轮询间隔限制 >= 3s（POLL_MIN_INTERVAL = 3000），防止高频刷新。

#### AgentStatsPage 进度条四色

| 完成率 | 颜色 |
|--------|------|
| >= 90% | 青色（cyan）|
| >= 80% | 绿色（green）|
| >= 50% | 黄色（yellow）|
| < 50% | 红色（red）|

原为三色（80%/50%/红），新增 90% 档 cyan。

#### KanbanPage done 列折叠分页

done 列默认折叠（仅显示最近5条），展开按钮 + 分页（usePagination）。

---

### V30-v3：Tooltip + 描述行距 + 空态文案 + DB 路径截断 (commit: 84d40c0)

UI 细节打磨：
- **Tooltip**：长文本截断时显示 title 属性 tooltip（GoalTrackingPage / KanbanPage）
- **描述行距**：TaskDetailPage leading-relaxed 增加行间距
- **空态文案**：GoalTrackingPage 无任务时显示引导文案
- **DB 路径截断**：SettingsPage 中 DB 路径过长时截断 + tooltip

---

### V30-v4：spec_file 字段支持 (commit: 3d56b5c)

#### 背景

`POST /dispatch` 通过 JSON body 传 description，长 spec 内容（如完整设计文档）容易超出 shell 转义限制，且不便版本管理。

#### 新增字段

```json
{
  "title": "实现登录功能",
  "assigned_to": "coder",
  "spec_file": "~/clawd/specs/login-spec.md"
}
```

后端读取文件内容，prepend 到 description 前（支持 ~ 展开）；文件读取失败返回 400。

DB schema：`ALTER TABLE tasks ADD COLUMN spec_file TEXT NOT NULL DEFAULT ''`


---

## V31 Failed/Blocked 降噪规则 v1 (commits: 489ed47, c851270, 727c36c)

### 背景

v1.0.0 发布后，多次出现 `vision` agent 因 Browser Relay 未 attach 导致无限退单循环（failed → autoRetry → 再次派给 vision → 再次失败），以及 retry 链路无上限增长的问题。V31 从三个维度系统性修复。

---

### 修复 A：Browser Relay 未 attach → 自动转 blocked (commit: 489ed47)

#### 问题

`vision` PATCH `{status: "failed", result: "Browser Relay cdpReady=false"}` 触发 `autoRetry`，任务被重新派发给 vision，形成无限循环。

#### 修复（internal/handler/handler.go）

在 `patchTask` handler 中，PATCH failed 写入 store 之前，检测 result/failure_reason 是否匹配 Browser Relay 未 attach 的特征文本：

```go
// V31-BrowserRelay: PATCH failed 时，若 result 匹配 Browser Relay 未 attach 文本
// 自动将 status 改为 blocked，避免进入 failed/autoRetry/stale 路径
if req.Status == StatusFailed && isBrowserRelayNotAttachedText(payload) {
    req.Status = &blocked  // StatusBlocked
    req.Result = "...original result...
matched_rule=browser_relay_not_attached; route_reason=needs_user_attach"
    req.Note = "browser relay not attached"
}
```

**效果：**
- vision agent 即使写了 PATCH failed，服务端自动转为 blocked
- blocked 状态不触发 autoRetry，任务暂停等待用户手动恢复
- CEO 看到 blocked 任务，attach Browser Relay 后手动 PATCH pending 重试

---

### 修复 B：autoRetry 深度上限 + coder 超时自重试 (commit: c851270)

#### 问题1：retry 链路无上限增长

任务标题中 `retry:`/`fix:`/`re-review:` 前缀可无限叠加（如 `fix: retry: fix: retry: fix: ...`），系统持续派发新任务，CEO 频道收到大量噪音通知。

#### 修复：retry 深度 >= 3 时触发 CEO 告警并停止派发

```go
// handleFailedTask 中
retryDepth := strings.Count(task.Title, "retry:") +
              strings.Count(task.Title, "fix:") +
              strings.Count(task.Title, "re-review:")
if retryDepth >= 3 {
    // 调用 OnFailed 通知 CEO，停止 autoRetry
    h.sessionN.OnFailed(task)
    return  // 不继续 autoRetry
}
```

#### 问题2：coder agent_timeout 触发 catch-all 路由，任务派给无关 agent

#### 修复：新增 retry_routing 规则（internal/db/db.go）

```go
// V31-P0-4: coder 超时 → coder 自重试（不走 catch-all）
{"coder", "agent_timeout", "coder", 10}
```

---

### 修复 C：changed_by 空字符串绕过 failed→done 权限检查 (commit: 727c36c)

#### 问题

V31-P1-C 新增的 `failed→done` 权限检查（只允许 original assignee 或 system）存在绕过漏洞：

```go
// 修复前：caller='' AND assignedTo='' → 条件为 false → 绕过权限检查
if current.AssignedTo != req.ChangedBy && req.ChangedBy != "system" {
    // 当两者都为空时，两个条件都为 false，整个 if 为 false → 不拒绝
}
```

#### 修复（internal/store/store.go）

```go
// 修复后：空 changed_by 显式拒绝
if req.ChangedBy == "" || (current.AssignedTo != req.ChangedBy && req.ChangedBy != "system") {
    return ValidationError("failed→done: only original assignee or system may recover")
}
```

- `changed_by == ""` 的匿名调用方直接返回 422
- 不再因 `assignedTo == ""` 而侥幸通过

---

### 降噪规则总结（v1）

| 规则 | 触发条件 | 处理方式 |
|------|---------|---------|
| Browser Relay 拦截 | PATCH failed result 含 cdpReady=false 等特征 | 服务端自动转 blocked |
| retry 深度上限 | title 中 retry:/fix:/re-review: 计数 >= 3 | 停止 autoRetry，通知 CEO 介入 |
| coder 超时自重试 | coder failed，failure_reason=agent_timeout | retry_routing → coder（不走 catch-all）|
| 匿名 failed→done 拦截 | changed_by == "" | 422 ValidationError，拒绝恢复 |

## Failed/Blocked 降噪规则 v1（运维速查）

> 本节为 V31 降噪规则的运维侧速查汇总，含 V31 发布后补充的 [TEST] 识别规则。

### 规则一：[TEST] 任务识别（测试任务隔离）

**触发条件：**
- `title`（case-insensitive）含 `[TEST]`、`[E2E]`、`e2e-`
- 或 `assigned_to` 前缀为 `e2e-` / `test`

**处理方式：**
- 标记为测试任务，不触发 `notify_ceo_on_complete` 通知
- 失败时不走 `retry_routing`，不产生 CEO 频道噪音
- 用于隔离 e2e/集成测试场景产生的临时任务

---

### 规则二：Browser Relay 未 attach → 自动转 blocked

**触发条件：**
- PATCH `{status: "failed"}` 且 `result` 含 Browser Relay 未 attach 特征文本（`cdpReady=false` 等）

**处理方式：**
- 服务端自动将 `status` 从 `failed` 转为 `blocked`
- `result` 追加 `matched_rule=browser_relay_not_attached; route_reason=needs_user_attach`
- `blocked` 不触发 `autoRetry`，任务暂停等待用户手动 attach → CEO PATCH `pending` 恢复

---

### 规则三：autoRetry 深度上限（最大3次）

**触发条件：**
- `title` 中 `retry:`、`fix:`、`re-review:` 前缀累计计数 ≥ 3

**处理方式：**
- 停止 `autoRetry`，不再派发新任务
- 调用 `OnFailed` 通知 CEO 介入
- 防止 retry 链路无限增长（如 `fix: retry: fix: retry: fix: ...`）

---

### 规则四：coder 超时自重试

**触发条件：**
- `assigned_to = "coder"` 且 `failure_reason = "agent_timeout"`

**处理方式：**
- `retry_routing` 规则：`("coder", "agent_timeout", "coder", delay=10s)`
- coder 超时优先重派给 coder 自己，不走 catch-all 路由
- 避免超时任务被错误派给无关 agent

---

### 规则汇总

| # | 规则名 | 触发条件 | 处理方式 |
|---|--------|---------|---------|
| 1 | [TEST] 识别 | title 含 [TEST]/[E2E]/e2e- 或 assigned_to 前缀 e2e-/test | 隔离测试任务，跳过 retry_routing / notify |
| 2 | Browser Relay 拦截 | PATCH failed + result 含 cdpReady=false 特征 | 服务端自动转 blocked，等用户恢复 |
| 3 | retry 深度上限 | title 中 retry:/fix:/re-review: 计数 ≥ 3 | 停止 autoRetry，通知 CEO 介入 |
| 4 | coder 超时自重试 | coder failed，failure_reason=agent_timeout | retry_routing → coder（delay 10s） |
