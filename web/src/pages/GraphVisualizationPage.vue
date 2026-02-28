<script setup lang="ts">
import { ref, computed, onMounted } from 'vue'
import AppLayout from '@/layouts/AppLayout.vue'
import DagGraph from '@/components/DagGraph.vue'
import type { Task } from '@/types'

interface ChainGroup {
  chain_id: string
  tasks: Task[]
}

const chains = ref<ChainGroup[]>([])
const selectedChainId = ref<string>('')
const loading = ref(false)
const error = ref<string | null>(null)

async function loadChains() {
  loading.value = true
  error.value = null
  try {
    const resp = await fetch('/api/chains')
    const data = await resp.json()
    chains.value = (data.chains ?? []).filter((c: ChainGroup) => c.tasks.length > 0)
    if (chains.value.length > 0 && !selectedChainId.value) {
      selectedChainId.value = chains.value[0].chain_id
    }
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    loading.value = false
  }
}

async function selectChain(chainId: string) {
  selectedChainId.value = chainId
}

onMounted(loadChains)

const selectedTasks = computed<Task[]>(() => {
  if (!selectedChainId.value) return []
  return chains.value.find((c) => c.chain_id === selectedChainId.value)?.tasks ?? []
})

const stats = computed(() => {
  const tasks = selectedTasks.value
  return {
    total:   tasks.length,
    done:    tasks.filter((t) => t.status === 'done').length,
    active:  tasks.filter((t) => ['in_progress', 'claimed', 'review'].includes(t.status)).length,
    pending: tasks.filter((t) => t.status === 'pending').length,
    failed:  tasks.filter((t) => t.status === 'failed').length,
  }
})

function chainLabel(chain: ChainGroup): string {
  return chain.tasks[0]?.title?.slice(0, 32) ?? chain.chain_id.slice(-16) + '…'
}
</script>

<template>
  <AppLayout>
    <div class="flex flex-col h-[calc(100vh-56px)] p-4 gap-4">
      <!-- Header -->
      <div class="flex items-center justify-between shrink-0">
        <div>
          <h1 class="text-xl font-bold text-gray-100">🕸 DAG 可视化</h1>
          <p class="text-gray-500 text-sm">任务依赖图 — 节点=任务，边=依赖关系</p>
        </div>
        <button
          class="text-xs text-gray-500 hover:text-gray-300 border border-gray-700 px-2.5 py-1.5 rounded-lg"
          :disabled="loading"
          @click="loadChains"
        >⟳ 刷新</button>
      </div>

      <!-- Chain selector -->
      <div class="shrink-0 flex gap-2 flex-wrap">
        <button
          v-for="chain in chains"
          :key="chain.chain_id"
          class="text-xs px-3 py-1.5 rounded-lg border transition-colors max-w-48 truncate"
          :class="selectedChainId === chain.chain_id
            ? 'border-blue-500/60 bg-blue-500/10 text-blue-400'
            : 'border-gray-700 text-gray-500 hover:border-gray-500 hover:text-gray-300'"
          :title="chain.chain_id"
          @click="selectChain(chain.chain_id)"
        >{{ chainLabel(chain) }}</button>
        <span v-if="!chains.length && !loading" class="text-gray-600 text-xs py-1.5">暂无链路数据</span>
      </div>

      <!-- Error -->
      <div v-if="error" class="shrink-0 p-3 bg-red-900/40 border border-red-500 rounded text-red-300 text-sm">{{ error }}</div>

      <!-- Stats bar -->
      <div v-if="selectedTasks.length" class="shrink-0 flex gap-4 text-xs">
        <span class="text-gray-500">共 <span class="text-gray-200">{{ stats.total }}</span> 任务</span>
        <span class="text-green-400">✓ {{ stats.done }} 完成</span>
        <span class="text-blue-400">▶ {{ stats.active }} 进行中</span>
        <span class="text-yellow-400">○ {{ stats.pending }} 待处理</span>
        <span v-if="stats.failed" class="text-red-400">✗ {{ stats.failed }} 失败</span>
      </div>

      <!-- DAG graph (fills remaining height) -->
      <div class="flex-1 bg-gray-900 border border-gray-800 rounded-2xl overflow-hidden min-h-0">
        <DagGraph
          :tasks="selectedTasks"
          :loading="loading && !selectedTasks.length"
        />
      </div>

      <!-- Legend -->
      <div class="shrink-0 flex flex-wrap gap-3 text-xs text-gray-500 pb-1">
        <span v-for="s in ['pending','claimed','in_progress','review','done','failed','blocked']" :key="s"
          class="flex items-center gap-1.5">
          <span
            class="w-2 h-2 rounded-full"
            :style="{ background: { pending:'#6b7280', claimed:'#60a5fa', in_progress:'#3b82f6', review:'#a855f7', done:'#22c55e', failed:'#ef4444', blocked:'#f97316' }[s] ?? '#6b7280' }"
          ></span>{{ s }}
        </span>
      </div>
    </div>
  </AppLayout>
</template>
