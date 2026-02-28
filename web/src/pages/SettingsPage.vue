<script setup lang="ts">
import { ref, onMounted } from 'vue'
import AppLayout from '@/layouts/AppLayout.vue'

interface Config {
  version: string
  agents: Array<{ name: string; label: string }>
  outbound_webhook_url?: string
  db_path?: string
  pid?: number
  uptime?: string
  listen_addr?: string
}

const config = ref<Config | null>(null)
const loading = ref(true)
const error = ref<string | null>(null)
const cleanupLoading = ref(false)
const cleanupMessage = ref<string | null>(null)

async function load() {
  loading.value = true
  error.value = null
  try {
    const resp = await fetch('/api/config')
    if (!resp.ok) throw new Error(`HTTP ${resp.status}`)
    config.value = await resp.json()
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    loading.value = false
  }
}

onMounted(load)

async function cleanupTestTasks() {
  cleanupLoading.value = true
  cleanupMessage.value = null
  try {
    const resp = await fetch('/api/admin/cleanup-test-tasks', { method: 'DELETE' })
    if (!resp.ok) throw new Error(`HTTP ${resp.status}`)
    const data = await resp.json()
    cleanupMessage.value = `已清理 ${data.deleted_count ?? 0} 条测试任务`
    await load()
  } catch (e) {
    cleanupMessage.value = e instanceof Error ? e.message : String(e)
  } finally {
    cleanupLoading.value = false
  }
}

function maskUrl(url: string): string {
  try {
    const u = new URL(url)
    return u.origin + '/***'
  } catch {
    return url.slice(0, 30) + '…'
  }
}
</script>

<template>
  <AppLayout>
    <div class="max-w-2xl mx-auto px-6 py-10">
      <div class="mb-8">
        <h1 class="text-2xl font-bold text-gray-100 mb-2">⚙️ 设置</h1>
        <p class="text-gray-500 text-sm">系统配置与集成状态</p>
      </div>

      <div v-if="loading" class="text-gray-600 text-center py-20">加载中…</div>
      <div v-else-if="error" class="p-3 bg-red-900/40 border border-red-500 rounded text-red-300 text-sm">{{ error }}</div>

      <div v-else-if="config" class="space-y-4">
        <!-- System info -->
        <div class="bg-gray-900 border border-gray-700 rounded-2xl p-5">
          <h2 class="text-sm font-semibold text-gray-300 mb-4">系统信息</h2>
          <div class="grid grid-cols-2 gap-3 text-sm">
            <div>
              <div class="text-gray-500 text-xs">版本</div>
              <div class="text-gray-200 mt-1 font-mono">{{ config.version }}</div>
            </div>
            <div>
              <div class="text-gray-500 text-xs">已知 Agents</div>
              <div class="text-gray-200 mt-1">{{ config.agents.length }} 个</div>
            </div>
            <div>
              <div class="text-gray-500 text-xs">DB 路径</div>
              <div class="text-gray-200 mt-1 font-mono text-xs truncate" :title="config.db_path ?? ''">{{ config.db_path ?? '-' }}</div>
            </div>
            <div>
              <div class="text-gray-500 text-xs">进程 PID</div>
              <div class="text-gray-200 mt-1 font-mono">{{ config.pid ?? '-' }}</div>
            </div>
            <div>
              <div class="text-gray-500 text-xs">Uptime</div>
              <div class="text-gray-200 mt-1 font-mono">{{ config.uptime ?? '-' }}</div>
            </div>
            <div>
              <div class="text-gray-500 text-xs">监听地址</div>
              <div class="text-gray-200 mt-1 font-mono">{{ config.listen_addr ?? '-' }}</div>
            </div>
          </div>
        </div>

        <!-- Cleanup test data -->
        <div class="bg-gray-900 border border-gray-700 rounded-2xl p-5">
          <h2 class="text-sm font-semibold text-gray-300 mb-2">测试数据清理</h2>
          <p class="text-xs text-gray-500 mb-4">仅清理终态任务（done/failed/cancelled）且 title/assigned_to 匹配测试规则。</p>
          <button
            class="text-sm px-4 py-2 rounded-lg bg-red-600/20 text-red-300 border border-red-500/30 hover:bg-red-600/30 disabled:opacity-50"
            :disabled="cleanupLoading"
            @click="cleanupTestTasks"
          >
            <span v-if="cleanupLoading">清理中…</span>
            <span v-else>🧹 清理测试数据</span>
          </button>
          <div v-if="cleanupMessage" class="text-xs text-gray-400 mt-2">{{ cleanupMessage }}</div>
        </div>

        <!-- Webhook config -->
        <div class="bg-gray-900 border border-gray-700 rounded-2xl p-5">
          <h2 class="text-sm font-semibold text-gray-300 mb-4">Webhook 外发通知</h2>

          <div v-if="config.outbound_webhook_url" class="space-y-3">
            <div class="flex items-center gap-2">
              <div class="w-2 h-2 bg-green-400 rounded-full shrink-0"></div>
              <span class="text-xs text-green-400 font-medium">已启用</span>
            </div>
            <div>
              <div class="text-xs text-gray-500 mb-1">Webhook URL</div>
              <div class="bg-gray-800 rounded-lg px-3 py-2 font-mono text-xs text-gray-300 border border-gray-700">
                {{ maskUrl(config.outbound_webhook_url) }}
              </div>
            </div>
            <div class="bg-blue-900/20 border border-blue-500/20 rounded-lg p-3 text-xs text-blue-300 space-y-1.5">
              <div class="font-medium">触发事件</div>
              <div>• <span class="font-mono">task.done</span> — 任务完成</div>
              <div>• <span class="font-mono">task.failed</span> — 任务失败</div>
              <div>• <span class="font-mono">task.cancelled</span> — 任务取消</div>
            </div>
            <div class="bg-gray-800 rounded-lg p-3 text-xs text-gray-400 space-y-1">
              <div class="font-medium text-gray-300 mb-2">请求格式</div>
              <div class="font-mono text-gray-400">POST {{ maskUrl(config.outbound_webhook_url) }}</div>
              <div class="font-mono text-gray-500">Content-Type: application/json</div>
              <div class="font-mono text-gray-500">X-Signature: sha256=&lt;HMAC-SHA256&gt;</div>
            </div>
          </div>

          <div v-else class="space-y-3">
            <div class="flex items-center gap-2">
              <div class="w-2 h-2 bg-gray-600 rounded-full shrink-0"></div>
              <span class="text-xs text-gray-500">未配置</span>
            </div>
            <div class="bg-gray-800 rounded-lg p-3 text-xs text-gray-500 space-y-1">
              <div>设置环境变量以启用：</div>
              <div class="font-mono text-gray-400 mt-1.5">AGENT_QUEUE_WEBHOOK_URL=https://...</div>
              <div class="font-mono text-gray-400">AGENT_QUEUE_WEBHOOK_SECRET=your-secret</div>
            </div>
          </div>
        </div>

        <!-- Agents list -->
        <div class="bg-gray-900 border border-gray-700 rounded-2xl p-5">
          <h2 class="text-sm font-semibold text-gray-300 mb-4">已知 Agents</h2>
          <div class="grid grid-cols-2 gap-2">
            <div
              v-for="agent in config.agents"
              :key="agent.name"
              class="flex items-center gap-2 bg-gray-800 rounded-lg px-3 py-2"
            >
              <span class="font-mono text-xs text-blue-400">{{ agent.name }}</span>
              <span class="text-xs text-gray-500">{{ agent.label }}</span>
            </div>
          </div>
        </div>
      </div>
    </div>
  </AppLayout>
</template>
