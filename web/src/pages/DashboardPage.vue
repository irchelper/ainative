<script setup lang="ts">
import { computed, ref } from 'vue'
import AppLayout from '@/layouts/AppLayout.vue'
import { useDashboardStore } from '@/stores/dashboardStore'
import { usePolling } from '@/composables/usePolling'
import { useSSE } from '@/composables/useSSE'
import { api } from '@/api/client'
import type { Task } from '@/types'

const store = useDashboardStore()
const { loading, error, refresh } = usePolling(() => store.fetch(), 10_000)

// V17: SSE real-time updates — refresh dashboard on any task event.
useSSE(() => store.fetch(), { fallbackInterval: 10_000 })

// Human todo tasks (assigned_to === 'human')
const humanTodos = computed(() =>
  (store.data?.todo ?? []).filter((t) => t.assigned_to === 'human'),
)
const exceptions = computed(() => store.data?.exceptions ?? [])
const todoCount = computed(() => humanTodos.value.length)
const exceptionCount = computed(() => exceptions.value.length)

const actingIds = ref<Set<string>>(new Set())

// Countdown helper: returns remaining time string or null
function countdownStr(task: Task): string | null {
  if (!task.timeout_minutes || !task.created_at) return null
  const createdMs = new Date(task.created_at).getTime()
  const deadlineMs = createdMs + task.timeout_minutes * 60_000
  const remainMs = deadlineMs - Date.now()
  if (remainMs <= 0) return '⏰ 已超时'
  const totalSec = Math.floor(remainMs / 1000)
  const h = Math.floor(totalSec / 3600)
  const m = Math.floor((totalSec % 3600) / 60)
  const s = totalSec % 60
  if (h > 0) return `${h}:${String(m).padStart(2, '0')}:${String(s).padStart(2, '0')}`
  return `${String(m).padStart(2, '0')}:${String(s).padStart(2, '0')}`
}

function countdownColor(task: Task): string {
  if (!task.timeout_minutes || !task.created_at) return 'text-gray-400'
  const createdMs = new Date(task.created_at).getTime()
  const deadlineMs = createdMs + task.timeout_minutes * 60_000
  const remainRatio = (deadlineMs - Date.now()) / (task.timeout_minutes * 60_000)
  if (remainRatio < 0) return 'text-red-400'
  if (remainRatio < 0.2) return 'text-red-400 animate-pulse'
  if (remainRatio < 0.4) return 'text-amber-400'
  return 'text-gray-400'
}

async function patchTask(id: string, data: Record<string, unknown>) {
  actingIds.value.add(id)
  try {
    await api.patchTask(id, data)
    await store.fetch()
  } finally {
    actingIds.value.delete(id)
  }
}

async function completeTodo(task: Task) {
  // need to advance through FSM: pending → claimed → in_progress → done
  // But human tasks may already be in_progress. Handle gracefully.
  let version = task.version
  try {
    if (task.status === 'pending') {
      const r = await api.patchTask(task.id, { status: 'claimed', version, agent: 'human' })
      version = r.task.version
      const r2 = await api.patchTask(task.id, { status: 'in_progress', version })
      version = r2.task.version
    }
    await patchTask(task.id, { status: 'done', result: 'human-completed', version })
  } catch {
    await store.fetch()
  }
}

async function skipTodo(task: Task) {
  let version = task.version
  try {
    if (task.status === 'pending') {
      const r = await api.patchTask(task.id, { status: 'claimed', version, agent: 'human' })
      version = r.task.version
      const r2 = await api.patchTask(task.id, { status: 'in_progress', version })
      version = r2.task.version
    }
    await patchTask(task.id, { status: 'done', result: 'human timeout: skipped', timeout_action: 'skip', version })
  } catch {
    await store.fetch()
  }
}

function statusBadgeClass(status: string): string {
  const map: Record<string, string> = {
    failed: 'bg-red-500/20 text-red-400 border-red-500/30',
    blocked: 'bg-orange-500/20 text-orange-400 border-orange-500/30',
  }
  return map[status] ?? 'bg-gray-700 text-gray-400'
}
</script>

<template>
  <AppLayout>
    <!-- Error banner -->
    <div
      v-if="error"
      class="mx-6 mt-4 p-3 bg-red-900/40 border border-red-500 rounded text-red-300 text-sm flex items-center justify-between"
    >
      <span>{{ error }}</span>
      <button class="text-xs underline ml-4" @click="refresh">重试</button>
    </div>

    <div class="grid grid-cols-2 gap-0 h-full">
      <!-- Left: Human Todo -->
      <div class="border-r border-gray-800 flex flex-col">
        <div class="px-6 py-4 border-b border-gray-800 flex items-center justify-between">
          <div class="flex items-center gap-2">
            <span class="text-lg">🙋</span>
            <h2 class="font-semibold text-gray-100">我的待办</h2>
            <span
              v-if="todoCount > 0"
              class="bg-amber-500 text-gray-900 text-xs font-bold px-1.5 py-0.5 rounded-full"
            >{{ todoCount }}</span>
          </div>
          <div class="flex items-center gap-3">
            <span class="text-xs text-gray-600">10s 自动刷新</span>
            <button
              class="text-xs text-gray-500 hover:text-gray-300"
              :disabled="loading"
              @click="refresh"
            >⟳</button>
          </div>
        </div>
        <div class="flex-1 overflow-y-auto p-4 space-y-3">
          <div v-if="loading && !store.data" class="text-center text-gray-600 py-12">加载中…</div>
          <div v-else-if="humanTodos.length === 0" class="text-center text-gray-600 py-12">
            <div class="text-4xl mb-3">✨</div>
            <div class="text-sm">暂无待办</div>
          </div>

          <!-- Todo Card -->
          <div
            v-for="task in humanTodos"
            :key="task.id"
            class="bg-gray-900 border rounded-xl p-4 transition-colors"
            :class="countdownStr(task)?.includes('超时')
              ? 'border-red-500/50 hover:border-red-500/70'
              : 'border-gray-700/60 hover:border-amber-500/40'"
          >
            <div class="flex items-start justify-between gap-3 mb-3">
              <div class="flex-1 min-w-0">
                <div class="flex items-center gap-2 mb-1 flex-wrap">
                  <span class="text-xs font-medium bg-amber-500/15 text-amber-400 border border-amber-500/20 px-2 py-0.5 rounded-full whitespace-nowrap">
                    👤 人工节点
                  </span>
                  <span v-if="task.chain_id" class="text-xs text-gray-600 truncate">
                    链路 {{ task.chain_id.slice(-8) }}
                  </span>
                </div>
                <h3 class="font-medium text-gray-100 text-sm leading-snug">{{ task.title }}</h3>
              </div>
              <!-- Countdown -->
              <div v-if="task.timeout_minutes" class="text-right shrink-0">
                <div class="font-mono font-bold text-sm" :class="countdownColor(task)">
                  {{ countdownStr(task) }}
                </div>
                <div class="text-xs text-gray-600 mt-0.5">剩余</div>
              </div>
            </div>

            <!-- Description preview -->
            <div
              v-if="task.description"
              class="bg-gray-800/60 rounded-lg p-3 mb-3 border border-gray-700/50"
            >
              <div class="text-xs text-gray-500 mb-1.5 font-medium">📄 前置摘要</div>
              <p class="text-xs text-gray-300 leading-relaxed line-clamp-3">{{ task.description }}</p>
            </div>

            <!-- Actions -->
            <div class="flex gap-2">
              <button
                class="flex-1 flex items-center justify-center gap-1.5 bg-green-600 hover:bg-green-500 text-white text-xs font-medium py-2 rounded-lg transition-colors disabled:opacity-50"
                :disabled="actingIds.has(task.id)"
                @click="completeTodo(task)"
              >✅ 完成</button>
              <button
                class="px-3 flex items-center justify-center gap-1.5 bg-gray-700 hover:bg-gray-600 text-gray-300 text-xs font-medium py-2 rounded-lg transition-colors disabled:opacity-50"
                :disabled="actingIds.has(task.id)"
                @click="skipTodo(task)"
              >⏭ 跳过</button>
            </div>
          </div>
        </div>
      </div>

      <!-- Right: Exception Panel -->
      <div class="flex flex-col">
        <div class="px-6 py-4 border-b border-gray-800 flex items-center justify-between">
          <div class="flex items-center gap-2">
            <span class="text-lg">🔴</span>
            <h2 class="font-semibold text-gray-100">异常面板</h2>
            <span
              v-if="exceptionCount > 0"
              class="bg-red-500 text-white text-xs font-bold px-1.5 py-0.5 rounded-full"
            >{{ exceptionCount }}</span>
          </div>
        </div>
        <div class="flex-1 overflow-y-auto p-4 space-y-3">
          <div v-if="exceptions.length === 0" class="text-center text-gray-600 py-12">
            <div class="text-4xl mb-3">✅</div>
            <div class="text-sm">无异常任务</div>
          </div>

          <div
            v-for="task in exceptions"
            :key="task.id"
            class="bg-gray-900 border border-gray-700/60 rounded-xl p-4 hover:border-red-500/30 transition-colors cursor-pointer"
            @click="$router.push(`/tasks/${task.id}`)"
          >
            <div class="flex items-start justify-between gap-3 mb-2">
              <div class="flex-1 min-w-0">
                <h3 class="font-medium text-gray-100 text-sm mb-1.5 truncate">{{ task.title }}</h3>
                <div class="flex items-center gap-2 flex-wrap">
                  <span
                    class="text-xs font-medium px-2 py-0.5 rounded-full border"
                    :class="statusBadgeClass(task.status)"
                  >{{ task.status }}</span>
                  <span class="text-xs text-gray-500">{{ task.assigned_to }}</span>
                </div>
              </div>
              <span class="text-gray-600 text-sm shrink-0">›</span>
            </div>
            <div
              v-if="task.failure_reason || task.result"
              class="text-xs text-gray-400 bg-gray-800/60 rounded-lg p-2.5 line-clamp-2 border border-gray-700/40"
            >
              {{ task.failure_reason || task.result }}
            </div>
            <div class="flex gap-2 mt-3">
              <button
                class="flex-1 text-xs py-1.5 rounded-lg bg-gray-700 hover:bg-gray-600 text-gray-300 transition-colors disabled:opacity-50"
                :disabled="actingIds.has(task.id)"
                @click.stop="patchTask(task.id, { status: 'pending', version: task.version })"
              >🔄 重试</button>
              <button
                class="flex-1 text-xs py-1.5 rounded-lg bg-gray-700 hover:bg-red-900 text-gray-300 hover:text-red-300 transition-colors disabled:opacity-50"
                :disabled="actingIds.has(task.id)"
                @click.stop="patchTask(task.id, { status: 'cancelled', version: task.version })"
              >✖ 取消</button>
            </div>
          </div>
        </div>
      </div>
    </div>
  </AppLayout>
</template>
