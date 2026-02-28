<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/layouts/AppLayout.vue'
import KeyboardHelpModal from '@/components/KeyboardHelpModal.vue'
import { useDashboardStore } from '@/stores/dashboardStore'
import { usePolling } from '@/composables/usePolling'
import { useSSE } from '@/composables/useSSE'
import { useExport } from '@/composables/useExport'
import { useKeyboardNav } from '@/composables/useKeyboardNav'
import { api } from '@/api/client'
import type { Task } from '@/types'

const { t } = useI18n()
const { exportTasks } = useExport()

const store = useDashboardStore()
const { loading, error, refresh } = usePolling(() => store.fetch(), 10_000)

// V17: SSE real-time updates — refresh dashboard on any task event.
useSSE(() => store.fetch(), { fallbackInterval: 10_000 })

// V21: Search
const searchQuery = ref('')
const searchResults = ref<Task[] | null>(null)
const searching = ref(false)
let searchTimer: ReturnType<typeof setTimeout> | null = null

async function doSearch(q: string) {
  if (!q.trim()) {
    searchResults.value = null
    return
  }
  searching.value = true
  try {
    const resp = await api.listTasks({ limit: 100 })
    // Client-side filter using already-fetched data from /tasks endpoint
    const all = resp.tasks ?? []
    const lower = q.toLowerCase()
    searchResults.value = all.filter(
      (t) =>
        t.title.toLowerCase().includes(lower) ||
        (t.description ?? '').toLowerCase().includes(lower),
    )
  } catch {
    searchResults.value = null
  } finally {
    searching.value = false
  }
}

watch(searchQuery, (q) => {
  if (searchTimer) clearTimeout(searchTimer)
  if (!q.trim()) {
    searchResults.value = null
    return
  }
  searchTimer = setTimeout(() => doSearch(q), 300)
})

// Todo tasks: show active tasks (claimed or in_progress) assigned to human
// requires_review filter removed — most tasks don't set this flag, causing empty panel
const humanTodos = computed(() =>
  (store.data?.todo ?? []).filter(
    (t) => t.status === 'claimed' || t.status === 'in_progress',
  ),
)
const exceptions = computed(() => store.data?.exceptions ?? [])
const todoCount = computed(() => humanTodos.value.length)
const exceptionCount = computed(() => exceptions.value.length)

// V29b TASK-B: classify exceptions into retryable vs needsHuman
function isRetryable(task: Task): boolean {
  const r = task.failure_reason ?? task.result ?? ''
  return r.startsWith('agent_timeout') || r.startsWith('stale max')
}

const retryableExceptions = computed(() => exceptions.value.filter(isRetryable))
const needsHumanExceptions = computed(() => exceptions.value.filter(t => !isRetryable(t)))

// V29b TASK-B: agent pill filter + counts
const agentFilter = ref<string | null>(null)

const agentCounts = computed(() => {
  const m: Record<string, number> = {}
  for (const t of exceptions.value) {
    if (t.status === 'failed') {
      m[t.assigned_to] = (m[t.assigned_to] ?? 0) + 1
    }
  }
  return Object.entries(m).sort((a, b) => b[1] - a[1])
})

function filteredExceptions(list: Task[]): Task[] {
  if (!agentFilter.value) return list
  return list.filter(t => t.assigned_to === agentFilter.value)
}

// V29b TASK-C: expand/collapse failure_reason per task
const expandedReasons = ref<Set<string>>(new Set())
function toggleReason(id: string) {
  const s = new Set(expandedReasons.value)
  if (s.has(id)) s.delete(id)
  else s.add(id)
  expandedReasons.value = s
}

const actingIds = ref<Set<string>>(new Set())

// V26-A: keyboard navigation on exception list
const { selectedIndex: kbIndex, showHelp: showKbHelp } = useKeyboardNav(
  () => exceptions.value,
  async (id, data) => { await patchTask(id, data) }
)

// V26-A: export format dropdown
const exportMenuOpen = ref(false)
const allVisibleTasks = computed(() => [
  ...(store.data?.todo ?? []),
  ...exceptions.value,
])

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

// V29b TASK-C: retry with confirm
async function retryWithConfirm(task: Task) {
  if (!window.confirm(`确认重试任务：${task.title}?`)) return
  await patchTask(task.id, { status: 'pending', version: task.version })
}

// V22: Bulk operations
const selectedIds = ref<Set<string>>(new Set())
const bulkLoading = ref(false)
const bulkError = ref<string | null>(null)
const reassignTarget = ref('')

function toggleSelect(id: string) {
  const s = new Set(selectedIds.value)
  if (s.has(id)) s.delete(id)
  else s.add(id)
  selectedIds.value = s
}

function clearSelection() {
  selectedIds.value = new Set()
  reassignTarget.value = ''
  bulkError.value = null
}

// V29b TASK-C: select all visible exceptions
function selectAll() {
  const all = exceptions.value
  if (selectedIds.value.size === all.length) {
    selectedIds.value = new Set()
  } else {
    selectedIds.value = new Set(all.map(t => t.id))
  }
}

// V29b TASK-C: bulk retry (PATCH each selected to pending)
async function bulkRetry() {
  if (!selectedIds.value.size) return
  if (!window.confirm(`确认批量重试 ${selectedIds.value.size} 个任务？`)) return
  bulkLoading.value = true
  bulkError.value = null
  const ids = Array.from(selectedIds.value)
  const failedIds: string[] = []
  for (const id of ids) {
    const task = exceptions.value.find(t => t.id === id)
    if (!task) continue
    try {
      await api.patchTask(id, { status: 'pending', version: task.version })
    } catch {
      failedIds.push(id)
    }
  }
  if (failedIds.length > 0) bulkError.value = `${failedIds.length} 个任务重试失败`
  clearSelection()
  await store.fetch()
  bulkLoading.value = false
}

async function bulkAction(action: 'cancel' | 'reassign') {
  if (!selectedIds.value.size) return
  if (action === 'reassign' && !reassignTarget.value.trim()) {
    bulkError.value = t('dashboard.agentName') + ' is required'
    return
  }
  bulkLoading.value = true
  bulkError.value = null
  try {
    const body: Record<string, unknown> = {
      action,
      task_ids: Array.from(selectedIds.value),
    }
    if (action === 'reassign') body.assigned_to = reassignTarget.value.trim()
    const resp = await fetch('/api/tasks/bulk', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    })
    const data = await resp.json()
    if (!resp.ok) throw new Error(data.error ?? `HTTP ${resp.status}`)
    if (data.failed > 0) {
      bulkError.value = `${data.failed} 个任务操作失败`
    }
    clearSelection()
    await store.fetch()
  } catch (e) {
    bulkError.value = e instanceof Error ? e.message : String(e)
  } finally {
    bulkLoading.value = false
  }
}
</script>

<template>
  <AppLayout>
    <!-- V26-A: keyboard help modal -->
    <KeyboardHelpModal :show="showKbHelp" @close="showKbHelp = false" />

    <!-- Error banner -->
    <div
      v-if="error"
      class="mx-6 mt-4 p-3 bg-red-900/40 border border-red-500 rounded text-red-300 text-sm flex items-center justify-between"
    >
      <span>{{ error }}</span>
      <button class="text-xs underline ml-4" @click="refresh">重试</button>
    </div>

    <!-- V21: Search bar -->
    <div class="mx-6 mt-4 mb-0 relative">
      <div class="flex items-center gap-2 bg-gray-900 border border-gray-700 focus-within:border-blue-500/50 rounded-xl px-3 py-2 transition-colors">
        <span class="text-gray-500 text-sm">🔍</span>
        <input
          v-model="searchQuery"
          type="text"
          :placeholder="t('dashboard.searchPlaceholder')"
          class="flex-1 bg-transparent text-sm text-gray-200 placeholder-gray-600 focus:outline-none"
        />
        <button
          v-if="searchQuery"
          class="text-gray-500 hover:text-gray-300 text-xs"
          @click="searchQuery = ''"
        >✕</button>
        <span v-if="searching" class="text-gray-600 text-xs">…</span>
      </div>

      <!-- Search results overlay -->
      <div
        v-if="searchResults !== null"
        class="absolute left-0 right-0 top-full mt-1 bg-gray-900 border border-gray-700 rounded-xl shadow-2xl z-50 max-h-80 overflow-y-auto"
      >
        <div v-if="!searchResults.length" class="px-4 py-6 text-center text-gray-600 text-sm">{{ t('dashboard.noResults') }}</div>
        <div
          v-for="task in searchResults"
          :key="task.id"
          class="flex items-start gap-3 px-4 py-3 hover:bg-gray-800 cursor-pointer border-b border-gray-800 last:border-0 transition-colors"
          @click="$router.push(`/tasks/${task.id}`); searchQuery = ''"
        >
          <span
            class="text-xs px-1.5 py-0.5 rounded border mt-0.5 shrink-0"
            :class="{
              'text-green-400 border-green-500/30 bg-green-500/10': task.status === 'done',
              'text-red-400 border-red-500/30 bg-red-500/10': task.status === 'failed',
              'text-blue-400 border-blue-500/30 bg-blue-500/10': task.status === 'in_progress',
              'text-gray-400 border-gray-600 bg-gray-700/40': !['done','failed','in_progress'].includes(task.status),
            }"
          >{{ task.status }}</span>
          <div class="flex-1 min-w-0">
            <div class="text-sm text-gray-200 truncate">{{ task.title }}</div>
            <div class="text-xs text-gray-500 mt-0.5">{{ task.assigned_to }}</div>
          </div>
        </div>
        <div class="px-4 py-2 text-xs text-gray-600 border-t border-gray-800">
          共 {{ searchResults.length }} 个结果
        </div>
      </div>
    </div>

    <div class="grid grid-cols-1 md:grid-cols-2 gap-0">
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
          <div v-if="loading && !store.data" class="text-center text-gray-600 py-12">{{ t('common.loading') }}</div>
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
          <!-- V26-A: export dropdown -->
          <div class="relative flex items-center gap-2">
            <button
              class="text-xs text-gray-500 hover:text-white px-1 py-1"
              title="键盘快捷键 (?)"
              @click="showKbHelp = !showKbHelp"
            >⌨️</button>
            <div class="relative">
              <button
                class="text-xs bg-gray-800 border border-gray-700 text-gray-400 hover:text-white hover:border-gray-500 px-2.5 py-1 rounded-lg flex items-center gap-1 transition-colors"
                @click="exportMenuOpen = !exportMenuOpen"
              >⬇ 导出</button>
              <div
                v-if="exportMenuOpen"
                class="absolute right-0 top-8 bg-gray-900 border border-gray-700 rounded-lg shadow-xl z-10 py-1 w-32"
              >
                <button
                  class="w-full text-left px-3 py-1.5 text-xs text-gray-300 hover:bg-gray-800 transition-colors"
                  @click="exportTasks(allVisibleTasks, 'csv'); exportMenuOpen = false"
                >📄 导出 CSV</button>
                <button
                  class="w-full text-left px-3 py-1.5 text-xs text-gray-300 hover:bg-gray-800 transition-colors"
                  @click="exportTasks(allVisibleTasks, 'json'); exportMenuOpen = false"
                >📦 导出 JSON</button>
              </div>
            </div>
          </div>
        </div>

        <!-- V29b TASK-B: Agent pill filter bar -->
        <div v-if="agentCounts.length > 0" class="px-4 pt-3 pb-0 flex items-center gap-2 flex-wrap border-b border-gray-800/50 pb-3">
          <button
            class="text-xs px-2.5 py-1 rounded-full border transition-colors"
            :class="agentFilter === null ? 'bg-blue-600 border-blue-500 text-white' : 'bg-gray-800 border-gray-700 text-gray-400 hover:border-gray-500'"
            @click="agentFilter = null"
          >全部 ({{ exceptionCount }})</button>
          <button
            v-for="[agent, count] in agentCounts"
            :key="agent"
            class="text-xs px-2.5 py-1 rounded-full border transition-colors"
            :class="agentFilter === agent ? 'bg-blue-600 border-blue-500 text-white' : 'bg-gray-800 border-gray-700 text-gray-400 hover:border-gray-500'"
            @click="agentFilter = agentFilter === agent ? null : agent"
          >{{ agent }} ({{ count }})</button>
        </div>

        <!-- V22/V29b: Bulk toolbar -->
        <div
          class="px-4 py-2.5 border-b border-gray-800 flex items-center gap-2 flex-wrap"
          :class="selectedIds.size > 0 ? 'bg-blue-950/40 border-blue-500/20' : ''"
        >
          <!-- V29b TASK-C: select-all checkbox -->
          <input
            type="checkbox"
            class="h-4 w-4 rounded border-gray-600 bg-gray-800 accent-blue-500 cursor-pointer"
            :checked="selectedIds.size > 0 && selectedIds.size === exceptions.length"
            :indeterminate="selectedIds.size > 0 && selectedIds.size < exceptions.length"
            @change="selectAll()"
          />
          <span v-if="selectedIds.size > 0" class="text-xs text-blue-400 font-medium">{{ t('dashboard.selected', { n: selectedIds.size }) }}</span>
          <span v-else class="text-xs text-gray-600">全选</span>
          <template v-if="selectedIds.size > 0">
            <!-- V29b TASK-C: bulk retry -->
            <button
              class="text-xs px-3 py-1.5 bg-green-800/60 hover:bg-green-700/60 text-green-300 rounded-lg transition-colors disabled:opacity-50"
              :disabled="bulkLoading"
              @click="bulkRetry()"
            >🔄 批量重试</button>
            <button
              class="text-xs px-3 py-1.5 bg-red-800/60 hover:bg-red-700/60 text-red-300 rounded-lg transition-colors disabled:opacity-50"
              :disabled="bulkLoading"
              @click="bulkAction('cancel')"
            >✖ {{ t('dashboard.bulkCancel') }}</button>
            <div class="flex items-center gap-1.5">
              <input
                v-model="reassignTarget"
                type="text"
                :placeholder="t('dashboard.agentName') + '…'"
                class="text-xs bg-gray-800 border border-gray-600 rounded-lg px-2 py-1.5 text-gray-200 w-24 focus:outline-none focus:border-blue-500"
              />
              <button
                class="text-xs px-3 py-1.5 bg-blue-800/60 hover:bg-blue-700/60 text-blue-300 rounded-lg transition-colors disabled:opacity-50"
                :disabled="bulkLoading || !reassignTarget.trim()"
                @click="bulkAction('reassign')"
              >↩ {{ t('dashboard.bulkReassign') }}</button>
            </div>
            <button
              class="text-xs text-gray-500 hover:text-gray-300 ml-auto"
              @click="clearSelection"
            >✕ {{ t('dashboard.clearSelection') }}</button>
          </template>
          <div v-if="bulkError" class="w-full text-xs text-red-400">{{ bulkError }}</div>
        </div>

        <div class="flex-1 overflow-y-auto p-4 space-y-3">
          <div v-if="exceptions.length === 0" class="text-center text-gray-600 py-12">
            <div class="text-4xl mb-3">✅</div>
            <div class="text-sm">{{ t('dashboard.noExceptions') }}</div>
          </div>

          <!-- V29b TASK-B: 🔧 可重试 section -->
          <template v-if="filteredExceptions(retryableExceptions).length > 0">
            <div class="text-xs text-gray-500 font-semibold px-1 mt-2 mb-1">
              🔧 可重试 ({{ filteredExceptions(retryableExceptions).length }})
            </div>
            <div
              v-for="(task, idx) in filteredExceptions(retryableExceptions)"
              :key="task.id"
              :data-keyboard-index="idx"
              class="bg-gray-800/50 border rounded-xl p-4 transition-colors cursor-pointer"
              :class="[
                selectedIds.has(task.id) ? 'border-blue-500/50 bg-blue-500/5' : 'border-gray-700/40 hover:border-gray-600',
                kbIndex === idx ? 'ring-2 ring-blue-500' : '',
              ]"
              @click="$router.push(`/tasks/${task.id}`)"
            >
              <div class="flex items-start gap-3 mb-2">
                <input type="checkbox" class="mt-0.5 h-4 w-4 rounded border-gray-600 bg-gray-800 accent-blue-500 cursor-pointer shrink-0"
                  :checked="selectedIds.has(task.id)" @click.stop="toggleSelect(task.id)" />
                <div class="flex-1 min-w-0">
                  <h3 class="font-medium text-gray-200 text-sm mb-1.5 truncate">{{ task.title }}</h3>
                  <div class="flex items-center gap-2 flex-wrap">
                    <span class="text-xs font-medium px-2 py-0.5 rounded-full border" :class="statusBadgeClass(task.status)">{{ task.status }}</span>
                    <span class="text-xs text-gray-500">{{ task.assigned_to }}</span>
                  </div>
                </div>
                <span class="text-gray-600 text-sm shrink-0">›</span>
              </div>
              <div v-if="task.failure_reason || task.result" class="mb-2">
                <div class="text-xs text-gray-500 bg-gray-800/60 rounded-lg p-2.5 border border-gray-700/40"
                  :class="expandedReasons.has(task.id) ? '' : 'line-clamp-2'">
                  {{ task.failure_reason || task.result }}
                </div>
                <button class="text-xs text-gray-600 hover:text-gray-400 mt-1" @click.stop="toggleReason(task.id)">
                  {{ expandedReasons.has(task.id) ? '收起' : '展开' }}
                </button>
              </div>
              <div class="flex gap-2">
                <button class="flex-1 text-xs py-1.5 rounded-lg bg-gray-700 hover:bg-gray-600 text-gray-300 transition-colors disabled:opacity-50"
                  :disabled="actingIds.has(task.id)" @click.stop="retryWithConfirm(task)">🔄 重试</button>
                <button class="flex-1 text-xs py-1.5 rounded-lg bg-gray-700 hover:bg-red-900 text-gray-300 hover:text-red-300 transition-colors disabled:opacity-50"
                  :disabled="actingIds.has(task.id)" @click.stop="patchTask(task.id, { status: 'cancelled', version: task.version })">✖ 取消</button>
              </div>
            </div>
          </template>

          <!-- V29b TASK-B: 👤 需人工 section -->
          <template v-if="filteredExceptions(needsHumanExceptions).length > 0">
            <div class="text-xs text-gray-500 font-semibold px-1 mt-3 mb-1">
              👤 需人工 ({{ filteredExceptions(needsHumanExceptions).length }})
            </div>
            <div
              v-for="(task, idx) in filteredExceptions(needsHumanExceptions)"
              :key="task.id"
              :data-keyboard-index="retryableExceptions.length + idx"
              class="bg-gray-900 border rounded-xl p-4 transition-colors cursor-pointer"
              :class="[
                selectedIds.has(task.id) ? 'border-blue-500/50 bg-blue-500/5' : 'border-gray-700/60 hover:border-red-500/30',
                kbIndex === retryableExceptions.length + idx ? 'ring-2 ring-blue-500' : '',
              ]"
              @click="$router.push(`/tasks/${task.id}`)"
            >
              <div class="flex items-start gap-3 mb-2">
                <input type="checkbox" class="mt-0.5 h-4 w-4 rounded border-gray-600 bg-gray-800 accent-blue-500 cursor-pointer shrink-0"
                  :checked="selectedIds.has(task.id)" @click.stop="toggleSelect(task.id)" />
                <div class="flex-1 min-w-0">
                  <h3 class="font-medium text-gray-100 text-sm mb-1.5 truncate">{{ task.title }}</h3>
                  <div class="flex items-center gap-2 flex-wrap">
                    <span class="text-xs font-medium px-2 py-0.5 rounded-full border" :class="statusBadgeClass(task.status)">{{ task.status }}</span>
                    <span class="text-xs text-gray-500">{{ task.assigned_to }}</span>
                  </div>
                </div>
                <span class="text-gray-600 text-sm shrink-0">›</span>
              </div>
              <div v-if="task.failure_reason || task.result" class="mb-2">
                <div class="text-xs text-gray-400 bg-gray-800/60 rounded-lg p-2.5 border border-gray-700/40"
                  :class="expandedReasons.has(task.id) ? '' : 'line-clamp-2'">
                  {{ task.failure_reason || task.result }}
                </div>
                <button class="text-xs text-gray-600 hover:text-gray-400 mt-1" @click.stop="toggleReason(task.id)">
                  {{ expandedReasons.has(task.id) ? '收起' : '展开' }}
                </button>
              </div>
              <div class="flex gap-2">
                <button class="flex-1 text-xs py-1.5 rounded-lg bg-gray-700 hover:bg-gray-600 text-gray-300 transition-colors disabled:opacity-50"
                  :disabled="actingIds.has(task.id)" @click.stop="retryWithConfirm(task)">🔄 重试</button>
                <button class="flex-1 text-xs py-1.5 rounded-lg bg-gray-700 hover:bg-red-900 text-gray-300 hover:text-red-300 transition-colors disabled:opacity-50"
                  :disabled="actingIds.has(task.id)" @click.stop="patchTask(task.id, { status: 'cancelled', version: task.version })">✖ 取消</button>
              </div>
            </div>
          </template>
        </div>
      </div>
    </div>
  </AppLayout>
</template>
