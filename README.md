# agent-queue

轻量级 multi-agent 任务队列服务——SQLite 持久化 + Go REST API。

## 快速开始

```bash
make build
./agent-queue                         # 监听 localhost:19827
# 自定义端口和数据库路径
./agent-queue --port 8080 --db /path/to/queue.db
```

## 环境变量

| 变量 | 说明 |
|------|------|
| `AGENT_QUEUE_PORT` | 监听端口（默认 19827） |
| `AGENT_QUEUE_DISCORD_WEBHOOK_URL` | Discord Incoming Webhook URL |
| `AGENT_QUEUE_DISCORD_USER_ID` | 任务完成通知中 @mention 的 Discord 用户 ID |

## API 端点

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/health` | 健康检查 |
| POST | `/tasks` | 创建任务 |
| GET | `/tasks` | 查询任务列表（支持 status/assigned_to/parent_id/deps_met 过滤） |
| GET | `/tasks/:id` | 查询任务详情（含依赖 + 历史） |
| PATCH | `/tasks/:id` | 更新状态/结果（乐观锁） |
| DELETE | `/tasks/:id` | 删除任务 |
| POST | `/tasks/:id/claim` | 乐观锁认领 |
| GET | `/tasks/:id/deps-met` | 查询依赖是否满足 |

## 示例

```bash
# 创建串行任务链 A→B
A=$(curl -s -X POST localhost:19827/tasks \
  -H 'Content-Type: application/json' \
  -d '{"title":"Task A"}' | jq -r .id)

curl -s -X POST localhost:19827/tasks \
  -H 'Content-Type: application/json' \
  -d "{\"title\":\"Task B\",\"depends_on\":[\"$A\"]}"

# 认领 A
VER=$(curl -s localhost:19827/tasks/$A | jq .version)
curl -s -X POST localhost:19827/tasks/$A/claim \
  -H 'Content-Type: application/json' \
  -d "{\"version\":$VER,\"agent\":\"coder\"}"
```

## 部署（macOS launchd）

### 1. 填写环境变量

编辑 `launchd/com.irchelper.agent-queue.plist`，填入实际值：

```xml
<key>AGENT_QUEUE_DISCORD_WEBHOOK_URL</key>
<string>https://discord.com/api/webhooks/...</string>
<key>AGENT_QUEUE_DISCORD_USER_ID</key>
<string>你的 Discord 用户 ID</string>
```

### 2. 安装并启动服务

```bash
make build                        # 确保编译产物是最新的
bash scripts/launchd-install.sh   # 安装 launchd service 并启动
```

### 3. 验证

```bash
launchctl list | grep agent-queue           # 应显示 PID
curl http://localhost:19827/health          # 应返回 {"status":"ok"}
tail -f ~/Library/Logs/agent-queue/stdout.log
```

### 4. 卸载

```bash
bash scripts/launchd-uninstall.sh
```

### 注意事项

- 服务崩溃后 launchd 会自动重启（`KeepAlive: true`）
- 修改 plist 后需要 `launchctl unload` + `launchctl load` 才能生效
- 日志目录：`~/Library/Logs/agent-queue/`

## 开发

```bash
make test    # 运行全部测试（含 -race）
make vet     # go vet
make build   # 编译
```
