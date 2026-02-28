<script setup lang="ts">
import { ref, onMounted, computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import AppLayout from '@/layouts/AppLayout.vue'
import { api } from '@/api/client'
import type { Task, TaskHistory } from '@/types'

const route = useRoute()
const router = useRouter()
const task = ref<Task | null>(null)
const history = ref<TaskHistory[]>([])
const chainTasks = ref<Task[]>([])
const loading = ref(true)
const error = ref<string | null>(null)

async function load() {
  loading.value = true
  error.value = null
  try {
    const id = route.params.id as string
    const resp = await api.getTimeline(id)
    task.value = resp.task
    // Reverse so newest first
    history.value = [...(resp.history ?? [])].reverse()

    // V24-B: load comments
    await loadComments()

    // V23-B: load chain tasks for inline chain display
    if (resp.task.chain_id) {
      try {
        const graphResp = await fetch(`/api/graph/${encodeURIComponent(resp.task.chain_id)}`)
        if (graphResp.ok) {
          const graphData = await graphResp.json()
          chainTasks.value = graphData.tasks ?? []
        }
      } catch {
        // best-effort — chain display is non-critical
      }
    }
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    loading.value = false
  }
}

// V23-B: compute duration for each timeline entry
// history[] is newest-first; original list is oldest-first → build from reversed
const historyWithDuration = computed(() => {
  // oldest-first for duration calc
  const oldest = [...history.value].reverse()
  return oldest.map((h, i) => {
    const prev = oldest[i - 1]
    let duration = ''
    if (prev) {
      const ms = new Date(h.changed_at).getTime() - new Date(prev.changed_at).getTime()
      if (ms > 0) duration = formatDuration(ms)
    }
    return { ...h, duration }
  }).reverse() // back to newest-first for rendering
})

function formatDuration(ms: number): string {
  const totalSec = Math.round(ms / 1000)
  const m = Math.floor(totalSec / 60)
  const s = totalSec % 60
  if (m === 0) return `${s}s`
  return s === 0 ? `${m}m` : `${m}m ${s}s`
}

onMounted(load)

function statusColor(status: string): string {
  const map: Record<string, string> = {
    done: 'bg-green-500/15 text-green-400 border-green-500/30',
    failed: 'bg-red-500/15 text-red-400 border-red-500/30',
    in_progress: 'bg-blue-500/15 text-blue-400 border-blue-500/30',
    pending: 'bg-yellow-500/15 text-yellow-400 border-yellow-500/30',
    cancelled: 'bg-gray-700 text-gray-500 border-gray-600',
    blocked: 'bg-orange-500/15 text-orange-400 border-orange-500/30',
    review: 'bg-purple-500/15 text-purple-400 border-purple-500/30',
    claimed: 'bg-cyan-500/15 text-cyan-400 border-cyan-500/30',
  }
  return map[status] ?? 'bg-gray-700 text-gray-400 border-gray-600'
}

function historyStatusColor(status: string): string {
  const map: Record<string, string> = {
    done: 'text-green-400',
    failed: 'text-red-400',
    in_progress: 'text-blue-400',
    pending: 'text-yellow-400',
    cancelled: 'text-gray-500',
    blocked: 'text-orange-400',
    review: 'text-purple-400',
    claimed: 'text-cyan-400',
  }
  return map[status] ?? 'text-gray-400'
}

function historyDot(status: string): string {
  const map: Record<string, string> = {
    done: 'bg-green-400',
    failed: 'bg-red-400',
    in_progress: 'bg-blue-400',
    pending: 'bg-yellow-400',
    cancelled: 'bg-gray-600',
    blocked: 'bg-orange-400',
    review: 'bg-purple-400',
    claimed: 'bg-cyan-400',
  }
  return map[status] ?? 'bg-gray-500'
}

function formatTime(iso: string): string {
  const d = new Date(iso)
  return d.toLocaleString('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit' })
}

const isHumanTask = computed(() => task.value?.assigned_to === 'human')
const isPendingApproval = computed(
  () => task.value?.assigned_to === 'human' && task.value?.status === 'in_progress'
)

// Approve / Reject state
const approving = ref(false)
const rejecting = ref(false)
const rejectReason = ref('')
const showRejectInput = ref(false)
const actionError = ref<string | null>(null)

async function approve() {
  if (!task.value) return
  approving.value = true
  actionError.value = null
  try {
    await api.patchTask(task.value.id, {
      status: 'done',
      result: '✅ 人工审批通过',
      version: task.value.version,
    })
    await load()
  } catch (e) {
    actionError.value = e instanceof Error ? e.message : String(e)
  } finally {
    approving.value = false
  }
}

async function reject() {
  if (!task.value) return
  if (!rejectReason.value.trim()) {
    showRejectInput.value = true
    return
  }
  rejecting.value = true
  actionError.value = null
  try {
    await api.patchTask(task.value.id, {
      status: 'failed',
      failure_reason: rejectReason.value.trim(),
      version: task.value.version,
    })
    await load()
    showRejectInput.value = false
    rejectReason.value = ''
  } catch (e) {
    actionError.value = e instanceof Error ? e.message : String(e)
  } finally {
    rejecting.value = false
  }
}

// Priority
const priorityOptions = [
  { value: 0, label: '普通', color: 'text-gray-400' },
  { value: 1, label: '⬆ 高', color: 'text-blue-400' },
  { value: 2, label: '🔴 紧急', color: 'text-red-400' },
]
const updatingPriority = ref(false)
const priorityError = ref<string | null>(null)

async function updatePriority(value: number) {
  if (!task.value) return
  updatingPriority.value = true
  priorityError.value = null
  try {
    const resp = await api.patchTask(task.value.id, {
      priority: value,
      version: task.value.version,
    })
    task.value = resp.task
  } catch (e) {
    priorityError.value = e instanceof Error ? e.message : String(e)
  } finally {
    updatingPriority.value = false
  }
}

// V24-B: Comments
interface Comment {
  id: number
  task_id: string
  author: string
  content: string
  created_at: string
}

const comments = ref<Comment[]>([])
const newCommentContent = ref('')
const newCommentAuthor = ref('')
const commentLoading = ref(false)
const commentError = ref<string | null>(null)
const commentSubmitting = ref(false)

async function loadComments() {
  if (!task.value) return
  commentLoading.value = true
  try {
    const resp = await fetch(`/api/tasks/${task.value.id}/comments`)
    if (resp.ok) {
      const data = await resp.json()
      comments.value = data.comments ?? []
    }
  } catch {
    // best-effort
  } finally {
    commentLoading.value = false
  }
}

async function submitComment() {
  if (!task.value || !newCommentContent.value.trim()) return
  commentSubmitting.value = true
  commentError.value = null
  try {
    const resp = await fetch(`/api/tasks/${task.value.id}/comments`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        author: newCommentAuthor.value.trim() || 'human',
        content: newCommentContent.value.trim(),
      }),
    })
    if (!resp.ok) {
      const err = await resp.json()
      throw new Error(err.error ?? `HTTP ${resp.status}`)
    }
    newCommentContent.value = ''
    await loadComments()
  } catch (e) {
    commentError.value = e instanceof Error ? e.message : String(e)
  } finally {
    commentSubmitting.value = false
  }
}
</script>

<template>
  <AppLayout>
    <div class="max-w-4xl mx-auto p-6">
      <!-- Back -->
      <button
        class="text-sm text-gray-500 hover:text-gray-300 mb-5 flex items-center gap-1"
        @click="router.back()"
      >← 返回</button>

      <div v-if="loading" class="text-gray-600 text-center py-20">加载中…</div>
      <div v-else-if="error" class="p-4 bg-red-900/40 border border-red-500 rounded text-red-300">{{ error }}</div>

      <div v-else-if="task" class="grid grid-cols-5 gap-6">
        <!-- Left: Task detail (3/5) -->
        <div class="col-span-3 space-y-4">
          <div class="bg-gray-900 border border-gray-700 rounded-2xl p-5">
            <!-- Title + status -->
            <div class="flex items-start justify-between gap-3 mb-4">
              <h1 class="text-lg font-bold text-gray-100 leading-snug flex-1">{{ task.title }}</h1>
              <span
                class="text-xs font-medium px-2.5 py-1 rounded-full border shrink-0"
                :class="statusColor(task.status)"
              >{{ task.status }}</span>
            </div>

            <!-- Meta grid -->
            <div class="grid grid-cols-2 gap-3 text-sm mb-4">
              <div>
                <span class="text-gray-500 text-xs">执行人</span>
                <div class="text-gray-200 mt-0.5 flex items-center gap-1">
                  <span>{{ isHumanTask ? '👤' : '🤖' }}</span>
                  {{ task.assigned_to }}
                </div>
              </div>
              <div>
                <span class="text-gray-500 text-xs">版本</span>
                <div class="text-gray-200 mt-0.5">v{{ task.version }}</div>
              </div>
              <div>
                <span class="text-gray-500 text-xs">创建时间</span>
                <div class="text-gray-200 mt-0.5 text-xs">{{ formatTime(task.created_at) }}</div>
              </div>
              <div v-if="task.started_at">
                <span class="text-gray-500 text-xs">开始时间</span>
                <div class="text-gray-200 mt-0.5 text-xs">{{ formatTime(task.started_at as unknown as string) }}</div>
              </div>
              <div v-if="task.chain_id" class="col-span-2">
                <span class="text-gray-500 text-xs">链路</span>
                <div class="text-gray-400 mt-0.5 font-mono text-xs truncate cursor-pointer hover:text-gray-200"
                  @click="$router.push('/goals')">
                  {{ task.chain_id }}
                </div>
              </div>
              <div v-if="task.timeout_minutes">
                <span class="text-gray-500 text-xs">超时限制</span>
                <div class="text-gray-200 mt-0.5">{{ task.timeout_minutes }} 分钟</div>
              </div>
              <!-- Priority selector -->
              <div>
                <span class="text-gray-500 text-xs">优先级</span>
                <div class="flex items-center gap-1.5 mt-1">
                  <button
                    v-for="opt in priorityOptions"
                    :key="opt.value"
                    class="text-xs px-2 py-0.5 rounded-md border transition-colors"
                    :class="task.priority === opt.value
                      ? 'border-blue-500/60 bg-blue-500/10 ' + opt.color
                      : 'border-gray-700 text-gray-600 hover:border-gray-500 hover:text-gray-400'"
                    :disabled="updatingPriority"
                    @click="updatePriority(opt.value)"
                  >{{ opt.label }}</button>
                </div>
                <div v-if="priorityError" class="text-xs text-red-400 mt-1">{{ priorityError }}</div>
              </div>
            </div>

            <!-- Description -->
            <div v-if="task.description" class="mb-4">
              <div class="text-xs text-gray-500 mb-1.5">描述</div>
              <div class="bg-gray-800 rounded-lg p-3 text-sm text-gray-300 whitespace-pre-wrap leading-normal border border-gray-700/50">{{ task.description }}</div>
            </div>

            <!-- Result -->
            <div v-if="task.result" class="mb-4">
              <div class="text-xs text-gray-500 mb-1.5">执行结果</div>
              <div class="bg-gray-800 rounded-lg p-3 text-sm text-gray-300 border border-gray-700/50">{{ task.result }}</div>
            </div>

            <!-- Failure reason -->
            <div v-if="task.failure_reason" class="mb-4">
              <div class="text-xs text-red-500 mb-1.5">失败原因</div>
              <div class="bg-red-900/20 rounded-lg p-3 text-sm text-red-300 border border-red-700/30">{{ task.failure_reason }}</div>
            </div>

            <!-- Commit URL -->
            <div v-if="task.commit_url">
              <div class="text-xs text-gray-500 mb-1.5">Commit</div>
              <a
                :href="task.commit_url"
                target="_blank"
                rel="noopener"
                class="text-blue-400 hover:text-blue-300 hover:underline text-sm break-all"
              >{{ task.commit_url }}</a>
            </div>

            <!-- Human Approval Panel -->
            <div v-if="isPendingApproval" class="mt-4 border border-amber-500/30 bg-amber-500/5 rounded-xl p-4">
              <div class="flex items-center gap-2 mb-3">
                <span class="text-base">👤</span>
                <span class="text-sm font-semibold text-amber-300">等待人工审批</span>
              </div>

              <div v-if="actionError" class="mb-3 p-2 bg-red-900/30 border border-red-500/30 rounded text-xs text-red-300">
                {{ actionError }}
              </div>

              <!-- Reject reason input -->
              <div v-if="showRejectInput" class="mb-3">
                <textarea
                  v-model="rejectReason"
                  placeholder="请填写拒绝原因..."
                  rows="3"
                  class="w-full bg-gray-800 border border-gray-600 rounded-lg px-3 py-2 text-sm text-gray-200 placeholder-gray-500 focus:outline-none focus:border-red-500/60 resize-none"
                />
              </div>

              <div class="flex gap-2">
                <button
                  class="flex-1 py-2 rounded-lg text-sm font-medium bg-green-500/15 text-green-400 border border-green-500/30 hover:bg-green-500/25 disabled:opacity-50 transition-colors"
                  :disabled="approving || rejecting"
                  @click="approve"
                >
                  <span v-if="approving">处理中…</span>
                  <span v-else>✅ 批准</span>
                </button>
                <button
                  class="flex-1 py-2 rounded-lg text-sm font-medium bg-red-500/15 text-red-400 border border-red-500/30 hover:bg-red-500/25 disabled:opacity-50 transition-colors"
                  :disabled="approving || rejecting"
                  @click="reject"
                >
                  <span v-if="rejecting">处理中…</span>
                  <span v-else-if="showRejectInput">❌ 确认拒绝</span>
                  <span v-else>❌ 拒绝</span>
                </button>
              </div>
            </div>
          </div>
        </div>

        <!-- Right: Timeline (2/5) -->
        <div class="col-span-2 space-y-4">
          <!-- V23-B: Chain inline (only if chain_id exists and chain has >1 task) -->
          <div v-if="chainTasks.length > 1" class="bg-gray-900 border border-gray-700 rounded-2xl p-4">
            <h2 class="text-xs font-semibold text-gray-400 mb-3 flex items-center gap-1.5">
              🔗 链路任务
              <span class="text-gray-600 font-normal">{{ task!.chain_id }}</span>
            </h2>
            <div class="flex flex-col gap-1">
              <div
                v-for="(ct, idx) in chainTasks"
                :key="ct.id"
                class="flex items-center gap-1.5"
              >
                <!-- Arrow connector (not for first item) -->
                <span v-if="idx > 0" class="text-gray-600 text-xs shrink-0">↓</span>
                <span v-else class="w-3 shrink-0"></span>
                <button
                  class="flex-1 flex items-center gap-1.5 text-left rounded-lg px-2 py-1.5 transition-colors min-w-0"
                  :class="ct.id === task!.id
                    ? 'bg-blue-500/15 border border-blue-500/30'
                    : 'bg-gray-800 border border-gray-700/50 hover:border-gray-600'"
                  @click="ct.id !== task!.id && $router.push(`/tasks/${ct.id}`)"
                >
                  <span
                    class="shrink-0 text-[10px] px-1.5 py-0.5 rounded-full border font-medium"
                    :class="statusColor(ct.status)"
                  >{{ ct.status }}</span>
                  <span
                    class="text-xs truncate"
                    :class="ct.id === task!.id ? 'text-blue-300 font-medium' : 'text-gray-400'"
                  >{{ ct.title }}</span>
                </button>
              </div>
            </div>
          </div>

          <!-- Timeline -->
          <div class="bg-gray-900 border border-gray-700 rounded-2xl p-5">
            <h2 class="text-sm font-semibold text-gray-300 mb-4 flex items-center gap-2">
              <span>⏱</span> 时间线
              <span class="text-xs text-gray-600 font-normal">（最新在上）</span>
            </h2>

            <div v-if="!historyWithDuration.length" class="text-gray-600 text-sm text-center py-6">无记录</div>

            <div class="relative pl-4">
              <!-- Vertical line -->
              <div class="absolute left-1.5 top-2 bottom-2 border-l border-gray-700"></div>

              <div v-for="h in historyWithDuration" :key="h.id" class="mb-4 relative">
                <!-- Dot -->
                <div
                  class="absolute -left-4 top-1 w-2.5 h-2.5 rounded-full border-2 border-gray-900"
                  :class="historyDot(h.to_status)"
                />
                <div class="flex items-center gap-2 mb-0.5">
                  <span class="text-xs text-gray-600">{{ formatTime(h.changed_at) }}</span>
                  <!-- V23-B: duration badge -->
                  <span
                    v-if="h.duration"
                    class="text-[10px] text-gray-500 bg-gray-800 border border-gray-700 rounded px-1 font-mono"
                  >{{ h.duration }}</span>
                </div>
                <div class="text-sm">
                  <span v-if="h.from_status" class="text-gray-500 text-xs">{{ h.from_status }} → </span>
                  <span class="font-medium" :class="historyStatusColor(h.to_status)">{{ h.to_status }}</span>
                  <span v-if="h.changed_by" class="text-gray-600 text-xs ml-1">by {{ h.changed_by }}</span>
                </div>
                <div v-if="h.note" class="text-xs text-gray-500 mt-0.5">{{ h.note }}</div>
              </div>
            </div>
          </div>
        </div>
      </div>

      <!-- V24-B: Comments section (full width below the grid) -->
      <div v-if="task" class="mt-6 bg-gray-900 border border-gray-700 rounded-2xl p-5">
        <h2 class="text-sm font-semibold text-gray-300 mb-4 flex items-center gap-2">
          💬 评论
          <span class="text-xs text-gray-600 font-normal">{{ comments.length }} 条</span>
        </h2>

        <!-- Comment list -->
        <div v-if="commentLoading" class="text-gray-600 text-sm py-4 text-center">加载中…</div>
        <div v-else-if="comments.length === 0" class="text-gray-600 text-sm text-center py-4">暂无评论</div>
        <div v-else class="space-y-3 mb-4">
          <div
            v-for="c in comments"
            :key="c.id"
            class="flex gap-3"
          >
            <div class="w-7 h-7 rounded-full bg-gray-700 flex items-center justify-center text-xs text-gray-400 shrink-0 mt-0.5">
              {{ (c.author || '?')[0].toUpperCase() }}
            </div>
            <div class="flex-1 min-w-0">
              <div class="flex items-center gap-2 mb-1">
                <span class="text-xs font-medium text-gray-300">{{ c.author || 'anonymous' }}</span>
                <span class="text-xs text-gray-600">{{ formatTime(c.created_at) }}</span>
              </div>
              <div class="text-sm text-gray-400 bg-gray-800 rounded-lg px-3 py-2 border border-gray-700/50 whitespace-pre-wrap break-words">{{ c.content }}</div>
            </div>
          </div>
        </div>

        <!-- Comment input -->
        <div class="border-t border-gray-800 pt-4 space-y-2">
          <div class="flex gap-2">
            <input
              v-model="newCommentAuthor"
              type="text"
              placeholder="作者（可选）"
              class="w-28 bg-gray-800 border border-gray-700 rounded-lg px-3 py-2 text-sm text-gray-200 placeholder-gray-600 focus:outline-none focus:border-blue-500/50"
            />
            <textarea
              v-model="newCommentContent"
              placeholder="写下评论…"
              rows="2"
              class="flex-1 bg-gray-800 border border-gray-700 rounded-lg px-3 py-2 text-sm text-gray-200 placeholder-gray-600 focus:outline-none focus:border-blue-500/50 resize-none"
              @keydown.ctrl.enter="submitComment"
            />
          </div>
          <div v-if="commentError" class="text-xs text-red-400">{{ commentError }}</div>
          <div class="flex justify-end">
            <button
              class="text-sm px-4 py-1.5 bg-blue-600 hover:bg-blue-500 text-white rounded-lg disabled:opacity-50 transition-colors"
              :disabled="commentSubmitting || !newCommentContent.trim()"
              @click="submitComment"
            >
              <span v-if="commentSubmitting">发送中…</span>
              <span v-else>💬 发送</span>
            </button>
          </div>
        </div>
      </div>
    </div>
  </AppLayout>
</template>
