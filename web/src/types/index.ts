export type TaskStatus =
  | 'pending'
  | 'claimed'
  | 'in_progress'
  | 'review'
  | 'done'
  | 'failed'
  | 'blocked'
  | 'cancelled'

export interface Task {
  id: string
  title: string
  description?: string
  status: TaskStatus
  assigned_to: string
  depends_on?: string[]
  retry_assigned_to?: string
  superseded_by?: string
  chain_id?: string
  notify_ceo_on_complete?: boolean
  stale_dispatch_count?: number
  parent_id?: string
  mode?: string
  requires_review?: boolean
  result?: string
  failure_reason?: string
  version: number
  priority?: number
  timeout_minutes?: number | null
  timeout_action?: string | null
  commit_url?: string | null
  started_at?: string | null
  created_at: string
  updated_at: string
}

export interface TaskHistory {
  id: number
  task_id: string
  from_status?: string
  to_status: string
  changed_by?: string
  note?: string
  changed_at: string
}

export interface RetryRoute {
  id: number
  assigned_to: string
  error_keyword: string
  retry_assigned_to: string
  priority: number
}

export interface ChainTask {
  chain_id: string
  tasks: Task[]
}

export interface DashboardData {
  todo: Task[]
  exceptions: Task[]
  stats: {
    total: number
    pending: number
    in_progress: number
    done: number
    failed: number
    blocked: number
  }
}

export interface Agent {
  name: string
  label: string
  session_key?: string
}

export interface ConfigData {
  agents: Agent[]
  version: string
}
