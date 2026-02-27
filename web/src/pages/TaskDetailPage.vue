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
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    loading.value = false
  }
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
            </div>

            <!-- Description -->
            <div v-if="task.description" class="mb-4">
              <div class="text-xs text-gray-500 mb-1.5">描述</div>
              <div class="bg-gray-800 rounded-lg p-3 text-sm text-gray-300 whitespace-pre-wrap leading-relaxed border border-gray-700/50">{{ task.description }}</div>
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
        <div class="col-span-2">
          <div class="bg-gray-900 border border-gray-700 rounded-2xl p-5">
            <h2 class="text-sm font-semibold text-gray-300 mb-4 flex items-center gap-2">
              <span>⏱</span> 时间线
              <span class="text-xs text-gray-600 font-normal">（最新在上）</span>
            </h2>

            <div v-if="!history.length" class="text-gray-600 text-sm text-center py-6">无记录</div>

            <div class="relative pl-4">
              <!-- Vertical line -->
              <div class="absolute left-1.5 top-2 bottom-2 border-l border-gray-700"></div>

              <div v-for="h in history" :key="h.id" class="mb-4 relative">
                <!-- Dot -->
                <div
                  class="absolute -left-4 top-1 w-2.5 h-2.5 rounded-full border-2 border-gray-900"
                  :class="historyDot(h.to_status)"
                />
                <div class="text-xs text-gray-600 mb-0.5">{{ formatTime(h.changed_at) }}</div>
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
    </div>
  </AppLayout>
</template>
