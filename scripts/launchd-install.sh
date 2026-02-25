#!/bin/bash
# 安装 agent-queue launchd service
set -e
PLIST_SRC="$(dirname "$0")/../launchd/com.irchelper.agent-queue.plist"
PLIST_DST="$HOME/Library/LaunchAgents/com.irchelper.agent-queue.plist"
mkdir -p ~/Library/Logs/agent-queue
cp "$PLIST_SRC" "$PLIST_DST"
launchctl load "$PLIST_DST"
echo "agent-queue service installed and started"
echo "Check status: launchctl list | grep agent-queue"
echo "Logs: tail -f ~/Library/Logs/agent-queue/stdout.log"
