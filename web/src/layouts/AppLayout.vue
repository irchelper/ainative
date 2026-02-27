<script setup lang="ts">
import { computed } from 'vue'
import { RouterLink, useRoute } from 'vue-router'
import { useDashboardStore } from '@/stores/dashboardStore'

const route = useRoute()
const dashStore = useDashboardStore()

const todoCount = computed(() => dashStore.data?.todo.length ?? 0)
const exceptionCount = computed(() => dashStore.data?.exceptions.length ?? 0)

const navItems = [
  { path: '/', label: 'Dashboard', icon: '🏠' },
  { path: '/goals/new', label: '目标输入', icon: '🎯' },
  { path: '/goals', label: '目标追踪', icon: '📈' },
  { path: '/kanban', label: '看板', icon: '📋' },
  { path: '/graph', label: 'DAG', icon: '🕸' },
]

function isActive(path: string) {
  if (path === '/') return route.path === '/'
  return route.path.startsWith(path)
}
</script>

<template>
  <div class="min-h-screen bg-gray-950 text-gray-100 flex flex-col">
    <!-- Top Bar -->
    <header class="fixed top-0 left-0 right-0 z-50 bg-gray-900 border-b border-gray-800 h-14 flex items-center px-6">
      <div class="flex items-center gap-3">
        <div class="w-7 h-7 bg-blue-600 rounded-lg flex items-center justify-center text-xs font-bold">AQ</div>
        <span class="font-semibold text-gray-100">agent-queue</span>
        <span class="text-gray-600 text-sm">工作台</span>
      </div>
      <nav class="flex items-center gap-1 ml-8">
        <RouterLink
          v-for="item in navItems"
          :key="item.path"
          :to="item.path"
          class="px-3 py-1.5 rounded-md text-sm transition-colors"
          :class="isActive(item.path)
            ? 'bg-gray-800 text-white font-medium'
            : 'text-gray-400 hover:text-white hover:bg-gray-800'"
        >{{ item.label }}</RouterLink>
      </nav>
      <div class="ml-auto flex items-center gap-4">
        <div class="flex items-center gap-2">
          <span class="flex items-center gap-1.5 bg-amber-500/15 text-amber-400 text-xs font-semibold px-2.5 py-1 rounded-full border border-amber-500/20">
            🙋 {{ todoCount }} 待办
          </span>
          <span class="flex items-center gap-1.5 bg-red-500/15 text-red-400 text-xs font-semibold px-2.5 py-1 rounded-full border border-red-500/20">
            🔴 {{ exceptionCount }} 异常
          </span>
        </div>
        <div class="w-px h-4 bg-gray-700"></div>
        <div class="flex items-center gap-1.5 text-xs text-gray-500">
          <div class="w-1.5 h-1.5 bg-green-500 rounded-full"></div>
          <span>已连接</span>
        </div>
      </div>
    </header>

    <!-- Layout body -->
    <div class="flex pt-14 h-screen">
      <!-- Sidebar -->
      <aside class="w-56 bg-gray-900 border-r border-gray-800 flex flex-col py-4 px-3 shrink-0">
        <div class="space-y-1">
          <RouterLink
            v-for="item in navItems"
            :key="item.path"
            :to="item.path"
            class="w-full flex items-center gap-2.5 px-3 py-2 rounded-lg text-sm transition-colors"
            :class="isActive(item.path)
              ? 'bg-gray-800 font-medium text-white'
              : 'text-gray-400 hover:bg-gray-800 hover:text-white'"
          >
            <span class="text-base">{{ item.icon }}</span>
            {{ item.label }}
            <span
              v-if="item.path === '/' && todoCount > 0"
              class="ml-auto bg-amber-500 text-gray-900 text-xs font-bold px-1.5 py-0.5 rounded-full"
            >{{ todoCount }}</span>
          </RouterLink>
        </div>
        <div class="mt-auto pt-4 border-t border-gray-800">
          <div class="px-3 py-2 text-xs text-gray-600">
            <div class="flex justify-between mb-1">
              <span>任务队列</span>
              <span class="text-gray-400">{{ (dashStore.data?.stats.total ?? 0) }}</span>
            </div>
            <div class="flex justify-between mb-1">
              <span>进行中</span>
              <span class="text-blue-400">{{ (dashStore.data?.stats.in_progress ?? 0) }}</span>
            </div>
            <div class="flex justify-between">
              <span>已完成</span>
              <span class="text-green-400">{{ (dashStore.data?.stats.done ?? 0) }}</span>
            </div>
          </div>
        </div>
      </aside>

      <!-- Page content -->
      <main class="flex-1 overflow-y-auto">
        <slot />
      </main>
    </div>
  </div>
</template>
