<script setup lang="ts">
import { ref, computed } from 'vue'
import AppLayout from '@/layouts/AppLayout.vue'
import Pagination from '@/components/Pagination.vue'
import { usePolling } from '@/composables/usePolling'
import { usePagination } from '@/composables/usePagination'
import { api } from '@/api/client'
import type { Task } from '@/types'

interface ChainGroup {
  chain_id: string
  tasks: Task[]
}

type TabKey = 'all' | 'active' | 'done' | 'exception'

const chains = ref<ChainGroup[]>([])
const expanded = ref<Set<string>>(new Set())
const activeTab = ref<TabKey>('active')

async function fetchChains() {
  const resp = await api.getChains()
  const fetched: ChainGroup[] = resp.chains ?? []
  chains.value = fetched

  // Auto-expand: on first load, expand the latest active chain
  if (expanded.value.size === 0) {
    const active = fetched.filter(isActiveChain)
    if (active.length > 0) {
      expanded.value.add(active[active.length - 1].chain_id)
    }
  }
}

const { loading, error } = usePolling(fetchChains, 30_000)

// ─── Chain classification ─────────────────────────────────────────────────────
function isActiveChain(c: ChainGroup): boolean {
  return c.tasks.some((t) => ['pending', 'in_progress', 'claimed', 'review', 'blocked'].includes(t.status))
}
function isDoneChain(c: ChainGroup): boolean {
  return c.tasks.every((t) => t.status === 'done' || t.status === 'cancelled')
}
function isExceptionChain(c: ChainGroup): boolean {
  return c.tasks.some((t) => t.status === 'failed')
}

const tabs: { key: TabKey; label: string }[] = [
  { key: 'all',       label: '全部' },
  { key: 'active',    label: '活跃' },
  { key: 'done',      label: '完成' },
  { key: 'exception', label: '异常' },
]

function tabCount(key: TabKey): number {
  if (key === 'all')       return chains.value.length
  if (key === 'active')    return chains.value.filter(isActiveChain).length
  if (key === 'done')      return chains.value.filter(isDoneChain).length
  if (key === 'exception') return chains.value.filter(isExceptionChain).length
  return 0
}

const filteredChains = computed<ChainGroup[]>(() => {
  switch (activeTab.value) {
    case 'active':    return chains.value.filter(isActiveChain)
    case 'done':      return chains.value.filter(isDoneChain)
    case 'exception': return chains.value.filter(isExceptionChain)
    default:          return chains.value
  }
})

// Active chains (no pagination needed — usually small)
const activeChains = computed(() => filteredChains.value.filter(isActiveChain))
// Done chains → paginated
const doneChains = computed(() => filteredChains.value.filter(isDoneChain))
const { page: donePage, totalPages: doneTotalPages, total: doneTotal, items: doneChainPage, goTo: doneGoTo } =
  usePagination(() => doneChains.value, 20)

// ─── Helpers ─────────────────────────────────────────────────────────────────
function doneCount(tasks: Task[]) {
  return tasks.filter((t) => t.status === 'done' || t.status === 'cancelled').length
}
function progressPct(tasks: Task[]) {
  if (!tasks.length) return 0
  return Math.round((doneCount(tasks) / tasks.length) * 100)
}
function isHuman(task: Task) { return task.assigned_to === 'human' }
function taskIcon(task: Task) { return isHuman(task) ? '👤' : '🤖' }

function statusColor(status: string) {
  const map: Record<string, string> = {
    done: 'text-green-400', in_progress: 'text-blue-400', pending: 'text-yellow-400',
    failed: 'text-red-400', blocked: 'text-orange-400', cancelled: 'text-gray-500',
    claimed: 'text-cyan-400', review: 'text-purple-400',
  }
  return map[status] ?? 'text-gray-400'
}

function statusDot(status: string) {
  const map: Record<string, string> = {
    done: 'bg-green-400', in_progress: 'bg-blue-400', pending: 'bg-yellow-400',
    failed: 'bg-red-400', blocked: 'bg-orange-400', cancelled: 'bg-gray-600',
    claimed: 'bg-cyan-400', review: 'bg-purple-400',
  }
  return map[status] ?? 'bg-gray-500'
}

function chainTitle(chain: ChainGroup) {
  return chain.tasks[0]?.title ?? chain.chain_id.slice(-12)
}

function toggleExpand(chainId: string) {
  const s = new Set(expanded.value)
  if (s.has(chainId)) s.delete(chainId)
  else s.add(chainId)
  expanded.value = s
}

function segmentsFor(tasks: Task[]) {
  return tasks.map((t) => ({
    done:   t.status === 'done' || t.status === 'cancelled',
    human:  isHuman(t),
    status: t.status,
  }))
}
</script>

<template>
  <AppLayout>
    <div class="p-6">
      <!-- Header -->
      <div class="flex items-center justify-between mb-5">
        <div>
          <h1 class="text-xl font-bold text-gray-100">📈 目标追踪</h1>
          <p class="text-gray-500 text-sm mt-1">任务链路进度概览</p>
        </div>
        <router-link
          to="/goals/new"
          class="bg-blue-600 hover:bg-blue-500 text-white text-sm font-medium px-4 py-2 rounded-xl transition-colors"
        >+ 新建目标</router-link>
      </div>

      <!-- Tab filter -->
      <div class="flex gap-1 mb-5 border-b border-gray-800 pb-0">
        <button
          v-for="tab in tabs"
          :key="tab.key"
          class="px-4 py-2 text-sm font-medium border-b-2 -mb-px transition-colors"
          :class="activeTab === tab.key
            ? 'border-blue-500 text-blue-400'
            : 'border-transparent text-gray-500 hover:text-gray-300'"
          @click="activeTab = tab.key"
        >
          {{ tab.label }}
          <span
            class="ml-1.5 text-xs px-1.5 py-0.5 rounded-full"
            :class="activeTab === tab.key ? 'bg-blue-500/20 text-blue-300' : 'bg-gray-800 text-gray-500'"
          >{{ tabCount(tab.key) }}</span>
        </button>
      </div>

      <div v-if="error" class="mb-4 p-3 bg-red-900/40 border border-red-500 rounded text-red-300 text-sm">{{ error }}</div>
      <div v-if="loading && !chains.length" class="text-gray-600 text-center py-20">加载中…</div>
      <div v-else-if="!filteredChains.length" class="text-center py-20 text-gray-600">
        <div class="text-4xl mb-3">🎯</div>
        <div class="text-sm mb-4">
          {{ activeTab === 'all' ? '暂无链路任务' : `暂无${tabs.find(t=>t.key===activeTab)?.label}链路` }}
        </div>
        <router-link v-if="activeTab === 'all'" to="/goals/new" class="text-blue-400 text-sm hover:underline">创建第一个目标 →</router-link>
      </div>

      <div v-else class="space-y-6">

        <!-- ── 活跃链路 ── -->
        <div v-if="activeChains.length > 0 && (activeTab === 'all' || activeTab === 'active')">
          <div v-if="activeTab === 'all'" class="text-xs font-semibold text-gray-500 mb-2 px-1 uppercase tracking-wider">
            🟢 活跃中 ({{ activeChains.length }})
          </div>
          <div class="space-y-3">
            <div
              v-for="chain in activeChains"
              :key="chain.chain_id"
              class="bg-gray-900 border border-blue-500/20 rounded-2xl overflow-hidden hover:border-blue-500/40 transition-colors"
            >
              <div
                class="px-5 py-4 cursor-pointer flex items-center gap-4"
                @click="toggleExpand(chain.chain_id)"
              >
                <div class="flex-1 min-w-0">
                  <div class="flex items-center gap-2 mb-2">
                    <h3 class="font-medium text-gray-100 text-sm truncate">{{ chainTitle(chain) }}</h3>
                    <span class="text-xs text-gray-500 shrink-0">{{ doneCount(chain.tasks) }}/{{ chain.tasks.length }}</span>
                  </div>
                  <div class="flex gap-0.5 h-2 rounded-full overflow-hidden bg-gray-800">
                    <div
                      v-for="(seg, i) in segmentsFor(chain.tasks)"
                      :key="i"
                      class="flex-1 rounded-sm transition-all"
                      :class="seg.done
                        ? (seg.human ? 'bg-orange-400' : 'bg-blue-500')
                        : seg.status === 'in_progress'
                          ? (seg.human ? 'bg-orange-400/50' : 'bg-blue-500/50')
                          : 'bg-gray-700'"
                    />
                  </div>
                  <div class="flex items-center gap-3 mt-1.5 text-xs text-gray-600">
                    <span>{{ progressPct(chain.tasks) }}% 完成</span>
                  </div>
                </div>
                <span class="text-gray-500 text-sm transition-transform duration-200" :class="expanded.has(chain.chain_id) ? 'rotate-90' : ''">›</span>
              </div>
              <div v-if="expanded.has(chain.chain_id)" class="border-t border-gray-800">
                <div
                  v-for="task in chain.tasks"
                  :key="task.id"
                  class="flex items-center gap-3 px-5 py-3 hover:bg-gray-800/50 border-b border-gray-800/50 last:border-0 cursor-pointer"
                  @click="$router.push(`/tasks/${task.id}`)"
                >
                  <span class="text-sm shrink-0">{{ taskIcon(task) }}</span>
                  <div class="w-2 h-2 rounded-full shrink-0" :class="statusDot(task.status)" />
                  <span class="text-sm text-gray-200 flex-1 truncate">{{ task.title }}</span>
                  <span class="text-xs shrink-0" :class="statusColor(task.status)">{{ task.status }}</span>
                  <span class="text-xs text-gray-600 shrink-0 w-20 text-right truncate">{{ task.assigned_to }}</span>
                </div>
              </div>
            </div>
          </div>
        </div>

        <!-- ── 已完成链路（折叠区块 + 分页） ── -->
        <div v-if="doneChains.length > 0 && (activeTab === 'all' || activeTab === 'done')">
          <div class="text-xs font-semibold text-gray-500 mb-2 px-1 uppercase tracking-wider">
            ✅ 已完成 ({{ doneTotal }})
          </div>
          <div class="space-y-2">
            <div
              v-for="chain in doneChainPage"
              :key="chain.chain_id"
              class="bg-gray-900/60 border border-gray-800 rounded-2xl overflow-hidden hover:border-gray-700 transition-colors"
            >
              <div
                class="px-5 py-3 cursor-pointer flex items-center gap-4"
                @click="toggleExpand(chain.chain_id)"
              >
                <div class="flex-1 min-w-0">
                  <div class="flex items-center gap-2">
                    <h3 class="font-medium text-gray-400 text-sm truncate">{{ chainTitle(chain) }}</h3>
                    <span class="text-xs text-gray-600 shrink-0">{{ chain.tasks.length }} 任务</span>
                    <span class="text-xs text-green-500/80 shrink-0">100%</span>
                  </div>
                </div>
                <span class="text-gray-600 text-sm transition-transform duration-200" :class="expanded.has(chain.chain_id) ? 'rotate-90' : ''">›</span>
              </div>
              <div v-if="expanded.has(chain.chain_id)" class="border-t border-gray-800">
                <div
                  v-for="task in chain.tasks"
                  :key="task.id"
                  class="flex items-center gap-3 px-5 py-2.5 hover:bg-gray-800/40 border-b border-gray-800/50 last:border-0 cursor-pointer"
                  @click="$router.push(`/tasks/${task.id}`)"
                >
                  <span class="text-sm shrink-0">{{ taskIcon(task) }}</span>
                  <div class="w-2 h-2 rounded-full shrink-0" :class="statusDot(task.status)" />
                  <span class="text-sm text-gray-400 flex-1 truncate">{{ task.title }}</span>
                  <span class="text-xs shrink-0" :class="statusColor(task.status)">{{ task.status }}</span>
                </div>
              </div>
            </div>
          </div>
          <Pagination
            :page="donePage"
            :total-pages="doneTotalPages"
            :total="doneTotal"
            @go="doneGoTo"
          />
        </div>

        <!-- ── 异常链路 ── -->
        <div v-if="(activeTab === 'exception') && filteredChains.filter(isExceptionChain).length">
          <div class="text-xs font-semibold text-gray-500 mb-2 px-1 uppercase tracking-wider">
            🔴 异常 ({{ filteredChains.filter(isExceptionChain).length }})
          </div>
          <div class="space-y-3">
            <div
              v-for="chain in filteredChains.filter(isExceptionChain)"
              :key="chain.chain_id"
              class="bg-gray-900 border border-red-500/20 rounded-2xl overflow-hidden hover:border-red-500/40 transition-colors"
            >
              <div
                class="px-5 py-4 cursor-pointer flex items-center gap-4"
                @click="toggleExpand(chain.chain_id)"
              >
                <div class="flex-1 min-w-0">
                  <div class="flex items-center gap-2 mb-1">
                    <h3 class="font-medium text-gray-100 text-sm truncate">{{ chainTitle(chain) }}</h3>
                    <span class="text-xs text-red-400 shrink-0">
                      {{ chain.tasks.filter(t => t.status === 'failed').length }} 失败
                    </span>
                  </div>
                </div>
                <span class="text-gray-500 text-sm transition-transform duration-200" :class="expanded.has(chain.chain_id) ? 'rotate-90' : ''">›</span>
              </div>
              <div v-if="expanded.has(chain.chain_id)" class="border-t border-gray-800">
                <div
                  v-for="task in chain.tasks"
                  :key="task.id"
                  class="flex items-center gap-3 px-5 py-3 hover:bg-gray-800/50 border-b border-gray-800/50 last:border-0 cursor-pointer"
                  @click="$router.push(`/tasks/${task.id}`)"
                >
                  <span class="text-sm shrink-0">{{ taskIcon(task) }}</span>
                  <div class="w-2 h-2 rounded-full shrink-0" :class="statusDot(task.status)" />
                  <span class="text-sm text-gray-200 flex-1 truncate">{{ task.title }}</span>
                  <span class="text-xs shrink-0" :class="statusColor(task.status)">{{ task.status }}</span>
                </div>
              </div>
            </div>
          </div>
        </div>
      </div>
    </div>
  </AppLayout>
</template>
