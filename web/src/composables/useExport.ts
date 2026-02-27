import type { Task } from '@/types'

const CSV_COLUMNS: Array<keyof Task> = ['id', 'title', 'status', 'assigned_to', 'priority', 'created_at', 'result']

function escapeCSV(val: unknown): string {
  const s = val == null ? '' : String(val)
  if (s.includes(',') || s.includes('"') || s.includes('\n')) {
    return '"' + s.replace(/"/g, '""') + '"'
  }
  return s
}

function todayStr(): string {
  return new Date().toISOString().slice(0, 10)
}

function downloadBlob(blob: Blob, filename: string) {
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  document.body.appendChild(a)
  a.click()
  document.body.removeChild(a)
  URL.revokeObjectURL(url)
}

export function useExport() {
  function exportCSV(tasks: Task[]) {
    const header = CSV_COLUMNS.join(',')
    const rows = tasks.map(t =>
      CSV_COLUMNS.map(col => escapeCSV(t[col])).join(',')
    )
    const csv = [header, ...rows].join('\n')
    const blob = new Blob([csv], { type: 'text/csv;charset=utf-8;' })
    downloadBlob(blob, `ainative-tasks-${todayStr()}.csv`)
  }

  function exportJSON(tasks: Task[]) {
    const json = JSON.stringify(tasks, null, 2)
    const blob = new Blob([json], { type: 'application/json' })
    downloadBlob(blob, `ainative-tasks-${todayStr()}.json`)
  }

  function exportTasks(tasks: Task[], format: 'csv' | 'json') {
    if (format === 'csv') exportCSV(tasks)
    else exportJSON(tasks)
  }

  return { exportTasks }
}
