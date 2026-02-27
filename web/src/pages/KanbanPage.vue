<script setup lang="ts">
import { ref, computed } from 'vue'
import AppLayout from '@/layouts/AppLayout.vue'
import { usePolling } from '@/composables/usePolling'
import { useSSE } from '@/composables/useSSE'
import { api } from '@/api/client'
import type { Task, TaskStatus } from '@/types'

const allTasks = ref<Task[]>([])

async function fetchTasks() {
  const resp = await api.listTasks({ limit: 500 })
  allTasks.value = resp.tasks ?? []
}

const { loading, error, refresh } = usePolling(fetchTasks, 60_000)

// V17: SSE real-time updates — refresh board on any task event.
const { connected: sseConnected } = useSSE(() => fetchTasks(), { fallbackInterval: 60_000 })

const columns: { key: TaskStatus; label: string; color: string }[] = [
  { key: 'pending', label: '待处理', color: 'text-yellow-400' },
  { key: 'claimed', label: '已认领', color: 'text-cyan-400' },
  { key: 'in_progress', label: '进行中', color: 'text-blue-400' },
  { key: 'review', label: '审核中', color: 'text-purple-400' },
  { key: 'done', label: '完成', color: 'text-green-400' },
  { key: 'blocked', label: '阻塞', color: 'text-orange-400' },
  { key: 'failed', label: '失败', color: 'text-red-400' },
]

const tasksByStatus = computed(() => {
  const map: Partial<Record<TaskStatus, Task[]>> = {}
  for (const col of columns) {
    map[col.key] = allTasks.value.filter((t) => t.status === col.key)
  }
  return map
})

function relativeTime(iso: string): string {
  const diff = Date.now() - new Date(iso).getTime()
  const m = Math.floor(diff / 60_000)
  if (m < 1) return '刚刚'
  if (m < 60) return `${m}分前`
  const h = Math.floor(m / 60)
  if (h < 24) return `${h}时前`
  return `${Math.floor(h / 24)}天前`
}

function agentBadge(assignedTo: string): string {
  const isHuman = assignedTo === 'human'
  return isHuman
    ? 'bg-amber-500/20 text-amber-400'
    : 'bg-gray-700 text-gray-400'
}

function isPendingApproval(task: Task): boolean {
  return task.assigned_to === 'human'
}

function cardClass(task: Task): string {
  if (isPendingApproval(task)) {
    return 'bg-gray-800 border border-amber-500/50 rounded-lg p-3 cursor-pointer hover:border-amber-400 transition-colors ring-1 ring-amber-500/20'
  }
  return 'bg-gray-800 hover:bg-gray-750 border border-gray-700/40 rounded-lg p-3 cursor-pointer hover:border-gray-600 transition-colors'
}
</script>

<template>
  <AppLayout>
    <div class="p-6">
      <div class="flex items-center justify-between mb-6">
        <div>
          <h1 class="text-xl font-bold text-gray-100">📋 看板</h1>
          <p class="text-gray-500 text-sm mt-1">全局任务审计视图</p>
        </div>
        <div class="flex items-center gap-3">
          <span
            class="text-xs"
            :class="sseConnected ? 'text-green-500' : 'text-gray-600'"
            :title="sseConnected ? 'SSE 实时连接' : 'SSE 未连接（轮询兜底）'"
          >{{ sseConnected ? '🟢 实时' : '⚪ 轮询' }}</span>
          <button
            class="text-sm text-gray-500 hover:text-gray-300 disabled:opacity-40"
            :disabled="loading"
            @click="refresh"
          >⟳ 刷新</button>
        </div>
      </div>

      <div v-if="error" class="mb-4 p-3 bg-red-900/40 border border-red-500 rounded text-sm text-red-300">{{ error }}</div>
      <div v-if="loading && !allTasks.length" class="text-gray-600 text-center py-20">加载中…</div>

      <div v-else class="flex gap-3 overflow-x-auto pb-4">
        <div
          v-for="col in columns"
          :key="col.key"
          class="bg-gray-900 border border-gray-800 rounded-xl flex-shrink-0 w-56 flex flex-col"
        >
          <!-- Column header -->
          <div class="px-3 py-3 border-b border-gray-800 flex items-center justify-between">
            <span class="text-xs font-semibold" :class="col.color">{{ col.label }}</span>
            <span class="text-xs text-gray-600 bg-gray-800 px-1.5 py-0.5 rounded-full">
              {{ tasksByStatus[col.key]?.length ?? 0 }}
            </span>
          </div>

          <!-- Tasks -->
          <div class="flex-1 overflow-y-auto p-2 space-y-2 max-h-[calc(100vh-200px)]">
            <div
              v-for="task in tasksByStatus[col.key]"
              :key="task.id"
              :class="cardClass(task)"
              @click="$router.push(`/tasks/${task.id}`)"
            >
              <!-- Human task badge -->
              <div v-if="isPendingApproval(task)" class="flex items-center gap-1 mb-1.5">
                <span class="text-xs text-amber-400 font-semibold">👤 人工任务</span>
              </div>
              <div class="text-xs font-medium text-gray-200 leading-snug mb-2 line-clamp-2">
                {{ task.title }}
              </div>
              <div class="flex items-center justify-between gap-1">
                <span
                  class="text-xs px-1.5 py-0.5 rounded-md"
                  :class="agentBadge(task.assigned_to)"
                >
                  {{ task.assigned_to === 'human' ? '👤' : '🤖' }} {{ task.assigned_to }}
                </span>
                <span class="text-xs text-gray-600">{{ relativeTime(task.updated_at) }}</span>
              </div>
            </div>
            <div v-if="!tasksByStatus[col.key]?.length" class="text-center py-6 text-gray-700 text-xs">
              空
            </div>
          </div>
        </div>
      </div>
    </div>
  </AppLayout>
</template>
