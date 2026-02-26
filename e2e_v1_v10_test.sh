#!/usr/bin/env bash
# e2e_v1_v10_test.sh — QA-P4 全场景验收（V1-V10 × 9条标准链路）
# 对线上 agent-queue 服务 (localhost:19827) 执行 e2e 验收
#
# RULE: 测试任务 assigned_to 禁止使用真实 agent 名
#       （coder/thinker/qa/devops/writer/ops/vision/pm/uiux/security）
#       例外：验证 retry_routing 表路由的场景（V7/V8/V10）必须使用真实名，
#       此类任务 title 须加 [TEST] 前缀，且脚本结束时自动 cancel 清理。
set -euo pipefail

BASE="http://localhost:19827"
PASS=0
FAIL=0
ERRORS=()

# 记录使用真实 agent 名的任务 ID，脚本结束时 cancel 清理
REAL_AGENT_TASK_IDS=()

# ── helpers ──────────────────────────────────────────────────────────
ok()   { PASS=$((PASS+1)); echo "  ✅ $1"; }
fail() { FAIL=$((FAIL+1)); ERRORS+=("$1"); echo "  ❌ $1"; }

# Use python for JSON parsing to avoid jq control-character issues
pj() {
  python3 -c "
import sys, json
data = json.load(sys.stdin)
# Auto-unwrap {task: {...}} wrapper from PATCH responses
if isinstance(data, dict) and 'task' in data and isinstance(data['task'], dict) and len(sys.argv) > 1:
    path = sys.argv[1]
    # If path doesn't start with 'task.' and the key exists in data['task'], unwrap
    if not path.startswith('task.') and not path.startswith('tasks.'):
        inner = data['task']
        first_key = path.split('.')[0]
        if first_key in inner and first_key not in data:
            data = inner
path = sys.argv[1] if len(sys.argv) > 1 else ''
for key in path.split('.'):
    if not key: continue
    if isinstance(data, list):
        data = data[int(key)]
    elif isinstance(data, dict):
        data = data.get(key)
    else:
        data = None
        break
if data is None:
    print('null')
else:
    print(data)
" "$@"
}

create_task() {
  local extra="${3:-}"
  local payload="{\"title\":\"$1\",\"assigned_to\":\"$2\"${extra:+,$extra}}"
  curl -sf -X POST "$BASE/tasks" -H 'Content-Type: application/json' -d "$payload"
}

get_task() { curl -sf "$BASE/tasks/$1"; }

claim_task() {
  curl -sf -X POST "$BASE/tasks/$1/claim" \
    -H 'Content-Type: application/json' \
    -d "{\"version\":$2,\"agent\":\"$3\"}"
}

patch_status() {
  local extra="${4:-}"
  local payload="{\"status\":\"$2\",\"version\":$3${extra:+,$extra}}"
  curl -sf -X PATCH "$BASE/tasks/$1" \
    -H 'Content-Type: application/json' \
    -d "$payload"
}

get_ver() { echo "$1" | pj "version"; }
get_id() { echo "$1" | pj "id"; }
get_st() { echo "$1" | pj "status"; }

drive_to_done() {
  local claimed ver ip ip_ver
  claimed=$(claim_task "$1" "$2" "$3")
  ver=$(get_ver "$claimed")
  ip=$(patch_status "$1" "in_progress" "$ver")
  ip_ver=$(get_ver "$ip")
  patch_status "$1" "done" "$ip_ver"
}

drive_to_failed() {
  local claimed ver ip ip_ver extra="${5:-}"
  claimed=$(claim_task "$1" "$2" "$3")
  ver=$(get_ver "$claimed")
  ip=$(patch_status "$1" "in_progress" "$ver")
  ip_ver=$(get_ver "$ip")
  patch_status "$1" "failed" "$ip_ver" "\"result\":\"$4\"${extra:+,$extra}"
}

create_chain() {
  local extra="${2:-}"
  local payload="{\"tasks\":$1${extra:+,$extra}}"
  curl -sf -X POST "$BASE/dispatch/chain" \
    -H 'Content-Type: application/json' \
    -d "$payload"
}

deps_met() { curl -sf "$BASE/tasks/$1/deps-met" | pj "deps_met"; }

poll_agent() { curl -sf "$BASE/tasks/poll?assigned_to=$1"; }

# Find task by title prefix using python (robust to control chars)
find_tasks_by_prefix() {
  # $1=prefix [$2=exclude_id]
  local exclude="${2:-}"
  curl -sf "$BASE/tasks" | python3 -c "
import sys, json
data = json.load(sys.stdin)
prefix = sys.argv[1]
exclude = sys.argv[2] if len(sys.argv) > 2 else ''
for t in data.get('tasks', []):
    if t['title'].startswith(prefix) and t['id'] != exclude:
        print(t['id'])
" "$1" "$exclude"
}

refresh_ver() { curl -sf "$BASE/tasks/$1" | pj "version"; }

# cancel_task: cancel a task that is pending or failed
cancel_task() {
  local id="$1"
  local st=$(curl -sf "$BASE/tasks/$id" | pj "status")
  local ver=$(refresh_ver "$id")
  case "$st" in
    pending|failed)
      curl -sf -X PATCH "$BASE/tasks/$id" \
        -H 'Content-Type: application/json' \
        -d "{\"status\":\"cancelled\",\"version\":$ver}" > /dev/null 2>&1 || true
      ;;
  esac
}

# cleanup_real_agent_tasks: cancel all tracked real-agent tasks and their auto-created retries
cleanup_real_agent_tasks() {
  if [[ ${#REAL_AGENT_TASK_IDS[@]} -eq 0 ]]; then return; fi
  echo ""
  echo "── cleanup: cancelling [TEST] 任务残留 ──"
  # Also find any tasks with [TEST] prefix title that are pending/failed
  ALL_TEST_IDS=$(curl -sf "$BASE/tasks" | python3 -c "
import sys, json
data = json.load(sys.stdin)
for t in data.get('tasks', []):
    if t['title'].startswith('[TEST]') and t['status'] in ('pending','failed'):
        print(t['id'])
")
  for id in "${REAL_AGENT_TASK_IDS[@]}" $ALL_TEST_IDS; do
    cancel_task "$id" && echo "  cancelled $id" || true
  done
}

echo "═══════════════════════════════════════════════════════"
echo "  QA-P4 E2E 验收 — V1-V10 × 9条标准链路"
echo "═══════════════════════════════════════════════════════"
echo ""

# ─────────────────────────────────────────────────────────
# V1: 基础任务状态机 (pending→claimed→in_progress→done)
# assigned_to: e2e-scaffold (非真实 agent)
# ─────────────────────────────────────────────────────────
echo "── V1: 基础任务状态机 ──"

T=$(create_task "v1-basic" "e2e-scaffold")
TID=$(get_id "$T")
TVER=$(get_ver "$T")
[[ $(get_st "$T") == "pending" ]] && ok "V1.1 创建任务 → pending" || fail "V1.1 创建任务状态不是 pending"

CLAIMED=$(claim_task "$TID" "$TVER" "e2e-scaffold")
CVER=$(get_ver "$CLAIMED")
CST=$(get_st "$CLAIMED")
[[ "$CST" == "claimed" ]] && ok "V1.2 claim → claimed" || fail "V1.2 claim 后状态不是 claimed，得到 $CST"

IP=$(patch_status "$TID" "in_progress" "$CVER")
IPVER=$(get_ver "$IP")
[[ $(get_st "$IP") == "in_progress" ]] && ok "V1.3 → in_progress" || fail "V1.3 状态不是 in_progress"

DONE=$(patch_status "$TID" "done" "$IPVER")
[[ $(get_st "$DONE") == "done" ]] && ok "V1.4 → done" || fail "V1.4 状态不是 done"

DVER=$(get_ver "$DONE")
HTTP=$(curl -s -o /dev/null -w "%{http_code}" -X PATCH "$BASE/tasks/$TID" \
  -H 'Content-Type: application/json' -d "{\"status\":\"in_progress\",\"version\":$DVER}")
[[ "$HTTP" == "422" ]] && ok "V1.5 非法转换 done→in_progress 返回 422" || fail "V1.5 非法转换应返回 422，得到 $HTTP"

echo ""

# ─────────────────────────────────────────────────────────
# V2: /poll 和 /dispatch
# assigned_to: e2e-scaffold (非真实 agent)
# ─────────────────────────────────────────────────────────
echo "── V2: poll + dispatch ──"

DISP=$(curl -sf -X POST "$BASE/dispatch" -H 'Content-Type: application/json' \
  -d '{"title":"v2-dispatch","assigned_to":"e2e-scaffold","description":"test dispatch"}')
DID=$(get_id "$DISP")
[[ -n "$DID" && "$DID" != "null" ]] && ok "V2.1 /dispatch 创建任务" || fail "V2.1 /dispatch 失败"

POLL=$(poll_agent "e2e-scaffold")
PTASK=$(echo "$POLL" | pj "task.id")
[[ -n "$PTASK" && "$PTASK" != "null" ]] && ok "V2.2 /poll?assigned_to=e2e-scaffold 返回任务" || fail "V2.2 poll 未返回任务"

POLL_NONE=$(poll_agent "nonexistent-agent-xyz")
PNULL=$(echo "$POLL_NONE" | pj "task")
[[ "$PNULL" == "None" || "$PNULL" == "null" ]] && ok "V2.3 poll 不存在的 agent 返回 null" || fail "V2.3 poll 应返回 null"

# 清理
DVER2=$(get_ver "$DISP")
drive_to_done "$DID" "$DVER2" "e2e-scaffold" > /dev/null 2>&1 || true

echo ""

# ─────────────────────────────────────────────────────────
# V3: 串行链 — 链路1: e2e-coder→e2e-thinker→e2e-qa→e2e-devops
# assigned_to: e2e-* (非真实 agent)
# ─────────────────────────────────────────────────────────
echo "── V3+链路1: 后端L2 e2e-coder→e2e-thinker→e2e-qa→e2e-devops ──"

CHAIN=$(create_chain '[
  {"title":"L2-coder","assigned_to":"e2e-coder"},
  {"title":"L2-thinker","assigned_to":"e2e-thinker"},
  {"title":"L2-qa","assigned_to":"e2e-qa"},
  {"title":"L2-devops","assigned_to":"e2e-devops"}
]')

C0=$(echo "$CHAIN" | pj "tasks.0.id"); V0=$(echo "$CHAIN" | pj "tasks.0.version")
C1=$(echo "$CHAIN" | pj "tasks.1.id")
C2=$(echo "$CHAIN" | pj "tasks.2.id")
C3=$(echo "$CHAIN" | pj "tasks.3.id")

[[ $(deps_met "$C1") == "False" ]] && ok "V3.1 e2e-thinker deps_met=false" || fail "V3.1 thinker 不应该 deps_met"

drive_to_done "$C0" "$V0" "e2e-coder" > /dev/null; sleep 0.3
[[ $(deps_met "$C1") == "True" ]] && ok "V3.2 e2e-coder done → e2e-thinker deps_met=true" || fail "V3.2 thinker 应 deps_met"
[[ $(deps_met "$C2") == "False" ]] && ok "V3.3 e2e-qa deps_met=false" || fail "V3.3 qa 不应 deps_met"

V1R=$(refresh_ver "$C1")
drive_to_done "$C1" "$V1R" "e2e-thinker" > /dev/null; sleep 0.3
[[ $(deps_met "$C2") == "True" ]] && ok "V3.4 e2e-thinker done → e2e-qa deps_met=true" || fail "V3.4 qa 应 deps_met"

V2R=$(refresh_ver "$C2")
drive_to_done "$C2" "$V2R" "e2e-qa" > /dev/null; sleep 0.3
[[ $(deps_met "$C3") == "True" ]] && ok "V3.5 e2e-qa done → e2e-devops deps_met=true" || fail "V3.5 devops 应 deps_met"

V3R=$(refresh_ver "$C3")
drive_to_done "$C3" "$V3R" "e2e-devops" > /dev/null
ok "V3.6 链路1完整流转 done"

echo ""

# ─────────────────────────────────────────────────────────
# 链路2-9: 正常链路流转 (e2e-* 前缀)
# ─────────────────────────────────────────────────────────

drive_chain() {
  local label="$1"; shift
  local chain_json="$1"; shift
  local agents=("$@")
  
  local CH=$(create_chain "$chain_json")
  local count=${#agents[@]}
  
  for ((i=0; i<count; i++)); do
    local ID=$(echo "$CH" | pj "tasks.$i.id")
    local V=$(refresh_ver "$ID")
    drive_to_done "$ID" "$V" "${agents[$i]}" > /dev/null
    sleep 0.2
  done
  ok "$label 完整流转"
}

echo "── 链路2: 后端L1 e2e-coder→e2e-qa→e2e-devops ──"
drive_chain "链路2 e2e-coder→e2e-qa→e2e-devops" \
  '[{"title":"L1-coder","assigned_to":"e2e-coder"},{"title":"L1-qa","assigned_to":"e2e-qa"},{"title":"L1-devops","assigned_to":"e2e-devops"}]' \
  e2e-coder e2e-qa e2e-devops
echo ""

echo "── 链路3: 前端L2 e2e-coder→e2e-thinker→e2e-vision→e2e-qa→e2e-devops ──"
drive_chain "链路3 前端L2" \
  '[{"title":"FE2-coder","assigned_to":"e2e-coder"},{"title":"FE2-thinker","assigned_to":"e2e-thinker"},{"title":"FE2-vision","assigned_to":"e2e-vision"},{"title":"FE2-qa","assigned_to":"e2e-qa"},{"title":"FE2-devops","assigned_to":"e2e-devops"}]' \
  e2e-coder e2e-thinker e2e-vision e2e-qa e2e-devops
echo ""

echo "── 链路4: 前端L1 e2e-coder→e2e-vision→e2e-qa→e2e-devops ──"
drive_chain "链路4 前端L1" \
  '[{"title":"FE1-coder","assigned_to":"e2e-coder"},{"title":"FE1-vision","assigned_to":"e2e-vision"},{"title":"FE1-qa","assigned_to":"e2e-qa"},{"title":"FE1-devops","assigned_to":"e2e-devops"}]' \
  e2e-coder e2e-vision e2e-qa e2e-devops
echo ""

echo "── 链路5: 安全敏感 e2e-coder→e2e-security→e2e-thinker→e2e-qa→e2e-devops ──"
drive_chain "链路5 安全敏感" \
  '[{"title":"SEC-coder","assigned_to":"e2e-coder"},{"title":"SEC-security","assigned_to":"e2e-security"},{"title":"SEC-thinker","assigned_to":"e2e-thinker"},{"title":"SEC-qa","assigned_to":"e2e-qa"},{"title":"SEC-devops","assigned_to":"e2e-devops"}]' \
  e2e-coder e2e-security e2e-thinker e2e-qa e2e-devops
echo ""

echo "── 链路6: 新功能全流程 e2e-pm→e2e-uiux→e2e-coder→e2e-thinker→e2e-vision→e2e-qa→e2e-devops ──"
drive_chain "链路6 新功能全流程" \
  '[{"title":"FULL-pm","assigned_to":"e2e-pm"},{"title":"FULL-uiux","assigned_to":"e2e-uiux"},{"title":"FULL-coder","assigned_to":"e2e-coder"},{"title":"FULL-thinker","assigned_to":"e2e-thinker"},{"title":"FULL-vision","assigned_to":"e2e-vision"},{"title":"FULL-qa","assigned_to":"e2e-qa"},{"title":"FULL-devops","assigned_to":"e2e-devops"}]' \
  e2e-pm e2e-uiux e2e-coder e2e-thinker e2e-vision e2e-qa e2e-devops
echo ""

echo "── 链路7: 文档变更 ──"
drive_chain "链路7a 关键文档 e2e-writer→e2e-thinker" \
  '[{"title":"DOC-writer","assigned_to":"e2e-writer"},{"title":"DOC-thinker","assigned_to":"e2e-thinker"}]' \
  e2e-writer e2e-thinker
T7B=$(create_task "DOC-self" "e2e-writer")
T7BID=$(get_id "$T7B"); T7BV=$(get_ver "$T7B")
drive_to_done "$T7BID" "$T7BV" "e2e-writer" > /dev/null
ok "链路7b 非关键文档 e2e-writer 自检 done"
echo ""

echo "── 链路8: 需求变更 e2e-pm→e2e-writer→e2e-thinker ──"
drive_chain "链路8 需求变更" \
  '[{"title":"REQ-pm","assigned_to":"e2e-pm"},{"title":"REQ-writer","assigned_to":"e2e-writer"},{"title":"REQ-thinker","assigned_to":"e2e-thinker"}]' \
  e2e-pm e2e-writer e2e-thinker
echo ""

echo "── 链路9: 设计变更 e2e-pm→e2e-uiux→e2e-vision→e2e-writer ──"
drive_chain "链路9 设计变更" \
  '[{"title":"DES-pm","assigned_to":"e2e-pm"},{"title":"DES-uiux","assigned_to":"e2e-uiux"},{"title":"DES-vision","assigned_to":"e2e-vision"},{"title":"DES-writer","assigned_to":"e2e-writer"}]' \
  e2e-pm e2e-uiux e2e-vision e2e-writer
echo ""

# ─────────────────────────────────────────────────────────
# V4: 失败状态验证
# assigned_to: e2e-scaffold (非真实 agent，不触发 retry_routing)
# ─────────────────────────────────────────────────────────
echo "── V4: 失败状态 + CEO 重试 ──"

TF=$(create_task "v4-fail" "e2e-scaffold")
TFID=$(get_id "$TF"); TFV=$(get_ver "$TF")
drive_to_failed "$TFID" "$TFV" "e2e-scaffold" "崩溃了" > /dev/null 2>&1 || true
TF_ST=$(curl -sf "$BASE/tasks/$TFID" | pj "status")
[[ "$TF_ST" == "failed" ]] && ok "V4.1 任务正确进入 failed" || fail "V4.1 应为 failed，得到 $TF_ST"

TFV2=$(refresh_ver "$TFID")
patch_status "$TFID" "pending" "$TFV2" > /dev/null 2>&1 || true
TF_ST2=$(curl -sf "$BASE/tasks/$TFID" | pj "status")
[[ "$TF_ST2" == "pending" ]] && ok "V4.2 failed→pending (CEO重试)" || fail "V4.2 failed→pending 失败，得到 $TF_ST2"

# 清理
TFV3=$(refresh_ver "$TFID")
cancel_task "$TFID" || true

echo ""

# ─────────────────────────────────────────────────────────
# V5: poll 按 assigned_to 过滤
# assigned_to: e2e-* (非真实 agent)
# ─────────────────────────────────────────────────────────
echo "── V5: poll 过滤 ──"

TP1=$(create_task "v5-coder" "e2e-coder")
TP2=$(create_task "v5-qa" "e2e-qa")
TP1ID=$(get_id "$TP1"); TP2ID=$(get_id "$TP2")

PQ=$(poll_agent "e2e-qa")
PQ_ID=$(echo "$PQ" | pj "task.id")
[[ "$PQ_ID" == "$TP2ID" ]] && ok "V5.1 e2e-qa poll 返回自己的任务" || fail "V5.1 e2e-qa poll 应返回 $TP2ID，得到 $PQ_ID"

# e2e-coder poll should not return e2e-qa's task
PC=$(poll_agent "e2e-coder")
PC_ID=$(echo "$PC" | pj "task.id")
[[ "$PC_ID" != "$TP2ID" ]] && ok "V5.2 e2e-coder poll 不返回 e2e-qa 任务" || fail "V5.2 e2e-coder poll 不应返回 e2e-qa 任务"

drive_to_done "$TP1ID" "$(get_ver "$TP1")" "e2e-coder" > /dev/null 2>&1 || true
drive_to_done "$TP2ID" "$(get_ver "$TP2")" "e2e-qa" > /dev/null 2>&1 || true

echo ""

# ─────────────────────────────────────────────────────────
# V6: 并行任务
# assigned_to: e2e-* (非真实 agent)
# ─────────────────────────────────────────────────────────
echo "── V6: 并行任务 ──"

P1=$(create_task "v6-p1" "e2e-coder"); P1ID=$(get_id "$P1"); P1V=$(get_ver "$P1")
P2=$(create_task "v6-p2" "e2e-thinker"); P2ID=$(get_id "$P2"); P2V=$(get_ver "$P2")
P3=$(create_task "v6-p3" "e2e-qa"); P3ID=$(get_id "$P3"); P3V=$(get_ver "$P3")

C1R=$(claim_task "$P1ID" "$P1V" "e2e-coder"); S1=$(get_st "$C1R")
C2R=$(claim_task "$P2ID" "$P2V" "e2e-thinker"); S2=$(get_st "$C2R")
C3R=$(claim_task "$P3ID" "$P3V" "e2e-qa"); S3=$(get_st "$C3R")
[[ "$S1" == "claimed" && "$S2" == "claimed" && "$S3" == "claimed" ]] && ok "V6.1 3个并行任务独立 claim → claimed" || fail "V6.1 并行 claim: $S1 $S2 $S3"

for ID in "$P1ID" "$P2ID" "$P3ID"; do
  V=$(refresh_ver "$ID")
  patch_status "$ID" "in_progress" "$V" > /dev/null
  V=$(refresh_ver "$ID")
  patch_status "$ID" "done" "$V" > /dev/null
done
ok "V6.2 并行任务独立完成"

echo ""

# ─────────────────────────────────────────────────────────
# V7: superseded_by + 依赖解锁
# NOTE: 使用真实 agent 名以验证 retry_routing 表路由 (coder → thinker)
# 任务 title 加 [TEST] 前缀；脚本结束时 cancel 残留
# ─────────────────────────────────────────────────────────
echo "── V7: superseded_by + 自动恢复 (retry_routing: coder→thinker) ──"

CH7=$(create_chain '[
  {"title":"[TEST] V7-coder","assigned_to":"coder"},
  {"title":"[TEST] V7-qa","assigned_to":"e2e-qa"}
]')
V7C=$(echo "$CH7" | pj "tasks.0.id"); V7CV=$(echo "$CH7" | pj "tasks.0.version")
V7Q=$(echo "$CH7" | pj "tasks.1.id")
REAL_AGENT_TASK_IDS+=("$V7C" "$V7Q")

drive_to_failed "$V7C" "$V7CV" "coder" "编译失败" > /dev/null
sleep 0.5

[[ $(deps_met "$V7Q") == "False" ]] && ok "V7.1 coder failed → e2e-qa deps_met=false" || fail "V7.1 qa 不应 deps_met"

SSB=$(curl -sf "$BASE/tasks/$V7C" | pj "superseded_by")
[[ -n "$SSB" && "$SSB" != "null" && "$SSB" != "None" ]] && ok "V7.2 superseded_by=$SSB" || fail "V7.2 superseded_by 未设置"

if [[ -n "$SSB" && "$SSB" != "null" && "$SSB" != "None" ]]; then
  REAL_AGENT_TASK_IDS+=("$SSB")
  RETRY_AGENT=$(curl -sf "$BASE/tasks/$SSB" | pj "assigned_to")
  [[ "$RETRY_AGENT" == "thinker" ]] && ok "V7.2b retry_routing coder→thinker ✓" || fail "V7.2b 应为 thinker，得到 $RETRY_AGENT"
  RV=$(refresh_ver "$SSB")
  drive_to_done "$SSB" "$RV" "thinker" > /dev/null; sleep 0.3
  [[ $(deps_met "$V7Q") == "True" ]] && ok "V7.3 retry done → e2e-qa deps_met=true" || fail "V7.3 qa 应 deps_met"
  # drive qa to done and cancel
  V7QV=$(refresh_ver "$V7Q")
  drive_to_done "$V7Q" "$V7QV" "e2e-qa" > /dev/null
fi

echo ""

# ─────────────────────────────────────────────────────────
# V8: chain_id + retry_routing
# NOTE: 使用真实 coder agent 以验证 retry_routing (coder→thinker)
# 任务 title 加 [TEST] 前缀
# ─────────────────────────────────────────────────────────
echo "── V8: chain_id + retry_routing ──"

CH8=$(create_chain '[
  {"title":"[TEST] V8-coder","assigned_to":"coder"},
  {"title":"[TEST] V8-qa","assigned_to":"e2e-qa"}
]' '"notify_ceo_on_complete":true,"chain_title":"V8测试链"')
CHAIN_ID=$(echo "$CH8" | pj "chain_id")
V8C=$(echo "$CH8" | pj "tasks.0.id")
V8Q=$(echo "$CH8" | pj "tasks.1.id")
REAL_AGENT_TASK_IDS+=("$V8C" "$V8Q")

[[ -n "$CHAIN_ID" && "$CHAIN_ID" != "null" && "$CHAIN_ID" != "None" ]] && ok "V8.1 chain_id=$CHAIN_ID" || fail "V8.1 chain_id 缺失"

V8C_CID=$(curl -sf "$BASE/tasks/$V8C" | pj "chain_id")
[[ "$V8C_CID" == "$CHAIN_ID" ]] && ok "V8.2 task.chain_id 正确" || fail "V8.2 chain_id 不匹配"

V8CV=$(refresh_ver "$V8C")
drive_to_failed "$V8C" "$V8CV" "coder" "完全崩溃" > /dev/null
sleep 0.5

V8_SSB=$(curl -sf "$BASE/tasks/$V8C" | pj "superseded_by")
if [[ -n "$V8_SSB" && "$V8_SSB" != "null" && "$V8_SSB" != "None" ]]; then
  REAL_AGENT_TASK_IDS+=("$V8_SSB")
  RETRY_AGENT=$(curl -sf "$BASE/tasks/$V8_SSB" | pj "assigned_to")
  RETRY_CID=$(curl -sf "$BASE/tasks/$V8_SSB" | pj "chain_id")
  [[ "$RETRY_AGENT" == "thinker" ]] && ok "V8.3 retry_routing coder→thinker" || fail "V8.3 应为 thinker，得到 $RETRY_AGENT"
  [[ "$RETRY_CID" == "$CHAIN_ID" ]] && ok "V8.4 retry 继承 chain_id" || fail "V8.4 chain_id 不匹配"
  # cancel retry + qa to avoid stale
  cancel_task "$V8_SSB" || true
else
  fail "V8.3 coder 失败后未创建 retry"
fi
cancel_task "$V8Q" || true

echo ""

# ─────────────────────────────────────────────────────────
# V9: stale 状态验证
# assigned_to: e2e-scaffold (非真实 agent)
# ─────────────────────────────────────────────────────────
echo "── V9: stale ticker 状态 ──"

TS=$(create_task "v9-stale" "e2e-scaffold")
TSID=$(get_id "$TS"); TSV=$(get_ver "$TS")
CL9=$(claim_task "$TSID" "$TSV" "e2e-scaffold")
TST=$(get_st "$CL9")
[[ "$TST" == "claimed" ]] && ok "V9.1 claimed 状态任务可被 stale ticker 扫描" || fail "V9.1 状态不是 claimed: $TST"

V9V=$(refresh_ver "$TSID")
patch_status "$TSID" "in_progress" "$V9V" > /dev/null
V9V2=$(refresh_ver "$TSID")
patch_status "$TSID" "done" "$V9V2" > /dev/null

echo ""

# ─────────────────────────────────────────────────────────
# V10: isReviewReject 二段链 — thinker
# NOTE: 使用真实 thinker/coder 以验证 isReviewReject 逻辑
# 任务 title 加 [TEST] 前缀
# ─────────────────────────────────────────────────────────
echo "── V10: isReviewReject 二段链 (thinker) ──"

CH10=$(create_chain '[
  {"title":"[TEST] V10R-impl","assigned_to":"coder"},
  {"title":"[TEST] V10R-review","assigned_to":"thinker"},
  {"title":"[TEST] V10R-qa","assigned_to":"e2e-qa"}
]')
V10I=$(echo "$CH10" | pj "tasks.0.id"); V10IV=$(echo "$CH10" | pj "tasks.0.version")
V10R=$(echo "$CH10" | pj "tasks.1.id")
V10Q=$(echo "$CH10" | pj "tasks.2.id")
REAL_AGENT_TASK_IDS+=("$V10I" "$V10R" "$V10Q")

drive_to_done "$V10I" "$V10IV" "coder" > /dev/null; sleep 0.3
V10RV=$(refresh_ver "$V10R")
drive_to_failed "$V10R" "$V10RV" "thinker" "REQUEST_CHANGES: 质量差 | retry_assigned_to: coder" > /dev/null
sleep 0.5

FIX_ID=$(find_tasks_by_prefix "fix: [TEST] V10R" | head -1)
RR_ID=$(find_tasks_by_prefix "re-review: [TEST] V10R" | head -1)

[[ -n "$FIX_ID" ]] && ok "V10.1 fix 任务已创建" || fail "V10.1 fix 未创建"
[[ -n "$RR_ID" ]] && ok "V10.2 re-review 已创建" || fail "V10.2 re-review 未创建"

if [[ -n "$FIX_ID" ]]; then REAL_AGENT_TASK_IDS+=("$FIX_ID"); fi
if [[ -n "$RR_ID" ]]; then REAL_AGENT_TASK_IDS+=("$RR_ID"); fi

if [[ -n "$FIX_ID" && -n "$RR_ID" ]]; then
  FA=$(curl -sf "$BASE/tasks/$FIX_ID" | pj "assigned_to")
  RA=$(curl -sf "$BASE/tasks/$RR_ID" | pj "assigned_to")
  [[ "$FA" == "coder" ]] && ok "V10.3 fix→coder" || fail "V10.3 fix 应为 coder: $FA"
  [[ "$RA" == "thinker" ]] && ok "V10.4 re-review→thinker" || fail "V10.4 re-review 应为 thinker: $RA"

  SSB10=$(curl -sf "$BASE/tasks/$V10R" | pj "superseded_by")
  [[ "$SSB10" == "$RR_ID" ]] && ok "V10.5 superseded_by → re-review" || fail "V10.5 superseded_by=$SSB10 应为 $RR_ID"

  [[ $(deps_met "$V10Q") == "False" ]] && ok "V10.6 e2e-qa deps_met=false" || fail "V10.6 qa 不应 deps_met"

  FV=$(refresh_ver "$FIX_ID")
  drive_to_done "$FIX_ID" "$FV" "coder" > /dev/null; sleep 0.3
  [[ $(deps_met "$V10Q") == "False" ]] && ok "V10.7 fix done, e2e-qa 仍 deps_met=false" || fail "V10.7 qa 不应 deps_met (re-review pending)"

  RRV=$(refresh_ver "$RR_ID")
  drive_to_done "$RR_ID" "$RRV" "thinker" > /dev/null; sleep 0.3
  [[ $(deps_met "$V10Q") == "True" ]] && ok "V10.8 re-review done → e2e-qa deps_met=true ✓" || fail "V10.8 qa 应 deps_met"

  # drive qa to done
  V10QV=$(refresh_ver "$V10Q")
  drive_to_done "$V10Q" "$V10QV" "e2e-qa" > /dev/null
fi

echo ""

# ─────────────────────────────────────────────────────────
# V10: isReviewReject — vision
# NOTE: 使用真实 vision agent 以验证 V10.1 补丁
# 任务 title 加 [TEST] 前缀
# ─────────────────────────────────────────────────────────
echo "── V10: isReviewReject (vision) ──"

CH10V=$(create_chain '[
  {"title":"[TEST] V10V2-impl","assigned_to":"coder"},
  {"title":"[TEST] V10V2-vision","assigned_to":"vision"},
  {"title":"[TEST] V10V2-qa","assigned_to":"e2e-qa"}
]')
V10VI=$(echo "$CH10V" | pj "tasks.0.id"); V10VIV=$(echo "$CH10V" | pj "tasks.0.version")
V10VR=$(echo "$CH10V" | pj "tasks.1.id")
V10VQ=$(echo "$CH10V" | pj "tasks.2.id")
REAL_AGENT_TASK_IDS+=("$V10VI" "$V10VR" "$V10VQ")

drive_to_done "$V10VI" "$V10VIV" "coder" > /dev/null; sleep 0.3
VRV=$(refresh_ver "$V10VR")
drive_to_failed "$V10VR" "$VRV" "vision" "UI偏差 | retry_assigned_to: coder" > /dev/null
sleep 0.5

VFIX=$(find_tasks_by_prefix "fix: [TEST] V10V2" | head -1)
VRR=$(find_tasks_by_prefix "re-review: [TEST] V10V2" | head -1)

[[ -n "$VFIX" ]] && ok "V10V.1 vision fix 创建" || fail "V10V.1 fix 未创建"
[[ -n "$VRR" ]] && ok "V10V.2 vision re-review 创建" || fail "V10V.2 re-review 未创建"
if [[ -n "$VFIX" ]]; then REAL_AGENT_TASK_IDS+=("$VFIX"); fi
if [[ -n "$VRR" ]]; then
  REAL_AGENT_TASK_IDS+=("$VRR")
  VRA=$(curl -sf "$BASE/tasks/$VRR" | pj "assigned_to")
  [[ "$VRA" == "vision" ]] && ok "V10V.3 re-review 回 vision" || fail "V10V.3 应为 vision: $VRA"
  VSSB=$(curl -sf "$BASE/tasks/$V10VR" | pj "superseded_by")
  [[ "$VSSB" == "$VRR" ]] && ok "V10V.4 superseded_by → re-review" || fail "V10V.4 superseded_by=$VSSB"
fi

echo ""

# ─────────────────────────────────────────────────────────
# V10: isReviewReject — security
# NOTE: 使用真实 security agent
# 任务 title 加 [TEST] 前缀
# ─────────────────────────────────────────────────────────
echo "── V10: isReviewReject (security) ──"

TS10=$(create_task "[TEST] V10S2-sec" "security")
TS10ID=$(get_id "$TS10"); TS10V=$(get_ver "$TS10")
REAL_AGENT_TASK_IDS+=("$TS10ID")
drive_to_failed "$TS10ID" "$TS10V" "security" "漏洞 | retry_assigned_to: coder" > /dev/null
sleep 0.5

SFIX=$(find_tasks_by_prefix "fix: [TEST] V10S2" | head -1)
SRR=$(find_tasks_by_prefix "re-review: [TEST] V10S2" | head -1)
[[ -n "$SFIX" ]] && ok "V10S.1 security fix 创建" || fail "V10S.1 fix 未创建"
[[ -n "$SRR" ]] && ok "V10S.2 security re-review 创建" || fail "V10S.2 re-review 未创建"
if [[ -n "$SFIX" ]]; then REAL_AGENT_TASK_IDS+=("$SFIX"); fi
if [[ -n "$SRR" ]]; then
  REAL_AGENT_TASK_IDS+=("$SRR")
  SRA=$(curl -sf "$BASE/tasks/$SRR" | pj "assigned_to")
  [[ "$SRA" == "security" ]] && ok "V10S.3 re-review 回 security" || fail "V10S.3 应为 security: $SRA"
fi

echo ""

# ─────────────────────────────────────────────────────────
# V7+V10 交叉: 多级 superseded_by
# NOTE: 使用真实 thinker/coder
# 任务 title 加 [TEST] 前缀
# ─────────────────────────────────────────────────────────
echo "── V7+V10 交叉: 多级 superseded_by ──"

CHM=$(create_chain '[
  {"title":"[TEST] MLT-impl","assigned_to":"coder"},
  {"title":"[TEST] MLT-review","assigned_to":"thinker"},
  {"title":"[TEST] MLT-qa","assigned_to":"e2e-qa"}
]')
MI=$(echo "$CHM" | pj "tasks.0.id"); MIV=$(echo "$CHM" | pj "tasks.0.version")
MR=$(echo "$CHM" | pj "tasks.1.id")
MQ=$(echo "$CHM" | pj "tasks.2.id")
REAL_AGENT_TASK_IDS+=("$MI" "$MR" "$MQ")

drive_to_done "$MI" "$MIV" "coder" > /dev/null; sleep 0.3

# First reject
MRV=$(refresh_ver "$MR")
drive_to_failed "$MR" "$MRV" "thinker" "REQUEST_CHANGES: 第一次 | retry_assigned_to: coder" > /dev/null
sleep 0.5

FIX1=$(find_tasks_by_prefix "fix: [TEST] MLT" | head -1)
RR1=$(find_tasks_by_prefix "re-review: [TEST] MLT" | head -1)

[[ -n "$FIX1" && -n "$RR1" ]] && ok "MULTI.1 第一次 reject: fix+re-review 创建" || fail "MULTI.1 fix1/rr1 未创建"
if [[ -n "$FIX1" ]]; then REAL_AGENT_TASK_IDS+=("$FIX1"); fi
if [[ -n "$RR1" ]]; then REAL_AGENT_TASK_IDS+=("$RR1"); fi

if [[ -n "$FIX1" && -n "$RR1" ]]; then
  # Drive fix1 done
  F1V=$(refresh_ver "$FIX1")
  drive_to_done "$FIX1" "$F1V" "coder" > /dev/null; sleep 0.3

  # Second reject on re-review1
  RR1V=$(refresh_ver "$RR1")
  drive_to_failed "$RR1" "$RR1V" "thinker" "REQUEST_CHANGES: 第二次 | retry_assigned_to: coder" > /dev/null
  sleep 0.5

  # Find second re-review (exclude RR1)
  RR2=$(find_tasks_by_prefix "re-review: " "$RR1" | while read id; do
    T=$(curl -sf "$BASE/tasks/$id" | pj "title")
    if echo "$T" | grep -q "MLT"; then
      echo "$id"
      break
    fi
  done)
  
  if [[ -z "$RR2" ]]; then
    RR2=$(find_tasks_by_prefix "re-review: re-review: [TEST] MLT" | head -1)
  fi

  if [[ -n "$RR2" ]]; then
    REAL_AGENT_TASK_IDS+=("$RR2")
    ok "MULTI.2 第二次 reject: re-review2 创建"

    ORIG_SSB=$(curl -sf "$BASE/tasks/$MR" | pj "superseded_by")
    [[ "$ORIG_SSB" == "$RR2" ]] && ok "MULTI.3 original.superseded_by → re-review2 (UpdateSupersededByChain)" || fail "MULTI.3 superseded_by=$ORIG_SSB 应为 $RR2"

    [[ $(deps_met "$MQ") == "False" ]] && ok "MULTI.4 e2e-qa deps_met=false (re-review2 pending)" || fail "MULTI.4 qa 不应 deps_met"

    FIX2=$(find_tasks_by_prefix "fix: re-review: [TEST] MLT" | head -1)
    if [[ -n "$FIX2" ]]; then
      REAL_AGENT_TASK_IDS+=("$FIX2")
      F2V=$(refresh_ver "$FIX2")
      drive_to_done "$FIX2" "$F2V" "coder" > /dev/null; sleep 0.3
    fi

    RR2V=$(refresh_ver "$RR2")
    drive_to_done "$RR2" "$RR2V" "thinker" > /dev/null; sleep 0.3

    [[ $(deps_met "$MQ") == "True" ]] && ok "MULTI.5 多级 reject → e2e-qa 最终 deps_met=true ✓" || fail "MULTI.5 qa 应 deps_met"

    MQV=$(refresh_ver "$MQ")
    drive_to_done "$MQ" "$MQV" "e2e-qa" > /dev/null
  else
    fail "MULTI.2 re-review2 未创建"
  fi
fi

echo ""

# ─────────────────────────────────────────────────────────
# retry_routing 表完整性
# ─────────────────────────────────────────────────────────
echo "── retry_routing 表完整性 ──"

RR_DATA=$(curl -sf "$BASE/retry-routing")
RR_COUNT=$(echo "$RR_DATA" | python3 -c "import sys,json; print(len(json.load(sys.stdin).get('routes',[])))")
[[ "$RR_COUNT" -ge 15 ]] && ok "RR.1 retry_routing $RR_COUNT 条规则 (≥15)" || fail "RR.1 规则数不足: $RR_COUNT"

check_route() {
  local from="$1" kw="$2" to="$3" label="$4"
  local found=$(echo "$RR_DATA" | python3 -c "
import sys, json
data = json.load(sys.stdin)
for r in data.get('routes', []):
    if r['assigned_to'] == '$from' and r['error_keyword'] == '$kw' and r['retry_assigned_to'] == '$to':
        print('yes')
        break
")
  [[ "$found" == "yes" ]] && ok "$label" || fail "$label"
}

check_route "vision" "" "coder" "RR.2 vision→coder"
check_route "vision" "设计" "uiux" "RR.3 vision+设计→uiux"
check_route "pm" "" "thinker" "RR.4 pm→thinker"
check_route "ops" "" "devops" "RR.5 ops→devops"
check_route "security" "" "coder" "RR.6 security→coder"
check_route "thinker" "代码" "coder" "RR.7 thinker+代码→coder"
check_route "thinker" "文档" "writer" "RR.8 thinker+文档→writer"

echo ""

# ─────────────────────────────────────────────────────────
# Cleanup: cancel 所有 [TEST] 任务残留
# ─────────────────────────────────────────────────────────
cleanup_real_agent_tasks

echo ""

# ─────────────────────────────────────────────────────────
# Summary
# ─────────────────────────────────────────────────────────
echo "═══════════════════════════════════════════════════════"
echo "  验收结果: ✅ $PASS 通过 / ❌ $FAIL 失败"
echo "═══════════════════════════════════════════════════════"

if [[ ${#ERRORS[@]} -gt 0 ]]; then
  echo ""
  echo "失败项:"
  for e in "${ERRORS[@]}"; do
    echo "  - $e"
  done
fi

echo ""
echo "TOTAL_PASS=$PASS"
echo "TOTAL_FAIL=$FAIL"
exit $FAIL
