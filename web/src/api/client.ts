import type { Task, TaskHistory, DashboardData, ConfigData, RetryRoute } from '@/types'

const BASE = ''

async function request<T>(path: string, options?: RequestInit): Promise<T> {
  const resp = await fetch(BASE + path, {
    headers: { 'Content-Type': 'application/json', ...options?.headers },
    ...options,
  })
  if (!resp.ok) {
    const text = await resp.text()
    throw new Error(`HTTP ${resp.status}: ${text}`)
  }
  return resp.json() as Promise<T>
}

// Tasks
export const api = {
  // GET /tasks
  listTasks(params?: { status?: string; assigned_to?: string; limit?: number }) {
    const q = new URLSearchParams()
    if (params?.status) q.set('status', params.status)
    if (params?.assigned_to) q.set('assigned_to', params.assigned_to)
    if (params?.limit) q.set('limit', String(params.limit))
    const qs = q.toString()
    return request<{ tasks: Task[]; total: number }>(`/tasks${qs ? '?' + qs : ''}`)
  },

  // GET /tasks/:id
  getTask(id: string) {
    return request<Task>(`/tasks/${id}`)
  },

  // POST /tasks
  createTask(data: Partial<Task>) {
    return request<Task>('/tasks', { method: 'POST', body: JSON.stringify(data) })
  },

  // PATCH /tasks/:id
  patchTask(id: string, data: Record<string, unknown>) {
    return request<{ task: Task }>(`/tasks/${id}`, { method: 'PATCH', body: JSON.stringify(data) })
  },

  // GET /api/dashboard
  getDashboard() {
    return request<DashboardData>('/api/dashboard')
  },

  // GET /api/timeline/:id
  getTimeline(id: string) {
    return request<{ task: Task; history: TaskHistory[] }>(`/api/timeline/${id}`)
  },

  // GET /api/chains
  getChains() {
    return request<{ chains: { chain_id: string; tasks: Task[] }[] }>('/api/chains')
  },

  // GET /api/config
  getConfig() {
    return request<ConfigData>('/api/config')
  },

  // GET /retry-routing
  getRetryRoutes() {
    return request<{ count: number; routes: RetryRoute[] }>('/retry-routing')
  },

  // GET /health
  health() {
    return request<{ status: string; database: string }>('/health')
  },
}
