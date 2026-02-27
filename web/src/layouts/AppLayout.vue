<script setup lang="ts">
import { computed, ref } from 'vue'
import { RouterLink, useRoute } from 'vue-router'
import { useI18n } from 'vue-i18n'
import { useDashboardStore } from '@/stores/dashboardStore'
import { toggleLocale } from '@/i18n'

const route = useRoute()
const dashStore = useDashboardStore()
const { t, locale } = useI18n()

const todoCount = computed(() => dashStore.data?.todo.filter(t => t.requires_review === true).length ?? 0)
const exceptionCount = computed(() => dashStore.data?.exceptions.length ?? 0)

const navItems = computed(() => [
  { path: '/', label: t('nav.dashboard'), icon: '🏠' },
  { path: '/goals/new', label: t('nav.goals'), icon: '🎯' },
  { path: '/goals', label: t('nav.tracking'), icon: '📈' },
  { path: '/kanban', label: t('nav.kanban'), icon: '📋' },
  { path: '/graph', label: t('nav.graph'), icon: '🕸' },
  { path: '/stats', label: t('nav.stats'), icon: '📊' },
  { path: '/settings', label: t('nav.settings'), icon: '⚙️' },
])

function isActive(path: string) {
  if (path === '/') return route.path === '/'
  return route.path.startsWith(path)
}

// V22: Mobile hamburger menu
const mobileMenuOpen = ref(false)
function closeMobileMenu() { mobileMenuOpen.value = false }

// V24-A: language toggle
const langLabel = computed(() => locale.value === 'zh' ? 'EN' : '中')
</script>

<template>
  <div class="min-h-screen bg-gray-950 text-gray-100 flex flex-col">
    <!-- Top Bar -->
    <header class="fixed top-0 left-0 right-0 z-50 bg-gray-900 border-b border-gray-800 h-14 flex items-center px-4 md:px-6">
      <!-- Brand -->
      <div class="flex items-center gap-3">
        <div class="w-7 h-7 bg-blue-600 rounded-lg flex items-center justify-center text-xs font-bold shrink-0">AQ</div>
        <span class="font-semibold text-gray-100 hidden sm:block">agent-queue</span>
        <span class="text-gray-600 text-sm hidden md:block">工作台</span>
      </div>

      <!-- Desktop nav -->
      <nav class="hidden md:flex items-center gap-1 ml-8">
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

      <!-- Right badges (desktop) -->
      <div class="ml-auto hidden sm:flex items-center gap-4">
        <div class="flex items-center gap-2">
          <span class="flex items-center gap-1.5 bg-amber-500/15 text-amber-400 text-xs font-semibold px-2.5 py-1 rounded-full border border-amber-500/20">
            🙋 {{ todoCount }}
          </span>
          <span class="flex items-center gap-1.5 bg-red-500/15 text-red-400 text-xs font-semibold px-2.5 py-1 rounded-full border border-red-500/20">
            🔴 {{ exceptionCount }}
          </span>
        </div>
        <div class="w-px h-4 bg-gray-700 hidden md:block"></div>
        <!-- V24-A: language toggle -->
        <button
          class="hidden md:flex items-center text-xs font-medium text-gray-400 hover:text-white bg-gray-800 hover:bg-gray-700 px-2.5 py-1 rounded-md border border-gray-700 transition-colors"
          @click="toggleLocale"
        >{{ langLabel }}</button>
        <div class="hidden md:flex items-center gap-1.5 text-xs text-gray-500">
          <div class="w-1.5 h-1.5 bg-green-500 rounded-full"></div>
        </div>
      </div>

      <!-- Mobile: badges + hamburger -->
      <div class="ml-auto flex items-center gap-2 md:hidden">
        <span v-if="todoCount > 0" class="bg-amber-500 text-gray-900 text-xs font-bold px-1.5 py-0.5 rounded-full">{{ todoCount }}</span>
        <span v-if="exceptionCount > 0" class="bg-red-500 text-white text-xs font-bold px-1.5 py-0.5 rounded-full">{{ exceptionCount }}</span>
        <button
          class="p-2 text-gray-400 hover:text-white transition-colors"
          @click="mobileMenuOpen = !mobileMenuOpen"
        >
          <svg v-if="!mobileMenuOpen" class="w-5 h-5" fill="none" viewBox="0 0 24 24" stroke="currentColor">
            <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M4 6h16M4 12h16M4 18h16"/>
          </svg>
          <svg v-else class="w-5 h-5" fill="none" viewBox="0 0 24 24" stroke="currentColor">
            <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M6 18L18 6M6 6l12 12"/>
          </svg>
        </button>
      </div>
    </header>

    <!-- Mobile dropdown menu -->
    <div
      v-if="mobileMenuOpen"
      class="fixed top-14 left-0 right-0 z-40 bg-gray-900 border-b border-gray-800 md:hidden shadow-lg"
    >
      <nav class="p-3 space-y-1">
        <RouterLink
          v-for="item in navItems"
          :key="item.path"
          :to="item.path"
          class="flex items-center gap-2.5 px-3 py-2.5 rounded-lg text-sm transition-colors"
          :class="isActive(item.path)
            ? 'bg-gray-800 text-white font-medium'
            : 'text-gray-400 hover:bg-gray-800 hover:text-white'"
          @click="closeMobileMenu"
        >
          <span>{{ item.icon }}</span>{{ item.label }}
        </RouterLink>
      </nav>
    </div>

    <!-- Layout body -->
    <div class="flex pt-14 min-h-screen">
      <!-- Sidebar (desktop only) -->
      <aside class="hidden lg:flex w-52 bg-gray-900 border-r border-gray-800 flex-col py-4 px-3 shrink-0 fixed top-14 bottom-0 left-0 overflow-y-auto">
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
              <span>{{ locale === 'zh' ? '任务队列' : 'Queue' }}</span>
              <span class="text-gray-400">{{ (dashStore.data?.stats.total ?? 0) }}</span>
            </div>
            <div class="flex justify-between mb-1">
              <span>{{ locale === 'zh' ? '进行中' : 'Active' }}</span>
              <span class="text-blue-400">{{ (dashStore.data?.stats.in_progress ?? 0) }}</span>
            </div>
            <div class="flex justify-between mb-3">
              <span>{{ locale === 'zh' ? '已完成' : 'Done' }}</span>
              <span class="text-green-400">{{ (dashStore.data?.stats.done ?? 0) }}</span>
            </div>
            <!-- V24-A: language toggle in sidebar -->
            <button
              class="w-full text-center text-xs font-medium text-gray-500 hover:text-white bg-gray-800 hover:bg-gray-700 px-2 py-1 rounded border border-gray-700 transition-colors"
              @click="toggleLocale"
            >{{ locale === 'zh' ? '🌐 English' : '🌐 中文' }}</button>
          </div>
        </div>
      </aside>

      <!-- Page content (lg: offset by sidebar width) -->
      <main class="flex-1 overflow-y-auto lg:ml-52">
        <slot />
      </main>
    </div>
  </div>
</template>
