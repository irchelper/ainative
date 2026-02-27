<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { useRoute } from 'vue-router'
import { api } from '@/api/client'
import type { Task, TaskHistory } from '@/types'

const route = useRoute()
const task = ref<Task | null>(null)
const history = ref<TaskHistory[]>([])
const loading = ref(false)

onMounted(async () => {
  loading.value = true
  try {
    const resp = await api.getTimeline(route.params.id as string)
    task.value = resp.task
    history.value = resp.history ?? []
  } catch {
    // fallback: just load task
    task.value = await api.getTask(route.params.id as string)
  } finally {
    loading.value = false
  }
})

const statusColor: Record<string, string> = {
  done: 'text-green-400',
  failed: 'text-red-400',
  in_progress: 'text-blue-400',
  pending: 'text-yellow-400',
  cancelled: 'text-slate-400',
  blocked: 'text-orange-400',
  review: 'text-purple-400',
  claimed: 'text-cyan-400',
}
</script>

<template>
  <div class="min-h-screen bg-slate-900 text-slate-100 p-6">
    <button
      class="text-sm text-slate-400 hover:text-white mb-4"
      @click="$router.back()"
    >← 返回</button>

    <div v-if="loading" class="text-slate-500">加载中…</div>
    <template v-else-if="task">
      <div class="bg-slate-800 rounded-lg p-5 mb-6">
        <div class="flex items-start justify-between gap-4">
          <h1 class="text-xl font-bold">{{ task.title }}</h1>
          <span
            class="text-sm font-medium px-3 py-1 rounded shrink-0"
            :class="[statusColor[task.status], 'bg-slate-700']"
          >{{ task.status }}</span>
        </div>
        <div class="mt-3 grid grid-cols-2 gap-2 text-sm text-slate-400">
          <div>执行人：<span class="text-slate-200">{{ task.assigned_to }}</span></div>
          <div>版本：<span class="text-slate-200">v{{ task.version }}</span></div>
          <div>创建：<span class="text-slate-200">{{ new Date(task.created_at).toLocaleString() }}</span></div>
          <div v-if="task.chain_id">链路：<span class="text-slate-200 font-mono text-xs">{{ task.chain_id }}</span></div>
        </div>
        <div v-if="task.description" class="mt-4 text-sm text-slate-300 whitespace-pre-wrap">
          {{ task.description }}
        </div>
        <div v-if="task.result" class="mt-4 p-3 bg-slate-700 rounded text-sm">
          <span class="text-slate-400">结果：</span>{{ task.result }}
        </div>
        <div v-if="task.commit_url" class="mt-2 text-sm">
          <a :href="task.commit_url" target="_blank" class="text-blue-400 hover:underline">
            {{ task.commit_url }}
          </a>
        </div>
      </div>

      <!-- Timeline -->
      <h2 class="text-lg font-semibold mb-3">⏱ 时间线</h2>
      <div class="relative pl-6">
        <div
          v-for="(h, i) in history"
          :key="h.id"
          class="mb-4 relative"
        >
          <div class="absolute -left-6 top-1 w-3 h-3 rounded-full bg-slate-600 border-2 border-slate-400" />
          <div v-if="i < history.length - 1" class="absolute -left-[18px] top-4 bottom-0 border-l border-slate-600" />
          <div class="text-xs text-slate-500 mb-1">
            {{ new Date(h.changed_at).toLocaleString() }}
            <span v-if="h.changed_by" class="ml-2 text-slate-400">by {{ h.changed_by }}</span>
          </div>
          <div class="text-sm">
            <span v-if="h.from_status" class="text-slate-400">{{ h.from_status }} → </span>
            <span :class="statusColor[h.to_status] ?? 'text-slate-200'">{{ h.to_status }}</span>
          </div>
          <div v-if="h.note" class="text-xs text-slate-400 mt-1">{{ h.note }}</div>
        </div>
      </div>
    </template>
  </div>
</template>
