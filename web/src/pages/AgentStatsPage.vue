<script setup lang="ts">
import { ref, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/layouts/AppLayout.vue'

const { t } = useI18n()

interface AgentStat {
  agent: string
  total_tasks: number
  done_count: number
  failed_count: number
  avg_duration_minutes: number
  success_rate: number
}

const stats = ref<AgentStat[]>([])
const loading = ref(true)
const error = ref<string | null>(null)

async function load() {
  loading.value = true
  error.value = null
  try {
    const resp = await fetch('/api/agents/stats')
    if (!resp.ok) throw new Error(`HTTP ${resp.status}`)
    const data = await resp.json()
    stats.value = data.stats ?? []
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    loading.value = false
  }
}

onMounted(load)

function agentIcon(agent: string): string {
  const map: Record<string, string> = {
    coder: '💻', thinker: '🧠', qa: '🧪', devops: '⚙️',
    writer: '✍️', pm: '📋', security: '🔐', human: '👤',
  }
  return map[agent] ?? '🤖'
}

function agentColor(agent: string): string {
  const map: Record<string, string> = {
    coder: 'border-blue-500/30 bg-blue-500/5',
    thinker: 'border-purple-500/30 bg-purple-500/5',
    qa: 'border-green-500/30 bg-green-500/5',
    devops: 'border-cyan-500/30 bg-cyan-500/5',
    writer: 'border-yellow-500/30 bg-yellow-500/5',
    pm: 'border-pink-500/30 bg-pink-500/5',
    security: 'border-red-500/30 bg-red-500/5',
    human: 'border-amber-500/30 bg-amber-500/5',
  }
  return map[agent] ?? 'border-gray-700 bg-gray-800/40'
}

function successBarColor(rate: number): string {
  if (rate >= 80) return 'bg-green-500'
  if (rate >= 50) return 'bg-yellow-500'
  return 'bg-red-500'
}

function formatDuration(minutes: number): string {
  if (minutes < 1) return `< 1 ${t('stats.minutes')}`
  if (minutes < 60) return `${Math.round(minutes)} ${t('stats.minutes')}`
  const h = Math.floor(minutes / 60)
  const m = Math.round(minutes % 60)
  return `${h}h ${m}m`
}
</script>

<template>
  <AppLayout>
    <div class="p-6">
      <!-- Header -->
      <div class="flex items-center justify-between mb-6">
        <div>
          <h1 class="text-xl font-bold text-gray-100">📊 {{ t('stats.title') }}</h1>
          <p class="text-gray-500 text-sm mt-1">{{ t('stats.subtitle') }}</p>
        </div>
        <button
          class="text-sm text-gray-500 hover:text-gray-300 disabled:opacity-40"
          :disabled="loading"
          @click="load"
        >⟳</button>
      </div>

      <div v-if="loading" class="text-gray-600 text-center py-20">{{ t('common.loading') }}</div>
      <div v-else-if="error" class="p-3 bg-red-900/40 border border-red-500 rounded text-red-300 text-sm">{{ error }}</div>
      <div v-else-if="!stats.length" class="text-gray-600 text-center py-20">{{ t('stats.noData') }}</div>

      <div v-else class="grid grid-cols-1 md:grid-cols-2 xl:grid-cols-3 gap-4">
        <div
          v-for="s in stats"
          :key="s.agent"
          class="border rounded-2xl p-5 transition-all hover:shadow-lg"
          :class="agentColor(s.agent)"
        >
          <!-- Agent name -->
          <div class="flex items-center gap-2 mb-4">
            <span class="text-2xl">{{ agentIcon(s.agent) }}</span>
            <div>
              <div class="font-semibold text-gray-100">{{ s.agent }}</div>
              <div class="text-xs text-gray-500">{{ s.total_tasks }} {{ t('stats.totalTasks') }}</div>
            </div>
          </div>

          <!-- Success rate progress bar -->
          <div class="mb-4">
            <div class="flex items-center justify-between text-xs text-gray-500 mb-1.5">
              <span>{{ t('stats.successRate') }}</span>
              <span :class="s.success_rate >= 80 ? 'text-green-400' : s.success_rate >= 50 ? 'text-yellow-400' : 'text-red-400'">
                {{ s.success_rate.toFixed(0) }}%
              </span>
            </div>
            <div class="h-2 bg-gray-800 rounded-full overflow-hidden">
              <div
                class="h-full rounded-full transition-all duration-500"
                :class="successBarColor(s.success_rate)"
                :style="{ width: s.success_rate + '%' }"
              ></div>
            </div>
          </div>

          <!-- Stats grid -->
          <div class="grid grid-cols-3 gap-2 text-center">
            <div class="bg-gray-800/60 rounded-lg py-2">
              <div class="text-base font-bold text-green-400">{{ s.done_count }}</div>
              <div class="text-[10px] text-gray-500 mt-0.5">{{ t('stats.done') }}</div>
            </div>
            <div class="bg-gray-800/60 rounded-lg py-2">
              <div class="text-base font-bold text-red-400">{{ s.failed_count }}</div>
              <div class="text-[10px] text-gray-500 mt-0.5">{{ t('stats.failed') }}</div>
            </div>
            <div class="bg-gray-800/60 rounded-lg py-2">
              <div class="text-sm font-bold text-gray-300">
                <span :title="s.avg_duration_minutes > 0 ? '' : '暂无历史数据'">
                  {{ s.avg_duration_minutes > 0 ? formatDuration(s.avg_duration_minutes) : '暂无数据' }}
                </span>
              </div>
              <div class="text-[10px] text-gray-500 mt-0.5">{{ t('stats.avgDuration') }}</div>
            </div>
          </div>
        </div>
      </div>
    </div>
  </AppLayout>
</template>
