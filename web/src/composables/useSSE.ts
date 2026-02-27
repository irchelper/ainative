import { onUnmounted, ref } from 'vue'

export interface SSEEvent {
  type: 'task_updated' | 'task_created' | 'task_failed' | 'chain_completed'
  task_id?: string
  status?: string
}

export type SSEEventHandler = (event: SSEEvent) => void

interface UseSSEOptions {
  /** Polling interval (ms) used as fallback when SSE is unavailable. Default: 30s */
  fallbackInterval?: number
  /** SSE reconnect delay (ms). Default: 3s */
  reconnectDelay?: number
  /** Maximum reconnect attempts before giving up (-1 = infinite). Default: -1 */
  maxRetries?: number
}

/**
 * useSSE — connects to GET /api/events SSE stream.
 * Auto-reconnects on disconnect. Falls back to polling when SSE is unavailable.
 *
 * Usage:
 *   const { connected } = useSSE((event) => {
 *     if (event.type === 'task_updated') refresh()
 *   })
 */
export function useSSE(
  onEvent: SSEEventHandler,
  options: UseSSEOptions = {},
): { connected: ReturnType<typeof ref<boolean>> } {
  const {
    fallbackInterval = 30_000,
    reconnectDelay = 3_000,
    maxRetries = -1,
  } = options

  const connected = ref(false)
  let es: EventSource | null = null
  let retryCount = 0
  let reconnectTimer: ReturnType<typeof setTimeout> | null = null
  let fallbackTimer: ReturnType<typeof setInterval> | null = null
  let destroyed = false

  function clearFallback() {
    if (fallbackTimer !== null) {
      clearInterval(fallbackTimer)
      fallbackTimer = null
    }
  }

  function startFallback() {
    if (fallbackTimer !== null) return
    fallbackTimer = setInterval(() => {
      onEvent({ type: 'task_updated' }) // generic refresh signal
    }, fallbackInterval)
  }

  function connect() {
    if (destroyed) return

    // EventSource is not available in all environments (e.g. older browsers).
    if (typeof EventSource === 'undefined') {
      startFallback()
      return
    }

    es = new EventSource('/api/events')

    es.onopen = () => {
      connected.value = true
      retryCount = 0
      clearFallback()
    }

    es.onmessage = (e: MessageEvent) => {
      try {
        const event = JSON.parse(e.data) as SSEEvent
        onEvent(event)
      } catch {
        // Ignore malformed events (e.g. keep-alive comments are not delivered here).
      }
    }

    es.onerror = () => {
      connected.value = false
      es?.close()
      es = null

      if (maxRetries !== -1 && retryCount >= maxRetries) {
        startFallback()
        return
      }
      retryCount++

      if (!destroyed) {
        reconnectTimer = setTimeout(connect, reconnectDelay)
      }
    }
  }

  function disconnect() {
    destroyed = true
    if (reconnectTimer !== null) {
      clearTimeout(reconnectTimer)
      reconnectTimer = null
    }
    clearFallback()
    es?.close()
    es = null
    connected.value = false
  }

  connect()
  onUnmounted(disconnect)

  return { connected }
}
