import { defineStore } from 'pinia'
import { ref } from 'vue'
import { api } from '@/api/client'
import type { Task } from '@/types'

export const useTaskStore = defineStore('task', () => {
  const tasks = ref<Task[]>([])
  const total = ref(0)
  const loading = ref(false)
  const error = ref<string | null>(null)

  async function fetchAll(params?: { status?: string; assigned_to?: string; limit?: number }) {
    loading.value = true
    error.value = null
    try {
      const resp = await api.listTasks(params)
      tasks.value = resp.tasks ?? []
      total.value = resp.total ?? 0
    } catch (err) {
      error.value = err instanceof Error ? err.message : String(err)
    } finally {
      loading.value = false
    }
  }

  async function patch(id: string, data: Record<string, unknown>) {
    const resp = await api.patchTask(id, data)
    const idx = tasks.value.findIndex((t) => t.id === id)
    if (idx >= 0) tasks.value[idx] = resp.task
    return resp.task
  }

  return { tasks, total, loading, error, fetchAll, patch }
})
