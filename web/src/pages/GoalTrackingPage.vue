<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { api } from '@/api/client'

const chains = ref<{ chain_id: string; tasks: { id: string; title: string; status: string; assigned_to: string }[] }[]>([])
const loading = ref(false)

onMounted(async () => {
  loading.value = true
  try {
    const resp = await api.getChains()
    chains.value = resp.chains ?? []
  } finally {
    loading.value = false
  }
})
</script>

<template>
  <div class="min-h-screen bg-slate-900 text-slate-100 p-6">
    <h1 class="text-2xl font-bold mb-6">🎯 目标追踪</h1>
    <div v-if="loading" class="text-slate-500">加载中…</div>
    <div v-for="chain in chains" :key="chain.chain_id" class="mb-6 bg-slate-800 rounded-lg p-4">
      <div class="text-sm text-slate-400 mb-3 font-mono">{{ chain.chain_id }}</div>
      <div
        v-for="task in chain.tasks"
        :key="task.id"
        class="flex items-center gap-2 py-1"
      >
        <span
          class="w-2 h-2 rounded-full shrink-0"
          :class="{
            'bg-green-400': task.status === 'done',
            'bg-blue-400': task.status === 'in_progress',
            'bg-yellow-400': task.status === 'pending',
            'bg-red-400': task.status === 'failed',
            'bg-slate-500': task.status === 'cancelled',
          }"
        />
        <span class="text-sm flex-1 truncate">{{ task.title }}</span>
        <span class="text-xs text-slate-400">{{ task.assigned_to }}</span>
      </div>
    </div>
    <div v-if="!loading && chains.length === 0" class="text-slate-500">暂无链路任务</div>
  </div>
</template>
