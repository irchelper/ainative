# agent-queue PRD

> Status: Draft → v5
> Owner: 产品经理
> Date: 2026-02-25
> Updated: 2026-02-26 (v10 — V8: 新增 F15 链路完成通知 CEO（notify_ceo_on_complete + chain_id）、F16 Go server retry 路由表（替代 SOUL.md 硬编码）; triggered 缺口修复说明)

---

## 1. 项目概述

**一句话定位：** agent-queue 是一个轻量级 multi-agent 任务队列服务——用 SQLite 持久化 + Go REST API，让多个 AI agent 通过 HTTP 协调执行串行/并行任务，不依赖任何单一 agent 在线调度。

### Why agent-queue?

我们在运行 OpenClaw + 8 个专家 bot 的 multi-agent 系统时，发现了一个根本性问题：**整个系统的推进完全依赖一个中心调度者（CEO agent）在线且有上下文**。CEO 崩了、上下文压缩了、用户离线了——任务链就断了。

这不是个例。任何基于"调度者模式"的 multi-agent 系统都会遇到同样的问题：

- 专家完成任务后，消息堆在队列里等 CEO 被唤醒
- 串行链路 A→B→C 中间有不确定的空窗期
- 用户必须在线才能触发推进，无法"发布任务后离开"
- 状态只存在于 agent 的上下文里，崩了就丢了

agent-queue 的答案是：**把任务状态从 agent 脑子里搬到 SQLite 里**。任何 agent 都能通过 HTTP 查状态、认领任务、报告完成。不需要某个 agent 在线调度，不需要用户盯着。

**解决什么问题：**

当前 multi-agent 系统普遍依赖"调度者模式"——一个中心 agent（如 CEO）负责派发任务、追踪状态、推进下一步。这带来三个致命问题：

1. **单点故障**：调度者崩溃/上下文丢失 = 整个任务链断裂，无法恢复
2. **被动响应**：调度者依赖外部唤醒才能感知任务完成，串行任务无法自动推进
3. **并发冲突**：多 agent 可能重复认领同一任务，缺乏原子性保障

agent-queue 通过将任务状态外部化到 SQLite，实现去中心化的任务协调：任何 agent 崩了都能从数据库恢复状态，乐观锁保证认领原子性，依赖关系驱动串行任务自动推进。

**设计原则：**

- **平台无关**：不绑定 OpenClaw/Claude/GPT 任何 AI 平台
- **语言无关**：任何语言的 agent 都能通过 HTTP 调用
- **零依赖部署**：单二进制，SQLite 内嵌，无需 Docker/数据库/消息队列
- **开源**：MIT License

---

## 2. 问题背景：7 个实际痛点

以下痛点来自实际运行 multi-agent 系统（OpenClaw + 8 个专家 bot）的生产环境，不是假设的需求。

### P1: CEO 被动响应

**现象：** 专家完成任务后 `sessions_send` CEO，但 CEO 不被自动唤醒，消息堆在队列里等用户发消息触发。
**根因：** 调度者是被动响应型（只有外部消息才唤醒），缺乏主动轮询机制。
**agent-queue 如何解决：** Go server 在任务状态变为 `done` 时，通过 Discord Incoming Webhook 主动推送通知，CEO 被动接收即可感知，无需轮询。

### P2: 串行任务断链

**现象：** 任务 A 完成后需要等 CEO 被唤醒才能派任务 B，中间有不确定的空窗期（几秒到几小时不等）。
**根因：** 任务间的依赖关系只存在于 CEO 上下文中，没有外部化的依赖图。
**agent-queue 如何解决：** `task_deps` 表声明依赖关系，A 完成时 Go server 自动检查依赖、解锁后续任务（`triggered` 字段返回被解锁的任务 ID），agent cron 轮询可认领任务即推进。

### P3: 通知不实时

**现象：** 专家完成了任务，但用户不知道，要自己来问"做完了吗？"。
**根因：** 通知链路依赖 CEO 在线转发，CEO 不在线 = 用户无感知。
**agent-queue 如何解决：** Go server 在任务完成时通过 Discord Incoming Webhook 直接推送通知给用户（@mention），秒级触达，不依赖任何 agent 在线转发。

### P4: 状态不持久

**现象：** CEO 上下文压缩或开新 session 后，之前的任务状态丢失。PENDING.md 是唯一的外部状态，但多 agent 并发写文件有竞态风险。
**根因：** 任务状态存在 agent 内存（LLM 上下文）中，不是真正的持久化。
**agent-queue 如何解决：** 所有状态写入 SQLite（ACID 事务），乐观锁防并发冲突。agent 崩了重启，`GET /tasks?status=in_progress` 立即恢复。

### P5: 任务依赖不透明

**现象：** 串行链路 A→B→C 的依赖关系只在 CEO 的上下文里，没有外部可查的依赖图。如果有人问"B 在等什么？"——只有 CEO 知道，且 CEO 可能已经忘了。
**根因：** 依赖关系是隐式的（存在 agent 的思维链中），不是显式数据。
**agent-queue 如何解决：** `task_deps` 表显式声明依赖，`GET /tasks/:id` 返回完整的依赖链，任何 agent 都能查。

### P6: 无验收节点

**现象：** 任务完成 = 专家说"我做完了"。没有自动触发 QA/review 的机制，质量全靠自觉。
**根因：** 没有状态机中的 `review` 态，也没有自动化的"做完→待审"流转。
**agent-queue 如何解决：** 7 态状态机包含 `review` 状态，`in_progress → review` 可触发 reviewer agent 认领审查任务，review 通过才能流转到 `done`。

### P7: 无法离线运行

**现象：** 用户必须在线（保持发消息）才能触发 CEO 推进任务。不能"发布一个任务链后去睡觉，早上看结果"。
**根因：** 整个系统的推进依赖用户→CEO 的消息触发链。
**agent-queue 如何解决：** 用户创建任务链后即可离开。agent cron 自动轮询 + 认领 + 执行 + `PATCH /tasks/:id` 写回状态。Go server 在任务完成时通过 webhook 直接通知用户，不依赖任何 agent 转发。

### 痛点验收矩阵

上线后，每个痛点消失的可验证条件：

| 痛点 | 验收条件 |
|------|---------|
| P1: CEO 被动响应 | 任务完成后 Go server 秒级 webhook 推送，CEO 无需主动轮询即感知（CEO 角色降级为监控者） |
| P2: 串行任务断链 | 3 步串行链 A→B→C 全程无人工推进，依赖自动解锁，步间间隔 = agent cron 轮询周期 + 执行时间 |
| P3: 通知不实时 | 任务完成后 ≤ 5 秒 webhook 推送到 Discord，用户直接收到 @mention |
| P4: 状态不持久 | CEO session 被 kill 后重启，通过 `GET /tasks` 恢复全部任务状态，不丢数据 |
| P5: 依赖不透明 | 任意 agent 调用 `GET /tasks/:id` 可查到完整依赖链和当前阻塞原因 |
| P6: 无验收节点 | 设定 review 流程的任务，`in_progress → done` 不合法（必须经过 review），422 拒绝 |
| P7: 无法离线运行 | 用户创建任务链后断开连接，任务链自动跑完并最终通知用户 |

---

## 3. 目标用户

### 主要用户

| 用户类型 | 描述 | 核心诉求 |
|---------|------|---------|
| **Multi-agent 系统构建者** | 搭建多 agent 协作工作流的开发者（如 OpenClaw、AutoGen、CrewAI 用户） | 可靠的任务编排，不依赖某一 agent 在线 |
| **AI agent 开发者** | 开发单个 agent 但需要与其他 agent 协作的工程师 | 简单的 HTTP API 接入，不需要学新 SDK |
| **个人 AI 工作站搭建者** | 在本机运行多个 AI agent 处理日常工作的技术用户 | 轻量部署，单二进制即跑，数据在本地 |

### 非目标用户（v1 不考虑）

- 需要企业级权限控制的团队
- 需要跨机器分布式部署的场景
- 需要图形化操作界面的非技术用户

---

## 4. 核心使用场景（用户故事）

### US-1: 串行任务自动推进

> 作为 **multi-agent 系统构建者**，
> 当我定义一个串行任务链（如：架构师出方案 → 编码员实现 → 测试员验证），
> 我想要前一个任务完成后自动解锁下一个任务，
> 以便整个链路无需人工干预即可跑完。

**验收标准：**
- [ ] 创建任务时可通过 `depends_on` 字段指定前置依赖
- [ ] 前置任务状态变为 `done` 后，后续任务变为可认领状态（`deps_met=true`）
- [ ] Agent 查询时可用 `deps_met=true` 过滤，只获取依赖已满足的任务
- [ ] PATCH 任务为 `done` 时，response 的 `triggered` 字段返回被解锁的任务 ID 列表
- [ ] 3 步串行链（A→B→C）全程无人工推进，自动跑完

### US-2: 并行任务协调

> 作为 **multi-agent 系统构建者**，
> 当我需要多个 agent 同时执行独立子任务（如：3 个编码员各写一个模块），
> 我想要所有并行任务完成后自动触发汇总任务，
> 以便减少等待时间并自动汇合结果。

**验收标准：**
- [ ] 一个父任务下可创建多个无互相依赖的子任务（`mode=parallel`）
- [ ] 多个 agent 可同时认领不同子任务，互不阻塞
- [ ] 汇总任务 `depends_on` 所有并行子任务，只有全部 `done` 才解锁
- [ ] 通过 `GET /tasks?parent_id=xxx` 查询所有子任务状态

### US-3: 断点恢复

> 作为 **AI agent 开发者**，
> 当我的 agent 在执行任务中途崩溃或上下文丢失，
> 我想要从队列中恢复任务状态和上下文，
> 以便从断点继续执行而非重头开始。

**验收标准：**
- [ ] 任务的完整 spec（`description`）和当前状态持久化在 SQLite 中
- [ ] Agent 重启后，通过 `GET /tasks?assigned_to=me&status=claimed,in_progress` 找回自己未完成的任务
- [ ] 任务描述包含足够信息让 agent 无需额外上下文即可继续执行
- [ ] `task_history` 表记录所有状态变更，可追溯任务执行过程

### US-4: 乐观锁认领（防重复认领）

> 作为 **multi-agent 系统构建者**，
> 当多个 agent 实例同时尝试认领同一个任务，
> 我想要只有一个能成功认领，其余收到明确的冲突反馈，
> 以便避免重复执行浪费资源。

**验收标准：**
- [ ] 认领接口 `POST /tasks/:id/claim` 使用 `version` 字段实现乐观锁
- [ ] 认领时必须传入当前 `version`，服务端原子校验 `WHERE version = ? AND status = 'pending'`
- [ ] 第一个认领成功返回 200 + 更新后的任务
- [ ] 并发的第二个认领返回 409 Conflict
- [ ] 10 个并发认领请求，有且只有 1 个成功

### US-5: 任务状态查询与监控

> 作为 **个人 AI 工作站搭建者**，
> 当我想了解当前所有任务的执行状态，
> 我想要通过 API 查询任务列表并按状态/assignee/父任务过滤，
> 以便快速掌握全局进度。

**验收标准：**
- [ ] `GET /tasks` 支持按 `status`、`assigned_to`、`parent_id`、`deps_met` 过滤
- [ ] `GET /tasks/:id` 返回任务详情 + 依赖关系 + 状态变更历史
- [ ] 所有 API response 包含 `count` 字段标识总数

### US-6: 离线发布任务（Fire and Forget）

> 作为 **个人 AI 工作站搭建者**，
> 当我发布一个多步骤任务链后，
> 我想要关掉电脑/离开聊天窗口，系统自动完成所有步骤，并在最后通知我结果，
> 以便我不需要盯着屏幕等每一步完成。

**验收标准：**
- [ ] 用户通过 API 创建包含依赖关系的任务链后，无需任何后续交互
- [ ] Agent cron 自动轮询 → 认领可执行任务 → 执行 → `PATCH /tasks/:id` 写回结果 → 自动解锁下一步
- [ ] 整条任务链的推进不依赖用户在线、不依赖某个特定 agent session 存活
- [ ] 每个任务完成时，Go server 通过 webhook 通知用户
- [ ] 通知通过 Discord Incoming Webhook 送达（@mention），支持通过 `Notifier` 接口扩展其他通道

**关联痛点：** P1（被动响应）、P2（断链）、P3（通知不实时）、P7（无法离线）

---

## 5. 核心功能列表（MVP 范围）

### F1: 任务 CRUD

| 功能 | 描述 | 优先级 |
|------|------|--------|
| 创建任务 | `POST /tasks`，支持 title/description/assigned_to/priority/depends_on/parent_id/mode/requires_review | P0 |
| 查询任务列表 | `GET /tasks`，支持 status/assigned_to/parent_id/deps_met 过滤 | P0 |
| 查询任务详情 | `GET /tasks/:id`，返回任务 + 依赖关系 + 变更历史 | P0 |
| 更新任务 | `PATCH /tasks/:id`，支持状态流转 + result 写回 + 乐观锁校验 | P0 |

### F2: 乐观锁认领

| 功能 | 描述 | 优先级 |
|------|------|--------|
| 认领任务 | `POST /tasks/:id/claim`，原子操作：校验 version + status=pending + assigned_to 匹配 | P0 |
| 冲突检测 | version 不匹配或已被认领 → 返回 409 Conflict | P0 |

### F3: 依赖关系与自动推进

| 功能 | 描述 | 优先级 |
|------|------|--------|
| 依赖关系管理 | 创建任务时通过 `depends_on` 数组声明前置依赖，存入 `task_deps` 表 | P0 |
| 依赖检查 | `GET /tasks/:id/deps-met` 返回依赖是否全部满足 | P0 |
| 自动解锁通知 | 任务完成时，response 返回 `triggered` 列表（被解锁的后续任务） | P1 |

### F4: 状态机

| 功能 | 描述 | 优先级 |
|------|------|--------|
| 8 态状态机 | pending → claimed → in_progress → review → done / blocked / failed / cancelled | P0 |
| 状态转换校验 | 非法转换返回 422 Unprocessable Entity | P0 |
| 状态变更历史 | 每次状态变更写入 `task_history`，记录 from/to/actor/note/timestamp | P0 |

**合法状态转换矩阵：**

```
pending     → claimed, cancelled
claimed     → in_progress, pending (释放)
in_progress → done, review, blocked, failed, pending (超时释放)
review      → done, in_progress (打回)
blocked     → pending
failed      → pending (CEO 决策后重试)
done        → (终态)
cancelled   → (终态)
```

**`in_progress → failed`（专家主动上报）：** 专家执行中遇到无法自行解决的问题时主动调用。`PATCH /tasks/:id` 需同时传入 `failure_reason` 字段说明原因。触发后 Go server 自动通过 **SessionNotifier** 唤醒 CEO session，CEO 决策后可将任务重置为 pending（支持通过 `retry_assigned_to` 字段指定重试 agent）。

**`in_progress → pending` 说明：** 用于 agent 崩溃/超时场景——任务卡在 `in_progress` 但执行者已失联。此转换由超时检测 cron 或人工触发（`PATCH /tasks/:id` + note 说明原因），**不由 agent 主动调用**。转换时清空 `assigned_to`，使任务重新进入可认领池。

**新增字段：**
- `failure_reason VARCHAR`：`in_progress → failed` 时专家写入的失败原因，供 CEO 决策参考
- `retry_assigned_to VARCHAR`：CEO 在 `failed → pending` 时可指定的重试 agent，优先级高于原 `assigned_to`

**`requires_review` 条件路由：**

tasks 表新增字段：`requires_review BOOLEAN DEFAULT false`

任务创建时（`POST /tasks`）可传入 `requires_review: true`，启用强制 review 流程。状态转换条件：

| 转换 | `requires_review=false` | `requires_review=true` |
|------|------------------------|----------------------|
| `in_progress → done` | ✅ 合法 | ❌ 422 — 必须先经 review |
| `in_progress → review` | ❌ 422 — 无需 review 的任务不能进 review 态 | ✅ 合法 |

服务端在执行 `in_progress → done` 或 `in_progress → review` 转换前，必须检查 `requires_review` 字段并按上表校验。不匹配时返回 422 + 明确错误信息。

### F5: 健康检查

| 功能 | 描述 | 优先级 |
|------|------|--------|
| 健康端点 | `GET /health`，返回服务状态 + 数据库连接状态 | P0 |

### F6: Webhook 通知 + SessionNotifier

| 功能 | 描述 | 优先级 |
|------|------|--------|
| done 完成通知 | 任务状态变为 `done` 时，异步 POST Discord Incoming Webhook（通知用户） | P0 |
| failed 异常通知 | 任务状态变为 `failed` 时，调用 SessionNotifier 唤醒 CEO session（通知 CEO） | P0 |
| Webhook 配置 | Webhook URL 通过环境变量 `AGENT_QUEUE_DISCORD_WEBHOOK_URL` 配置 | P0 |
| SessionNotifier 配置 | 复用 F7 的 `AGENT_QUEUE_OPENCLAW_API_URL` + `AGENT_QUEUE_OPENCLAW_API_KEY` | P0 |
| 失败容错 | 重试 1 次，失败记 error log，不阻塞主流程 | P0 |

**done 通知格式（DiscordNotifier → 用户）：**
```
@用户ID ✅ 任务完成：[task title] (task_id: xxx)
```

**failed 通知格式（SessionNotifier → CEO session）：**
```
⚠️ 任务失败：[task title]
task_id: {id} | assigned_to: {agent}
failure_reason: {reason}
请检查并决定：重试 / 改派 / 取消
```

**接口抽象（双 Notifier 模式）：**
```go
type Notifier interface {
    Notify(task Task) error
}
// DiscordNotifier: done 时推送给用户（Discord Incoming Webhook）
// SessionNotifier: failed 时唤醒 CEO（OpenClaw /tools/invoke sessions_send）
```
- 环境变量未配置时，对应 Notifier 为 no-op（不报错，不发送，仅 log.Info）
- 支持未来扩展（Telegram、Slack 等），实现 `Notifier` 接口即可

**设计原则：** 正常路径（done）CEO 零干扰；异常路径（failed）精确唤醒 CEO，避免 CEO 持续轮询。

### F7: POST /dispatch（原子化派发接口）

| 功能 | 描述 | 优先级 |
|------|------|--------|
| 原子派发 | 一步完成"建任务 + 触发专家 session"，替代 POST /tasks + sessions_send 两步 | P0 |
| 优雅降级 | sessions_send 失败时任务仍创建，响应含 `notified=false` + `notify_error` | P0 |
| OpenClaw 集成 | 通过环境变量 `AGENT_QUEUE_OPENCLAW_API_URL` / `AGENT_QUEUE_OPENCLAW_API_KEY` 配置 | P0 |

**行为：** 创建 SQLite 任务记录（status=pending）→ 调用 OpenClaw `/tools/invoke`（sessions_send）→ 返回 task + notified 状态。

**专家 session key 映射：** 已硬编码在 Go server `internal/openclaw` 包中。

**Gateway 配置前提：** `openclaw.json` gateway 节点需开放 `tools.allow: ["sessions_send"]`。

### F8: GET /tasks/summary（全局状态面板）

| 功能 | 描述 | 优先级 |
|------|------|--------|
| 全局状态 | 返回 pending/claimed/in_progress/done_today/failed 计数 | P0 |
| 任务列表 | 返回所有非 done 任务（按 updated_at 倒序） | P0 |
| CEO 启动集成 | CEO session 启动时一次调用替代逐个查询，掌握全局进度 | P1 |

### F9: GET /tasks/poll（专家自驱认领）

| 功能 | 描述 | 优先级 |
|------|------|--------|
| 自驱 poll | `GET /tasks/poll?assigned_to=X`，返回该 agent 最优可认领任务 | P0 |
| 依赖过滤 | 服务端自动过滤依赖未满足的任务（deps_met=true） | P0 |
| 优先级排序 | `priority DESC, created_at ASC`，专家无需客户端排序 | P0 |
| 空队列响应 | 无待处理任务时返回 `{"task": null}` | P0 |

### F10: POST /dispatch/chain（串行链原子派发）

| 功能 | 描述 | 优先级 |
|------|------|--------|
| 串行链创建 | 一次提交 tasks 数组，自动设置 `depends_on`（A→B→C） | P0 |
| 原子性 | 所有子任务在同一事务中创建，失败时全部回滚 | P0 |
| 返回 | 所有子任务对象（含各自 task_id 和 depends_on） | P0 |

### F11: 异常感知（failed 状态 + SessionNotifier）

| 功能 | 描述 | 优先级 |
|------|------|--------|
| failed 状态 | 新增 8 态：`in_progress → failed`（专家主动上报） | P0 |
| failure_reason 字段 | PATCH failed 时写入失败原因，供 CEO 决策 | P0 |
| retry_assigned_to 字段 | CEO 可在 failed→pending 时指定重试 agent | P1 |
| SessionNotifier | 任务 failed 时调用 `/tools/invoke sessions_send` 唤醒 CEO session | P0 |
| CEO 重试 | CEO 收到通知后 PATCH status=pending（可选 retry_assigned_to）恢复任务 | P0 |

**SessionNotifier 消息格式规范（极简模式）：**

```
新任务通知：[agent-queue] 你有新的待处理任务。请执行 poll 流程认领。
失败通知：[agent-queue] ❌ 任务失败需介入：{title}
result: {result}
task_id: {id}
```

**设计原则：** 不含任务 title/description，避免 LLM 将通知误解为直接任务指令。专家被唤醒后，SOUL.md 的 poll 流程自动 `GET /tasks/poll` → claim → 读 description → 执行。SessionNotifier 仅发给需要知道的 1 个 session，不广播。

**验收标准：**
- [ ] 专家 `PATCH failed + result` 后，Go server 自动触发 SessionNotifier 唤醒 CEO
- [ ] CEO session 收到极简通知（含 task_id + result），格式符合上述规范
- [ ] CEO `PATCH failed→pending`（含 retry_assigned_to）后任务重回可认领池
- [ ] 正常 `done` 流程不触发 SessionNotifier（CEO 零干扰验证）
- [ ] `/dispatch` 发出的新任务通知消息不含 title，只有极简唤醒文本
- [ ] SessionNotifier 仅发 1 个目标 session（不广播）

### F12: retry_assigned_to 自动退单机制

| 功能 | 描述 | 优先级 |
|------|------|--------|
| result 内嵌退单声明 | 专家在 PATCH failed 的 result 中写 `retry_assigned_to: <专家名>` | P0 |
| 自动建新任务 | Go server 解析 result → 自动建 `retry: {原任务title}` 任务，派给目标专家 | P0 |
| 不通知 CEO | 退单自动完成，不触发 SessionNotifier（CEO 零干扰） | P0 |
| CEO 兜底 | result 中无 `retry_assigned_to` → 触发 SessionNotifier，CEO 人工决策 | P0 |
| 网状退单支持 | 任意专家间可互相退单，无需 CEO 介入 | P1 |

**退单映射规范（各专家退单合同）：**

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

**result 格式合同：**
```
"<失败原因描述> | retry_assigned_to: <专家名>"

示例：
"登录按钮无响应 | retry_assigned_to: coder"
"第3章登录态存储方案未定义 | retry_assigned_to: pm"
"容器无法启动，端口设计有误 | retry_assigned_to: thinker"
```

**验收标准：**
- [ ] PATCH failed + result 含 `retry_assigned_to: coder` → Go server 自动建 `retry: {title}` 任务，assigned_to=coder，不唤醒 CEO
- [ ] PATCH failed + result 无 `retry_assigned_to` → 触发 SessionNotifier 唤醒 CEO
- [ ] 自动建的 retry 任务可被目标专家通过 `GET /tasks/poll` 正常认领
- [ ] 退单不影响原任务的 failed 终态（原任务保持 failed，不自动重置）

### F13: 数据库持久化防误删

| 功能 | 描述 | 优先级 |
|------|------|--------|
| `AGENT_QUEUE_DB_PATH` 环境变量 | 覆盖默认 db 路径，支持绝对路径配置，防止 WorkingDirectory 变动导致找不到 db | P0 |
| Makefile clean 规范 | `make clean` 只删二进制；`make clean-all` 才删 db（需显式操作） | P0 |
| launchd 环境变量 | plist 中配置 `AGENT_QUEUE_DB_PATH` 为绝对路径，确保服务重启路径不变 | P0 |

**防误删规范：**
- `make clean`：`rm -f agent-queue`（仅删二进制，安全）
- `make clean-all`：`rm -f agent-queue data/queue.db`（清空所有数据，需显式调用）
- 开发迭代禁用 `make clean-all`，避免丢失任务历史

**验收标准：**
- [ ] `AGENT_QUEUE_DB_PATH` 环境变量生效（优先级高于 `--db` 默认值）
- [ ] `make clean` 后 `data/queue.db` 仍存在
- [ ] `make clean-all` 后 db 才被删除
- [ ] launchd plist 含 `AGENT_QUEUE_DB_PATH` 绝对路径配置

### F14: 失败链路自动恢复（superseded_by）

| 功能 | 描述 | 优先级 |
|------|------|--------|
| `superseded_by` 字段 | tasks 表新增 `superseded_by TEXT NOT NULL DEFAULT ''`，autoRetry 时记录 X.superseded_by = X'.id | P0 |
| retry 继承原依赖 | autoRetry 创建的 X' 继承 X 的 `depends_on` 列表，链路位置保持一致 | P0 |
| `depsMetForID` SQL 扩展 | dep 自己 done **OR** dep.superseded_by 对应任务 done → 依赖视为满足 | P0 |
| `blocked_downstream` 响应字段 | PATCH → failed 时，response 新增 `blocked_downstream` 列表（受影响的下游 pending 任务，只读扫描） | P1 |
| `failed→pending` 清空约束 | CEO PATCH failed→pending 时必须清空 `superseded_by`，防双重解锁冲突 | P0 |

**用户故事：**

> 作为 CEO，当专家 X 失败且 autoRetry 创建的 X' 完成后，X 的下游任务 B/C 应自动解锁继续执行，无需我手动重建任务链。

**端到端流程（A→B→C，A failed）：**

```
A PATCH failed, result="原因 | retry_assigned_to: qa"
  ↓
Go server autoRetry：
  1. 创建 A'，depends_on = A.depends_on，assigned_to = "qa"
  2. A.superseded_by = A'.id
  3. sessions_send 唤醒 qa
  ↓
A' → claim → in_progress → PATCH done
  ↓
Go server unlockDependents(A'.id)：
  - 直接下游：A' 的 depends_on 中的任务（若有）
  - depsMetForID(B)：B.depends_on 含 A，A.superseded_by = A'.id 且 A' done → 满足 → B 解锁
  - sessions_send 唤醒 B
  ↓
B → poll → claim → 执行 → done → C 解锁 ...
```

**验收标准：**
- [ ] 链 A→B→C，A PATCH failed + result 含 `retry_assigned_to: qa` → Go server 自动创建 A'，A.superseded_by = A'.id
- [ ] A' PATCH done → B 的 `depsMetForID` 返回 true（B 可被 poll 到）
- [ ] B 被 sessions_send 唤醒（链路从断点恢复，无 CEO 介入）
- [ ] PATCH failed 响应含 `blocked_downstream` 列表（含 B、C）
- [ ] CEO PATCH A: failed→pending（人工重试）后，A.superseded_by 被清空

### F15: 链路完成通知 CEO（V8 新增）

| 功能 | 描述 | 优先级 |
|------|------|--------|
| `notify_ceo_on_complete` 字段 | tasks 表新增 `notify_ceo_on_complete INTEGER NOT NULL DEFAULT 0`，dispatch/chain 时按请求体写入 | P0 |
| `chain_id` 字段 | tasks 表新增 `chain_id TEXT NOT NULL DEFAULT ''`，dispatch/chain 时 server 自动生成并写入全链任务 | P0 |
| 链路完成检测 | PATCH done 时若 `chain_id != "" && notify_ceo_on_complete=1`，调 `IsChainComplete(chain_id)` | P0 |
| `IsChainComplete` | SQL 检查链内所有任务是否全部 done/cancelled/superseded；含 superseded_by 兼容逻辑 | P0 |
| `OnChainComplete` | CEO session 收到完整链路汇总消息（chain_title + 所有子任务 result）| P0 |
| `GET /chains/:chain_id` | 查询链路下所有任务状态（可选，P1）| P1 |
| 向后兼容 | 未传 notify_ceo_on_complete 的请求行为不变（不通知 CEO） | P0 |

**dispatch/chain 请求体新增字段：**
```json
{
  "tasks": [...],
  "notify_ceo_on_complete": true,
  "chain_title": "需求分析→技术设计→编码实现"
}
```

**SessionNotifier.OnChainComplete 消息格式：**
```
[agent-queue] ✅ 任务链完成：{chain_title 或 "链路 chain_id"}
完成任务数：{done_count}/{total_count}
链路任务：
  ✅ {task1.title} ({task1.assigned_to}) — {task1.result}
  ✅ {task2.title} ({task2.assigned_to}) — {task2.result}
  ✅ {task3.title} ({task3.assigned_to}) — {task3.result}
chain_id: {chain_id}
```

**验收标准：**
- [ ] dispatch/chain 传 `notify_ceo_on_complete=true` + `chain_title` → 全链任务的 `notify_ceo_on_complete=1`、`chain_id=<生成值>`
- [ ] 链路最后一个任务 PATCH done → IsChainComplete 返回 true → CEO session 收到 OnChainComplete 消息
- [ ] 消息含 chain_title、所有子任务 title + assigned_to + result、chain_id
- [ ] notify_ceo_on_complete=false（或未传）时，PATCH done 不触发 CEO 通知
- [ ] autoRetry 替换的 X' 完成后，IsChainComplete 仍能正确检测（含 superseded_by 兼容）
- [ ] `GET /chains/:chain_id` 返回链路下所有任务（含 done 比例、is_complete 标志）

### F16: Go server retry 路由表（V8 新增）

| 功能 | 描述 | 优先级 |
|------|------|--------|
| `retry_routing` SQLite 表 | 存储 from_agent + error_keyword + to_agent + priority 映射 | P0 |
| 服务端自动查表 | PATCH failed 时按优先级链：body > failparser > retry_routing > CEO | P0 |
| 初始 seed 数据 | server 启动时写入 9 专家默认映射（12 条规则，含兜底规则） | P0 |
| `GET /retry-routing` | 查询全部路由规则 | P0 |
| `POST /retry-routing` | 添加规则（online CRUD，无需重启） | P1 |
| `DELETE /retry-routing/:id` | 删除规则 | P1 |
| SOUL.md 减负 | 各专家 SOUL.md 可删除 F12 退单映射表（15-20行）| P1 |

**查表 SQL：**
```sql
SELECT retry_assigned_to FROM retry_routing
WHERE assigned_to = ?
  AND (error_keyword = '' OR result LIKE '%' || error_keyword || '%')
ORDER BY priority DESC
LIMIT 1
```

**优先级链：**
```
1. PATCH body 显式传 retry_assigned_to → 直接用
2. failparser 解析 result "| retry_assigned_to: X" → 用
3. retry_routing 表查表（error_keyword 命中优先，兜底 ''）→ 用
4. 全未命中 → SessionNotifier CEO（人工决策）
```

**初始 seed（12 条规则）：**

| from | error_keyword | to | priority |
|------|---------------|----|---------|
| qa | bug | coder | 10 |
| qa | ui | uiux | 10 |
| qa | （兜底） | coder | 0 |
| coder | 架构 | thinker | 10 |
| coder | 需求 | pm | 10 |
| coder | （兜底） | thinker | 0 |
| writer | （兜底） | pm | 0 |
| devops | bug | coder | 10 |
| devops | 架构 | thinker | 10 |
| devops | （兜底） | coder | 0 |
| thinker | （兜底） | writer | 0 |
| uiux | （兜底） | pm | 0 |

**验收标准：**
- [ ] server 启动后 `GET /retry-routing` 返回 12 条初始规则
- [ ] qa PATCH failed + result 含 "bug" → autoRetry 自动退单 coder（不 CEO）
- [ ] devops PATCH failed + result 含 "架构" → autoRetry 自动退单 thinker（不 CEO）
- [ ] coder PATCH failed + result 不含关键词 → 查兜底规则 → autoRetry 退单 thinker
- [ ] result 不含 retry_assigned_to 且 assigned_to 无匹配规则 → SessionNotifier CEO
- [ ] body 显式 retry_assigned_to 优先级高于查表结果

### F16 补充说明：triggered 缺口修复（V8 bug fix）

| 缺口 | 影响 | 修复方案 |
|------|------|---------|
| patchTask done 时 unlockDependents 返回 triggered，但未调 SessionNotifier 唤醒下游 | 串行链依赖解锁后下游专家不会被自动唤醒；必须靠专家 startup poll 发现任务 | patchTask done 分支遍历 triggered 列表，逐一 SessionNotifier.Dispatch(assignedTo, taskID) |

**验收标准：**
- [ ] 链 A→B→C，A PATCH done → B 的 assigned_to 专家 session 收到 SessionNotifier 唤醒消息
- [ ] B 专家不需要等 startup 自驱 poll，会立即收到通知触发 poll

---

## 6. 非功能需求

### 性能

| 指标 | 要求 | 依据 |
|------|------|------|
| 单请求延迟 | < 10ms（本地 SQLite） | SQLite 单节点读写，无网络开销 |
| 并发认领 | 10 个并发请求正确处理（1 成功 + 9 冲突） | 乐观锁 + SQLite WAL 模式 |
| 数据规模 | 支持 10,000+ 任务记录 | 个人工作站场景，SQLite 轻松支撑 |

### 可靠性

| 指标 | 要求 | 依据 |
|------|------|------|
| 数据持久化 | 所有状态变更写入 SQLite，进程重启不丢数据 | SQLite WAL + `_busy_timeout=5000` |
| 进程自恢复 | 服务崩溃后自动重启（launchd / systemd） | KeepAlive=true |
| 备份 | SQLite 单文件，支持 `.backup` 命令 | 可配合 cron 每日备份 |

### 安全

| 指标 | 要求 | 依据 |
|------|------|------|
| 绑定地址 | 默认仅监听 `localhost`（127.0.0.1） | 本机 agent 调用，不对外暴露 |
| 输入校验 | 所有字段校验类型/长度/合法值 | 防止 SQL 注入、异常数据 |
| 无认证（MVP） | v1 不做 auth，本机调用信任模型 | 简化 MVP，v2 再加 token |

### 运维

| 指标 | 要求 | 依据 |
|------|------|------|
| 部署 | 单二进制，`go build` 即产出，无外部依赖 | 零运维负担 |
| 配置 | CLI 参数：`--port`、`--db`（数据文件路径）；环境变量：`AGENT_QUEUE_DISCORD_WEBHOOK_URL`（通知） | 最小化配置 |
| 日志 | stdout/stderr，支持重定向到文件 | 配合 launchd/systemd |

---

## 7. 验收标准（全局 Done Definition）

### API 层

- [ ] 7 个端点全部实现且返回正确的 HTTP 状态码（200/201/409/422/404/500）
- [ ] 所有请求/响应使用 JSON 格式，Content-Type 正确
- [ ] 非法请求（缺少必填字段、非法状态转换、version 冲突）返回明确的错误信息
- [ ] `GET /health` 返回 200 且包含数据库连接状态

### 乐观锁

- [ ] 模拟 10 个并发 claim 请求，有且只有 1 个返回 200，其余返回 409
- [ ] version 字段每次写操作自增 1

### 依赖推进

- [ ] 创建 A→B→C 串行链，A 完成后 B 可认领，B 完成后 C 可认领
- [ ] 创建 3 个并行任务 + 1 个汇总任务（depends_on 三者），三者全 done 后汇总可认领
- [ ] `deps_met=true` 过滤正确排除依赖未满足的任务

### 状态机

- [ ] 所有合法转换可执行（含 v3 新增 `in_progress → pending`）
- [ ] 所有非法转换返回 422
- [ ] 每次状态变更在 `task_history` 中有记录
- [ ] `requires_review=true` 的任务：`in_progress → done` 返回 422
- [ ] `requires_review=false` 的任务：`in_progress → review` 返回 422
- [ ] `in_progress → pending`（超时释放）后 `assigned_to` 被清空

### Webhook 通知（F6）

- [ ] 任务状态变为 `done` 时，Go server 异步 POST Discord Incoming Webhook
- [ ] 通知内容包含 task title + task_id + @用户
- [ ] Webhook URL 未配置时 graceful 降级（no-op + log.Info，不 panic）
- [ ] Webhook 调用失败重试 1 次，最终失败记 error log，不阻塞状态变更
- [ ] Go server 通过 `Notifier` 接口抽象，不直接 import Discord SDK

### 专家集成协议

- [ ] 专家通过 `PATCH /tasks/:id` 报告完成，Go server 自动触发 webhook + 依赖解锁 + sessions_send 唤醒下游
- [ ] `GET /tasks?assigned_to=agent_name` 正确返回该 agent 的所有任务
- [ ] 专家无需 sessions_send CEO（有 task_id 时），Go server webhook 是唯一通知通道

### 数据持久化

- [ ] 创建任务 → 重启进程 → 任务仍在
- [ ] 进程崩溃重启后，所有 in_progress 任务可查询到

### 部署

- [ ] `go build` 一条命令产出可执行文件
- [ ] 首次启动自动创建 SQLite 数据库 + 执行 schema migration
- [ ] `--port` 和 `--db` 参数可配置

### 痛点消除验证

- [ ] **P1 被动响应消除**：任务完成后 Go server 秒级 webhook 推送，CEO 无需轮询即感知
- [ ] **P2 断链消除**：3 步串行链全程自动推进（依赖自动解锁 + agent cron 认领），步间间隔 = agent cron 周期 + 执行时间
- [ ] **P3 通知延迟消除**：任务完成后 ≤ 5 秒 webhook 推送到 Discord，用户直接收到 @mention
- [ ] **P4 状态丢失消除**：kill CEO session → 重启 → `GET /tasks` 返回完整任务列表和状态，零丢失
- [ ] **P5 依赖黑盒消除**：`GET /tasks/:id` 返回 `depends_on` 列表及每个依赖的当前状态
- [ ] **P6 无验收消除**：启用 review 流程的任务，直接 `in_progress → done` 返回 422（必须经 review）
- [ ] **P7 离线不可用消除**：用户创建任务链 → 断开所有连接 → 任务链自动跑完 → 用户重新上线看到完成通知

---

## 8. 通知与集成架构

> **v4 架构：Go server webhook 推送，取代 v3 的 CEO cron 拉取。**

| 层 | 职责 |
|---|------|
| **Go server（agent-queue）** | 状态持久化 + REST API + 轻量 webhook 通知。任务完成时异步 POST Discord Incoming Webhook URL，通知内容为纯文本 @mention。 |
| **CEO（OpenClaw）** | 监控者角色。通过 webhook 推送被动感知任务完成/blocked，仅在异常时介入（超时、blocked、需要人工决策）。不主动轮询。 |
| **专家 agent** | 通过 HTTP API 直接操作任务：查询 → 认领 → 执行 → `PATCH /tasks/:id` 写回。不再通过 sessions_send 汇报 CEO。 |

**为什么从 cron 拉改为 webhook 推：**
- **延迟**：cron 拉取有 1 个轮询周期的固有延迟（3min），webhook 推送秒级触达
- **复杂度**：cron 拉取需要去重机制，webhook 推送是 fire-and-forget，无需状态管理
- **CEO 依赖**：cron 拉取依赖 CEO 在线，webhook 直接推到 Discord 频道，不依赖任何 agent
- **平台无关性保留**：Incoming Webhook 是标准 HTTP POST，不是 Discord SDK 绑定；通过 `Notifier` 接口抽象，可换任何平台

**验收条件：**
- Go server 通过 `Notifier` 接口发送通知，不直接 import Discord SDK
- 环境变量未配置时 graceful 降级（no-op + log）
- webhook 失败不阻塞主流程（状态变更已 commit）

---

## 9. 专家集成协议

专家 agent 通过 HTTP API 直接与 agent-queue 交互，不再经过 CEO 中转。

### 核心交互流程

```
专家 cron 触发
  → GET /tasks?status=pending&deps_met=true&assigned_to=agent_name
  → POST /tasks/:id/claim (version=N)
  → 执行任务
  → PATCH /tasks/:id {"status": "done", "result": "...", "version": N+1}
  → Go server 自动 webhook 通知
```

### API 调用规范

| 场景 | 调用 | 说明 |
|------|------|------|
| 查自己的任务 | `GET /tasks?assigned_to=agent_name` | 含所有状态的任务 |
| 查可认领任务 | `GET /tasks?status=pending&deps_met=true` | 依赖已满足的待认领任务 |
| 认领任务 | `POST /tasks/:id/claim` body `{"version": N, "agent": "agent_name"}` | 乐观锁防重复 |
| 报告完成 | `PATCH /tasks/:id` body `{"status": "done", "result": "...", "version": N}` | 触发 webhook + 依赖解锁 |
| 报告阻塞 | `PATCH /tasks/:id` body `{"status": "blocked", "note": "原因", "version": N}` | CEO 会收到 webhook 通知 |

### 专家不再做的事

- ❌ 不调用 `sessions_send` 向 CEO 汇报任务完成
- ❌ 不用 `message` tool 向 #首席ceo 发消息汇报进度
- ❌ 不主动 @CEO（通知由 Go server webhook 自动处理）

### 当前状态：Phase 2（全切）

专家汇报已全切至 `PATCH /tasks`，sessions_send 仅保留无 task_id 时的兜底 fallback。

| 阶段 | 模式 | 状态 |
|------|------|------|
| Phase 1 | `PATCH /tasks` + `sessions_send` 双写 | ✅ 历史阶段（已完成过渡） |
| **Phase 2** | **纯 `PATCH /tasks`** | **✅ 当前状态** |

---

## 10. CEO 集成说明

### 角色变更：推进者 → 监控者

| 维度 | v3（旧） | v4（新） |
|------|---------|---------|
| 感知方式 | cron 轮询（旧模式，已废弃） | webhook 推送被动接收 + startup GET /tasks/summary |
| 推进串行链 | CEO 发现 done → 手动派下一步 | Go server F3 自动解锁依赖，agent cron 自行认领 |
| 通知用户 | CEO 转发 | Go server webhook 直推 Discord |
| 介入时机 | 每个任务完成都介入 | 仅在 blocked / 超时 / 需人工决策时介入 |

### CEO 仍负责的事

- 创建任务链（`POST /tasks` + `depends_on`）
- 处理 `blocked` 任务（决策后 `PATCH /tasks/:id` 解除阻塞）
- 超时检测（cron 检查长时间 `in_progress` 的任务，触发 `in_progress → pending` 释放）
- 最终决策（涉及需求变更、资源分配等人工判断）

### CEO 不再做的事

- ❌ 不主动轮询 done 任务
- ❌ 不手动推进串行链下一步（依赖自动解锁）
- ❌ 不转发专家完成通知给用户（webhook 直推）

---

## 11. 非 MVP 范围（明确排除）

以下功能 **不在 v1 范围内**，后续版本按需评估：

| 排除项 | 原因 |
|--------|------|
| **认证/鉴权（API token / OAuth）** | v1 仅本机调用，信任模型足够；v2 若需远程访问再加 |
| **Web UI / Dashboard** | API 给 agent 用，不是给人用；CLI 工具或 cURL 已满足调试需求 |
| **WebSocket 实时推送** | v1 用 Incoming Webhook（HTTP POST）推送；WebSocket 双向通道 v2 再评估 |
| **分布式部署 / 多节点** | SQLite 是单机数据库，v1 定位本机使用；若需多机，v2 换 PostgreSQL |
| **任务超时自动处理** | v1 依赖人工/cron 检测超时任务；v2 加 `timeout` 字段 + 自动释放 |
| **任务优先级动态调整** | v1 创建时设定优先级即固定；v2 加 PATCH priority |
| **批量操作 API** | v1 单任务 CRUD；v2 加 batch create/update |
| **通用 Webhook 回调** | v1 内置 Discord Incoming Webhook（F6）；v2 加通用 webhook 配置（任意 URL + 自定义 payload） |
| **SDK / Client Library** | v1 纯 HTTP API + cURL；v2 按需出 Go/Python/TS client |
| **指标 / Prometheus** | v1 用日志 + health endpoint；v2 加 /metrics |

---

## 附录 A: 技术方案引用

完整技术设计由架构师输出，参考 Discord #架构师 频道 msgId `1476145473239908373`。

核心技术决策摘要：
- **存储**：SQLite WAL 模式，3 张表（tasks / task_deps / task_history）
- **API**：Go net/http，无框架，7 个端点，`localhost:19827`
- **乐观锁**：`version` 字段 + `WHERE version = ?` 原子更新
- **部署**：单二进制 + launchd（macOS）/ systemd（Linux）KeepAlive
- **数据位置**：`data/queue.db`（可通过 `--db` 自定义）
- **代码量（commit 7610e5e）**：handler ~540行 + store ~690行

**当前 schema 字段（tasks 表关键字段）：**
- `assigned_to VARCHAR` — 任务负责人（agent 名称），claim 时写入，超时释放时清空
- `retry_assigned_to VARCHAR` — failed 时指定重试 agent
- `requires_review BOOLEAN DEFAULT false` — 强制 review 路由，见 F4 条件路由
- `result VARCHAR` — done 时写摘要，failed 时写失败原因（支持 `原因 | retry_assigned_to: X` 格式）

**技术参考：**
- thinker 全盘梳理：Discord #架构师 msgId `1476245842980638750`（commit 7610e5e 对齐）

## 附录 B: 状态流转图

```
                    ┌───────────┐
              ┌────►│  pending   │◄──────────────────┐
              │     └─────┬─────┘                    │
              │           │ claim                    │ timeout/
              │     ┌─────▼─────┐                    │ release
              ├─────│  claimed   │                    │
              │     └─────┬─────┘                    │
              │ release    │ start                   │
              │     ┌─────▼──────┐                   │
              │     │ in_progress │──────────┬────────┘
              │     └──┬────┬────┘           │
              │        │    │                │ block
              │   done │    │ review   ┌─────▼─────┐
              │        │  ┌─▼──────┐   │  blocked   │
              │        │  │ review  │   └─────┬─────┘
              │        │  └─┬──┬───┘         │ unblock
              │        │    │  │ revise       │
              │        │done│  └──────►──────►│
              │     ┌──▼────▼──┐             │
              │     │   done    │◄────────────┘
              │     └──────────┘
              │     ┌──────────┐
              └────►│cancelled  │
                    └──────────┘

注：in_progress → pending（超时释放）由 cron/人工触发，
    清空 assigned_to，任务回到可认领池。
```

## 附录 C: JTBD 验证

> **当** 我运行多个 AI agent 处理一个多步骤任务链时，
> **我想要** 一个持久化的任务队列来协调它们的执行顺序和状态，
> **以便** 即使某个 agent 崩溃了，任务链也能从断点自动恢复而非重头开始。

**现有替代方案及不足：**
- 文件系统状态（如 PENDING.md）：无原子性，无法并发安全访问
- 内存状态（agent 上下文）：崩溃即丢，无持久化
- 重量级队列（RabbitMQ/Redis）：部署复杂，对个人工作站过重
- 通用任务管理（Vikunja/Todoist API）：缺少 `depends_on` + 乐观锁，核心需求不匹配
