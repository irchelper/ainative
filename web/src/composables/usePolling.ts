import { ref, onMounted, onUnmounted } from 'vue'

interface PollingOptions {
  immediate?: boolean
  onError?: (err: unknown) => void
}

export function usePolling(
  fetchFn: () => Promise<void>,
  intervalMs: number,
  options: PollingOptions = {},
) {
  const loading = ref(false)
  const error = ref<string | null>(null)
  let timer: ReturnType<typeof setInterval> | null = null
  let backoffMs = intervalMs

  async function run() {
    if (loading.value) return
    loading.value = true
    error.value = null
    try {
      await fetchFn()
      backoffMs = intervalMs // reset backoff on success
    } catch (err) {
      error.value = err instanceof Error ? err.message : String(err)
      options.onError?.(err)
      backoffMs = Math.min(backoffMs * 2, intervalMs * 8) // exponential backoff, max 8x
    } finally {
      loading.value = false
    }
  }

  function start() {
    if (timer) return
    if (options.immediate !== false) {
      run()
    }
    timer = setInterval(run, intervalMs)
  }

  function stop() {
    if (timer) {
      clearInterval(timer)
      timer = null
    }
  }

  // Pause when tab is hidden
  function handleVisibility() {
    if (document.hidden) {
      stop()
    } else {
      start()
    }
  }

  onMounted(() => {
    document.addEventListener('visibilitychange', handleVisibility)
    start()
  })

  onUnmounted(() => {
    document.removeEventListener('visibilitychange', handleVisibility)
    stop()
  })

  return { loading, error, refresh: run }
}
