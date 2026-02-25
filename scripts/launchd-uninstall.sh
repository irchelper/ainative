#!/bin/bash
PLIST="$HOME/Library/LaunchAgents/com.irchelper.agent-queue.plist"
launchctl unload "$PLIST" 2>/dev/null || true
rm -f "$PLIST"
echo "agent-queue service removed"
