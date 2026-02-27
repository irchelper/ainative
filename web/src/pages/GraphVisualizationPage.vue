<script setup lang="ts">
import { ref, computed, onMounted } from 'vue'
import { useRouter } from 'vue-router'
import AppLayout from '@/layouts/AppLayout.vue'
import type { Task } from '@/types'

const router = useRouter()

interface GraphData {
  chain_id: string
  tasks: Task[]
}

interface ChainGroup {
  chain_id: string
  tasks: Task[]
}

// All chains (for chain selector)
const chains = ref<ChainGroup[]>([])
const selectedChainId = ref<string>('')
const graphData = ref<GraphData | null>(null)
const loading = ref(false)
const error = ref<string | null>(null)

async function loadChains() {
  const resp = await fetch('/api/chains')
  const data = await resp.json()
  chains.value = (data.chains ?? []).filter((c: ChainGroup) => c.tasks.length > 0)
  if (chains.value.length > 0 && !selectedChainId.value) {
    selectedChainId.value = chains.value[0].chain_id
    await loadGraph(chains.value[0].chain_id)
  }
}

async function loadGraph(chainId: string) {
  loading.value = true
  error.value = null
  try {
    const resp = await fetch(`/api/graph/${encodeURIComponent(chainId)}`)
    if (!resp.ok) throw new Error(`HTTP ${resp.status}`)
    graphData.value = await resp.json()
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    loading.value = false
  }
}

async function selectChain(chainId: string) {
  selectedChainId.value = chainId
  await loadGraph(chainId)
}

onMounted(loadChains)

// Build layers from depends_on (topological sort for display)
const layers = computed<Task[][]>(() => {
  if (!graphData.value?.tasks.length) return []
  const tasks = graphData.value.tasks

  // Build: id → task, and id → inbound deps count
  const taskMap = new Map<string, Task>()
  const inDegree = new Map<string, number>()
  const adjOut = new Map<string, string[]>() // depender → list of tasks that depend on it

  for (const t of tasks) {
    taskMap.set(t.id, t)
    inDegree.set(t.id, 0)
    adjOut.set(t.id, [])
  }
  for (const t of tasks) {
    for (const depId of (t.depends_on ?? [])) {
      inDegree.set(t.id, (inDegree.get(t.id) ?? 0) + 1)
      if (adjOut.has(depId)) {
        adjOut.get(depId)!.push(t.id)
      }
    }
  }

  // Kahn's BFS — build layers
  const layerResult: Task[][] = []
  let queue = tasks.filter((t) => (inDegree.get(t.id) ?? 0) === 0)

  while (queue.length > 0) {
    layerResult.push([...queue])
    const next: Task[] = []
    for (const t of queue) {
      for (const neighborId of (adjOut.get(t.id) ?? [])) {
        const deg = (inDegree.get(neighborId) ?? 0) - 1
        inDegree.set(neighborId, deg)
        if (deg === 0) {
          const neighbor = taskMap.get(neighborId)
          if (neighbor) next.push(neighbor)
        }
      }
    }
    queue = next
  }

  // Fallback: any remaining tasks (shouldn't happen in valid DAG)
  const placed = new Set(layerResult.flat().map((t) => t.id))
  const remaining = tasks.filter((t) => !placed.has(t.id))
  if (remaining.length) layerResult.push(remaining)

  return layerResult
})

function statusColor(status: string): string {
  const map: Record<string, string> = {
    done: 'bg-green-500/20 border-green-500/50 text-green-400',
    failed: 'bg-red-500/20 border-red-500/50 text-red-400',
    in_progress: 'bg-blue-500/20 border-blue-500/50 text-blue-400',
    pending: 'bg-gray-700/60 border-gray-600 text-gray-400',
    cancelled: 'bg-gray-800 border-gray-700 text-gray-600',
    blocked: 'bg-orange-500/20 border-orange-500/50 text-orange-400',
    review: 'bg-purple-500/20 border-purple-500/50 text-purple-400',
    claimed: 'bg-yellow-500/20 border-yellow-500/50 text-yellow-400',
  }
  return map[status] ?? 'bg-gray-700 border-gray-600 text-gray-400'
}

function statusDot(status: string): string {
  const map: Record<string, string> = {
    done: 'bg-green-400',
    failed: 'bg-red-400',
    in_progress: 'bg-blue-400',
    pending: 'bg-gray-500',
    cancelled: 'bg-gray-700',
    blocked: 'bg-orange-400',
    review: 'bg-purple-400',
    claimed: 'bg-yellow-400',
  }
  return map[status] ?? 'bg-gray-500'
}

function chainLabel(chainId: string): string {
  // Use first task title as chain label if available
  const chain = chains.value.find((c) => c.chain_id === chainId)
  if (chain?.tasks.length) return chain.tasks[0].title.slice(0, 30)
  return chainId.slice(0, 16) + '…'
}

// Stats for selected chain
const stats = computed(() => {
  const tasks = graphData.value?.tasks ?? []
  return {
    total: tasks.length,
    done: tasks.filter((t) => t.status === 'done').length,
    failed: tasks.filter((t) => t.status === 'failed').length,
    active: tasks.filter((t) => ['in_progress', 'claimed', 'review'].includes(t.status)).length,
    pending: tasks.filter((t) => t.status === 'pending').length,
  }
})
</script>

<template>
  <AppLayout>
    <div class="p-6">
      <!-- Header -->
      <div class="flex items-center justify-between mb-6">
        <div>
          <h1 class="text-xl font-bold text-gray-100">🕸 DAG 可视化</h1>
          <p class="text-gray-500 text-sm mt-1">任务依赖图（节点=任务，箭头=依赖关系）</p>
        </div>
      </div>

      <!-- Chain selector -->
      <div class="flex gap-2 mb-5 flex-wrap">
        <button
          v-for="chain in chains"
          :key="chain.chain_id"
          class="text-xs px-3 py-1.5 rounded-lg border transition-colors max-w-48 truncate"
          :class="selectedChainId === chain.chain_id
            ? 'border-blue-500/60 bg-blue-500/10 text-blue-400'
            : 'border-gray-700 text-gray-500 hover:border-gray-500 hover:text-gray-300'"
          :title="chain.chain_id"
          @click="selectChain(chain.chain_id)"
        >{{ chainLabel(chain.chain_id) }}</button>
        <span v-if="!chains.length && !loading" class="text-gray-600 text-xs py-1.5">暂无链路数据</span>
      </div>

      <div v-if="loading" class="text-gray-600 text-center py-20">加载中…</div>
      <div v-else-if="error" class="p-3 bg-red-900/40 border border-red-500 rounded text-red-300 text-sm mb-4">{{ error }}</div>

      <template v-else-if="graphData">
        <!-- Stats bar -->
        <div class="flex gap-4 mb-5 text-xs">
          <span class="text-gray-500">共 <span class="text-gray-200">{{ stats.total }}</span> 任务</span>
          <span class="text-green-400">✓ {{ stats.done }} 完成</span>
          <span class="text-blue-400">▶ {{ stats.active }} 进行中</span>
          <span class="text-yellow-400">○ {{ stats.pending }} 待处理</span>
          <span v-if="stats.failed" class="text-red-400">✗ {{ stats.failed }} 失败</span>
        </div>

        <!-- DAG graph: layers displayed left→right -->
        <div class="overflow-x-auto pb-4">
          <div class="flex gap-0 min-w-max">
            <div
              v-for="(layer, layerIdx) in layers"
              :key="layerIdx"
              class="flex flex-col justify-center gap-3"
            >
              <!-- Layer column -->
              <div class="flex flex-col gap-3 px-3">
                <div
                  v-for="task in layer"
                  :key="task.id"
                  class="relative w-44 border rounded-xl p-3 cursor-pointer transition-all hover:shadow-lg hover:scale-[1.02]"
                  :class="statusColor(task.status)"
                  @click="router.push(`/tasks/${task.id}`)"
                >
                  <!-- Status dot -->
                  <div class="flex items-center gap-1.5 mb-1.5">
                    <span class="w-2 h-2 rounded-full shrink-0" :class="statusDot(task.status)"></span>
                    <span class="text-[10px] uppercase tracking-wider opacity-70">{{ task.status }}</span>
                  </div>
                  <!-- Title -->
                  <div class="text-xs font-medium leading-snug mb-2 line-clamp-2 text-gray-100">{{ task.title }}</div>
                  <!-- Agent badge -->
                  <div class="text-[10px] text-gray-500">{{ task.assigned_to === 'human' ? '👤' : '🤖' }} {{ task.assigned_to }}</div>
                </div>
              </div>

              <!-- Arrow between layers -->
              <div
                v-if="layerIdx < layers.length - 1"
                class="flex items-center justify-center text-gray-600 text-lg px-1 self-center"
              >→</div>
            </div>
          </div>
        </div>

        <!-- Legend -->
        <div class="mt-4 flex flex-wrap gap-3 text-xs text-gray-500">
          <span v-for="s in ['pending','claimed','in_progress','review','done','failed','blocked']" :key="s"
            class="flex items-center gap-1">
            <span class="w-2 h-2 rounded-full" :class="statusDot(s)"></span>{{ s }}
          </span>
        </div>
      </template>
    </div>
  </AppLayout>
</template>
