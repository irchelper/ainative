<script setup lang="ts">
import { useDashboardStore } from '@/stores/dashboardStore'
import { usePolling } from '@/composables/usePolling'

const store = useDashboardStore()
const { loading, error, refresh } = usePolling(() => store.fetch(), 10_000)
</script>

<template>
  <div class="min-h-screen bg-slate-900 text-slate-100 p-6">
    <header class="mb-6 flex items-center justify-between">
      <h1 class="text-2xl font-bold">🤖 Agent Queue Workbench</h1>
      <button
        class="px-3 py-1 text-sm bg-slate-700 hover:bg-slate-600 rounded"
        :disabled="loading"
        @click="refresh"
      >
        {{ loading ? '刷新中…' : '刷新' }}
      </button>
    </header>

    <div v-if="error" class="mb-4 p-3 bg-red-900/40 border border-red-500 rounded text-red-300">
      {{ error }}
    </div>

    <div v-if="store.data" class="grid grid-cols-3 gap-4 mb-6">
      <div class="bg-slate-800 rounded-lg p-4">
        <div class="text-sm text-slate-400 mb-1">待处理</div>
        <div class="text-3xl font-bold text-yellow-400">{{ store.data.stats.pending }}</div>
      </div>
      <div class="bg-slate-800 rounded-lg p-4">
        <div class="text-sm text-slate-400 mb-1">进行中</div>
        <div class="text-3xl font-bold text-blue-400">{{ store.data.stats.in_progress }}</div>
      </div>
      <div class="bg-slate-800 rounded-lg p-4">
        <div class="text-sm text-slate-400 mb-1">异常</div>
        <div class="text-3xl font-bold text-red-400">{{ store.data.stats.failed }}</div>
      </div>
    </div>

    <div v-if="store.data" class="grid grid-cols-2 gap-6">
      <!-- 待办 -->
      <section>
        <h2 class="text-lg font-semibold mb-3 text-slate-300">🙋 待办任务</h2>
        <div
          v-if="store.data.todo.length === 0"
          class="text-slate-500 text-sm"
        >暂无待办</div>
        <div
          v-for="task in store.data.todo"
          :key="task.id"
          class="bg-slate-800 rounded p-3 mb-2 hover:bg-slate-750 cursor-pointer"
          @click="$router.push(`/tasks/${task.id}`)"
        >
          <div class="flex items-center justify-between">
            <span class="font-medium truncate">{{ task.title }}</span>
            <span class="text-xs px-2 py-0.5 rounded bg-yellow-900 text-yellow-300 ml-2 shrink-0">
              {{ task.status }}
            </span>
          </div>
          <div class="text-xs text-slate-400 mt-1">{{ task.assigned_to }}</div>
        </div>
      </section>

      <!-- 异常 -->
      <section>
        <h2 class="text-lg font-semibold mb-3 text-slate-300">🔴 异常任务</h2>
        <div
          v-if="store.data.exceptions.length === 0"
          class="text-slate-500 text-sm"
        >暂无异常</div>
        <div
          v-for="task in store.data.exceptions"
          :key="task.id"
          class="bg-slate-800 rounded p-3 mb-2 cursor-pointer"
          @click="$router.push(`/tasks/${task.id}`)"
        >
          <div class="flex items-center justify-between">
            <span class="font-medium truncate">{{ task.title }}</span>
            <span class="text-xs px-2 py-0.5 rounded bg-red-900 text-red-300 ml-2 shrink-0">
              {{ task.status }}
            </span>
          </div>
          <div class="text-xs text-slate-400 mt-1">{{ task.failure_reason || task.result }}</div>
        </div>
      </section>
    </div>

    <div v-else-if="loading" class="text-slate-500 text-center py-20">加载中…</div>
  </div>
</template>
