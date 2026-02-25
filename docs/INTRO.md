# agent-queue

轻量级 multi-agent 任务队列服务。用 SQLite 持久化任务状态，任何 AI agent 通过 HTTP 协调执行串行/并行任务——不依赖单一调度者在线，崩了也能从断点恢复。
