<script setup lang="ts">
import { ref, onMounted, computed } from 'vue'
import { api } from '@/api/client'
import type { Task, TaskStatus } from '@/types'

const allTasks = ref<Task[]>([])
const loading = ref(false)

const columns: { key: TaskStatus; label: string }[] = [
  { key: 'pending', label: '待处理' },
  { key: 'claimed', label: '已认领' },
  { key: 'in_progress', label: '进行中' },
  { key: 'review', label: '审核中' },
  { key: 'done', label: '完成' },
  { key: 'failed', label: '失败' },
  { key: 'blocked', label: '阻塞' },
]

const tasksByStatus = computed(() => {
  const map: Partial<Record<TaskStatus, Task[]>> = {}
  for (const col of columns) {
    map[col.key] = allTasks.value.filter((t) => t.status === col.key)
  }
  return map
})

onMounted(async () => {
  loading.value = true
  try {
    const resp = await api.listTasks({ limit: 200 })
    allTasks.value = resp.tasks ?? []
  } finally {
    loading.value = false
  }
})
</script>

<template>
  <div class="min-h-screen bg-slate-900 text-slate-100 p-6">
    <h1 class="text-2xl font-bold mb-6">📋 看板</h1>
    <div v-if="loading" class="text-slate-500">加载中…</div>
    <div v-else class="flex gap-4 overflow-x-auto pb-4">
      <div
        v-for="col in columns"
        :key="col.key"
        class="bg-slate-800 rounded-lg p-3 min-w-[200px] flex-shrink-0"
      >
        <div class="font-semibold text-sm mb-3 text-slate-300">
          {{ col.label }}
          <span class="ml-1 text-xs text-slate-500">({{ tasksByStatus[col.key]?.length ?? 0 }})</span>
        </div>
        <div
          v-for="task in tasksByStatus[col.key]"
          :key="task.id"
          class="bg-slate-700 rounded p-2 mb-2 text-sm cursor-pointer hover:bg-slate-600"
          @click="$router.push(`/tasks/${task.id}`)"
        >
          <div class="truncate font-medium">{{ task.title }}</div>
          <div class="text-xs text-slate-400 mt-1">{{ task.assigned_to }}</div>
        </div>
      </div>
    </div>
  </div>
</template>
